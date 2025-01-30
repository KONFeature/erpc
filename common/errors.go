package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

func IsNull(err interface{}) bool {
	if err == nil || err == "" {
		return true
	}

	var be *BaseError
	if errors.As(err.(error), &be) {
		return be.Code == ""
	}

	return err == nil
}

func ErrorSummary(err interface{}) string {
	if err == nil {
		return ""
	}

	s := "ErrUnknown"

	if be, ok := err.(StandardError); ok {
		s = fmt.Sprintf("%s: %s", be.CodeChain(), cleanUpMessage(be.DeepestMessage()))
	} else if e, ok := err.(error); ok {
		s = cleanUpMessage(e.Error())
	}

	return s
}

var longHash = regexp.MustCompile(`0x[a-fA-F0-9][a-fA-F0-9]+`)
var txHashErr = regexp.MustCompile(`transaction [a-fA-F0-9][a-fA-F0-9][a-fA-F0-9]+`)
var trieNodeErr = regexp.MustCompile(`trie node [a-fA-F0-9][a-fA-F0-9][a-fA-F0-9]+`)
var revertAddr = regexp.MustCompile(`.*execution reverted.*`)
var ipAddr = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
var ddg = regexp.MustCompile(`\d\d+`)

func cleanUpMessage(s string) string {
	s = longHash.ReplaceAllString(s, "0xREDACTED")
	s = txHashErr.ReplaceAllString(s, "transaction 0xREDACTED")
	s = trieNodeErr.ReplaceAllString(s, "trie node 0xREDACTED")
	s = revertAddr.ReplaceAllString(s, "execution reverted")
	s = ipAddr.ReplaceAllString(s, "X.X.X.X")
	s = ddg.ReplaceAllString(s, "XX")

	if len(s) > 512 {
		s = s[:512]
	}

	return s
}

//
// Base Types
//

type ErrorCode string

type BaseError struct {
	Code    ErrorCode              `json:"code,omitempty"`
	Message string                 `json:"message,omitempty"`
	Cause   error                  `json:"cause,omitempty"`
	Details map[string]interface{} `json:"details,omitempty"`
}

type StandardError interface {
	HasCode(...ErrorCode) bool
	CodeChain() string
	DeepestMessage() string
	GetCause() error
	ErrorStatusCode() int
	Base() *BaseError
	MarshalZerologObject(v *zerolog.Event)
}

func (e *BaseError) GetCode() ErrorCode {
	return e.Code
}

func (e *BaseError) Unwrap() error {
	return e.Cause
}

func (e *BaseError) Error() string {
	var detailsStr string

	if e.Details != nil && len(e.Details) > 0 {
		s, er := SonicCfg.Marshal(e.Details)
		if er == nil {
			detailsStr = fmt.Sprintf("(%s)", s)
		} else {
			detailsStr = fmt.Sprintf("(%v)", e.Details)
		}
	}

	if e.Cause == nil {
		if detailsStr == "" {
			return fmt.Sprintf("%s: %s", e.Code, e.Message)
		}
		return fmt.Sprintf("%s: %s -> %s", e.Code, e.Message, detailsStr)
	} else {
		if detailsStr == "" {
			return fmt.Sprintf("%s: %s \ncaused by: %v", e.Code, e.Message, e.Cause.Error())
		}
		return fmt.Sprintf("%s: %s -> %s \ncaused by: %v", e.Code, e.Message, detailsStr, e.Cause.Error())
	}
}

func (e *BaseError) CodeChain() string {
	if e.Cause != nil {
		if be, ok := e.Cause.(StandardError); ok {
			return fmt.Sprintf("%s <- %s", e.GetCode(), be.CodeChain())
		}
	}

	return string(e.Code)
}

func (e *BaseError) DeepestMessage() string {
	if e.Cause != nil {
		if be, ok := e.Cause.(StandardError); ok {
			return be.DeepestMessage()
		}
		if e.Cause != nil {
			return e.Cause.Error()
		}
	}

	return e.Message
}

func (e *BaseError) GetCause() error {
	return e.Cause
}

