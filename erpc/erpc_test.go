package erpc

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
	"github.com/rs/zerolog/log"
)

func TestBoorstrap_GracefulShutdown(t *testing.T) {
	cfg := &config.Config{
		Server: &config.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
	}
	store := data.NewMemoryStore(&config.MemoryStoreConfig{
		MaxSize: "100mb",
	})
	lg := log.With().Logger()
	erpc, _ := NewERPC(&lg, store, cfg)
	erpc.Shutdown()
}

func TestBootstrap_UpstreamsRegistryFailure(t *testing.T) {
	cfg := &config.Config{
		Server: &config.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
		Projects: []*config.ProjectConfig{
			{
				Id: "test",
				Upstreams: []*config.UpstreamConfig{
					{
						Id:           "test",
						Architecture: "evm",
						Endpoint:     "http://localhost:8080",
						// missing "evmChainId" will cause an error
						Metadata: map[string]string{},
					},
				},
			},
		},
	}

	store := data.NewMemoryStore(&config.MemoryStoreConfig{
		MaxSize: "100mb",
	})
	lg := log.With().Logger()
	_, err := NewERPC(&lg, store, cfg)
	if err == nil {
		t.Error("expected error when bootstraping upstream orchestrator, got nil")
	}
}
