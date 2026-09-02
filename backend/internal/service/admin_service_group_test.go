//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type groupRepoStubForAdmin struct {
	created  *Group // 记录 Create 调用的参数
	updated  *Group // 记录 Update 调用的参数
	getByID  *Group // GetByID 返回值
	getErr   error  // GetByID 返回的错误
	createID int64

	getByIDByID map[int64]*Group

	deleteAccountGroupsByGroupIDFn func(groupID int64) (int64, error)
	bindAccountsToGroupFn          func(groupID int64, accountIDs []int64) error
	getAccountIDsByGroupIDsFn      func(groupIDs []int64) ([]int64, error)

	listWithFiltersCalls       int
	listWithFiltersParams      pagination.PaginationParams
	listWithFiltersPlatform    string
	listWithFiltersStatus      string
	listWithFiltersSearch      string
	listWithFiltersIsExclusive *bool
	listWithFiltersGroups      []Group
	listWithFiltersResult      *pagination.PaginationResult
	listWithFiltersErr         error
}

func (s *groupRepoStubForAdmin) Create(_ context.Context, g *Group) error {
	if s.createID > 0 {
		g.ID = s.createID
	} else if g.ID == 0 {
		g.ID = 101
	}
	s.created = g
	return nil
}

func (s *groupRepoStubForAdmin) Update(_ context.Context, g *Group) error {
	s.updated = g
	return nil
}

func (s *groupRepoStubForAdmin) GetByID(_ context.Context, id int64) (*Group, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getByIDByID != nil {
		if group, ok := s.getByIDByID[id]; ok {
			return group, nil
		}
		return nil, ErrGroupNotFound
	}
	return s.getByID, nil
}

func (s *groupRepoStubForAdmin) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getByIDByID != nil {
		if group, ok := s.getByIDByID[id]; ok {
			return group, nil
		}
		return nil, ErrGroupNotFound
	}
	return s.getByID, nil
}

func (s *groupRepoStubForAdmin) Delete(_ context.Context, _ int64) error {
	panic("unexpected Delete call")
}

func (s *groupRepoStubForAdmin) DeleteCascade(_ context.Context, _ int64) ([]int64, error) {
	panic("unexpected DeleteCascade call")
}

func (s *groupRepoStubForAdmin) List(_ context.Context, _ pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (s *groupRepoStubForAdmin) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, status, search string, isExclusive *bool) ([]Group, *pagination.PaginationResult, error) {
	s.listWithFiltersCalls++
	s.listWithFiltersParams = params
	s.listWithFiltersPlatform = platform
	s.listWithFiltersStatus = status
	s.listWithFiltersSearch = search
	s.listWithFiltersIsExclusive = isExclusive

	if s.listWithFiltersErr != nil {
		return nil, nil, s.listWithFiltersErr
	}

	result := s.listWithFiltersResult
	if result == nil {
		result = &pagination.PaginationResult{
			Total:    int64(len(s.listWithFiltersGroups)),
			Page:     params.Page,
			PageSize: params.PageSize,
		}
	}

	return s.listWithFiltersGroups, result, nil
}

func (s *groupRepoStubForAdmin) ListActive(_ context.Context) ([]Group, error) {
	panic("unexpected ListActive call")
}

func (s *groupRepoStubForAdmin) ListActiveByPlatform(_ context.Context, _ string) ([]Group, error) {
	panic("unexpected ListActiveByPlatform call")
}

func (s *groupRepoStubForAdmin) ExistsByName(_ context.Context, _ string) (bool, error) {
	panic("unexpected ExistsByName call")
}

func (s *groupRepoStubForAdmin) GetAccountCount(_ context.Context, _ int64) (int64, int64, error) {
	panic("unexpected GetAccountCount call")
}

func (s *groupRepoStubForAdmin) DeleteAccountGroupsByGroupID(_ context.Context, groupID int64) (int64, error) {
	if s.deleteAccountGroupsByGroupIDFn != nil {
		return s.deleteAccountGroupsByGroupIDFn(groupID)
	}
	panic("unexpected DeleteAccountGroupsByGroupID call")
}

func (s *groupRepoStubForAdmin) BindAccountsToGroup(_ context.Context, groupID int64, accountIDs []int64) error {
	if s.bindAccountsToGroupFn != nil {
		return s.bindAccountsToGroupFn(groupID, accountIDs)
	}
	panic("unexpected BindAccountsToGroup call")
}

func (s *groupRepoStubForAdmin) GetAccountIDsByGroupIDs(_ context.Context, groupIDs []int64) ([]int64, error) {
	if s.getAccountIDsByGroupIDsFn != nil {
		return s.getAccountIDsByGroupIDsFn(groupIDs)
	}
	panic("unexpected GetAccountIDsByGroupIDs call")
}

func (s *groupRepoStubForAdmin) UpdateSortOrders(_ context.Context, _ []GroupSortOrderUpdate) error {
	return nil
}

func TestAdminService_CreateGroup_RejectsTimePricing(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 51}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "time-pricing-group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1,
		ModelPricing: []ChannelModelPricing{{
			Platform:    PlatformOpenAI,
			Models:      []string{"gpt-5"},
			BillingMode: BillingModeToken,
			TimePricing: validTimePricingForTest(),
		}},
	})

	require.Error(t, err)
	appErr := infraerrors.FromError(err)
	require.Equal(t, int32(http.StatusBadRequest), appErr.Code)
	require.Equal(t, "GROUP_MODEL_TIME_PRICING_UNSUPPORTED", appErr.Reason)
	require.Nil(t, repo.created)
}

func TestAdminService_UpdateGroup_RejectsTimePricing(t *testing.T) {
	existing := &Group{ID: 1, Name: "existing", Platform: PlatformOpenAI, Status: StatusActive}
	repo := &groupRepoStubForAdmin{getByID: existing}
	svc := &adminServiceImpl{groupRepo: repo}
	pricing := []ChannelModelPricing{{
		Platform:    PlatformOpenAI,
		Models:      []string{"gpt-5"},
		BillingMode: BillingModeToken,
		TimePricing: validTimePricingForTest(),
	}}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{ModelPricing: &pricing})

	require.Error(t, err)
	appErr := infraerrors.FromError(err)
	require.Equal(t, int32(http.StatusBadRequest), appErr.Code)
	require.Equal(t, "GROUP_MODEL_TIME_PRICING_UNSUPPORTED", appErr.Reason)
	require.Nil(t, repo.updated)
}

func TestNormalizeGroupModelPricing_NormalizesEmptyTimePricing(t *testing.T) {
	pricing, err := normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{{
		Models:      []string{"gpt-5"},
		BillingMode: BillingModeToken,
		TimePricing: &ChannelTimePricing{Timezone: "Asia/Shanghai"},
	}})

	require.NoError(t, err)
	require.Len(t, pricing, 1)
	require.Nil(t, pricing[0].TimePricing)
}

type compositeRouteRepoStubForAdmin struct {
	routes    []CompositeModelRoute
	created   *CompositeModelRoute
	updated   *CompositeModelRoute
	deleted   []int64
	nextID    int64
	listErr   error
	createErr error
	updateErr error
	deleteErr error
}

