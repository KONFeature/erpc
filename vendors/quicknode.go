package vendors

import (
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
)

type QuicknodeVendor struct {
	common.Vendor
}

func CreateQuicknodeVendor() common.Vendor {
	return &QuicknodeVendor{}
}

func (v *QuicknodeVendor) Name() string {
	return "quicknode"
}

func (v *QuicknodeVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.OriginalCode(); code != 0 {
		msg := err.Message

		if code == -32602 && strings.Contains(msg, "eth_getLogs") && strings.Contains(msg, "limited") {
			return common.NewErrEndpointEvmLargeRange(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorEvmLogsLargeRange, msg, nil),
			)
		} else if code == -32000 {
			if strings.Contains(msg, "header not found") || strings.Contains(msg, "could not find block") {
				return common.NewErrEndpointNotSyncedYet(
					common.NewErrJsonRpcException(code, common.JsonRpcErrorNotSyncedYet, msg, nil),
				)
			} else if strings.Contains(msg, "execution timeout") {
				return common.NewErrEndpointNodeTimeout(
					common.NewErrJsonRpcException(code, common.JsonRpcErrorNodeTimeout, msg, nil),
				)
			}
		} else if code == -32009 || code == -32007 {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorCapacityExceeded, msg, nil),
			)
		} else if code == -32612 || code == -32613 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorCapacityExceeded, msg, nil),
			)
		} else if strings.Contains(msg, "failed to parse") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorParseException, msg, nil),
			)
		} else if code == -32010 || code == -32015 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorClientSideException, msg, nil),
			)
		} else if code == -32602 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorInvalidArgument, msg, nil),
			)
		} else if code == -32011 || code == -32603 {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorServerSideException, msg, nil),
			)
		} else if code == 3 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcException(
					code,
					common.JsonRpcErrorEvmReverted,
					msg,
					nil,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *QuicknodeVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".quiknode.pro")
}
