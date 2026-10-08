package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRepositoryListAllPreservesUserFiltersAndSoftDelete(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "all-keys@test.com")
	other := mustCreateAPIKeyRepoUser(t, ctx, client, "other-all-keys@test.com")
	group, err := client.Group.Create().SetName("Filtered").Save(ctx)
	require.NoError(t, err)
	create := func(userID int64, name, status string, groupID *int64) int64 {
		key := &service.APIKey{UserID: userID, Key: "sk-" + name, Name: name, Status: status, GroupID: groupID}
		require.NoError(t, repo.Create(ctx, key))
		return key.ID
	}
	first := create(user.ID, "match-first", service.StatusActive, &group.ID)
	second := create(user.ID, "match-second", service.StatusActive, &group.ID)
	ungrouped := create(user.ID, "match-ungrouped", service.StatusActive, nil)
	create(other.ID, "match-other", service.StatusActive, &group.ID)
	create(user.ID, "excluded-search", service.StatusActive, &group.ID)
	create(user.ID, "match-disabled", service.StatusDisabled, &group.ID)
	deleted := create(user.ID, "match-deleted", service.StatusActive, &group.ID)
	require.NoError(t, repo.Delete(ctx, deleted))
	zero := int64(0)
	for _, tc := range []struct {
		name string
		gid  *int64
		want []int64
	}{
		{"all matching", nil, []int64{first, second, ungrouped}},
		{"group", &group.ID, []int64{first, second}},
		{"ungrouped", &zero, []int64{ungrouped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := repo.ListAllByUserID(ctx, user.ID, service.APIKeyListFilters{Search: "match", Status: service.StatusActive, GroupID: tc.gid})
			require.NoError(t, err)
			got := make([]int64, 0, len(keys))
			for _, key := range keys {
				got = append(got, key.ID)
				if key.GroupID != nil {
					require.NotNil(t, key.Group)
				}
			}
			require.Equal(t, tc.want, got)
		})
	}
}
