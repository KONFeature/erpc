package erpc

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type PreparedNetwork struct {
	NetworkId        string
	ProjectId        string
	FailsafePolicies []failsafe.Policy[any]
	Config           *config.NetworkConfig
	Logger           *zerolog.Logger
	Upstreams        []*upstream.PreparedUpstream

	rateLimitersRegistry *resiliency.RateLimitersRegistry
	failsafeExecutor     failsafe.Executor[interface{}]

	rateLimiterDal data.RateLimitersDAL
	cacheDal       data.CacheDAL
}

var preparedNetworks map[string]*PreparedNetwork = make(map[string]*PreparedNetwork)

func (r *ProjectsRegistry) NewNetwork(
	logger *zerolog.Logger,
	database *data.Database,
	prjCfg *config.ProjectConfig,
	nwCfg *config.NetworkConfig,
) (*PreparedNetwork, error) {
	var key = prjCfg.Id + ":" + nwCfg.NetworkId

	if pn, ok := preparedNetworks[key]; ok {
		return pn, nil
	}

	var policies []failsafe.Policy[any]

	if (nwCfg != nil) && (nwCfg.Failsafe != nil) {
		pls, err := resiliency.CreateFailSafePolicies(key, nwCfg.Failsafe)
		if err != nil {
			return nil, err
		}
		policies = pls
	}

	var cacheDal data.CacheDAL
	var rateLimiterDal data.RateLimitersDAL
	if database != nil {
		switch nwCfg.Architecture {
		case "evm":
			cacheDal = database.EvmJsonRpcCache
		default:
			return nil, errors.New("unknown network architecture")
		}

		// if database.RateLimitSnapshots != nil {
		// 	rateLimiterDal = database.RateLimitSnapshots
		// }
	}

	ptr := logger.With().Str("network", nwCfg.NetworkId).Logger()
	preparedNetworks[key] = &PreparedNetwork{
		NetworkId:        nwCfg.NetworkId,
		ProjectId:        prjCfg.Id,
		FailsafePolicies: policies,
		Config:           nwCfg,
		Logger:           &ptr,

		cacheDal:             cacheDal,
		rateLimiterDal:       rateLimiterDal,
		rateLimitersRegistry: r.rateLimitersRegistry,
		failsafeExecutor:     failsafe.NewExecutor[interface{}](policies...),
	}

	return preparedNetworks[key], nil
}

func (n *PreparedNetwork) Architecture() string {
	return n.Config.Architecture
}