func (s *compositeRouteRepoStubForAdmin) ListByGroup(_ context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	routes := make([]CompositeModelRoute, 0, len(s.routes))
	for _, route := range s.routes {
		if route.GroupID != groupID {
			continue
		}
		if !includeDisabled && !route.Enabled {
			continue
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func (s *compositeRouteRepoStubForAdmin) Create(_ context.Context, route *CompositeModelRoute) error {
	if s.createErr != nil {
		return s.createErr
	}
	if s.nextID > 0 {
		route.ID = s.nextID
	}
	cloned := *route
	s.created = &cloned
	s.routes = append(s.routes, cloned)
	return nil
}

func (s *compositeRouteRepoStubForAdmin) Update(_ context.Context, route *CompositeModelRoute) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	cloned := *route
	s.updated = &cloned
	for i := range s.routes {
		if s.routes[i].ID == route.ID {
			s.routes[i] = cloned
			return nil
		}
	}
	s.routes = append(s.routes, cloned)
	return nil
}

func (s *compositeRouteRepoStubForAdmin) Delete(_ context.Context, id int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *compositeRouteRepoStubForAdmin) DeleteByGroup(_ context.Context, groupID int64) error {
	next := s.routes[:0]
	for _, route := range s.routes {
		if route.GroupID != groupID {
			next = append(next, route)
		}
	}
	s.routes = next
	return nil
}

func TestAdminService_ListGroups_PassesSortParams(t *testing.T) {
	repo := &groupRepoStubForAdmin{
		listWithFiltersGroups: []Group{{ID: 1, Name: "g1"}},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, _, err := svc.ListGroups(context.Background(), 3, 25, PlatformOpenAI, StatusActive, "needle", nil, "account_count", "ASC")
	require.NoError(t, err)
	require.Equal(t, pagination.PaginationParams{
		Page:      3,
		PageSize:  25,
		SortBy:    "account_count",
		SortOrder: "ASC",
	}, repo.listWithFiltersParams)
}

// TestAdminService_CreateGroup_WithImagePricing 测试创建分组时 ImagePrice 字段正确传递
func TestAdminService_CreateGroup_WithImagePricing(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	price1K := 0.10
	price2K := 0.15
	price4K := 0.30

	input := &CreateGroupInput{
		Name:           "test-group",
		Description:    "Test group",
		Platform:       PlatformAntigravity,
		RateMultiplier: 1.0,
		ImagePrice1K:   &price1K,
		ImagePrice2K:   &price2K,
		ImagePrice4K:   &price4K,
	}

	group, err := svc.CreateGroup(context.Background(), input)
	require.NoError(t, err)
	require.NotNil(t, group)

	// 验证 repo 收到了正确的字段
	require.NotNil(t, repo.created)
	require.NotNil(t, repo.created.ImagePrice1K)
	require.NotNil(t, repo.created.ImagePrice2K)
	require.NotNil(t, repo.created.ImagePrice4K)
	require.InDelta(t, 0.10, *repo.created.ImagePrice1K, 0.0001)
	require.InDelta(t, 0.15, *repo.created.ImagePrice2K, 0.0001)
	require.InDelta(t, 0.30, *repo.created.ImagePrice4K, 0.0001)
}

func TestAdminService_CreateGroup_WithVideoPricing(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	price480P := 0.08
	price720P := 0.12
	price1080P := 0.18
	videoMultiplier := 0.75

	input := &CreateGroupInput{
		Name:                 "grok-video",
		Description:          "Grok video group",
		Platform:             PlatformGrok,
		RateMultiplier:       1.0,
		VideoRateIndependent: true,
		VideoRateMultiplier:  &videoMultiplier,
		VideoPrice480P:       &price480P,
		VideoPrice720P:       &price720P,
		VideoPrice1080P:      &price1080P,
	}

	group, err := svc.CreateGroup(context.Background(), input)
	require.NoError(t, err)
	require.NotNil(t, group)

	require.NotNil(t, repo.created)
	require.True(t, repo.created.VideoRateIndependent)
	require.InDelta(t, 0.75, repo.created.VideoRateMultiplier, 1e-12)
	require.NotNil(t, repo.created.VideoPrice480P)
	require.NotNil(t, repo.created.VideoPrice720P)
	require.NotNil(t, repo.created.VideoPrice1080P)
	require.InDelta(t, 0.08, *repo.created.VideoPrice480P, 0.0001)
	require.InDelta(t, 0.12, *repo.created.VideoPrice720P, 0.0001)
	require.InDelta(t, 0.18, *repo.created.VideoPrice1080P, 0.0001)
}

// TestAdminService_CreateGroup_NilImagePricing 测试 ImagePrice 为 nil 时正常创建
func TestAdminService_CreateGroup_NilImagePricing(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	input := &CreateGroupInput{
		Name:           "test-group",
		Description:    "Test group",
		Platform:       PlatformAntigravity,
		RateMultiplier: 1.0,
		// ImagePrice 字段全部为 nil
	}

	group, err := svc.CreateGroup(context.Background(), input)
	require.NoError(t, err)
	require.NotNil(t, group)

	// 验证 ImagePrice 字段为 nil
	require.NotNil(t, repo.created)
	require.Nil(t, repo.created.ImagePrice1K)
	require.Nil(t, repo.created.ImagePrice2K)
	require.Nil(t, repo.created.ImagePrice4K)
}

func TestAdminService_CreateGroup_ExclusiveStandardGroupAutoGrantsCreator(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	userRepo := &userRepoStubForGroupUpdate{}
	operatorUserID := int64(7)
	svc := &adminServiceImpl{
		groupRepo: repo,
		userRepo:  userRepo,
	}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "vip-only",
		Description:    "exclusive group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1.0,
		IsExclusive:    true,
		OperatorUserID: &operatorUserID,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.True(t, userRepo.addGroupCalled)
	require.Equal(t, operatorUserID, userRepo.addedUserID)
	require.Equal(t, group.ID, userRepo.addedGroupID)
}

func TestAdminService_CreateGroup_PublicGroupDoesNotGrantCreator(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	userRepo := &userRepoStubForGroupUpdate{}
	operatorUserID := int64(7)
	svc := &adminServiceImpl{
		groupRepo: repo,
		userRepo:  userRepo,
	}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "public-group",
		Description:    "public",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1.0,
		IsExclusive:    false,
		OperatorUserID: &operatorUserID,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.False(t, userRepo.addGroupCalled)
}

func TestAdminService_CreateGroup_ExclusiveSubscriptionGroupDoesNotGrantCreator(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	userRepo := &userRepoStubForGroupUpdate{}
	operatorUserID := int64(7)
	svc := &adminServiceImpl{
		groupRepo: repo,
		userRepo:  userRepo,
	}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:             "exclusive-sub",
		Description:      "subscription group",
		Platform:         PlatformOpenAI,
		RateMultiplier:   1.0,
		IsExclusive:      true,
		OperatorUserID:   &operatorUserID,
		SubscriptionType: SubscriptionTypeSubscription,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.False(t, userRepo.addGroupCalled)
}

func TestAdminService_CreateGroup_DefaultsGrokMediaGenerationEnabled(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "grok-media",
		Description:    "Grok media group",
		Platform:       PlatformGrok,
		RateMultiplier: 1.0,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.True(t, repo.created.AllowImageGeneration)
	require.True(t, group.AllowImageGeneration)
}

func TestAdminService_CreateGroup_PreservesNonGrokImageGenerationDisabled(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "anthropic-text",
		Description:    "Anthropic text group",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1.0,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.False(t, repo.created.AllowImageGeneration)
	require.False(t, group.AllowImageGeneration)
}

// TestAdminService_UpdateGroup_WithImagePricing 测试更新分组时 ImagePrice 字段正确更新
func TestAdminService_UpdateGroup_WithImagePricing(t *testing.T) {
	existingGroup := &Group{
		ID:       1,
		Name:     "existing-group",
		Platform: PlatformAntigravity,
		Status:   StatusActive,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	price1K := 0.12
	price2K := 0.18
	price4K := 0.36

	input := &UpdateGroupInput{
		ImagePrice1K: &price1K,
		ImagePrice2K: &price2K,
		ImagePrice4K: &price4K,
	}

	group, err := svc.UpdateGroup(context.Background(), 1, input)
	require.NoError(t, err)
	require.NotNil(t, group)

	// 验证 repo 收到了更新后的字段
	require.NotNil(t, repo.updated)
	require.NotNil(t, repo.updated.ImagePrice1K)
	require.NotNil(t, repo.updated.ImagePrice2K)
	require.NotNil(t, repo.updated.ImagePrice4K)
	require.InDelta(t, 0.12, *repo.updated.ImagePrice1K, 0.0001)
	require.InDelta(t, 0.18, *repo.updated.ImagePrice2K, 0.0001)
	require.InDelta(t, 0.36, *repo.updated.ImagePrice4K, 0.0001)
}

func TestAdminService_UpdateGroup_WithVideoPricing(t *testing.T) {
	existingGroup := &Group{
		ID:       1,
		Name:     "existing-grok",
		Platform: PlatformGrok,
		Status:   StatusActive,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	price480P := 0.09
	price720P := 0.13
	price1080P := 0.19
	videoMultiplier := 0.6
	independent := true

	input := &UpdateGroupInput{
		VideoRateIndependent: &independent,
		VideoRateMultiplier:  &videoMultiplier,
		VideoPrice480P:       &price480P,
		VideoPrice720P:       &price720P,
		VideoPrice1080P:      &price1080P,
	}

	group, err := svc.UpdateGroup(context.Background(), 1, input)
	require.NoError(t, err)
	require.NotNil(t, group)

	require.NotNil(t, repo.updated)
	require.True(t, repo.updated.VideoRateIndependent)
	require.InDelta(t, 0.6, repo.updated.VideoRateMultiplier, 1e-12)
	require.InDelta(t, 0.09, *repo.updated.VideoPrice480P, 0.0001)
	require.InDelta(t, 0.13, *repo.updated.VideoPrice720P, 0.0001)
	require.InDelta(t, 0.19, *repo.updated.VideoPrice1080P, 0.0001)
}

// TestAdminService_UpdateGroup_PartialImagePricing 测试仅更新部分 ImagePrice 字段
func TestAdminService_UpdateGroup_PartialImagePricing(t *testing.T) {
	oldPrice2K := 0.15
	existingGroup := &Group{
		ID:           1,
		Name:         "existing-group",
		Platform:     PlatformAntigravity,
		Status:       StatusActive,
		ImagePrice2K: &oldPrice2K, // 已有 2K 价格
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	// 只更新 1K 价格
	price1K := 0.10
	input := &UpdateGroupInput{
		ImagePrice1K: &price1K,
		// ImagePrice2K 和 ImagePrice4K 为 nil，不更新
	}

	group, err := svc.UpdateGroup(context.Background(), 1, input)
	require.NoError(t, err)
	require.NotNil(t, group)

	// 验证：1K 被更新，2K 保持原值，4K 仍为 nil
	require.NotNil(t, repo.updated)
	require.NotNil(t, repo.updated.ImagePrice1K)
	require.InDelta(t, 0.10, *repo.updated.ImagePrice1K, 0.0001)
	require.NotNil(t, repo.updated.ImagePrice2K)
	require.InDelta(t, 0.15, *repo.updated.ImagePrice2K, 0.0001) // 原值保持
	require.Nil(t, repo.updated.ImagePrice4K)
}

func TestAdminService_UpdateGroup_PreservesImageGenerationControlsWhenOmitted(t *testing.T) {
	imageMultiplier := 0.5
	existingGroup := &Group{
		ID:                   1,
		Name:                 "existing-group",
		Platform:             PlatformOpenAI,
		Status:               StatusActive,
		AllowImageGeneration: true,
		ImageRateIndependent: true,
		ImageRateMultiplier:  imageMultiplier,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	updatedDesc := "updated"
	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		Description: &updatedDesc,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.True(t, repo.updated.AllowImageGeneration)
	require.True(t, repo.updated.ImageRateIndependent)
	require.InDelta(t, 0.5, repo.updated.ImageRateMultiplier, 1e-12)
}

func TestAdminService_UpdateGroup_ClearsDescriptionWhenEmptyString(t *testing.T) {
	existingGroup := &Group{
		ID:          1,
		Name:        "existing-group",
		Description: "Auto-created default group",
		Platform:    PlatformOpenAI,
		Status:      StatusActive,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	empty := ""
	_, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		Description: &empty,
	})
	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Equal(t, "", repo.updated.Description, "empty string should clear description")
}

func TestAdminService_UpdateGroup_PreservesDescriptionWhenNil(t *testing.T) {
	existingGroup := &Group{
		ID:          1,
		Name:        "existing-group",
		Description: "keep me",
		Platform:    PlatformOpenAI,
		Status:      StatusActive,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		Description: nil,
	})
	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Equal(t, "keep me", repo.updated.Description, "nil should preserve existing description")
}

func TestAdminService_UpdateGroup_RejectsNegativeImageRateMultiplier(t *testing.T) {
	existingGroup := &Group{
		ID:                  1,
		Name:                "existing-group",
		Platform:            PlatformOpenAI,
		Status:              StatusActive,
		ImageRateMultiplier: 1,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}
	negative := -0.1

	_, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		ImageRateMultiplier: &negative,
	})
	require.Error(t, err)
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_RejectsNegativeVideoRateMultiplier(t *testing.T) {
	existingGroup := &Group{
		ID:                  1,
		Name:                "existing-group",
		Platform:            PlatformGrok,
		Status:              StatusActive,
		VideoRateMultiplier: 1,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}
	negative := -0.1

	_, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		VideoRateMultiplier: &negative,
	})
	require.Error(t, err)
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_InvalidatesAuthCacheOnRPMLimitChange(t *testing.T) {
	existingGroup := &Group{
		ID:       1,
		Name:     "existing-group",
		Platform: PlatformAnthropic,
		Status:   StatusActive,
		RPMLimit: 10,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		groupRepo:            repo,
		authCacheInvalidator: invalidator,
	}

	rpmLimit := 60
	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		RPMLimit: &rpmLimit,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.Equal(t, 60, repo.updated.RPMLimit)
	require.Equal(t, []int64{1}, invalidator.groupIDs, "分组 RPMLimit 写入 auth snapshot，变更后必须失效 API Key 认证缓存")
}

func TestAdminService_UpdateGroup_InvalidatesAuthCacheOnProfitControlChange(t *testing.T) {
	existingGroup := &Group{
		ID:             1,
		Name:           "existing-group",
		Platform:       PlatformOpenAI,
		Status:         StatusActive,
		RateMultiplier: 1,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		groupRepo:            repo,
		authCacheInvalidator: invalidator,
	}

	enabled := true
	margin := 0.2
	buffer := 0.05
	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		ProfitControlEnabled: &enabled,
		ProfitMinMargin:      &margin,
		ProfitSafetyBuffer:   &buffer,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.True(t, repo.updated.ProfitControlEnabled)
	require.InDelta(t, 0.2, repo.updated.ProfitMinMargin, 1e-12)
	require.InDelta(t, 0.05, repo.updated.ProfitSafetyBuffer, 1e-12)
	require.Equal(t, []int64{1}, invalidator.groupIDs, "利润门读取 auth snapshot，配置变更后必须失效该分组的认证缓存")
}

func TestAdminService_UpdateGroup_ReasoningEffortMappingsTriState(t *testing.T) {
	tests := []struct {
		name  string
		input *UpdateGroupInput
		want  []ReasoningEffortMapping
	}{
		{
			name:  "nil preserves existing mappings",
			input: &UpdateGroupInput{},
			want:  []ReasoningEffortMapping{{From: "max", To: "xhigh"}},
		},
		{
			name: "empty array clears mappings",
			input: func() *UpdateGroupInput {
				empty := []ReasoningEffortMapping{}
				return &UpdateGroupInput{ReasoningEffortMappings: &empty}
			}(),
			want: []ReasoningEffortMapping{},
		},
		{
			name: "non empty array replaces and canonicalizes mappings",
			input: func() *UpdateGroupInput {
				replacement := []ReasoningEffortMapping{{From: " X-HIGH ", To: " high "}}
				return &UpdateGroupInput{ReasoningEffortMappings: &replacement}
			}(),
			want: []ReasoningEffortMapping{{From: "xhigh", To: "high"}},
		},
		{
			name: "model scoped mappings are canonicalized independently",
			input: func() *UpdateGroupInput {
				replacement := []ReasoningEffortMapping{
					{From: " MAX ", To: " low ", MatchType: "PREFIX", Model: " gpt "},
					{From: "max", To: "medium", Model: "gpt-5.4"},
				}
				return &UpdateGroupInput{ReasoningEffortMappings: &replacement}
			}(),
			want: []ReasoningEffortMapping{
				{From: "max", To: "low", MatchType: "prefix", Model: "gpt"},
				{From: "max", To: "medium", MatchType: "exact", Model: "gpt-5.4"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := &Group{
				ID:                      1,
				Name:                    "openai-group",
				Platform:                PlatformOpenAI,
				Status:                  StatusActive,
				ReasoningEffortMappings: []ReasoningEffortMapping{{From: "max", To: "xhigh"}},
			}
			repo := &groupRepoStubForAdmin{getByID: existing}
			svc := &adminServiceImpl{groupRepo: repo}

			_, err := svc.UpdateGroup(context.Background(), existing.ID, tt.input)

			require.NoError(t, err)
			require.Equal(t, tt.want, repo.updated.ReasoningEffortMappings)
		})
	}
}

func TestAdminService_UpdateGroup_RejectsInvalidReasoningEffortMappings(t *testing.T) {
	existing := &Group{
		ID:               1,
		Name:             "openai",
		Platform:         PlatformOpenAI,
		SubscriptionType: SubscriptionTypeStandard,
		RateMultiplier:   1,
		Status:           StatusActive,
	}
	repo := &groupRepoStubForInvalidRequestFallback{groups: map[int64]*Group{existing.ID: existing}}
	svc := &adminServiceImpl{groupRepo: repo}
	invalid := []ReasoningEffortMapping{
		{From: "max", To: "xhigh"},
		{From: " MAX ", To: "high"},
	}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		ReasoningEffortMappings: &invalid,
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate reasoning effort mapping source")
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_ClearsReasoningPolicyForUnsupportedPlatform(t *testing.T) {
	existing := &Group{
		ID:                          1,
		Name:                        "openai-group",
		Platform:                    PlatformOpenAI,
		Status:                      StatusActive,
		MaxReasoningEffort:          "medium",
		MaxReasoningEffortOverLimit: ReasoningEffortOverLimitDeny,
		ReasoningEffortMappings:     []ReasoningEffortMapping{{From: "max", To: "xhigh"}},
	}
	repo := &groupRepoStubForAdmin{getByID: existing}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{Platform: PlatformAnthropic})

	require.NoError(t, err)
	require.Empty(t, repo.updated.MaxReasoningEffort)
	require.Equal(t, ReasoningEffortOverLimitDowngrade, repo.updated.MaxReasoningEffortOverLimit)
	require.Empty(t, repo.updated.ReasoningEffortMappings)
}

func TestAdminService_UpdateGroup_ClearsPeakRateWhenChangingToStandard(t *testing.T) {
	existingGroup := &Group{
		ID:                 1,
		Name:               "existing-group",
		Platform:           PlatformOpenAI,
		Status:             StatusActive,
		SubscriptionType:   SubscriptionTypeSubscription,
		PeakRateEnabled:    true,
		PeakStart:          "14:00",
		PeakEnd:            "18:00",
		PeakRateMultiplier: 3,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		SubscriptionType: SubscriptionTypeStandard,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Equal(t, SubscriptionTypeStandard, repo.updated.SubscriptionType)
	require.False(t, repo.updated.PeakRateEnabled)
	require.Equal(t, "", repo.updated.PeakStart)
	require.Equal(t, "", repo.updated.PeakEnd)
	require.Equal(t, 1.0, repo.updated.PeakRateMultiplier)
}

func TestAdminService_CreateGroup_NormalizesMessagesDispatchModelConfig(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "dispatch-group",
		Description:    "dispatch config",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1.0,
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			OpusMappedModel:   " gpt-5.4-high ",
			SonnetMappedModel: " gpt-5.3-codex ",
			HaikuMappedModel:  " gpt-5.4-mini-medium ",
			ExactModelMappings: map[string]string{
				" claude-sonnet-4-5-20250929 ": " gpt-5.2-high ",
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{
		OpusMappedModel:   "gpt-5.4",
		SonnetMappedModel: "gpt-5.3-codex",
		HaikuMappedModel:  "gpt-5.4-mini",
		ExactModelMappings: map[string]string{
			"claude-sonnet-4-5-20250929": "gpt-5.2",
		},
	}, repo.created.MessagesDispatchModelConfig)
}

func TestAdminService_UpdateGroup_NormalizesMessagesDispatchModelConfig(t *testing.T) {
	existingGroup := &Group{
		ID:       1,
		Name:     "existing-group",
		Platform: PlatformOpenAI,
		Status:   StatusActive,
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		MessagesDispatchModelConfig: &OpenAIMessagesDispatchModelConfig{
			SonnetMappedModel: " gpt-5.4-medium ",
			ExactModelMappings: map[string]string{
				" claude-haiku-4-5-20251001 ": " gpt-5.4-mini-high ",
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{
		SonnetMappedModel: "gpt-5.4",
		ExactModelMappings: map[string]string{
			"claude-haiku-4-5-20251001": "gpt-5.4-mini",
		},
	}, repo.updated.MessagesDispatchModelConfig)
}

func TestAdminService_CreateGroup_ClearsMessagesDispatchFieldsForNonOpenAIPlatform(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                  "anthropic-group",
		Description:           "non-openai",
		Platform:              PlatformAnthropic,
		RateMultiplier:        1.0,
		AllowMessagesDispatch: true,
		DefaultMappedModel:    "gpt-5.4",
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			OpusMappedModel: "gpt-5.4",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.False(t, repo.created.AllowMessagesDispatch)
	require.Empty(t, repo.created.DefaultMappedModel)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{}, repo.created.MessagesDispatchModelConfig)
}

func TestAdminService_UpdateGroup_ClearsMessagesDispatchFieldsWhenPlatformChangesAwayFromOpenAI(t *testing.T) {
	existingGroup := &Group{
		ID:                    1,
		Name:                  "existing-openai-group",
		Platform:              PlatformOpenAI,
		Status:                StatusActive,
		AllowMessagesDispatch: true,
		DefaultMappedModel:    "gpt-5.4",
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			SonnetMappedModel: "gpt-5.3-codex",
		},
	}
	repo := &groupRepoStubForAdmin{getByID: existingGroup}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		Platform: PlatformAnthropic,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Equal(t, PlatformAnthropic, repo.updated.Platform)
	require.False(t, repo.updated.AllowMessagesDispatch)
	require.Empty(t, repo.updated.DefaultMappedModel)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{}, repo.updated.MessagesDispatchModelConfig)
}

func TestAdminService_ListGroups_WithSearch(t *testing.T) {
	// 测试：
	// 1. search 参数正常传递到 repository 层
	// 2. search 为空字符串时的行为
	// 3. search 与其他过滤条件组合使用

	t.Run("search 参数正常传递到 repository 层", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{
			listWithFiltersGroups: []Group{{ID: 1, Name: "alpha"}},
			listWithFiltersResult: &pagination.PaginationResult{Total: 1},
		}
		svc := &adminServiceImpl{groupRepo: repo}

		groups, total, err := svc.ListGroups(context.Background(), 1, 20, "", "", "alpha", nil, "", "")
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Equal(t, []Group{{ID: 1, Name: "alpha"}}, groups)

		require.Equal(t, 1, repo.listWithFiltersCalls)
		require.Equal(t, pagination.PaginationParams{Page: 1, PageSize: 20}, repo.listWithFiltersParams)
		require.Equal(t, "alpha", repo.listWithFiltersSearch)
		require.Nil(t, repo.listWithFiltersIsExclusive)
	})

	t.Run("search 为空字符串时传递空字符串", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{
			listWithFiltersGroups: []Group{},
			listWithFiltersResult: &pagination.PaginationResult{Total: 0},
		}
		svc := &adminServiceImpl{groupRepo: repo}

		groups, total, err := svc.ListGroups(context.Background(), 2, 10, "", "", "", nil, "", "")
		require.NoError(t, err)
		require.Empty(t, groups)
		require.Equal(t, int64(0), total)

		require.Equal(t, 1, repo.listWithFiltersCalls)
		require.Equal(t, pagination.PaginationParams{Page: 2, PageSize: 10}, repo.listWithFiltersParams)
		require.Equal(t, "", repo.listWithFiltersSearch)
		require.Nil(t, repo.listWithFiltersIsExclusive)
	})

	t.Run("search 与其他过滤条件组合使用", func(t *testing.T) {
		isExclusive := true
		repo := &groupRepoStubForAdmin{
			listWithFiltersGroups: []Group{{ID: 2, Name: "beta"}},
			listWithFiltersResult: &pagination.PaginationResult{Total: 42},
		}
		svc := &adminServiceImpl{groupRepo: repo}

		groups, total, err := svc.ListGroups(context.Background(), 3, 50, PlatformAntigravity, StatusActive, "beta", &isExclusive, "", "")
		require.NoError(t, err)
		require.Equal(t, int64(42), total)
		require.Equal(t, []Group{{ID: 2, Name: "beta"}}, groups)

		require.Equal(t, 1, repo.listWithFiltersCalls)
		require.Equal(t, pagination.PaginationParams{Page: 3, PageSize: 50}, repo.listWithFiltersParams)
		require.Equal(t, PlatformAntigravity, repo.listWithFiltersPlatform)
		require.Equal(t, StatusActive, repo.listWithFiltersStatus)
		require.Equal(t, "beta", repo.listWithFiltersSearch)
		require.NotNil(t, repo.listWithFiltersIsExclusive)
		require.True(t, *repo.listWithFiltersIsExclusive)
	})
}

func TestAdminService_ValidateFallbackGroup_DetectsCycle(t *testing.T) {
	groupID := int64(1)
	fallbackID := int64(2)
	repo := &groupRepoStubForFallbackCycle{
		groups: map[int64]*Group{
			groupID: {
				ID:              groupID,
				FallbackGroupID: &fallbackID,
			},
			fallbackID: {
				ID:              fallbackID,
				FallbackGroupID: &groupID,
			},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	err := svc.validateFallbackGroup(context.Background(), groupID, fallbackID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback group cycle")
}

type groupRepoStubForFallbackCycle struct {
	groups map[int64]*Group
}

func (s *groupRepoStubForFallbackCycle) Create(_ context.Context, _ *Group) error {
	panic("unexpected Create call")
}

func (s *groupRepoStubForFallbackCycle) Update(_ context.Context, _ *Group) error {
	panic("unexpected Update call")
}

func (s *groupRepoStubForFallbackCycle) GetByID(ctx context.Context, id int64) (*Group, error) {
	return s.GetByIDLite(ctx, id)
}

func (s *groupRepoStubForFallbackCycle) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if g, ok := s.groups[id]; ok {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

func (s *groupRepoStubForFallbackCycle) Delete(_ context.Context, _ int64) error {
	panic("unexpected Delete call")
}

func (s *groupRepoStubForFallbackCycle) DeleteCascade(_ context.Context, _ int64) ([]int64, error) {
	panic("unexpected DeleteCascade call")
}

func (s *groupRepoStubForFallbackCycle) List(_ context.Context, _ pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (s *groupRepoStubForFallbackCycle) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _, _, _ string, _ *bool) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (s *groupRepoStubForFallbackCycle) ListActive(_ context.Context) ([]Group, error) {
	panic("unexpected ListActive call")
}

func (s *groupRepoStubForFallbackCycle) ListActiveByPlatform(_ context.Context, _ string) ([]Group, error) {
	panic("unexpected ListActiveByPlatform call")
}

func (s *groupRepoStubForFallbackCycle) ExistsByName(_ context.Context, _ string) (bool, error) {
	panic("unexpected ExistsByName call")
}

func (s *groupRepoStubForFallbackCycle) GetAccountCount(_ context.Context, _ int64) (int64, int64, error) {
	panic("unexpected GetAccountCount call")
}

func (s *groupRepoStubForFallbackCycle) DeleteAccountGroupsByGroupID(_ context.Context, _ int64) (int64, error) {
	panic("unexpected DeleteAccountGroupsByGroupID call")
}

func (s *groupRepoStubForFallbackCycle) BindAccountsToGroup(_ context.Context, _ int64, _ []int64) error {
	panic("unexpected BindAccountsToGroup call")
}

func (s *groupRepoStubForFallbackCycle) GetAccountIDsByGroupIDs(_ context.Context, _ []int64) ([]int64, error) {
	panic("unexpected GetAccountIDsByGroupIDs call")
}

func (s *groupRepoStubForFallbackCycle) UpdateSortOrders(_ context.Context, _ []GroupSortOrderUpdate) error {
	return nil
}

type groupRepoStubForInvalidRequestFallback struct {
	groups  map[int64]*Group
	created *Group
	updated *Group
}

func (s *groupRepoStubForInvalidRequestFallback) Create(_ context.Context, g *Group) error {
	s.created = g
	return nil
}

func (s *groupRepoStubForInvalidRequestFallback) Update(_ context.Context, g *Group) error {
	s.updated = g
	return nil
}

func (s *groupRepoStubForInvalidRequestFallback) GetByID(ctx context.Context, id int64) (*Group, error) {
	return s.GetByIDLite(ctx, id)
}

func (s *groupRepoStubForInvalidRequestFallback) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if g, ok := s.groups[id]; ok {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

func (s *groupRepoStubForInvalidRequestFallback) Delete(_ context.Context, _ int64) error {
	panic("unexpected Delete call")
}

func (s *groupRepoStubForInvalidRequestFallback) DeleteCascade(_ context.Context, _ int64) ([]int64, error) {
	panic("unexpected DeleteCascade call")
}

func (s *groupRepoStubForInvalidRequestFallback) List(_ context.Context, _ pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (s *groupRepoStubForInvalidRequestFallback) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _, _, _ string, _ *bool) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (s *groupRepoStubForInvalidRequestFallback) ListActive(_ context.Context) ([]Group, error) {
	panic("unexpected ListActive call")
}

func (s *groupRepoStubForInvalidRequestFallback) ListActiveByPlatform(_ context.Context, _ string) ([]Group, error) {
	panic("unexpected ListActiveByPlatform call")
}

func (s *groupRepoStubForInvalidRequestFallback) ExistsByName(_ context.Context, _ string) (bool, error) {
	panic("unexpected ExistsByName call")
}

func (s *groupRepoStubForInvalidRequestFallback) GetAccountCount(_ context.Context, _ int64) (int64, int64, error) {
	panic("unexpected GetAccountCount call")
}

func (s *groupRepoStubForInvalidRequestFallback) DeleteAccountGroupsByGroupID(_ context.Context, _ int64) (int64, error) {
	panic("unexpected DeleteAccountGroupsByGroupID call")
}

func (s *groupRepoStubForInvalidRequestFallback) GetAccountIDsByGroupIDs(_ context.Context, _ []int64) ([]int64, error) {
	panic("unexpected GetAccountIDsByGroupIDs call")
}

func (s *groupRepoStubForInvalidRequestFallback) BindAccountsToGroup(_ context.Context, _ int64, _ []int64) error {
	panic("unexpected BindAccountsToGroup call")
}

func (s *groupRepoStubForInvalidRequestFallback) UpdateSortOrders(_ context.Context, _ []GroupSortOrderUpdate) error {
	return nil
}

func TestAdminService_CreateGroup_InvalidRequestFallbackRejectsUnsupportedPlatform(t *testing.T) {
	fallbackID := int64(10)
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			fallbackID: {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                            "g1",
		Platform:                        PlatformOpenAI,
		RateMultiplier:                  1.0,
		SubscriptionType:                SubscriptionTypeStandard,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid request fallback only supported for anthropic or antigravity groups")
	require.Nil(t, repo.created)
}

func TestAdminService_CreateGroup_InvalidRequestFallbackRejectsSubscription(t *testing.T) {
	fallbackID := int64(10)
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			fallbackID: {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		RateMultiplier:                  1.0,
		SubscriptionType:                SubscriptionTypeSubscription,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "subscription groups cannot set invalid request fallback")
	require.Nil(t, repo.created)
}

func TestAdminService_CreateGroup_InvalidRequestFallbackRejectsFallbackGroup(t *testing.T) {
	tests := []struct {
		name        string
		fallback    *Group
		wantMessage string
	}{
		{
			name:        "openai_target",
			fallback:    &Group{ID: 10, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard},
			wantMessage: "fallback group must be anthropic platform",
		},
		{
			name:        "antigravity_target",
			fallback:    &Group{ID: 10, Platform: PlatformAntigravity, SubscriptionType: SubscriptionTypeStandard},
			wantMessage: "fallback group must be anthropic platform",
		},
		{
			name:        "subscription_group",
			fallback:    &Group{ID: 10, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription},
			wantMessage: "fallback group cannot be subscription type",
		},
		{
			name: "nested_fallback",
			fallback: &Group{
				ID:                              10,
				Platform:                        PlatformAnthropic,
				SubscriptionType:                SubscriptionTypeStandard,
				FallbackGroupIDOnInvalidRequest: func() *int64 { v := int64(99); return &v }(),
			},
			wantMessage: "fallback group cannot have invalid request fallback configured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fallbackID := tc.fallback.ID
			repo := &groupRepoStubForInvalidRequestFallback{
				groups: map[int64]*Group{
					fallbackID: tc.fallback,
				},
			}
			svc := &adminServiceImpl{groupRepo: repo}

			_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
				Name:                            "g1",
				Platform:                        PlatformAnthropic,
				RateMultiplier:                  1.0,
				SubscriptionType:                SubscriptionTypeStandard,
				FallbackGroupIDOnInvalidRequest: &fallbackID,
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMessage)
			require.Nil(t, repo.created)
		})
	}
}

func TestAdminService_CreateGroup_InvalidRequestFallbackNotFound(t *testing.T) {
	fallbackID := int64(10)
	repo := &groupRepoStubForInvalidRequestFallback{}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		RateMultiplier:                  1.0,
		SubscriptionType:                SubscriptionTypeStandard,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback group not found")
	require.Nil(t, repo.created)
}

func TestAdminService_CreateGroup_InvalidRequestFallbackAllowsAntigravity(t *testing.T) {
	fallbackID := int64(10)
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			fallbackID: {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                            "g1",
		Platform:                        PlatformAntigravity,
		RateMultiplier:                  1.0,
		SubscriptionType:                SubscriptionTypeStandard,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.Equal(t, fallbackID, *repo.created.FallbackGroupIDOnInvalidRequest)
}

func TestAdminService_CreateGroup_InvalidRequestFallbackClearsOnZero(t *testing.T) {
	zero := int64(0)
	repo := &groupRepoStubForInvalidRequestFallback{}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		RateMultiplier:                  1.0,
		SubscriptionType:                SubscriptionTypeStandard,
		FallbackGroupIDOnInvalidRequest: &zero,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.created)
	require.Nil(t, repo.created.FallbackGroupIDOnInvalidRequest)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackPlatformMismatch(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:                              1,
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		SubscriptionType:                SubscriptionTypeStandard,
		Status:                          StatusActive,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		Platform: PlatformOpenAI,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid request fallback only supported for anthropic or antigravity groups")
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackSubscriptionMismatch(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:                              1,
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		SubscriptionType:                SubscriptionTypeStandard,
		Status:                          StatusActive,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		SubscriptionType: SubscriptionTypeSubscription,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "subscription groups cannot set invalid request fallback")
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackClearsOnZero(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:                              1,
		Name:                            "g1",
		Platform:                        PlatformAnthropic,
		SubscriptionType:                SubscriptionTypeStandard,
		Status:                          StatusActive,
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	clear := int64(0)
	group, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		Platform:                        PlatformOpenAI,
		FallbackGroupIDOnInvalidRequest: &clear,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Nil(t, repo.updated.FallbackGroupIDOnInvalidRequest)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackRejectsFallbackGroup(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:               1,
		Name:             "g1",
		Platform:         PlatformAnthropic,
		SubscriptionType: SubscriptionTypeStandard,
		Status:           StatusActive,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback group cannot be subscription type")
	require.Nil(t, repo.updated)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackSetSuccess(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:               1,
		Name:             "g1",
		Platform:         PlatformAnthropic,
		SubscriptionType: SubscriptionTypeStandard,
		Status:           StatusActive,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Equal(t, fallbackID, *repo.updated.FallbackGroupIDOnInvalidRequest)
}

func TestAdminService_UpdateGroup_InvalidRequestFallbackAllowsAntigravity(t *testing.T) {
	fallbackID := int64(10)
	existing := &Group{
		ID:               1,
		Name:             "g1",
		Platform:         PlatformAntigravity,
		SubscriptionType: SubscriptionTypeStandard,
		Status:           StatusActive,
	}
	repo := &groupRepoStubForInvalidRequestFallback{
		groups: map[int64]*Group{
			existing.ID: existing,
			fallbackID:  {ID: fallbackID, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard},
		},
	}
	svc := &adminServiceImpl{groupRepo: repo}

	group, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		FallbackGroupIDOnInvalidRequest: &fallbackID,
	})
	require.NoError(t, err)
	require.NotNil(t, group)
	require.NotNil(t, repo.updated)
	require.Equal(t, fallbackID, *repo.updated.FallbackGroupIDOnInvalidRequest)
}

type accountRepoStubForGroupQuota struct {
	accountRepoStub
	listByGroupData            map[int64][]Account
	listByGroupAllStatusesData map[int64][]Account
	listByGroupErr             map[int64]error
}

func (s *accountRepoStubForGroupQuota) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if err, ok := s.listByGroupErr[groupID]; ok {
		return nil, err
	}
	if rows, ok := s.listByGroupData[groupID]; ok {
		return rows, nil
	}
	return nil, nil
}

func (s *accountRepoStubForGroupQuota) ListByGroupAllStatuses(_ context.Context, groupID int64) ([]Account, error) {
	if err, ok := s.listByGroupErr[groupID]; ok {
		return nil, err
	}
	if rows, ok := s.listByGroupAllStatusesData[groupID]; ok {
		return rows, nil
	}
	if rows, ok := s.listByGroupData[groupID]; ok {
		return rows, nil
	}
	return nil, nil
}

func TestAdminService_GetGroupQuotaSummary_OpenAIBuckets(t *testing.T) {
	groupID := int64(1)
	repo := &accountRepoStubForGroupQuota{
		listByGroupData: map[int64][]Account{
			groupID: {
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_5h", "codex_5h_used_percent": 100}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_5h", "codex_5h_used_percent": 96}},
				{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_5h", "codex_5h_used_percent": 91}},
				{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_5h"}},
				{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_7d", "codex_7d_used_percent": 78}},
				{ID: 6, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_7d", "codex_7d_used_percent": 4}},
				{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_quota_strategy": "prefer_7d"}},
			},
		},
	}
	svc := &adminServiceImpl{
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: groupID, Platform: PlatformOpenAI, Name: "OpenAI Group", Status: StatusActive}},
		accountRepo: repo,
	}

	summary, err := svc.GetGroupQuotaSummary(context.Background(), groupID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Equal(t, groupID, summary.GroupID)
	require.Equal(t, PlatformOpenAI, summary.Platform)
	require.True(t, summary.Supported)
	require.Len(t, summary.Tabs, 3)

	require.Equal(t, GroupQuotaTabSummary{
		Window: "5h",
		BucketCounts: []GroupQuotaBucket{
			{UsedPercent: 100, AccountCount: 1},
			{UsedPercent: 90, AccountCount: 2},
		},
		MatchedAccountCount: 3,
		SkippedAccountCount: 1,
	}, summary.Tabs[0])

	require.Equal(t, GroupQuotaTabSummary{
		Window: "7d",
		BucketCounts: []GroupQuotaBucket{
			{UsedPercent: 70, AccountCount: 1},
			{UsedPercent: 0, AccountCount: 1},
		},
		MatchedAccountCount: 2,
		SkippedAccountCount: 1,
	}, summary.Tabs[1])

	require.Equal(t, "refresh", summary.Tabs[2].Window)
	require.Len(t, summary.Tabs[2].RefreshCounts, 8)
}

func TestAdminService_GetGroupQuotaSummary_IncludesSnapshotAccountsAcrossStatuses(t *testing.T) {
	groupID := int64(9)
	repo := &accountRepoStubForGroupQuota{
		listByGroupAllStatusesData: map[int64][]Account{
			groupID: {
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusError, Extra: map[string]any{"codex_5h_used_percent": 0, "codex_7d_used_percent": 0}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusDisabled, Extra: map[string]any{"codex_5h_used_percent": 105, "codex_7d_used_percent": -2}},
				{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{"openai_quota_strategy": "prefer_5h"}},
				{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{"openai_quota_strategy": "prefer_7d"}},
			},
		},
	}
	svc := &adminServiceImpl{
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: groupID, Platform: PlatformOpenAI, Name: "OpenAI Group", Status: StatusActive}},
		accountRepo: repo,
	}

	summary, err := svc.GetGroupQuotaSummary(context.Background(), groupID)
	require.NoError(t, err)
	require.Len(t, summary.Tabs, 3)

	require.Equal(t, GroupQuotaTabSummary{
		Window: "5h",
		BucketCounts: []GroupQuotaBucket{
			{UsedPercent: 100, AccountCount: 1},
			{UsedPercent: 0, AccountCount: 1},
		},
		MatchedAccountCount: 2,
		SkippedAccountCount: 1,
	}, summary.Tabs[0])

	require.Equal(t, GroupQuotaTabSummary{
		Window: "7d",
		BucketCounts: []GroupQuotaBucket{
			{UsedPercent: 0, AccountCount: 2},
		},
		MatchedAccountCount: 2,
		SkippedAccountCount: 1,
	}, summary.Tabs[1])

	require.Equal(t, "refresh", summary.Tabs[2].Window)
	require.Len(t, summary.Tabs[2].RefreshCounts, 8)
}

func TestBuildGroupQuotaRefreshTabSummary_BeijingDateBuckets(t *testing.T) {
	beijing := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Date(2026, 5, 10, 10, 0, 0, 0, beijing)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_7d_reset_at": "2026-05-10T15:00:00+08:00"}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_7d_reset_at": "2026-05-11T00:30:00+08:00"}},
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_7d_reset_after_seconds": 49 * 60 * 60}},
		{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_5h_reset_at": "2026-05-10T12:00:00+08:00"}},
		{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_7d_reset_at": "2026-05-18T00:00:00+08:00"}},
		{ID: 6, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: nil},
	}

	tab := buildGroupQuotaRefreshTabSummary(accounts, now)

	require.Equal(t, "refresh", tab.Window)
	require.Equal(t, 3, tab.MatchedAccountCount)
	require.Equal(t, 3, tab.SkippedAccountCount)
	require.Equal(t, []GroupQuotaRefreshCount{
		{Date: "2026-05-10", DateLabel: "2026年5月10日", DaysFromNow: 0, AccountCount: 1},
		{Date: "2026-05-11", DateLabel: "2026年5月11日", DaysFromNow: 1, AccountCount: 1},
		{Date: "2026-05-12", DateLabel: "2026年5月12日", DaysFromNow: 2, AccountCount: 1},
		{Date: "2026-05-13", DateLabel: "2026年5月13日", DaysFromNow: 3, AccountCount: 0},
		{Date: "2026-05-14", DateLabel: "2026年5月14日", DaysFromNow: 4, AccountCount: 0},
		{Date: "2026-05-15", DateLabel: "2026年5月15日", DaysFromNow: 5, AccountCount: 0},
		{Date: "2026-05-16", DateLabel: "2026年5月16日", DaysFromNow: 6, AccountCount: 0},
		{Date: "2026-05-17", DateLabel: "2026年5月17日", DaysFromNow: 7, AccountCount: 0},
	}, tab.RefreshCounts)
}

func TestAdminService_GetGroupQuotaSummary_UnsupportedPlatform(t *testing.T) {
	groupID := int64(2)
	svc := &adminServiceImpl{
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: groupID, Platform: PlatformAnthropic, Name: "Anthropic Group", Status: StatusActive}},
		accountRepo: &accountRepoStubForGroupQuota{},
	}

	summary, err := svc.GetGroupQuotaSummary(context.Background(), groupID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Equal(t, groupID, summary.GroupID)
	require.Equal(t, PlatformAnthropic, summary.Platform)
	require.False(t, summary.Supported)
	require.Empty(t, summary.Tabs)
}

func TestAdminService_CreateCompositeRoute_RejectsNonCompositeGroup(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByID: &Group{ID: 7, Platform: PlatformOpenAI},
	}
	routeRepo := &compositeRouteRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: groupRepo, compositeRouteRepo: routeRepo}

	_, err := svc.CreateCompositeRoute(context.Background(), 7, CompositeRouteInput{
		PublicModel:    "router/gpt-5",
		TargetPlatform: PlatformOpenAI,
		Enabled:        true,
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "not a composite group")
	require.Nil(t, routeRepo.created)
}

func TestAdminService_CreateCompositeRoute_NormalizesAndPersists(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByID: &Group{ID: 7, Platform: PlatformComposite},
	}
	routeRepo := &compositeRouteRepoStubForAdmin{nextID: 99}
	svc := &adminServiceImpl{groupRepo: groupRepo, compositeRouteRepo: routeRepo}

	route, err := svc.CreateCompositeRoute(context.Background(), 7, CompositeRouteInput{
		PublicModel:    " router/gpt- ",
		MatchType:      CompositeRouteMatchPrefix,
		TargetPlatform: PlatformOpenAI,
		Endpoint:       CompositeRouteEndpointResponses,
		Enabled:        true,
		Notes:          " route note ",
	})

	require.NoError(t, err)
	require.NotNil(t, route)
	require.Equal(t, int64(99), route.ID)
	require.Equal(t, "router/gpt-", route.PublicModel)
	require.Equal(t, CompositeRouteMatchPrefix, route.MatchType)
	require.Equal(t, PlatformOpenAI, route.TargetPlatform)
	// prefix 路由留空 upstream_model 不再回填 public_model：留空表示透传原始请求模型。
	require.Equal(t, "", route.UpstreamModel)
	require.Equal(t, CompositeRouteEndpointResponses, route.Endpoint)
	require.Equal(t, 100, route.Priority)
	require.True(t, route.Enabled)
	require.Equal(t, "route note", route.Notes)
	require.Equal(t, route, routeRepo.created)
}

// TestAdminService_CreateCompositeRoute_ExactEmptyUpstreamBackfillsPublicModel 锁定
// 保守行为：exact 路由留空 upstream_model 仍回填 public_model（持久化/展示契约不变）。
func TestAdminService_CreateCompositeRoute_ExactEmptyUpstreamBackfillsPublicModel(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByID: &Group{ID: 7, Platform: PlatformComposite},
	}
	routeRepo := &compositeRouteRepoStubForAdmin{nextID: 99}
	svc := &adminServiceImpl{groupRepo: groupRepo, compositeRouteRepo: routeRepo}

	route, err := svc.CreateCompositeRoute(context.Background(), 7, CompositeRouteInput{
		PublicModel:    "openrouter/gpt-5",
		MatchType:      CompositeRouteMatchExact,
		TargetPlatform: PlatformOpenAI,
		Endpoint:       CompositeRouteEndpointResponses,
		Enabled:        true,
	})

	require.NoError(t, err)
	require.NotNil(t, route)
	require.Equal(t, CompositeRouteMatchExact, route.MatchType)
	require.Equal(t, "openrouter/gpt-5", route.UpstreamModel)
}

func TestAdminService_UpdateAndDeleteCompositeRouteRequireRouteOwnership(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByID: &Group{ID: 7, Platform: PlatformComposite},
	}
	routeRepo := &compositeRouteRepoStubForAdmin{
		routes: []CompositeModelRoute{
			{ID: 11, GroupID: 7, PublicModel: "router/gpt-5", TargetPlatform: PlatformOpenAI, Enabled: true},
			{ID: 12, GroupID: 8, PublicModel: "router/other", TargetPlatform: PlatformGemini, Enabled: true},
		},
	}
	svc := &adminServiceImpl{groupRepo: groupRepo, compositeRouteRepo: routeRepo}

	updated, err := svc.UpdateCompositeRoute(context.Background(), 7, 11, CompositeRouteInput{
		PublicModel:    "router/gpt-5",
		TargetPlatform: PlatformGemini,
		UpstreamModel:  "gemini-2.5-pro",
		Endpoint:       CompositeRouteEndpointChatCompletions,
		Priority:       3,
		Enabled:        true,
	})
	require.NoError(t, err)
	require.Equal(t, int64(11), updated.ID)
	require.Equal(t, PlatformGemini, updated.TargetPlatform)
	require.Equal(t, "gemini-2.5-pro", updated.UpstreamModel)
	require.Equal(t, updated, routeRepo.updated)

	err = svc.DeleteCompositeRoute(context.Background(), 7, 12)
	require.ErrorIs(t, err, ErrCompositeRouteNotFound)
	require.Empty(t, routeRepo.deleted)

	err = svc.DeleteCompositeRoute(context.Background(), 7, 11)
	require.NoError(t, err)
	require.Equal(t, []int64{11}, routeRepo.deleted)
}

func TestAdminService_PreviewCompositeRouteUsesExplicitRoutes(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByID: &Group{ID: 7, Platform: PlatformComposite},
	}
	routeRepo := &compositeRouteRepoStubForAdmin{
		routes: []CompositeModelRoute{
			{
				ID:             11,
				GroupID:        7,
				PublicModel:    "openrouter/claude",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-sonnet-4-6",
				Endpoint:       CompositeRouteEndpointMessages,
				Priority:       100,
				Enabled:        true,
			},
		},
	}
	svc := &adminServiceImpl{groupRepo: groupRepo, compositeRouteRepo: routeRepo}

	decision, err := svc.PreviewCompositeRoute(context.Background(), 7, CompositeRoutePreviewRequest{
		Model:    "openrouter/claude",
		Endpoint: CompositeRouteEndpointMessages,
	})

	require.NoError(t, err)
	require.NotNil(t, decision)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformAnthropic, decision.TargetPlatform)
	require.Equal(t, "claude-sonnet-4-6", decision.UpstreamModel)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(11), decision.Route.ID)
}
