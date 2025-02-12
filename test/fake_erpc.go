package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

type ServerConfig struct {
	Port             int
	FailureRate      float64
	MinDelay         time.Duration
	MaxDelay         time.Duration
	SampleFile       string
	AdditionalConfig *common.UpstreamConfig
}

type StressTestConfig struct {
	ServicePort             int
	MetricsPort             int
	MaxRPS                  int
	ServerConfigs           []ServerConfig
	AdditionalProjectConfig *common.ProjectConfig
	AdditionalNetworkConfig *common.NetworkConfig
	AdditionalConfig        *common.Config
	Duration                string
	VUs                     int
}

type ServerStats struct {
	RequestsHandled int64
	RequestsSuccess int64
	RequestsFailed  int64
}

type CounterMetric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

type StressTestResult struct {
	CounterMetrics []*CounterMetric
}

func (s *StressTestResult) SumCounter(name string, groupBy []string) []*CounterMetric {
	result := []*CounterMetric{}
	groupMap := make(map[string]*CounterMetric)

	for _, metric := range s.CounterMetrics {
		if metric.Name != name {
			continue
		}

		groupKey := ""
		groupLabels := make(map[string]string)

		if len(groupBy) == 0 {
			groupKey = "overall"
		} else {
			keyParts := []string{}
			for _, label := range groupBy {
				if value, exists := metric.Labels[label]; exists {
					keyParts = append(keyParts, value)
					groupLabels[label] = value
				}
			}
			groupKey = strings.Join(keyParts, "|")
		}

		if existingMetric, exists := groupMap[groupKey]; exists {
			existingMetric.Value += metric.Value
		} else {
			newMetric := &CounterMetric{
				Name:   name,
				Labels: groupLabels,
				Value:  metric.Value,
			}
			groupMap[groupKey] = newMetric
			result = append(result, newMetric)
		}
	}

	return result
}

func createFakeServers(configs []ServerConfig) []*FakeServer {
	var fakeServers []*FakeServer
	for _, config := range configs {
		server, err := NewFakeServer(
			config.Port,
			config.FailureRate,
			config.MinDelay,
			config.MaxDelay,
			config.SampleFile,
		)
		if err != nil {
			log.Error().Err(err).Int("port", config.Port).Msg("Error creating fake server")
			continue
		}
		fakeServers = append(fakeServers, server)
	}
	return fakeServers
}

func startFakeServer(wg *sync.WaitGroup, server *FakeServer) {
	defer wg.Done()
	log.Info().Int("port", server.Port).Msg("Starting fake server")
	if err := server.Start(); err != nil && !strings.Contains(err.Error(), "Fake server closed") {
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Int("port", server.Port).Msg("Error starting fake server")
		}
	}
}

func loadSamples(filename string) ([]RequestResponseSample, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read sample file: %w", err)
	}

	var samples []RequestResponseSample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("failed to unmarshal samples: %w", err)
	}

	return samples, nil
}

func executeStressTest(config StressTestConfig) (*StressTestResult, error) {
	// Create fake servers
	fakeServers := createFakeServers(config.ServerConfigs)

	// Start all fake servers
	var wg sync.WaitGroup
	for _, server := range fakeServers {
		wg.Add(1)
		go startFakeServer(&wg, server)
	}

	fs := afero.NewOsFs()

	// Prepare eRPC configuration
	erpcConfig, localBaseUrl, err := prepareERPCConfig(fs, config)
	if err != nil {
		return nil, err
	}

	// Initialize eRPC
	shutdown, err := initializeERPC(fs, erpcConfig)
	if err != nil {
		return nil, err
	}
	if shutdown != nil {
		defer shutdown()
	}

	// Wait for servers to start
	time.Sleep(1 * time.Second)

	// Run stress test
	err = runK6StressTest(fs, localBaseUrl, config)
	if err != nil {
		return nil, err
	}

	// Stop all servers
	for _, server := range fakeServers {
		if err := server.Stop(); err != nil {
			log.Error().Err(err).Int("port", server.Port).Msg("Error stopping server")
		}
	}

	// Wait for all servers to finish
	wg.Wait()

	// Fetch prometheus metrics used for assertions
	return fetchPrometheusMetrics(config.MetricsPort)
}

