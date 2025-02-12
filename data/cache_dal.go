package data

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
)

type CacheDAL interface {
	Set(ctx context.Context, req *upstream.NormalizedRequest, res common.NormalizedResponse) error
	Get(ctx context.Context, req *upstream.NormalizedRequest) (common.NormalizedResponse, error)
	DeleteByGroupKey(ctx context.Context, groupKeys ...string) error
}
