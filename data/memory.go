package data

import (
	"context"
	"fmt"
	"strings"

	"github.com/flair-sdk/erpc/common"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	MemoryDriverName = "memory"
)

var _ Connector = (*MemoryConnector)(nil)

type MemoryConnector struct {
	cache *lru.Cache[string, string]
}

func NewMemoryConnector(ctx context.Context, cfg *common.MemoryConnectorConfig) (*MemoryConnector, error) {
	if cfg != nil && cfg.MaxItems <= 0 {
		return nil, fmt.Errorf("maxItems must be greater than 0")
	}

	maxItems := 1000
	if cfg != nil && cfg.MaxItems > 0 {
		maxItems = cfg.MaxItems
	}

	cache, err := lru.New[string, string](maxItems)
	if err != nil {
		return nil, fmt.Errorf("failed to create LRU cache: %w", err)
	}

	return &MemoryConnector{
		cache: cache,
	}, nil
}

func (m *MemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.cache.Add(key, value)
	return nil
}

func (m *MemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if strings.HasSuffix(partitionKey, "*") {
		return m.getWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	value, ok := m.cache.Get(key)
	if !ok {
		return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), MemoryDriverName)
	}
	return value, nil
}

func (m *MemoryConnector) getWithWildcard(_ context.Context, _, partitionKey, rangeKey string) (string, error) {
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	for _, k := range m.cache.Keys() {
		if common.WildcardMatch(key, k) {
			value, _ := m.cache.Get(k)
			return value, nil
		}
	}
	return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), MemoryDriverName)
}

func (m *MemoryConnector) Query(ctx context.Context, index, partitionKey, rangeKey string) ([]*DataRow, error) {
	prefix := strings.TrimSuffix(partitionKey, "*")
	var results []*DataRow

	for _, key := range m.cache.Keys() {
		parts := strings.Split(key, ":")
		if len(parts) == 2 && strings.HasPrefix(parts[0], prefix) {
			if rangeKey == "" || (strings.HasSuffix(rangeKey, "*") && strings.HasPrefix(parts[1], strings.TrimSuffix(rangeKey, "*"))) || parts[1] == rangeKey {
				value, _ := m.cache.Get(key)
				results = append(results, &DataRow{Value: value})
			}
		}
	}

	return results, nil
}

func (m *MemoryConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
	if strings.HasSuffix(partitionKey, "*") || strings.HasSuffix(rangeKey, "*") {
		return m.deleteWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.cache.Remove(key)
	return nil
}

func (m *MemoryConnector) deleteWithWildcard(_ context.Context, _, partitionKey, rangeKey string) error {
	prefixPK := strings.TrimSuffix(partitionKey, "*")
	prefixRK := strings.TrimSuffix(rangeKey, "*")

	for _, key := range m.cache.Keys() {
		parts := strings.Split(key, ":")
		if len(parts) == 2 &&
			(partitionKey == "*" || strings.HasPrefix(parts[0], prefixPK)) &&
			(rangeKey == "*" || strings.HasPrefix(parts[1], prefixRK)) {
			m.cache.Remove(key)
		}
	}

	return nil
}

func (m *MemoryConnector) Close(ctx context.Context) error {
	return nil
}