func (e BaseError) MarshalJSON() ([]byte, error) {
	type Alias BaseError
	cause := e.Cause
	if cs, ok := cause.(interface{ Unwrap() []error }); ok {
		// Handle joined errors
		causes := make([]interface{}, 0)
		for _, err := range cs.Unwrap() {
			if se, ok := err.(StandardError); ok {
				causes = append(causes, se)
			} else {
				causes = append(causes, err.Error())
			}
		}
		return SonicCfg.Marshal(&struct {
			Alias
			Cause []interface{} `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: causes,
		})
	} else if cs, ok := cause.(StandardError); ok {
		return SonicCfg.Marshal(&struct {
			Alias
			Cause StandardError `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: cs,
		})
	} else if cause != nil {
		return SonicCfg.Marshal(&struct {
			Alias
			Cause BaseError `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: BaseError{
				Code:    "ErrGeneric",
				Message: cause.Error(),
			},
		})
	}

	return SonicCfg.Marshal(&struct {
		Alias
		Cause interface{} `json:"cause"`
	}{
		Alias: (Alias)(e),
		Cause: nil,
	})
}

func (e *BaseError) Is(err error) bool {
	var is bool

	if be, ok := err.(*BaseError); ok {
		is = e.Code == be.Code
	} else if be, ok := err.(StandardError); ok {
		is = strings.Contains(be.CodeChain(), string(e.Code))
	}

	if !is && e.Cause != nil {
		is = errors.Is(e.Cause, err)
	}

	return is
}

func (e *BaseError) HasCode(codes ...ErrorCode) bool {
	for _, code := range codes {
		if e.Code == code {
			return true
		}
	}

	if e.Cause != nil {
		if be, ok := e.Cause.(StandardError); ok {
			return be.HasCode(codes...)
		}
		if cs, ok := e.Cause.(interface{ Unwrap() []error }); ok {
			for _, err := range cs.Unwrap() {
				if be, ok := err.(StandardError); ok {
					return be.HasCode(codes...)
				}
			}
		}
	}

	return false
}

func (e *BaseError) ErrorStatusCode() int {
	if e.Cause != nil {
		if be, ok := e.Cause.(StandardError); ok {
			return be.ErrorStatusCode()
		}
	}
	return http.StatusInternalServerError
}

func (e *BaseError) Base() *BaseError {
	return e
}

func (e *BaseError) MarshalZerologObject(v *zerolog.Event) {
	if e == nil {
		return
	}
	v.Str("code", string(e.Code))
	v.Str("message", e.Message)
	if e.Cause != nil {
		if multiErr, ok := e.Cause.(interface{ Unwrap() []error }); ok {
			v.Interface("cause", multiErr.Unwrap())
		} else if se, ok := e.Cause.(StandardError); ok {
			v.Object("cause", se)
		} else {
			v.Str("cause", e.Cause.Error())
		}
	}
	if e.Details != nil {
		v.Interface("details", e.Details)
	}
}

//
// Server
//

type ErrInvalidRequest struct{ BaseError }

const ErrCodeInvalidRequest ErrorCode = "ErrInvalidRequest"

var NewErrInvalidRequest = func(cause error) error {
	return &ErrInvalidRequest{
		BaseError{
			Code:    ErrCodeInvalidRequest,
			Message: "invalid request body or headers",
			Cause:   cause,
		},
	}
}

func (e *ErrInvalidRequest) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrInvalidUrlPath struct{ BaseError }

const ErrCodeInvalidUrlPath ErrorCode = "ErrInvalidUrlPath"

var NewErrInvalidUrlPath = func(reason, path string) error {
	return &ErrInvalidUrlPath{
		BaseError{
			Code:    ErrCodeInvalidUrlPath,
			Message: fmt.Sprintf("url path is not valid: %s", reason),
			Details: map[string]interface{}{
				"providedPath": path,
			},
		},
	}
}

func (e *ErrInvalidUrlPath) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrInvalidConfig struct{ BaseError }

var NewErrInvalidConfig = func(message string) error {
	return &ErrInvalidConfig{
		BaseError{
			Code:    "ErrInvalidConfig",
			Message: message,
		},
	}
}

type ErrRequestTimeout struct{ BaseError }

var NewErrRequestTimeout = func(timeout time.Duration) error {
	return &ErrRequestTimeout{
		BaseError{
			Code:    "ErrRequestTimeout",
			Message: "request timed out before any upstream could respond",
			Details: map[string]interface{}{
				"timeoutSeconds": timeout.Seconds(),
			},
		},
	}
}

func (e *ErrRequestTimeout) ErrorStatusCode() int {
	return http.StatusGatewayTimeout
}

type ErrInternalServerError struct{ BaseError }

var NewErrInternalServerError = func(cause error) error {
	return &ErrInternalServerError{
		BaseError{
			Code:    "ErrInternalServerError",
			Message: "internal server error",
			Cause:   cause,
		},
	}
}

//
// Auth
//

type ErrAuthUnauthorized struct{ BaseError }

const ErrCodeAuthUnauthorized ErrorCode = "ErrAuthUnauthorized"

var NewErrAuthUnauthorized = func(strategy string, message string) error {
	return &ErrAuthUnauthorized{
		BaseError{
			Code:    ErrCodeAuthUnauthorized,
			Message: message,
			Details: map[string]interface{}{
				"strategy": strategy,
			},
		},
	}
}

func (e *ErrAuthUnauthorized) ErrorStatusCode() int {
	return http.StatusUnauthorized
}

type ErrAuthRateLimitRuleExceeded struct{ BaseError }

const ErrCodeAuthRateLimitRuleExceeded ErrorCode = "ErrAuthRateLimitRuleExceeded"

var NewErrAuthRateLimitRuleExceeded = func(projectId, strategy, budget, rule string) error {
	return &ErrAuthRateLimitRuleExceeded{
		BaseError{
			Code:    ErrCodeAuthRateLimitRuleExceeded,
			Message: "auth-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"projectId": projectId,
				"strategy":  strategy,
				"budget":    budget,
				"rule":      rule,
			},
		},
	}
}

func (e *ErrAuthRateLimitRuleExceeded) ErrorStatusCode() int {
	return http.StatusTooManyRequests
}

//
// Projects
//

type ErrProjectNotFound struct{ BaseError }

var NewErrProjectNotFound = func(projectId string) error {
	return &ErrProjectNotFound{
		BaseError{
			Code:    "ErrProjectNotFound",
			Message: fmt.Sprintf("project '%s' not configured in the config", projectId),
		},
	}
}

func (e *ErrProjectNotFound) ErrorStatusCode() int {
	return http.StatusNotFound
}

type ErrProjectAlreadyExists struct{ BaseError }

var NewErrProjectAlreadyExists = func(projectId string) error {
	return &ErrProjectAlreadyExists{
		BaseError{
			Code:    "ErrProjectAlreadyExists",
			Message: "project already exists",
			Details: map[string]interface{}{
				"projectId": projectId,
			},
		},
	}
}

//
// Networks
//

type ErrNetworkNotFound struct{ BaseError }

var NewErrNetworkNotFound = func(networkId string) error {
	return &ErrNetworkNotFound{
		BaseError{
			Code:    "ErrNetworkNotFound",
			Message: "network not configured in the config",
			Details: map[string]interface{}{
				"networkId": networkId,
			},
		},
	}
}

func (e *ErrNetworkNotFound) ErrorStatusCode() int {
	return http.StatusNotFound
}

type ErrUnknownNetworkID struct{ BaseError }

var NewErrUnknownNetworkID = func(arch NetworkArchitecture) error {
	return &ErrUnknownNetworkID{
		BaseError{
			Code:    "ErrUnknownNetworkID",
			Message: "could not resolve network ID not from config nor from the upstreams",
			Details: map[string]interface{}{
				"architecture": arch,
			},
		},
	}
}

func (e *ErrUnknownNetworkID) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrUnknownNetworkArchitecture struct{ BaseError }

var NewErrUnknownNetworkArchitecture = func(arch NetworkArchitecture) error {
	return &ErrUnknownNetworkArchitecture{
		BaseError{
			Code:    "ErrUnknownNetworkArchitecture",
			Message: "unknown network architecture",
			Details: map[string]interface{}{
				"architecture": arch,
			},
		},
	}
}

func (e *ErrUnknownNetworkArchitecture) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrNotImplemented struct{ BaseError }

var NewErrNotImplemented = func(msg string) error {
	return &ErrNotImplemented{
		BaseError{
			Code:    "ErrNotImplemented",
			Message: msg,
		},
	}
}

func (e *ErrNotImplemented) ErrorStatusCode() int {
	return http.StatusNotImplemented
}

type ErrInvalidEvmChainId struct{ BaseError }

var NewErrInvalidEvmChainId = func(chainId any) error {
	return &ErrInvalidEvmChainId{
		BaseError{
			Code:    "ErrInvalidEvmChainId",
			Message: "invalid EVM chain ID, it must be a number",
			Details: map[string]interface{}{
				"chainId": fmt.Sprintf("%+v", chainId),
			},
		},
	}
}

func (e *ErrInvalidEvmChainId) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrFinalizedBlockUnavailable struct{ BaseError }

const ErrCodeFinalizedBlockUnavailable ErrorCode = "ErrFinalizedBlockUnavailable"

var NewErrFinalizedBlockUnavailable = func(blockNumber int64) error {
	return &ErrFinalizedBlockUnavailable{
		BaseError{
			Code:    ErrCodeFinalizedBlockUnavailable,
			Message: "finalized/latest blocks are not available yet when checking block finality",
			Details: map[string]interface{}{
				"blockNumber": blockNumber,
			},
		},
	}
}

//
// Upstreams
//

type ErrUpstreamClientInitialization struct{ BaseError }

var NewErrUpstreamClientInitialization = func(cause error, upstreamId string) error {
	return &ErrUpstreamRequest{
		BaseError{
			Code:    "ErrUpstreamClientInitialization",
			Message: "could not initialize upstream client",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamRequest struct{ BaseError }

var NewErrUpstreamRequest = func(cause error, upsId, networkId, method string, duration time.Duration, attempts, retries, hedges int) error {
	return &ErrUpstreamRequest{
		BaseError{
			Code:    "ErrUpstreamRequest",
			Message: "failed to make request to upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"durationMs": duration.Milliseconds(),
				"networkId":  networkId,
				"method":     method,
				"upstreamId": upsId,
				"attempts":   attempts,
				"retries":    retries,
				"hedges":     hedges,
			},
		},
	}
}

func (e *ErrUpstreamRequest) IsObjectNull() bool {
	return e == nil || e.Code == ""
}

func (e *ErrUpstreamRequest) UpstreamId() string {
	if e.Details == nil {
		return ""
	}
	if upstreamId, ok := e.Details["upstreamId"].(string); ok {
		return upstreamId
	}
	return ""
}

func (e *ErrUpstreamRequest) FromCache() bool {
	return false
}

func (e *ErrUpstreamRequest) Attempts() int {
	if e.Details == nil {
		return 0
	}
	if attempts, ok := e.Details["attempts"].(int); ok {
		return attempts
	}
	return 0
}

func (e *ErrUpstreamRequest) Retries() int {
	if e.Details == nil {
		return 0
	}
	if retries, ok := e.Details["retries"].(int); ok {
		return retries
	}
	return 0
}

func (e *ErrUpstreamRequest) Hedges() int {
	if e.Details == nil {
		return 0
	}
	if hedges, ok := e.Details["hedges"].(int); ok {
		return hedges
	}
	return 0
}

type ErrUpstreamMalformedResponse struct{ BaseError }

var NewErrUpstreamMalformedResponse = func(cause error, upstreamId string) error {
	return &ErrUpstreamMalformedResponse{
		BaseError{
			Code:    "ErrUpstreamMalformedResponse",
			Message: "malformed response from upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

func (e *ErrUpstreamMalformedResponse) ErrorStatusCode() int {
	return http.StatusBadRequest
}

type ErrUpstreamsExhausted struct{ BaseError }

const ErrCodeUpstreamsExhausted ErrorCode = "ErrUpstreamsExhausted"

var NewErrUpstreamsExhausted = func(
	req *NormalizedRequest,
	ersObj *sync.Map,
	prjId, netId, method string,
	duration time.Duration,
	attempts, retries, hedges int,
) error {
	ers := []error{}
	ersObj.Range(func(key, value any) bool {
		ers = append(ers, value.(error))
		return true
	})
	e := &ErrUpstreamsExhausted{
		BaseError{
			Code:    ErrCodeUpstreamsExhausted,
			Message: "all upstream attempts failed",
			Cause:   errors.Join(ers...),
			Details: map[string]interface{}{
				"durationMs": duration.Milliseconds(),
				"projectId":  prjId,
				"networkId":  netId,
				"method":     method,
				"attempts":   attempts,
				"retries":    retries,
				"hedges":     hedges,
			},
		},
	}

	sm := e.SummarizeCauses()
	if sm != "" {
		e.Message += " (" + sm + ")"
	}

	return e
}

func (e *ErrUpstreamsExhausted) IsObjectNull() bool {
	return e == nil || e.Code == ""
}

func (e *ErrUpstreamsExhausted) ErrorStatusCode() int {
	if e.Cause != nil {
		if be, ok := e.Cause.(StandardError); ok {
			return be.ErrorStatusCode()
		}
		// if it's an array of errors (Unwrap)
		if joinedErr, ok := e.Cause.(interface{ Unwrap() []error }); ok {
			fsc := 503
			for _, e := range joinedErr.Unwrap() {
				if be, ok := e.(StandardError); ok {
					sc := be.ErrorStatusCode()
					if sc != 503 {
						fsc = sc
					}
				} else if nje, ok := e.(interface{ Unwrap() []error }); ok {
					for _, e := range nje.Unwrap() {
						if be, ok := e.(StandardError); ok {
							sc := be.ErrorStatusCode()
							if sc != 503 {
								fsc = sc
							}
						}
					}
				}
			}
			return fsc
		}
	}
	return 503
}

func (e *ErrUpstreamsExhausted) CodeChain() string {
	codeChain := string(e.Code)

	if e.Cause == nil {
		return codeChain
	}

	s := e.SummarizeCauses()
	if s != "" {
		return codeChain + " (" + s + ")"
	}

	return codeChain
}

func (e *ErrUpstreamsExhausted) SummarizeCauses() string {
	if joinedErr, ok := e.Cause.(interface{ Unwrap() []error }); ok {
		unsupported := 0
		missing := 0
		timeout := 0
		serverError := 0
		rateLimit := 0
		cbOpen := 0
		billing := 0
		skips := 0
		other := 0
		client := 0
		transport := 0
		cancelled := 0
		unsynced := 0
		excluded := 0
		nodeTypeMismatch := 0

		for _, e := range joinedErr.Unwrap() {
			if HasErrorCode(e, ErrCodeEndpointUnsupported) {
				unsupported++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointMissingData) {
				missing++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointCapacityExceeded) ||
				HasErrorCode(e, ErrCodeUpstreamRateLimitRuleExceeded) {
				rateLimit++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointBillingIssue) {
				billing++
				continue
			} else if HasErrorCode(e, ErrCodeFailsafeCircuitBreakerOpen) {
				cbOpen++
				continue
			} else if errors.Is(e, context.DeadlineExceeded) || HasErrorCode(e, ErrCodeEndpointRequestTimeout, ErrCodeNetworkRequestTimeout, ErrCodeFailsafeTimeoutExceeded) {
				timeout++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointServerSideException) {
				serverError++
				continue
			} else if HasErrorCode(e, ErrCodeUpstreamHedgeCancelled) {
				cancelled++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointClientSideException, ErrCodeJsonRpcRequestUnmarshal, ErrCodeInvalidRequest, ErrCodeInvalidUrlPath) {
				client++
				continue
			} else if HasErrorCode(e, ErrCodeEndpointTransportFailure) {
				transport++
				continue
			} else if HasErrorCode(e, ErrCodeUpstreamSyncing) {
				unsynced++
				continue
			} else if HasErrorCode(e, ErrCodeUpstreamExcludedByPolicy) {
				excluded++
				continue
			} else if HasErrorCode(e, ErrCodeUpstreamNodeTypeMismatch) {
				nodeTypeMismatch++
				continue
			} else if HasErrorCode(e, ErrCodeUpstreamMethodIgnored, ErrCodeUpstreamRequestSkipped) {
				skips++
				continue
			} else {
				other++
			}
		}

		reasons := []string{}
		if unsupported > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream unsupported method", unsupported))
		}
		if missing > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream missing data", missing))
		}
		if timeout > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream timeout", timeout))
		}
		if serverError > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream server errors", serverError))
		}
		if rateLimit > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream rate limited", rateLimit))
		}
		if cbOpen > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream circuit breaker open", cbOpen))
		}
		if billing > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream billing issues", billing))
		}
		if cancelled > 0 {
			reasons = append(reasons, fmt.Sprintf("%d hedges cancelled", cancelled))
		}
		if client > 0 {
			reasons = append(reasons, fmt.Sprintf("%d user errors", client))
		}
		if unsynced > 0 {
			reasons = append(reasons, fmt.Sprintf("%d syncing nodes", unsynced))
		}
		if excluded > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream excluded by policy", excluded))
		}
		if nodeTypeMismatch > 0 {
			reasons = append(reasons, fmt.Sprintf("%d node type mismatches", nodeTypeMismatch))
		}
		if skips > 0 {
			reasons = append(reasons, fmt.Sprintf("%d upstream skipped", skips))
		}
		if other > 0 {
			reasons = append(reasons, fmt.Sprintf("%d other upstream errors", other))
		}

		return strings.Join(reasons, ", ")
	}

	return ""
}

func (e *ErrUpstreamsExhausted) Errors() []error {
	if e.Cause == nil {
		return nil
	}

	errs, ok := e.Cause.(interface{ Unwrap() []error })
	if !ok {
		return nil
	}

	return errs.Unwrap()
}

func (e *ErrUpstreamsExhausted) DeepestMessage() string {
	if e.Cause == nil {
		return e.Message
	}

	s := e.SummarizeCauses()
	if s != "" {
		return s
	}

	if joinedErr, ok := e.Cause.(interface{ Unwrap() []error }); ok {
		children := joinedErr.Unwrap()
		if len(children) == 1 && children[0] != nil {
			if ste, ok := children[0].(StandardError); ok {
				return ste.DeepestMessage()
			} else {
				return children[0].Error()
			}
		}
		return ""
	}

	return ""
}

func (e *ErrUpstreamsExhausted) UpstreamId() string {
	return ""
}

func (e *ErrUpstreamsExhausted) FromCache() bool {
	return false
}

func (e *ErrUpstreamsExhausted) Attempts() int {
	if e.Details == nil {
		return 0
	}
	if attempts, ok := e.Details["attempts"].(int); ok {
		return attempts
	}
	return 0
}

func (e *ErrUpstreamsExhausted) Retries() int {
	if e.Details == nil {
		return 0
	}
	if retries, ok := e.Details["retries"].(int); ok {
		return retries
	}
	return 0
}

func (e *ErrUpstreamsExhausted) Hedges() int {
	if e.Details == nil {
		return 0
	}
	if hedges, ok := e.Details["hedges"].(int); ok {
		return hedges
	}
	return 0
}

type ErrNoUpstreamsDefined struct{ BaseError }

var NewErrNoUpstreamsDefined = func(project string) error {
	return &ErrNoUpstreamsDefined{
		BaseError{
			Code:    "ErrNoUpstreamsDefined",
			Message: "no upstreams defined for project",
			Details: map[string]interface{}{
				"project": project,
			},
		},
	}
}

func (e *ErrNoUpstreamsDefined) ErrorStatusCode() int { return http.StatusNotFound }

type ErrNoUpstreamsFound struct{ BaseError }

var NewErrNoUpstreamsFound = func(project string, network string) error {
	return &ErrNoUpstreamsFound{
		BaseError{
			Code:    "ErrNoUpstreamsFound",
			Message: fmt.Sprintf("no upstreams found for network '%s' and project '%s'", network, project),
			Details: map[string]interface{}{
				"project": project,
				"network": network,
			},
		},
	}
}

func (e *ErrNoUpstreamsFound) ErrorStatusCode() int { return http.StatusNotFound }

type ErrUpstreamNetworkNotDetected struct{ BaseError }

var NewErrUpstreamNetworkNotDetected = func(projectId string, upstreamId string) error {
	return &ErrUpstreamNetworkNotDetected{
		BaseError{
			Code:    "ErrUpstreamNetworkNotDetected",
			Message: "network not detected for upstream either from config nor by calling the endpoint",
			Details: map[string]interface{}{
				"projectId":  projectId,
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamInitialization struct{ BaseError }

var NewErrUpstreamInitialization = func(cause error, upstreamId string) error {
	return &ErrUpstreamInitialization{
		BaseError{
			Code:    "ErrUpstreamInitialization",
			Message: "failed to initialize upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamRequestSkipped struct{ BaseError }

const ErrCodeUpstreamRequestSkipped ErrorCode = "ErrUpstreamRequestSkipped"

var NewErrUpstreamRequestSkipped = func(reason error, upstreamId string) error {
	return &ErrUpstreamRequestSkipped{
		BaseError{
			Code:    ErrCodeUpstreamRequestSkipped,
			Message: "skipped forwarding request to upstream",
			Cause:   reason,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamMethodIgnored struct{ BaseError }

const ErrCodeUpstreamMethodIgnored ErrorCode = "ErrUpstreamMethodIgnored"

var NewErrUpstreamMethodIgnored = func(method string, upstreamId string) error {
	return &ErrUpstreamMethodIgnored{
		BaseError{
			Code:    ErrCodeUpstreamMethodIgnored,
			Message: "method ignored by upstream configuration",
			Details: map[string]interface{}{
				"method":     method,
				"upstreamId": upstreamId,
			},
		},
	}
}

func (e *ErrUpstreamMethodIgnored) ErrorStatusCode() int {
	return http.StatusUnsupportedMediaType
}

type ErrUpstreamSyncing struct{ BaseError }

const ErrCodeUpstreamSyncing ErrorCode = "ErrUpstreamSyncing"

var NewErrUpstreamSyncing = func(upstreamId string) error {
	return &ErrUpstreamSyncing{
		BaseError{
			Code:    ErrCodeUpstreamSyncing,
			Message: "upstream is syncing",
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamNotAllowed struct{ BaseError }

var NewErrUpstreamNotAllowed = func(upstreamId string) error {
	return &ErrUpstreamNotAllowed{
		BaseError{
			Code:    "ErrUpstreamNotAllowed",
			Message: "upstream not allowed based on use-upstream directive",
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamHedgeCancelled struct{ BaseError }

const ErrCodeUpstreamHedgeCancelled ErrorCode = "ErrUpstreamHedgeCancelled"

var NewErrUpstreamHedgeCancelled = func(upstreamId string, cause error) error {
	return &ErrUpstreamHedgeCancelled{
		BaseError{
			Code:    ErrCodeUpstreamHedgeCancelled,
			Message: "hedged request cancelled in favor of another upstream response",
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
			Cause: cause,
		},
	}
}

type ErrResponseWriteLock struct{ BaseError }

var NewErrResponseWriteLock = func(writerId string) error {
	return &ErrResponseWriteLock{
		BaseError{
			Code:    "ErrResponseWriteLock",
			Message: "failed to acquire write lock, potentially being writen by another upstream/writer",
			Details: map[string]interface{}{
				"writerId": writerId,
			},
		},
	}
}

//
// Clients
//

type ErrJsonRpcRequestUnmarshal struct {
	BaseError
}

const ErrCodeJsonRpcRequestUnmarshal ErrorCode = "ErrJsonRpcRequestUnmarshal"

var NewErrJsonRpcRequestUnmarshal = func(cause error) error {
	if _, ok := cause.(*BaseError); ok {
		return &ErrJsonRpcRequestUnmarshal{
			BaseError{
				Code:    ErrCodeJsonRpcRequestUnmarshal,
				Message: "failed to unmarshal json-rpc request",
				Cause:   cause,
			},
		}
	} else if cause != nil {
		return &ErrJsonRpcRequestUnmarshal{
			BaseError{
				Code:    ErrCodeJsonRpcRequestUnmarshal,
				Message: fmt.Sprintf("%s", cause),
			},
		}
	}
	return &ErrJsonRpcRequestUnmarshal{
		BaseError{
			Code:    ErrCodeJsonRpcRequestUnmarshal,
			Message: "failed to unmarshal json-rpc request",
		},
	}
}

func (e *ErrJsonRpcRequestUnmarshal) ErrorStatusCode() int { return 400 }

type ErrJsonRpcRequestUnresolvableMethod struct {
	BaseError
}

var NewErrJsonRpcRequestUnresolvableMethod = func(rpcRequest interface{}) error {
	return &ErrJsonRpcRequestUnresolvableMethod{
		BaseError{
			Code:    "ErrJsonRpcRequestUnresolvableMethod",
			Message: "could not resolve method in json-rpc request",
			Details: map[string]interface{}{
				"request": rpcRequest,
			},
		},
	}
}

type ErrJsonRpcRequestPreparation struct {
	BaseError
}

var NewErrJsonRpcRequestPreparation = func(cause error, details map[string]interface{}) error {
	return &ErrJsonRpcRequestPreparation{
		BaseError{
			Code:    "ErrJsonRpcRequestPreparation",
			Message: "failed to prepare json-rpc request",
			Cause:   cause,
			Details: details,
		},
	}
}

//
// Failsafe
//

type ErrFailsafeConfiguration struct{ BaseError }

var NewErrFailsafeConfiguration = func(cause error, details map[string]interface{}) error {
	return &ErrFailsafeConfiguration{
		BaseError{
			Code:    "ErrFailsafeConfiguration",
			Message: "failed to configure failsafe policy",
			Cause:   cause,
			Details: details,
		},
	}
}

type ErrFailsafeTimeoutExceeded struct{ BaseError }

const ErrCodeFailsafeTimeoutExceeded ErrorCode = "ErrFailsafeTimeoutExceeded"

var NewErrFailsafeTimeoutExceeded = func(scope Scope, cause error, startTime *time.Time) error {
	var duration time.Duration
	if startTime != nil {
		duration = time.Since(*startTime)
	}
	var msg string
	if duration > 0 {
		msg = fmt.Sprintf("failsafe timeout policy exceeded on %s-level after %s", scope, duration)
	} else {
		msg = fmt.Sprintf("failsafe timeout policy exceeded on %s-level", scope)
	}
	return &ErrFailsafeTimeoutExceeded{
		BaseError{
			Code:    ErrCodeFailsafeTimeoutExceeded,
			Message: msg,
			Cause:   cause,
		},
	}
}

func (e *ErrFailsafeTimeoutExceeded) ErrorStatusCode() int {
	return http.StatusGatewayTimeout
}

func (e *ErrFailsafeTimeoutExceeded) DeepestMessage() string {
	if e.Cause != nil {
		if se, ok := e.Cause.(StandardError); ok {
			return fmt.Sprintf("%s: %s", e.Message, se.DeepestMessage())
		} else {
			return fmt.Sprintf("%s: %s", e.Message, e.Cause.Error())
		}
	}
	return e.Message
}

type ErrFailsafeRetryExceeded struct{ BaseError }

const ErrCodeFailsafeRetryExceeded ErrorCode = "ErrFailsafeRetryExceeded"

var NewErrFailsafeRetryExceeded = func(scope Scope, cause error, startTime *time.Time) error {
	var dt map[string]interface{}
	var duration time.Duration
	if startTime != nil {
		duration = time.Since(*startTime)
		dt = map[string]interface{}{
			"durationMs": duration.Milliseconds(),
		}
	}
	var msg string
	if duration > 0 {
		msg = fmt.Sprintf("failsafe retry policy exceeded on %s-level after %s", scope, duration)
	} else {
		msg = fmt.Sprintf("failsafe retry policy exceeded on %s-level", scope)
	}
	return &ErrFailsafeRetryExceeded{
		BaseError{
			Code:    ErrCodeFailsafeRetryExceeded,
			Message: msg,
			Cause:   cause,
			Details: dt,
		},
	}
}

func (e *ErrFailsafeRetryExceeded) ErrorStatusCode() int {
	return 503
}

func (e *ErrFailsafeRetryExceeded) DeepestMessage() string {
	if e.Cause != nil {
		if se, ok := e.Cause.(StandardError); ok {
			return fmt.Sprintf("%s: %s", e.Message, se.DeepestMessage())
		} else {
			return fmt.Sprintf("%s: %s", e.Message, e.Cause)
		}
	}
	return e.Message
}

type ErrFailsafeCircuitBreakerOpen struct{ BaseError }

const ErrCodeFailsafeCircuitBreakerOpen ErrorCode = "ErrFailsafeCircuitBreakerOpen"

var NewErrFailsafeCircuitBreakerOpen = func(scope Scope, cause error, startTime *time.Time) error {
	var dt map[string]interface{}
	var duration time.Duration
	if startTime != nil {
		duration = time.Since(*startTime)
		dt = map[string]interface{}{
			"durationMs": duration.Milliseconds(),
		}
	}
	var msg string
	if duration > 0 {
		msg = fmt.Sprintf("failsafe circuit breaker open on %s-level after %s", scope, duration)
	} else {
		msg = fmt.Sprintf("failsafe circuit breaker open on %s-level", scope)
	}
	return &ErrFailsafeCircuitBreakerOpen{
		BaseError{
			Code:    ErrCodeFailsafeCircuitBreakerOpen,
			Message: msg,
			Cause:   cause,
			Details: dt,
		},
	}
}

func (e *ErrFailsafeCircuitBreakerOpen) DeepestMessage() string {
	if e.Cause != nil {
		if se, ok := e.Cause.(StandardError); ok {
			return fmt.Sprintf("%s: %s", e.Message, se.DeepestMessage())
		} else {
			return fmt.Sprintf("%s: %s", e.Message, e.Cause)
		}
	}
	return e.Message
}

//
// Rate Limiters
//

type ErrRateLimitBudgetNotFound struct{ BaseError }

var NewErrRateLimitBudgetNotFound = func(budgetId string) error {
	return &ErrRateLimitBudgetNotFound{
		BaseError{
			Code:    "ErrRateLimitBudgetNotFound",
			Message: "rate limit budget not found",
			Details: map[string]interface{}{
				"budgetId": budgetId,
			},
		},
	}
}

type ErrRateLimitRuleNotFound struct{ BaseError }

var NewErrRateLimitRuleNotFound = func(budgetId, method string) error {
	return &ErrRateLimitRuleNotFound{
		BaseError{
			Code:    "ErrRateLimitRuleNotFound",
			Message: "rate limit rule not found",
			Details: map[string]interface{}{
				"budgetId": budgetId,
				"method":   method,
			},
		},
	}
}

type ErrRateLimitInvalidConfig struct{ BaseError }

var NewErrRateLimitInvalidConfig = func(cause error) error {
	return &ErrRateLimitInvalidConfig{
		BaseError{
			Code:    "ErrRateLimitInvalidConfig",
			Message: "invalid rate limit config",
			Cause:   cause,
		},
	}
}

type ErrProjectRateLimitRuleExceeded struct{ BaseError }

const ErrCodeProjectRateLimitRuleExceeded ErrorCode = "ErrProjectRateLimitRuleExceeded"

var NewErrProjectRateLimitRuleExceeded = func(project string, budget string, rule string) error {
	return &ErrProjectRateLimitRuleExceeded{
		BaseError{
			Code:    ErrCodeProjectRateLimitRuleExceeded,
			Message: "project-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"project": project,
				"budget":  budget,
				"rule":    rule,
			},
		},
	}
}

func (e *ErrProjectRateLimitRuleExceeded) ErrorStatusCode() int {
	return http.StatusTooManyRequests
}

type ErrNetworkRateLimitRuleExceeded struct{ BaseError }

const ErrCodeNetworkRateLimitRuleExceeded ErrorCode = "ErrNetworkRateLimitRuleExceeded"

var NewErrNetworkRateLimitRuleExceeded = func(project string, network string, budget string, rule string) error {
	return &ErrNetworkRateLimitRuleExceeded{
		BaseError{
			Code:    ErrCodeNetworkRateLimitRuleExceeded,
			Message: "network-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"project": project,
				"network": network,
				"budget":  budget,
				"rule":    rule,
			},
		},
	}
}

func (e *ErrNetworkRateLimitRuleExceeded) ErrorStatusCode() int {
	return http.StatusTooManyRequests
}

type ErrNetworkRequestTimeout struct{ BaseError }

const ErrCodeNetworkRequestTimeout ErrorCode = "ErrNetworkRequestTimeout"

var NewErrNetworkRequestTimeout = func(duration time.Duration, cause error) error {
	return &ErrNetworkRequestTimeout{
		BaseError{
			Code:    ErrCodeNetworkRequestTimeout,
			Message: fmt.Sprintf("network-level request towards one or more upstreams timed out after %dms", duration.Milliseconds()),
			Cause:   cause,
		},
	}
}

func (e *ErrNetworkRequestTimeout) ErrorStatusCode() int {
	return http.StatusGatewayTimeout
}

type ErrUpstreamRateLimitRuleExceeded struct{ BaseError }

const ErrCodeUpstreamRateLimitRuleExceeded ErrorCode = "ErrUpstreamRateLimitRuleExceeded"

var NewErrUpstreamRateLimitRuleExceeded = func(upstreamId string, budget string, rule string) error {
	return &ErrUpstreamRateLimitRuleExceeded{
		BaseError{
			Code:    ErrCodeUpstreamRateLimitRuleExceeded,
			Message: "upstream-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
				"budget":     budget,
				"rule":       rule,
			},
		},
	}
}

func (e *ErrUpstreamRateLimitRuleExceeded) ErrorStatusCode() int {
	return http.StatusTooManyRequests
}

type ErrUpstreamExcludedByPolicy struct{ BaseError }

const ErrCodeUpstreamExcludedByPolicy ErrorCode = "ErrUpstreamExcludedByPolicy"

var NewErrUpstreamExcludedByPolicy = func(upstreamId string) error {
	return &ErrUpstreamExcludedByPolicy{
		BaseError{
			Code:    ErrCodeUpstreamExcludedByPolicy,
			Message: "upstream excluded by selection policy evaluation",
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

//
// Endpoint (3rd party providers, RPC nodes)
// Main purpose of these error types is internal eRPC error handling (retries, etc)
//

type ErrEndpointUnauthorized struct{ BaseError }

const ErrCodeEndpointUnauthorized = "ErrEndpointUnauthorized"

var NewErrEndpointUnauthorized = func(cause error) error {
	return &ErrEndpointUnauthorized{
		BaseError{
			Code:    ErrCodeEndpointUnauthorized,
			Message: "remote endpoint responded with Unauthorized",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointUnauthorized) ErrorStatusCode() int {
	return 401
}

type ErrEndpointUnsupported struct{ BaseError }

const ErrCodeEndpointUnsupported = "ErrEndpointUnsupported"

var NewErrEndpointUnsupported = func(cause error) error {
	return &ErrEndpointUnsupported{
		BaseError{
			Code:    ErrCodeEndpointUnsupported,
			Message: "remote endpoint does not support requested method",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointUnsupported) ErrorStatusCode() int {
	return http.StatusUnsupportedMediaType
}

type ErrEndpointClientSideException struct{ BaseError }

const ErrCodeEndpointClientSideException = "ErrEndpointClientSideException"

var NewErrEndpointClientSideException = func(cause error) error {
	return &ErrEndpointClientSideException{
		BaseError{
			Code:    ErrCodeEndpointClientSideException,
			Message: "client-side error when sending request to remote endpoint",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointClientSideException) ErrorStatusCode() int {
	if e.Cause != nil {
		if er, ok := e.Cause.(*ErrJsonRpcExceptionInternal); ok {
			switch er.NormalizedCode() {
			case JsonRpcErrorEvmReverted, JsonRpcErrorCallException:
				return 200
			}
		}
	}

	return http.StatusBadRequest
}

type ErrEndpointTransportFailure struct{ BaseError }

const ErrCodeEndpointTransportFailure = "ErrEndpointTransportFailure"

var NewErrEndpointTransportFailure = func(cause error) error {
	return &ErrEndpointTransportFailure{
		BaseError{
			Code:    ErrCodeEndpointTransportFailure,
			Message: "failure when sending request to remote endpoint",
			Cause:   cause,
		},
	}
}

type ErrEndpointServerSideException struct{ BaseError }

const ErrCodeEndpointServerSideException = "ErrEndpointServerSideException"

var NewErrEndpointServerSideException = func(cause error, details map[string]interface{}) error {
	return &ErrEndpointServerSideException{
		BaseError{
			Code:    ErrCodeEndpointServerSideException,
			Message: "an internal error on remote endpoint",
			Cause:   cause,
			Details: details,
		},
	}
}

func (e *ErrEndpointServerSideException) ErrorStatusCode() int {
	return 500
}

type ErrEndpointRequestTimeout struct{ BaseError }

const ErrCodeEndpointRequestTimeout = "ErrEndpointRequestTimeout"

var NewErrEndpointRequestTimeout = func(dur time.Duration, cause error) error {
	return &ErrEndpointRequestTimeout{
		BaseError{
			Code:    ErrCodeEndpointRequestTimeout,
			Message: "remote endpoint request timeout",
			Details: map[string]interface{}{
				"durationMs": dur.Milliseconds(),
			},
			Cause: cause,
		},
	}
}

func (e *ErrEndpointRequestTimeout) ErrorStatusCode() int {
	return http.StatusGatewayTimeout
}

type ErrEndpointRequestCanceled struct{ BaseError }

const ErrCodeEndpointRequestCanceled = "ErrEndpointRequestCanceled"

var NewErrEndpointRequestCanceled = func(cause error) error {
	return &ErrEndpointRequestCanceled{
		BaseError{
			Code:    ErrCodeEndpointRequestCanceled,
			Message: "remote endpoint request canceled (e.g. discarded hedge request)",
			Cause:   cause,
		},
	}
}

type ErrEndpointCapacityExceeded struct{ BaseError }

const ErrCodeEndpointCapacityExceeded = "ErrEndpointCapacityExceeded"

var NewErrEndpointCapacityExceeded = func(cause error) error {
	return &ErrEndpointCapacityExceeded{
		BaseError{
			Code:    ErrCodeEndpointCapacityExceeded,
			Message: "remote endpoint capacity exceeded",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointCapacityExceeded) ErrorStatusCode() int {
	return 429
}

type ErrEndpointBillingIssue struct{ BaseError }

const ErrCodeEndpointBillingIssue = "ErrEndpointBillingIssue"

var NewErrEndpointBillingIssue = func(cause error) error {
	return &ErrEndpointBillingIssue{
		BaseError{
			Code:    ErrCodeEndpointBillingIssue,
			Message: "remote endpoint billing issue",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointBillingIssue) ErrorStatusCode() int {
	return http.StatusPaymentRequired
}

type ErrEndpointMissingData struct{ BaseError }

const ErrCodeEndpointMissingData = "ErrEndpointMissingData"

var NewErrEndpointMissingData = func(cause error) error {
	return &ErrEndpointMissingData{
		BaseError{
			Code:    ErrCodeEndpointMissingData,
			Message: "remote endpoint does not have this data/block or not synced yet",
			Cause:   cause,
		},
	}
}

type ErrUpstreamNodeTypeMismatch struct{ BaseError }

const ErrCodeUpstreamNodeTypeMismatch = "ErrUpstreamNodeTypeMismatch"

var NewErrUpstreamNodeTypeMismatch = func(cause error, expected EvmNodeType, actual EvmNodeType) error {
	return &ErrUpstreamNodeTypeMismatch{
		BaseError{
			Code:    ErrCodeUpstreamNodeTypeMismatch,
			Message: "node type does not match what is required for this request",
			Cause:   cause,
			Details: map[string]interface{}{
				"expected": expected,
				"actual":   actual,
			},
		},
	}
}

type ErrEndpointRequestTooLarge struct{ BaseError }

const ErrCodeEndpointRequestTooLarge = "ErrEndpointRequestTooLarge"

type TooLargeComplaint string

const EvmBlockRangeTooLarge TooLargeComplaint = "evm_block_range"
const EvmAddressesTooLarge TooLargeComplaint = "evm_addresses"

var NewErrEndpointRequestTooLarge = func(cause error, complaint TooLargeComplaint) error {
	return &ErrEndpointRequestTooLarge{
		BaseError{
			Code:    ErrCodeEndpointRequestTooLarge,
			Message: "remote endpoint complained about large request (e.g. block range, number of addresses, etc)",
			Cause:   cause,
			Details: map[string]interface{}{
				"complaint": complaint,
			},
		},
	}
}

func (e *ErrEndpointRequestTooLarge) ErrorStatusCode() int {
	return http.StatusRequestEntityTooLarge
}

//
// JSON-RPC
//

//
// These error numbers are necessary to represent errors in json-rpc spec (i.e. numeric codes)
// eRPC will do a best-effort to normalize these errors across 3rd-party providers
// The main purpose of these error numbers is for client usage i.e. internal error handling of erpc must rely on error types above
//

type JsonRpcErrorNumber int

const (
	JsonRpcErrorUnknown JsonRpcErrorNumber = -99999

	// Standard JSON-RPC codes
	JsonRpcErrorCallException        JsonRpcErrorNumber = -32000
	JsonRpcErrorClientSideException  JsonRpcErrorNumber = -32600
	JsonRpcErrorUnsupportedException JsonRpcErrorNumber = -32601
	JsonRpcErrorInvalidArgument      JsonRpcErrorNumber = -32602
	JsonRpcErrorServerSideException  JsonRpcErrorNumber = -32603
	JsonRpcErrorParseException       JsonRpcErrorNumber = -32700

	// Defacto codes used by majority of 3rd-party providers
	JsonRpcErrorEvmReverted JsonRpcErrorNumber = 3

	// Normalized blockchain-specific codes by eRPC
	JsonRpcErrorCapacityExceeded JsonRpcErrorNumber = -32005
	JsonRpcErrorEvmLargeRange    JsonRpcErrorNumber = -32012
	JsonRpcErrorMissingData      JsonRpcErrorNumber = -32014
	JsonRpcErrorNodeTimeout      JsonRpcErrorNumber = -32015
	JsonRpcErrorUnauthorized     JsonRpcErrorNumber = -32016
)

// This struct represents an json-rpc error with erpc structure (i.e. code is string)
type ErrJsonRpcExceptionInternal struct{ BaseError }

const ErrCodeJsonRpcExceptionInternal = "ErrJsonRpcExceptionInternal"

var NewErrJsonRpcExceptionInternal = func(originalCode int, normalizedCode JsonRpcErrorNumber, message string, cause error, details map[string]interface{}) *ErrJsonRpcExceptionInternal {
	if details == nil {
		details = make(map[string]interface{})
	}
	if originalCode != 0 {
		details["originalCode"] = originalCode
	}
	if normalizedCode != 0 {
		details["normalizedCode"] = normalizedCode
	}

	return &ErrJsonRpcExceptionInternal{
		BaseError{
			Code:    ErrCodeJsonRpcExceptionInternal,
			Message: message,
			Details: details,
			Cause:   cause,
		},
	}
}

func (e *ErrJsonRpcExceptionInternal) ErrorStatusCode() int {
	if e.Cause != nil {
		if er, ok := e.Cause.(StandardError); ok {
			return er.ErrorStatusCode()
		}
	}
	return http.StatusBadRequest
}

func (e *ErrJsonRpcExceptionInternal) CodeChain() string {
	return fmt.Sprintf("%d <- %s", e.NormalizedCode(), e.BaseError.CodeChain())
}

func (e *ErrJsonRpcExceptionInternal) NormalizedCode() JsonRpcErrorNumber {
	if code, ok := e.Details["normalizedCode"]; ok {
		return code.(JsonRpcErrorNumber)
	}
	return 0
}

func (e *ErrJsonRpcExceptionInternal) OriginalCode() int {
	if code, ok := e.Details["originalCode"]; ok {
		if ic, ok := code.(int); ok {
			return ic
		} else if fc, ok := code.(float64); ok {
			return int(fc)
		}
	}
	return 0
}

// This struct represents an json-rpc error with standard structure (i.e. code is int)
type ErrJsonRpcExceptionExternal struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	// Some errors such as execution reverted carry "data" field which has additional information
	Data interface{} `json:"data,omitempty"`
}

func NewErrJsonRpcExceptionExternal(code int, message string, data string) *ErrJsonRpcExceptionExternal {
	return &ErrJsonRpcExceptionExternal{
		Code:    code,
		Message: message,
		Data:    data,
	}
}

func (e *ErrJsonRpcExceptionExternal) Error() string {
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

func (e *ErrJsonRpcExceptionExternal) CodeChain() string {
	return fmt.Sprintf("%d", e.Code)
}

func (e *ErrJsonRpcExceptionExternal) HasCode(code ErrorCode) bool {
	// This specific type does not have "cause" as it's directly returned by upstreams
	return false
}

func (e *ErrJsonRpcExceptionExternal) DeepestMessage() string {
	// This specific type does not have "cause" as it's directly returned by upstreams
	return e.Message
}

func (e *ErrJsonRpcExceptionExternal) GetCause() error {
	// This specific type does not have "cause" as it's directly returned by upstreams
	return nil
}

//
// Store
//

type ErrInvalidConnectorDriver struct{ BaseError }

const ErrCodeInvalidConnectorDriver = "ErrInvalidConnectorDriver"

var NewErrInvalidConnectorDriver = func(driver ConnectorDriverType) error {
	return &ErrInvalidConnectorDriver{
		BaseError{
			Code:    ErrCodeInvalidConnectorDriver,
			Message: "invalid store driver",
			Details: map[string]interface{}{
				"driver": driver,
			},
		},
	}
}

type ErrRecordNotFound struct{ BaseError }

const ErrCodeRecordNotFound = "ErrRecordNotFound"

var NewErrRecordNotFound = func(pk, rk, driver string) error {
	return &ErrRecordNotFound{
		BaseError{
			Code:    ErrCodeRecordNotFound,
			Message: "record not found",
			Details: map[string]interface{}{
				"partitionKey": pk,
				"rangeKey":     rk,
				"driver":       driver,
			},
		},
	}
}

type ErrRecordExpired struct{ BaseError }

const ErrCodeRecordExpired = "ErrRecordExpired"

var NewErrRecordExpired = func(pk, rk, driver string, now, expirationTime int64) error {
	return &ErrRecordExpired{
		BaseError{
			Code:    ErrCodeRecordExpired,
			Message: "record expired",
			Details: map[string]interface{}{
				"partitionKey": pk,
				"rangeKey":     rk,
				"driver":       driver,
				"now":          now,
				"expiration":   expirationTime,
			},
		},
	}
}

func HasErrorCode(err error, codes ...ErrorCode) bool {
	if err == nil {
		return false
	}

	if be, ok := err.(StandardError); ok {
		return be.HasCode(codes...)
	}

	if be, ok := err.(*BaseError); ok {
		for _, code := range codes {
			if be.Code == code {
				return true
			}
		}
	}

	if arr, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range arr.Unwrap() {
			if HasErrorCode(e, codes...) {
				return true
			}
		}
	}

	return false
}

func IsRetryableTowardsUpstream(err error) bool {
	return !HasErrorCode(
		err,

		// Circuit breaker is open -> No Retry
		ErrCodeFailsafeCircuitBreakerOpen,

		// Unsupported features and methods -> No Retry
		ErrCodeUpstreamRequestSkipped,
		ErrCodeUpstreamMethodIgnored,
		ErrCodeEndpointUnsupported,

		// Do not try when 3rd-party providers run out of monthly capacity or billing issues
		ErrCodeEndpointBillingIssue,

		// 400 / 404 / 405 / 413 -> No Retry
		// RPC-RPC client-side error (invalid params) -> No Retry
		ErrCodeEndpointClientSideException,
		ErrCodeJsonRpcRequestUnmarshal,

		// Upstream-level + 401 / 403 -> No Retry
		// RPC vendor billing/capacity/auth -> No Retry
		ErrCodeEndpointUnauthorized,
	)
}

func IsCapacityIssue(err error) bool {
	return HasErrorCode(
		err,
		ErrCodeProjectRateLimitRuleExceeded,
		ErrCodeNetworkRateLimitRuleExceeded,
		ErrCodeUpstreamRateLimitRuleExceeded,
		ErrCodeAuthRateLimitRuleExceeded,
		ErrCodeEndpointCapacityExceeded,
	)
}

func IsClientError(err error) bool {
	return err != nil && (HasErrorCode(
		err,
		ErrCodeEndpointClientSideException,
		ErrCodeJsonRpcRequestUnmarshal,
	))
}

// Severity represents how "alert-worthy" an error is.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

func ClassifySeverity(err error) Severity {
	if err == nil {
		return SeverityInfo
	}
	if IsClientError(err) {
		return SeverityInfo
	}
	if IsCapacityIssue(err) || !IsRetryableTowardsUpstream(err) {
		return SeverityWarning
	}
	// Usually context cancellation is due to discarded hedged requests.
	// Theoretically any cancellation must be intentional and must not be considered as a critical error.
	// Upstream timeouts will be DeadlineExceeded errors or other forms of timeout errors which will be classified as SeverityCritical.
	// This is considered warning (not info) because it still means the upstream was too slow to respond (in case of a hedge).
	if HasErrorCode(err, ErrCodeEndpointRequestCanceled) {
		return SeverityWarning
	}
	return SeverityCritical
}