func prepareERPCConfig(fs afero.Fs, config StressTestConfig) (string, string, error) {
	localBaseUrl := fmt.Sprintf("http://localhost:%d", config.ServicePort)

	upsList := []*common.UpstreamConfig{}
	for _, serverConfig := range config.ServerConfigs {
		ucfg := &common.UpstreamConfig{
			Id:       fmt.Sprintf("server-%d", serverConfig.Port),
			Endpoint: fmt.Sprintf("http://localhost:%d", serverConfig.Port),
			Type:     "evm",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		if serverConfig.AdditionalConfig != nil {
			ucfg = MergeStructs(ucfg, serverConfig.AdditionalConfig)
		}
		upsList = append(upsList, ucfg)
	}

	prjCfg := &common.ProjectConfig{
		Id:        "main",
		Upstreams: upsList,
	}
	if config.AdditionalProjectConfig != nil {
		prjCfg = MergeStructs(prjCfg, config.AdditionalProjectConfig)
	}

	mergedConfig := &common.Config{
		LogLevel: "DEBUG",
		Server: &common.ServerConfig{
			HttpHost: "localhost",
			HttpPort: config.ServicePort,
		},
		Metrics: &common.MetricsConfig{
			Enabled: true,
			Host:    "localhost",
			Port:    config.MetricsPort,
		},
		Projects: []*common.ProjectConfig{prjCfg},
	}

	if config.AdditionalConfig != nil {
		mergedConfig = MergeStructs(mergedConfig, config.AdditionalConfig)
	}

	cfgYaml, err := yaml.Marshal(mergedConfig)
	os.Stdout.Write(cfgYaml)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal merged config: %w", err)
	}
	cfg, err := createTempFile(fs, "erpc*.yaml", string(cfgYaml))
	if err != nil {
		return "", "", err
	}

	return cfg.Name(), localBaseUrl, nil
}

// func generateUpstreamConfig(configs []ServerConfig) string {
// 	var upstreamsCfg string
// 	for _, config := range configs {
// 		upstreamsCfg += fmt.Sprintf(`
//     - id: server-%d
//       endpoint: http://localhost:%d
//       type: evm
//       evm:
//         chainId: 123
// `, config.Port, config.Port)
// 	}
// 	return upstreamsCfg
// }

func initializeERPC(fs afero.Fs, configPath string) (func() error, error) {
	args := []string{"erpc-test", configPath}
	logger := log.With().Logger()
	shutdown, err := erpc.Init(context.Background(), &logger, fs, args)
	return shutdown, err
}

func runK6StressTest(fs afero.Fs, baseUrl string, config StressTestConfig) error {
	// Load all samples
	allSamples, err := loadAllSamples(config.ServerConfigs)
	if err != nil {
		return err
	}

	// Create k6 script
	script := createK6Script(baseUrl, allSamples, config)

	// Write script to temporary file
	tmpfile, err := createTempFile(fs, "k6script*.js", script)
	if err != nil {
		return err
	}
	defer fs.Remove(tmpfile.Name())

	// Execute k6
	// resultsFile, err := createTempFile(fs, "k6results*.json", "")
	// if err != nil {
	// 	return StressTestResult{}, fmt.Errorf("failed to create results file: %w", err)
	// }
	// defer fs.Remove(resultsFile.Name())

	cmd := exec.Command("k6", "run", tmpfile.Name()) //, "--out", "json="+resultsFile.Name()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k6 execution failed: %w", err)
	}

	// Parse k6 output and create StressTestResult
	// return parseK6Results(fs, resultsFile)
	return nil
}

func loadAllSamples(configs []ServerConfig) ([]RequestResponseSample, error) {
	var allSamples []RequestResponseSample
	for _, config := range configs {
		samples, err := loadSamples(config.SampleFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load samples from %s: %w", config.SampleFile, err)
		}
		allSamples = append(allSamples, samples...)
	}
	return allSamples, nil
}