func (n *PreparedNetwork) Forward(ctx context.Context, req *common.NormalizedRequest, w common.ResponseWriter) error {
	n.Logger.Debug().Object("req", req).Msgf("forwarding request")

	if n.cacheDal != nil {
		cacheReader, err := n.cacheDal.GetWithReader(ctx, req)
		if err != nil {
			n.Logger.Debug().Err(err).Msgf("could not find response in cache")
		}
		if cacheReader != nil {
			if w.TryLock() {
				w.AddHeader("Content-Type", "application/json")
				w.AddHeader("X-ERPC-Network", n.NetworkId)
				w.AddHeader("X-ERPC-Cache", "Hit")
				w, err := io.Copy(w, cacheReader)
				n.Logger.Info().Object("req", req).Int64("written", w).Err(err).Msgf("response served from cache")
				return err
			} else {
				return common.NewErrResponseWriteLock("<cache store>")
			}
		}
	}

	if err := n.acquireRateLimitPermit(req); err != nil {
		return err
	}

	var errorsByUpstream = []error{}

	// Configure the cache writer on the response writer so result can be cached
	go (func() {
		if n.cacheDal != nil {
			cwr, err := n.cacheDal.SetWithWriter(ctx, req)
			if err != nil {
				n.Logger.Warn().Err(err).Msgf("could not create cache response writer")
			} else {
				w.AddBodyWriter(cwr)
			}
		}
	})()

	// Function to prepare and forward the request to an upstream
	tryForward := func(
		u *upstream.PreparedUpstream,
		ctx context.Context,
	) (skipped bool, err error) {
		lg := u.Logger.With().Str("network", n.NetworkId).Logger()
		if u.Score < 0 {
			lg.Debug().Msgf("skipping upstream with negative score %f", u.Score)
			return true, nil
		}

		pr, err := u.PrepareRequest(req)
		lg.Debug().Err(err).Msgf("prepared request: %v", pr)
		if pr == nil && err == nil {
			return true, nil
		}
		if err != nil {
			return false, err
		}

		err = n.forwardToUpstream(u, ctx, pr, w)
		if !common.IsNull(err) {
			return false, err
		}

		lg.Info().Msgf("successfully forward request")
		return false, nil
	}

	if n.FailsafePolicies == nil || len(n.FailsafePolicies) == 0 {
		// Handling via simple loop over upstreams until one responds
		for _, u := range n.Upstreams {
			if _, err := tryForward(u, ctx); err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
			return nil
		}

		return common.NewErrUpstreamsExhausted(errorsByUpstream)
	}

	// Handling when FailsafePolicies are defined
	mtx := sync.Mutex{}
	i := 0
	_, execErr := n.failsafeExecutor.WithContext(ctx).GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
		// We should try all upstreams at least once, but using "i" we make sure
		// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
		// This mimicks a round-robin behavior, for example when doing hedge or retries.
		// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
		ln := len(n.Upstreams)
		for count := 0; count < ln; count++ {
			mtx.Lock()
			u := n.Upstreams[i]
			n.Logger.Debug().Msgf("executing forward current index: %d", i)
			i++
			if i >= ln {
				i = 0
			}
			mtx.Unlock()
			n.Logger.Debug().Msgf("executing forward to upstream: %s", u.Id)

			skipped, err := tryForward(u, exec.Context())
			if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && exec.Hedges() > 0 {
				n.Logger.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Id, skipped)
				return nil, err
			}

			n.Logger.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Id, skipped, err)
			if !skipped {
				return nil, err
			} else if err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
		}

		return nil, common.NewErrUpstreamsExhausted(errorsByUpstream)
	})

	if execErr != nil {
		return resiliency.TranslateFailsafeError(execErr)
	}

	return nil
}

func (n *PreparedNetwork) acquireRateLimitPermit(req *common.NormalizedRequest) error {
	if n.Config.RateLimitBucket == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBucket(n.Config.RateLimitBucket)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}

	rules := rlb.GetRulesByMethod(method)
	n.Logger.Debug().Msgf("found %d network-level rate limiters for network: %s method: %s", len(rules), n.NetworkId, method)

	if len(rules) > 0 {
		for _, rule := range rules {
			permit := (*rule.Limiter).TryAcquirePermit()
			if !permit {
				health.MetricNetworkRequestLocalRateLimited.WithLabelValues(
					n.ProjectId,
					n.NetworkId,
					method,
				).Inc()
				return common.NewErrNetworkRateLimitRuleExceeded(
					n.ProjectId,
					n.NetworkId,
					n.Config.RateLimitBucket,
					rule.Config,
				)
			} else {
				n.Logger.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
			}
		}
	}

	return nil
}

func (n *PreparedNetwork) forwardToUpstream(
	thisUpstream *upstream.PreparedUpstream,
	ctx context.Context,
	r interface{},
	w common.ResponseWriter,
) error {
	var category string = ""
	if jrr, ok := r.(*common.JsonRpcRequest); ok {
		category = jrr.Method
	}
	health.MetricUpstreamRequestTotal.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	).Inc()
	timer := prometheus.NewTimer(health.MetricUpstreamRequestDuration.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	))
	defer timer.ObserveDuration()

	return thisUpstream.Forward(ctx, n.NetworkId, r, w)
}
