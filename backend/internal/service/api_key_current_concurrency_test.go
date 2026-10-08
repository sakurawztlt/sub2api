//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type apiKeyLiveConcurrencyRepo struct {
	quotaBaseAPIKeyRepoStub
	keys           []APIKey
	allCalls       int
	paginatedCalls int
	userID         int64
	filters        APIKeyListFilters
}

func (r *apiKeyLiveConcurrencyRepo) ListAllByUserID(_ context.Context, userID int64, filters APIKeyListFilters) ([]APIKey, error) {
	r.allCalls++
	r.userID, r.filters = userID, filters
	return append([]APIKey(nil), r.keys...), nil
}

func (r *apiKeyLiveConcurrencyRepo) ListByUserID(_ context.Context, userID int64, _ pagination.PaginationParams, filters APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	r.paginatedCalls++
	r.userID, r.filters = userID, filters
	return append([]APIKey(nil), r.keys[:1]...), &pagination.PaginationResult{Total: int64(len(r.keys))}, nil
}

func (r *apiKeyLiveConcurrencyRepo) GetByID(_ context.Context, id int64) (*APIKey, error) {
	for _, key := range r.keys {
		if key.ID == id {
			return &key, nil
		}
	}
	return nil, ErrAPIKeyNotFound
}

type apiKeyLiveConcurrencyCache struct {
	ConcurrencyCache
	counts map[int64]int
	reads  [][]int64
	err    error
}

func (c *apiKeyLiveConcurrencyCache) TrackAPIKeySlot(context.Context, int64, string) error {
	return nil
}
func (c *apiKeyLiveConcurrencyCache) ReleaseAPIKeySlot(context.Context, int64, string) error {
	return nil
}
func (c *apiKeyLiveConcurrencyCache) GetAPIKeyConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	c.reads = append(c.reads, append([]int64(nil), ids...))
	return c.counts, c.err
}

func TestAPIKeyCurrentConcurrencySortBeforePagination(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order string
		page  int
		want  []int64
	}{
		{"descending first", "desc", 1, []int64{3, 2}},
		{"descending second", "desc", 2, []int64{4, 1}},
		{"ascending first", "asc", 1, []int64{1, 4}},
		{"ascending second", "asc", 2, []int64{2, 3}},
		{"out of range", "desc", 3, []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &apiKeyLiveConcurrencyRepo{keys: []APIKey{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}}
			cache := &apiKeyLiveConcurrencyCache{counts: map[int64]int{1: 1, 2: 5, 3: 5, 4: 2}}
			svc := &APIKeyService{apiKeyRepo: repo}
			svc.SetConcurrencyService(NewConcurrencyService(cache))
			groupID := int64(10)
			filters := APIKeyListFilters{Search: "mine", Status: StatusActive, GroupID: &groupID}
			keys, result, err := svc.List(context.Background(), 77, pagination.PaginationParams{
				Page: tc.page, PageSize: 2, SortBy: " CURRENT_CONCURRENCY ", SortOrder: tc.order,
			}, filters)
			require.NoError(t, err)
			got := make([]int64, 0, len(keys))
			for _, key := range keys {
				got = append(got, key.ID)
				require.Equal(t, cache.counts[key.ID], key.CurrentConcurrency)
			}
			require.Equal(t, tc.want, got)
			require.EqualValues(t, 4, result.Total)
			require.Equal(t, 2, result.Pages)
			require.Equal(t, 1, repo.allCalls)
			require.Zero(t, repo.paginatedCalls)
			require.EqualValues(t, 77, repo.userID)
			require.Equal(t, filters, repo.filters)
			require.Equal(t, [][]int64{{1, 2, 3, 4}}, cache.reads, "fetch all live counts in one batch before sorting")
		})
	}
}

func TestAPIKeyCurrentConcurrencyListAndGet(t *testing.T) {
	repo := &apiKeyLiveConcurrencyRepo{keys: []APIKey{{ID: 1}, {ID: 2}}}
	cache := &apiKeyLiveConcurrencyCache{counts: map[int64]int{1: 7, 2: 4}}
	svc := &APIKeyService{apiKeyRepo: repo}
	svc.SetConcurrencyService(NewConcurrencyService(cache))
	keys, _, err := svc.List(context.Background(), 77, pagination.PaginationParams{Page: 1, PageSize: 1, SortBy: "name"}, APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, 7, keys[0].CurrentConcurrency)
	require.Equal(t, 1, repo.paginatedCalls)
	require.Zero(t, repo.allCalls)
	key, err := svc.GetByID(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, 4, key.CurrentConcurrency)
	require.Equal(t, [][]int64{{1}, {2}}, cache.reads)
}

func TestAPIKeyCurrentConcurrencyRedisFailureKeepsKeysAvailable(t *testing.T) {
	repo := &apiKeyLiveConcurrencyRepo{keys: []APIKey{{ID: 1}, {ID: 2}}}
	cache := &apiKeyLiveConcurrencyCache{err: errors.New("redis unavailable")}
	svc := &APIKeyService{apiKeyRepo: repo}
	svc.SetConcurrencyService(NewConcurrencyService(cache))
	keys, _, err := svc.List(context.Background(), 77, pagination.PaginationParams{Page: 1, PageSize: 2, SortBy: "current_concurrency", SortOrder: "desc"}, APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, keys, 2)
	require.EqualValues(t, 2, keys[0].ID)
	require.Zero(t, keys[0].CurrentConcurrency)
	require.Zero(t, keys[1].CurrentConcurrency)
}