func createK6Script(baseUrl string, samples []RequestResponseSample, config StressTestConfig) string {
	samplesJSON, _ := json.Marshal(samples)
	return fmt.Sprintf(`
		import http from 'k6/http';
		import { check, sleep } from 'k6';
		import { Rate } from 'k6/metrics';

		const baseUrl = '%s/main/evm/123';
		const samples = %s;

		const errorRate = new Rate('errors');

		export let options = {
			vus: %d,
			duration: '%s',
			rps: %d
		};

		export default function() {
			const sample = samples[Math.floor(Math.random() * samples.length)];
			const payload = JSON.stringify(sample.request);
			const params = {
				headers: { 'Content-Type': 'application/json' },
			};

			const res = http.post(baseUrl, payload, params);

			check(res, {
				'status is 200': (r) => r.status === 200,
				'response has no error': (r) => {
					const body = JSON.parse(r.body);
					return body && (body.error === undefined || body.error === null);
				},
			});

			errorRate.add(res.status !== 200);

			sleep(1);
		}
	`, baseUrl, samplesJSON, config.VUs, config.Duration, config.MaxRPS)
}

func createTempFile(fs afero.Fs, pattern, content string) (afero.File, error) {
	tmpfile, err := afero.TempFile(fs, "", pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	if err := tmpfile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	return tmpfile, nil
}

// func parseK6Results(fs afero.Fs, resultsFile afero.File) (StressTestResult, error) {
// 	// This is a simplified parser. You might need to adjust it based on the actual k6 output format.
// 	result := StressTestResult{
// 		Success:          true,
// 		FailuresByError:  make(map[string]ErrorStat),
// 		ServerStatistics: make(map[int]ServerStats),
// 	}

// 	resultsStr, err := afero.ReadFile(fs, resultsFile.Name())
// 	if err != nil {
// 		return StressTestResult{}, fmt.Errorf("failed to read results file: %w", err)
// 	}

// 	// parse every line in resultsStr to an object
// 	lines := strings.Split(string(resultsStr), "\n")
// 	for _, line := range lines {
// 		var resultLine map[string]interface{}
// 		if err := json.Unmarshal([]byte(line), &resultLine); err != nil {
// 			log.Error().Err(err).Str("line", line).Msg("failed to unmarshal results line")
// 			continue
// 			// return StressTestResult{}, fmt.Errorf("failed to unmarshal results line: %w", err)
// 		}

// 		log.Debug().Interface("resultLine", resultLine).Msg("Result line")
// 	}

// 	// TODO
// 	//
// 	// request failure rate overall and per upstream and per error type
// 	// total avg./p90 latency overall and per upstream
// 	//

// 	return result, nil
// }

func fetchPrometheusMetrics(port int) (*StressTestResult, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/metrics", port))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch prometheus metrics: %w", err)
	}

	body, _ := io.ReadAll(resp.Body)
	os.Stdout.Write(body)

	testResult := &StressTestResult{
		CounterMetrics: []*CounterMetric{},
	}

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return testResult, fmt.Errorf("failed to gather metrics: %w", err)
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			var project, network, upstream, category string
			for _, label := range labels {
				if label.GetName() == "project" {
					project = label.GetValue()
				}
				if label.GetName() == "network" {
					network = label.GetValue()
				}
				if label.GetName() == "upstream" {
					upstream = label.GetValue()
				}
				if label.GetName() == "category" {
					category = label.GetValue()
				}
			}

			if strings.HasSuffix(mf.GetName(), "total") {
				mt := &CounterMetric{
					Name:  mf.GetName(),
					Value: m.GetCounter().GetValue(),
					Labels: map[string]string{
						"project":  project,
						"network":  network,
						"upstream": upstream,
						"category": category,
					},
				}

				testResult.CounterMetrics = append(testResult.CounterMetrics, mt)
			}
		}
	}

	return testResult, nil
}
