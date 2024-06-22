package erpc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type Network struct {
	Config *common.NetworkConfig

	NetworkId string
	ProjectId string
	Logger    *zerolog.Logger
	Upstreams []*upstream.Upstream

	upstreamsMutex     *sync.RWMutex
	reorderStopChannel chan bool

	failsafePolicies     []failsafe.Policy[common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[common.NormalizedResponse]
	rateLimitersRegistry *upstream.RateLimitersRegistry
	rateLimiterDal       data.RateLimitersDAL
	cacheDal             data.CacheDAL
	evmBlockTracker      *EvmBlockTracker
}

func (n *Network) Bootstrap(ctx context.Context) error {
	if err := n.resolveNetworkId(ctx); err != nil {
		return err
	}

	if n.Architecture() == common.ArchitectureEvm {
		n.evmBlockTracker = NewEvmBlockTracker(n)
		if err := n.evmBlockTracker.Bootstrap(ctx); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("network architecture not supported: %s", n.Architecture())
	}

	go func(pn *Network) {
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-pn.reorderStopChannel:
				ticker.Stop()
				return
			case <-ticker.C:
				upsList := reorderUpstreams(pn.Upstreams)
				pn.upstreamsMutex.Lock()
				pn.Upstreams = upsList
				pn.upstreamsMutex.Unlock()
			}
		}
	}(n)

	return nil
}

func (n *Network) Shutdown() {
	if n.evmBlockTracker != nil {
		n.evmBlockTracker.Shutdown()
	}

	if n.reorderStopChannel != nil {
		n.reorderStopChannel <- true
	}
}

func (n *Network) Id() string {
	return n.NetworkId
}

func (n *Network) Architecture() common.NetworkArchitecture {
	if n.Config.Architecture == "" {
		if n.Config.Evm != nil {
			n.Config.Architecture = common.ArchitectureEvm
		}
	}

	return n.Config.Architecture
}

func (n *Network) Forward(ctx context.Context, req *upstream.NormalizedRequest) (common.NormalizedResponse, error) {
	n.Logger.Debug().Object("req", req).Msgf("forwarding request")
	req.WithNetwork(n)

	if n.cacheDal != nil {
		n.Logger.Debug().Msgf("checking cache for request")
		resp, err := n.cacheDal.Get(ctx, req)
		n.Logger.Debug().Err(err).Msgf("cache response: %v", resp)
		if err != nil {
			n.Logger.Debug().Err(err).Msgf("could not find response in cache")
		} else if resp != nil {
			n.Logger.Info().Object("req", req).Err(err).Msgf("response served from cache")
			return resp, err
		}
	}

	if err := n.acquireRateLimitPermit(req); err != nil {
		return nil, err
	}

	var errorsByUpstream = []error{}

	// Function to prepare and forward the request to an upstream
	tryForward := func(
		u *upstream.Upstream,
		ctx context.Context,
	) (resp common.NormalizedResponse, skipped bool, err error) {
		lg := u.Logger.With().Str("network", n.NetworkId).Logger()
		if u.Score < 0 {
			lg.Debug().Msgf("skipping upstream with negative score %d", u.Score)
			return nil, true, nil
		}

		resp, err = n.forwardToUpstream(u, ctx, req)
		if !common.IsNull(err) {
			return nil, false, err
		}

		lg.Info().Msgf("successfully forwarded request to upstream")
		return resp, false, nil
	}

	mtx := sync.Mutex{}
	i := 0
	resp, execErr := n.failsafeExecutor.
		WithContext(ctx).
		GetWithExecution(func(exec failsafe.Execution[common.NormalizedResponse]) (common.NormalizedResponse, error) {
			n.upstreamsMutex.RLock()
			upsList := n.Upstreams
			n.upstreamsMutex.RUnlock()

			// We should try all upstreams at least once, but using "i" we make sure
			// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
			// This mimicks a round-robin behavior, for example when doing hedge or retries.
			// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
			ln := len(upsList)
			for count := 0; count < ln; count++ {
				mtx.Lock()
				u := upsList[i]
				n.Logger.Debug().Msgf("executing forward current index: %d", i)
				i++
				if i >= ln {
					i = 0
				}
				mtx.Unlock()
				n.Logger.Debug().Msgf("executing forward to upstream: %s", u.Config().Id)

				resp, skipped, err := tryForward(u, exec.Context())
				if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && exec.Hedges() > 0 {
					n.Logger.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Config().Id, skipped)
					return nil, err
				}

				n.Logger.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Config().Id, skipped, err)
				if !skipped {
					n.Logger.Debug().Interface("resp", resp).Msgf("storing response in cache")
					go (func(resp common.NormalizedResponse) {
						if n.cacheDal != nil && resp != nil {
							c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							err := n.cacheDal.Set(c, req, resp)
							if err != nil {
								n.Logger.Warn().Err(err).Msgf("could not store response in cache")
							}
						}
					})(resp)
					return resp, err
				} else if err != nil {
					errorsByUpstream = append(errorsByUpstream, err)
					continue
				}
			}

			return nil, common.NewErrUpstreamsExhausted(errorsByUpstream)
		})

	if execErr != nil {
		return nil, upstream.TranslateFailsafeError(execErr)
	}

	return resp, nil
}

func (n *Network) EvmGetChainId(ctx context.Context) (string, error) {
	pr := upstream.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[]}`)).WithNetwork(n)
	resp, err := n.Forward(ctx, pr)
	if err != nil {
		return "", err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}

	log.Debug().Msgf("eth_chainId response: %+v", jrr)

	hex, err := common.NormalizeHex(jrr.Result)
	if err != nil {
		return "", err
	}

	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
}

func (n *Network) EvmIsBlockFinalized(blockNumber uint64) (bool, error) {
	if n.evmBlockTracker == nil {
		return false, nil
	}

	finalizedBlock := n.evmBlockTracker.FinalizedBlockNumber
	latestBlock := n.evmBlockTracker.LatestBlockNumber
	if latestBlock == 0 && finalizedBlock == 0 {
		n.Logger.Debug().
			Uint64("finalizedBlock", finalizedBlock).
			Uint64("latestBlock", latestBlock).
			Uint64("blockNumber", blockNumber).
			Msgf("finalized/latest blocks are not available yet when checking block finality")
		return false, nil
	}

	n.Logger.Debug().
		Uint64("finalizedBlock", finalizedBlock).
		Uint64("latestBlock", latestBlock).
		Uint64("blockNumber", blockNumber).
		Msgf("calculating block finality")

	if finalizedBlock > 0 {
		return blockNumber <= finalizedBlock, nil
	}

	if latestBlock == 0 {
		return false, nil
	}

	var fb uint64

	if n.Config.Evm != nil {
		fb = latestBlock - n.Config.Evm.FinalityDepth
	} else {
		fb = latestBlock - 128
	}

	n.Logger.Debug().
		Uint64("inferredFinalizedBlock", fb).
		Uint64("latestBlock", latestBlock).
		Uint64("blockNumber", blockNumber).
		Msgf("calculating block finality using inferred finalized block")

	return blockNumber <= fb, nil
}

func (n *Network) resolveNetworkId(ctx context.Context) error {
	if n.NetworkId != "" {
		n.Logger.Trace().Msgf("network id already resolved")
		return nil
	}

	n.Logger.Debug().Msgf("resolving network id")
	if n.Architecture() == common.ArchitectureEvm {
		if n.Config.Evm != nil && n.Config.Evm.ChainId > 0 {
			n.NetworkId = strconv.Itoa(n.Config.Evm.ChainId)
		} else {
			nid, err := n.EvmGetChainId(ctx)
			if err != nil {
				return err
			}
			n.NetworkId = nid
		}
	}

	if n.NetworkId == "" {
		return common.NewErrUnknownNetworkID(n.Architecture())
	}

	n.Logger.Debug().Msgf("resolved network id to: %s", n.NetworkId)

	return nil
}

func (n *Network) acquireRateLimitPermit(req *upstream.NormalizedRequest) error {
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
					fmt.Sprintf("%+v", rule.Config),
				)
			} else {
				n.Logger.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
			}
		}
	}

	return nil
}

func (n *Network) forwardToUpstream(
	ups *upstream.Upstream,
	ctx context.Context,
	req *upstream.NormalizedRequest,
) (common.NormalizedResponse, error) {
	var category string = ""
	if m, _ := req.Method(); m != "" {
		category = m
	}
	health.MetricUpstreamRequestTotal.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		ups.Config().Id,
		category,
	).Inc()
	timer := prometheus.NewTimer(health.MetricUpstreamRequestDuration.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		ups.Config().Id,
		category,
	))
	defer timer.ObserveDuration()

	return ups.Forward(ctx, req)
}

func weightedRandomSelect(upstreams []*upstream.Upstream) *upstream.Upstream {
	totalScore := 0
	for _, upstream := range upstreams {
		totalScore += upstream.Score
	}

	if totalScore == 0 {
		return upstreams[0]
	}

	randomValue := rand.Intn(totalScore)

	for _, upstream := range upstreams {
		if randomValue < upstream.Score {
			return upstream
		}
		randomValue -= upstream.Score
	}

	// This should never be reached
	return upstreams[len(upstreams)-1]
}

func reorderUpstreams(upstreams []*upstream.Upstream) []*upstream.Upstream {
	reordered := make([]*upstream.Upstream, len(upstreams))
	remaining := append([]*upstream.Upstream{}, upstreams...)

	for i := range reordered {
		selected := weightedRandomSelect(remaining)
		reordered[i] = selected

		// Remove selected item from remaining upstreams
		for j, upstream := range remaining {
			if upstream.Config().Id == selected.Config().Id {
				remaining = append(remaining[:j], remaining[j+1:]...)
				break
			}
		}
	}

	return reordered
}