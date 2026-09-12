package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/ent/authidentitychannel"
	"github.com/Wei-Shaw/sub2api/ent/usagelog"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// AdminService interface defines admin management operations
type AdminService interface {
	// User management
	ListUsers(ctx context.Context, page, pageSize int, filters UserListFilters, sortBy, sortOrder string) ([]User, int64, error)
	GetUser(ctx context.Context, id int64) (*User, error)
	GetUserIncludeDeleted(ctx context.Context, id int64) (*User, error)
	CreateUser(ctx context.Context, input *CreateUserInput) (*User, error)
	UpdateUser(ctx context.Context, id int64, input *UpdateUserInput) (*User, error)
	DeleteUser(ctx context.Context, id int64) error
	UpdateUserBalance(ctx context.Context, userID int64, balance float64, operation string, notes string) (*User, error)
	BatchUpdateConcurrency(ctx context.Context, userIDs []int64, value int, mode string) (int, error)
	BatchUpdateLimits(ctx context.Context, userIDs []int64, concurrency, rpmLimit *int) (int, error)
	GetUserAPIKeys(ctx context.Context, userID int64, page, pageSize int, sortBy, sortOrder string) ([]APIKey, int64, error)
	GetUserUsageStats(ctx context.Context, userID int64, period string) (any, error)
	GetUserRPMStatus(ctx context.Context, userID int64) (*UserRPMStatus, error)
	ImportAPIRequestIPBlocklistFromDisabledUsers(ctx context.Context) (*ImportAPIRequestIPBlocklistResult, error)
	// GetUserBalanceHistory returns paginated balance/concurrency change records for a user.
	// codeType is optional - pass empty string to return all types.
	// Also returns totalRecharged (sum of all positive balance top-ups).
	GetUserBalanceHistory(ctx context.Context, userID int64, page, pageSize int, codeType string) ([]RedeemCode, int64, float64, error)
	BindUserAuthIdentity(ctx context.Context, userID int64, input AdminBindAuthIdentityInput) (*AdminBoundAuthIdentity, error)

	// Group management
	ListGroups(ctx context.Context, page, pageSize int, platform, status, search string, isExclusive *bool, sortBy, sortOrder string) ([]Group, int64, error)
	GetAllGroups(ctx context.Context) ([]Group, error)
	GetAllGroupsByPlatform(ctx context.Context, platform string) ([]Group, error)
	// GetAllGroupsIncludingInactive returns all groups regardless of status (active + disabled),
	// ordered by sort_order then id. Used by the API Key group filter dropdown.
	GetAllGroupsIncludingInactive(ctx context.Context) ([]Group, error)
	GetGroup(ctx context.Context, id int64) (*Group, error)
	GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error)
	CreateGroup(ctx context.Context, input *CreateGroupInput) (*Group, error)
	UpdateGroup(ctx context.Context, id int64, input *UpdateGroupInput) (*Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	DeleteGroupIfEmpty(ctx context.Context, id int64) error
	ListCompositeRoutes(ctx context.Context, groupID int64) ([]CompositeModelRoute, error)
	CreateCompositeRoute(ctx context.Context, groupID int64, input CompositeRouteInput) (*CompositeModelRoute, error)
	UpdateCompositeRoute(ctx context.Context, groupID, routeID int64, input CompositeRouteInput) (*CompositeModelRoute, error)
	DeleteCompositeRoute(ctx context.Context, groupID, routeID int64) error
	PreviewCompositeRoute(ctx context.Context, groupID int64, input CompositeRoutePreviewRequest) (*CompositeRouteDecision, error)
	GetGroupAPIKeys(ctx context.Context, groupID int64, page, pageSize int) ([]APIKey, int64, error)
	GetGroupQuotaSummary(ctx context.Context, groupID int64) (*GroupQuotaSummary, error)
	GetGroupRateMultipliers(ctx context.Context, groupID int64) ([]UserGroupRateEntry, error)
	ClearGroupRateMultipliers(ctx context.Context, groupID int64) error
	BatchSetGroupRateMultipliers(ctx context.Context, groupID int64, entries []GroupRateMultiplierInput) error
	ClearGroupRPMOverrides(ctx context.Context, groupID int64) error
	BatchSetGroupRPMOverrides(ctx context.Context, groupID int64, entries []GroupRPMOverrideInput) error
	UpdateGroupSortOrders(ctx context.Context, updates []GroupSortOrderUpdate) error

	// API Key management (admin)
	AdminUpdateAPIKeyGroupID(ctx context.Context, keyID int64, groupID *int64) (*AdminUpdateAPIKeyGroupIDResult, error)
	AdminResetAPIKeyRateLimitUsage(ctx context.Context, keyID int64) (*APIKey, error)

	// ReplaceUserGroup 替换用户的专属分组：授予新分组权限、迁移 Key、移除旧分组权限
	ReplaceUserGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (*ReplaceUserGroupResult, error)

	// Account management
	ListAccounts(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode string, sortBy, sortOrder string) ([]Account, int64, error)
	// ListAccountsForSchedulerScoreFilter 返回符合过滤条件的全部账号（不分页），
	// 作为账号列表页计算 OpenAI 调度分数的过滤范围池。
	ListAccountsForSchedulerScoreFilter(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error)
	// ListOpenAISchedulableAccountsForSchedulerScore 返回指定分组（nil 为未分组）内
	// 可调度的 OpenAI 账号，用于按组计算调度分数。
	ListOpenAISchedulableAccountsForSchedulerScore(ctx context.Context, groupID *int64) ([]Account, error)
	GetAccount(ctx context.Context, id int64) (*Account, error)
	GetAccountsByIDs(ctx context.Context, ids []int64) ([]*Account, error)
	CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error)
	ValidateAccountGroupBindings(ctx context.Context, groupIDs []int64) error
	// DuplicateAccount creates an independent account from an existing account's configuration.
	// First-class runtime columns are intentionally reset by the normal account creation path.
	DuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error)
	// RecoverDuplicateAccount returns a previously committed duplicate for an ambiguous retry.
	// It never creates an account.
	RecoverDuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error)
	UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error)
	// UpdateAccountExtra 仅对 Extra 做 JSONB 增量合并（key 级覆盖），不会影响其它字段或运行态键。
	// 用于刷新流程持久化 account_uuid / org_uuid 等少量键，避免被全量快照覆盖。
	UpdateAccountExtra(ctx context.Context, id int64, updates map[string]any) error
	DeleteAccount(ctx context.Context, id int64) error
	RefreshAccountCredentials(ctx context.Context, id int64) (*Account, error)
	ClearAccountError(ctx context.Context, id int64) (*Account, error)
	SetAccountError(ctx context.Context, id int64, errorMsg string) error
	// EnsureOpenAIPrivacy 检查 OpenAI OAuth 账号 privacy_mode，未设置则尝试关闭训练数据共享并持久化。
	EnsureOpenAIPrivacy(ctx context.Context, account *Account) string
	// EnsureAntigravityPrivacy 检查 Antigravity OAuth 账号 privacy_mode，未设置则调用 setUserSettings 并持久化。
	EnsureAntigravityPrivacy(ctx context.Context, account *Account) string
	// ForceOpenAIPrivacy 强制重新设置 OpenAI OAuth 账号隐私，无论当前状态。
	ForceOpenAIPrivacy(ctx context.Context, account *Account) string
	// ForceAntigravityPrivacy 强制重新设置 Antigravity OAuth 账号隐私，无论当前状态。
	ForceAntigravityPrivacy(ctx context.Context, account *Account) string
	SetAccountSchedulable(ctx context.Context, id int64, schedulable bool) (*Account, error)
	BulkUpdateAccounts(ctx context.Context, input *BulkUpdateAccountsInput) (*BulkUpdateAccountsResult, error)
	CheckMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error
	// RevertAccountProxyFallback 将账号的 proxy_id 切回 proxy_fallback_origin_id，并清空 origin 字段。
	// 若账号不存在返回 ErrAccountNotFound；若账号存在但不在 fallback 状态，返回 ErrAccountNotInFallback。
	RevertAccountProxyFallback(ctx context.Context, id int64) error
	// CreateShadow creates one spark-dimension shadow linked to an OpenAI OAuth parent.
	CreateShadow(ctx context.Context, parentID int64, opts ShadowOptions) (*Account, error)

	// Proxy management
	ListProxies(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]Proxy, int64, error)
	ListProxiesWithAccountCount(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]ProxyWithAccountCount, int64, error)
	GetAllProxies(ctx context.Context) ([]Proxy, error)
	GetAllProxiesWithAccountCount(ctx context.Context) ([]ProxyWithAccountCount, error)
	GetProxy(ctx context.Context, id int64) (*Proxy, error)
	GetProxiesByIDs(ctx context.Context, ids []int64) ([]Proxy, error)
	CreateProxy(ctx context.Context, input *CreateProxyInput) (*Proxy, error)
	UpdateProxy(ctx context.Context, id int64, input *UpdateProxyInput) (*Proxy, error)
	DeleteProxy(ctx context.Context, id int64) error
	BatchDeleteProxies(ctx context.Context, ids []int64) (*ProxyBatchDeleteResult, error)
	GetProxyAccounts(ctx context.Context, proxyID int64) ([]ProxyAccountSummary, error)
	CheckProxyExists(ctx context.Context, host string, port int, username, password string) (bool, error)
	TestProxy(ctx context.Context, id int64) (*ProxyTestResult, error)
	CheckProxyQuality(ctx context.Context, id int64) (*ProxyQualityCheckResult, error)

	// Redeem code management
	ListRedeemCodes(ctx context.Context, page, pageSize int, codeType, status, search string, sortBy, sortOrder string) ([]RedeemCode, int64, error)
	GetRedeemCode(ctx context.Context, id int64) (*RedeemCode, error)
	GenerateRedeemCodes(ctx context.Context, input *GenerateRedeemCodesInput) ([]RedeemCode, error)
	DeleteRedeemCode(ctx context.Context, id int64) error
	BatchDeleteRedeemCodes(ctx context.Context, ids []int64) (int64, error)
	ExpireRedeemCode(ctx context.Context, id int64) (*RedeemCode, error)
	ResetAccountQuota(ctx context.Context, id int64) error
}

type AdminGroupOperation string

const (
	AdminGroupOperationBasic          AdminGroupOperation = "basic"
	AdminGroupOperationDuplicate      AdminGroupOperation = "duplicate"
	AdminGroupOperationCompositeRoute AdminGroupOperation = "composite_route"
	AdminGroupOperationMultiplier     AdminGroupOperation = "multiplier"
	AdminGroupOperationRPMOverride    AdminGroupOperation = "rpm_override"
	AdminGroupOperationSort           AdminGroupOperation = "sort"
)

func ValidateSimpleModeGroupOperation(cfg *config.Config, operation AdminGroupOperation) error {
	if cfg != nil && cfg.RunMode == config.RunModeSimple && operation != AdminGroupOperationBasic {
		return infraerrors.New(http.StatusForbidden, "SIMPLE_MODE_OPERATION_UNSUPPORTED", "This operation is not supported in simple mode")
	}
	return nil
}

// CreateUserInput represents input for creating a new user via admin operations.
type CreateUserInput struct {
	Email         string
	Password      string
	Username      string
	Notes         string
	Balance       *float64
	Concurrency   int
	RPMLimit      int
	AllowedGroups []int64
}

type UpdateUserInput struct {
	Email         string
	Password      string
	Username      *string
	Notes         *string
	Balance       *float64 // 使用指针区分"未提供"和"设置为0"
	Concurrency   *int     // 使用指针区分"未提供"和"设置为0"
	RPMLimit      *int     // 使用指针区分"未提供"和"设置为0"
	Status        string
	AllowedGroups *[]int64 // 使用指针区分"未提供"和"设置为空数组"
	// RestrictPublicGroups 指针区分"未提供"和"显式开关"。
	RestrictPublicGroups *bool
	// GroupRates 用户专属分组倍率配置
	// map[groupID]*rate，nil 表示删除该分组的专属倍率
	GroupRates map[int64]*float64
}

type ImportAPIRequestIPBlocklistResult struct {
	DisabledUserCount int      `json:"disabled_user_count"`
	ScannedIPCount    int      `json:"scanned_ip_count"`
	AddedIPCount      int      `json:"added_ip_count"`
	TotalIPCount      int      `json:"total_ip_count"`
	AddedIPs          []string `json:"added_ips"`
}

type AdminBindAuthIdentityInput struct {
	ProviderType    string
	ProviderKey     string
	ProviderSubject string
	Issuer          *string
	Metadata        map[string]any
	Channel         *AdminBindAuthIdentityChannelInput
}

type AdminBindAuthIdentityChannelInput struct {
	Channel        string
	ChannelAppID   string
	ChannelSubject string
	Metadata       map[string]any
}

type AdminBoundAuthIdentity struct {
	UserID          int64                          `json:"user_id"`
	ProviderType    string                         `json:"provider_type"`
	ProviderKey     string                         `json:"provider_key"`
	ProviderSubject string                         `json:"provider_subject"`
	VerifiedAt      *time.Time                     `json:"verified_at,omitempty"`
	Issuer          *string                        `json:"issuer,omitempty"`
	Metadata        map[string]any                 `json:"metadata"`
	CreatedAt       time.Time                      `json:"created_at"`
	UpdatedAt       time.Time                      `json:"updated_at"`
	Channel         *AdminBoundAuthIdentityChannel `json:"channel,omitempty"`
}

type AdminBoundAuthIdentityChannel struct {
	Channel        string         `json:"channel"`
	ChannelAppID   string         `json:"channel_app_id"`
	ChannelSubject string         `json:"channel_subject"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type CreateGroupInput struct {
	AllowBatchImageGeneration    bool
	BatchImageDiscountMultiplier *float64
	BatchImageHoldMultiplier     *float64
	Name                         string
	Description                  string
	Platform                     string
	RateMultiplier               float64
	IsExclusive                  bool
	OperatorUserID               *int64
	SubscriptionType             string   // standard/subscription
	DailyLimitUSD                *float64 // 日限额 (USD)
	WeeklyLimitUSD               *float64 // 周限额 (USD)
	MonthlyLimitUSD              *float64 // 月限额 (USD)
	LongContextPricingEnabled    bool
	ModelPricing                 []ChannelModelPricing
	// 图片生成计费配置（仅 antigravity 平台使用）
	ImagePrice1K         *float64
	ImagePrice2K         *float64
	ImagePrice4K         *float64
	AllowImageGeneration bool
	ImageRateIndependent bool
	ImageRateMultiplier  *float64
	VideoRateIndependent bool
	VideoRateMultiplier  *float64
	// 高峰时段倍率配置（PeakRateMultiplier 为 nil 时按 1.0 处理）
	PeakRateEnabled              bool
	PeakStart                    string
	PeakEnd                      string
	PeakRateMultiplier           *float64
	ProfitControlEnabled         bool
	ProfitMinMargin              *float64
	ProfitSafetyBuffer           *float64
	VideoPrice480P               *float64
	VideoPrice720P               *float64
	VideoPrice1080P              *float64
	VideoModelPrices             map[string]map[string]float64
	WebSearchPricePerCall        *float64
	SearchPricePer1k             *float64
	AudioRealtimePricePerMin     *float64
	AudioTTSPricePerMillionChars *float64
	AudioSTTPricePerHour         *float64
	ClaudeCodeOnly               bool   // 仅允许 Claude Code 客户端
	FallbackGroupID              *int64 // 降级分组 ID
	// 无效请求兜底分组 ID（仅 anthropic 平台使用）
	FallbackGroupIDOnInvalidRequest *int64
	// 模型路由配置（仅 anthropic 平台使用）
	ModelRouting        map[string][]int64
	ModelRoutingEnabled bool // 是否启用模型路由
	MCPXMLInject        *bool
	// 支持的模型系列（仅 antigravity 平台使用）
	SupportedModelScopes []string
	// OpenAI Messages 调度配置（仅 openai 平台使用）
	AllowMessagesDispatch       bool
	AllowLive                   bool
	ForceOpenAIFast             bool
	FreeOpenAIFast              bool
	DefaultMappedModel          string
	RequireOAuthOnly            bool
	RequirePrivacySet           bool
	MessagesDispatchModelConfig OpenAIMessagesDispatchModelConfig
	ModelAllowlist              GroupModelAllowlist
	// CodexModelsManifestConfig 固定账号 manifest 配置；创建路径禁止开启，仅编辑可配置。
	CodexModelsManifestConfig GroupCodexModelsManifestConfig
	// RPMLimit 分组 RPM 上限（0 = 不限制）
	RPMLimit int
	// MaxReasoningEffort Anthropic/OpenAI 请求的推理强度上限，空字符串表示不限制。
	MaxReasoningEffort string
	// MaxReasoningEffortOverLimit 超过上限时的访问控制：downgrade（默认）或 deny。
	MaxReasoningEffortOverLimit string
	// ReasoningEffortMappings Anthropic/OpenAI 推理强度映射，可按模型精确名、前缀或后缀限定。
	ReasoningEffortMappings []ReasoningEffortMapping
	// 从指定分组复制账号（创建分组后在同一事务内绑定）
	CopyAccountsFromGroupIDs []int64
}

type UpdateGroupInput struct {
	AllowBatchImageGeneration    *bool
	BatchImageDiscountMultiplier *float64
	BatchImageHoldMultiplier     *float64
	Name                         string
	Description                  *string
	Platform                     string
	RateMultiplier               *float64 // 使用指针以支持设置为0
	IsExclusive                  *bool
	Status                       string
	SubscriptionType             string   // standard/subscription
	DailyLimitUSD                *float64 // 日限额 (USD)
	WeeklyLimitUSD               *float64 // 周限额 (USD)
	MonthlyLimitUSD              *float64 // 月限额 (USD)
	LongContextPricingEnabled    *bool
	ModelPricing                 *[]ChannelModelPricing
	// 图片生成计费配置（仅 antigravity 平台使用）
	ImagePrice1K         *float64
	ImagePrice2K         *float64
	ImagePrice4K         *float64
	AllowImageGeneration *bool
	ImageRateIndependent *bool
	ImageRateMultiplier  *float64
	VideoRateIndependent *bool
	VideoRateMultiplier  *float64
	// 高峰时段倍率配置（nil 表示不修改）
	PeakRateEnabled              *bool
	PeakStart                    *string
	PeakEnd                      *string
	PeakRateMultiplier           *float64
	ProfitControlEnabled         *bool
	ProfitMinMargin              *float64
	ProfitSafetyBuffer           *float64
	VideoPrice480P               *float64
	VideoPrice720P               *float64
	VideoPrice1080P              *float64
	VideoModelPrices             map[string]map[string]float64
	WebSearchPricePerCall        *float64
	SearchPricePer1k             *float64
	AudioRealtimePricePerMin     *float64
	AudioTTSPricePerMillionChars *float64
	AudioSTTPricePerHour         *float64
	ClaudeCodeOnly               *bool  // 仅允许 Claude Code 客户端
	FallbackGroupID              *int64 // 降级分组 ID
	// 无效请求兜底分组 ID（仅 anthropic 平台使用）
	FallbackGroupIDOnInvalidRequest *int64
	// 模型路由配置（仅 anthropic 平台使用）
	ModelRouting        map[string][]int64
	ModelRoutingEnabled *bool // 是否启用模型路由
	MCPXMLInject        *bool
	// 支持的模型系列（仅 antigravity 平台使用）
	SupportedModelScopes *[]string
	// OpenAI Messages 调度配置（仅 openai 平台使用）
	AllowMessagesDispatch       *bool
	AllowLive                   *bool
	ForceOpenAIFast             *bool
	FreeOpenAIFast              *bool
	DefaultMappedModel          *string
	RequireOAuthOnly            *bool
	RequirePrivacySet           *bool
	MessagesDispatchModelConfig *OpenAIMessagesDispatchModelConfig
	ModelAllowlist              *GroupModelAllowlist
	// CodexModelsManifestConfig nil 表示不修改；非 openai 平台会被归一化为关闭。
	CodexModelsManifestConfig *GroupCodexModelsManifestConfig
	// RPMLimit 分组 RPM 上限（0 = 不限制），nil 表示未提供不改动。
	RPMLimit *int
	// MaxReasoningEffort 空字符串表示清除上限；nil 表示未提供不改动。
	MaxReasoningEffort *string
	// MaxReasoningEffortOverLimit 空字符串视为 downgrade；nil 表示未提供不改动。
	MaxReasoningEffortOverLimit *string
	// ReasoningEffortMappings nil 表示不修改，空数组表示清空，非空数组表示替换。
	ReasoningEffortMappings *[]ReasoningEffortMapping
	// 从指定分组复制账号（同步操作：先清空当前分组的账号绑定，再绑定源分组的账号）
	CopyAccountsFromGroupIDs []int64
}

type GroupQuotaBucket struct {
	UsedPercent  int `json:"used_percent"`
	AccountCount int `json:"account_count"`
}

type GroupQuotaRefreshCount struct {
	Date         string `json:"date"`
	DateLabel    string `json:"date_label"`
	DaysFromNow  int    `json:"days_from_now"`
	AccountCount int    `json:"account_count"`
}

type GroupQuotaTabSummary struct {
	Window              string                   `json:"window"`
	BucketCounts        []GroupQuotaBucket       `json:"bucket_counts"`
	RefreshCounts       []GroupQuotaRefreshCount `json:"refresh_counts,omitempty"`
	MatchedAccountCount int                      `json:"matched_account_count"`
	SkippedAccountCount int                      `json:"skipped_account_count"`
}

type GroupQuotaSummary struct {
	GroupID   int64                  `json:"group_id"`
	Platform  string                 `json:"platform"`
	Supported bool                   `json:"supported"`
	Tabs      []GroupQuotaTabSummary `json:"tabs"`
}

type CreateAccountInput struct {
	Name               string
	Notes              *string
	Platform           string
	Type               string
	Credentials        map[string]any
	Extra              map[string]any
	ProxyID            *int64
	Concurrency        int
	Priority           int
	RateMultiplier     *float64 // 账号计费倍率（>=0，允许 0）
	LoadFactor         *int
	GroupIDs           []int64
	ExpiresAt          *int64
	AutoPauseOnExpired *bool
	ProbeEnabled       *bool
	// SkipDefaultGroupBind prevents auto-binding to platform default group when GroupIDs is empty.
	SkipDefaultGroupBind bool
	// SkipMixedChannelCheck skips the mixed channel risk check when binding groups.
	// This should only be set when the caller has explicitly confirmed the risk.
	SkipMixedChannelCheck bool
}

// ShadowOptions is the input for CreateShadow. Credentials and proxy identity
// are owned by the parent account; only independent scheduling fields live on
// the shadow.
type ShadowOptions struct {
	Name        string
	Priority    int
	Concurrency int
	GroupIDs    []int64
}

type UpdateAccountInput struct {
	Name                  string
	Notes                 *string
	Type                  string // Account type: oauth, setup-token, apikey
	Credentials           map[string]any
	Extra                 map[string]any
	ProxyID               *int64
	Concurrency           *int     // 使用指针区分"未提供"和"设置为0"
	Priority              *int     // 使用指针区分"未提供"和"设置为0"
	RateMultiplier        *float64 // 账号计费倍率（>=0，允许 0）
	LoadFactor            *int
	Status                string
	GroupIDs              *[]int64
	ExpiresAt             *int64
	AutoPauseOnExpired    *bool
	ProbeEnabled          *bool
	RateSyncEnabled       *bool
	SkipMixedChannelCheck bool // 跳过混合渠道检查（用户已确认风险）
}

// BulkUpdateAccountsInput describes the payload for bulk updating accounts.
type BulkUpdateAccountsInput struct {
	AccountIDs     []int64
	Filters        *BulkUpdateAccountFilters
	Name           string
	ProxyID        *int64
	Concurrency    *int
	Priority       *int
	RateMultiplier *float64 // 账号计费倍率（>=0，允许 0）
	LoadFactor     *int
	Status         string
	Schedulable    *bool
	GroupIDs       *[]int64
	Credentials    map[string]any
	Extra          map[string]any
	ProbeEnabled   *bool
	// SkipMixedChannelCheck skips the mixed channel risk check when binding groups.
	// This should only be set when the caller has explicitly confirmed the risk.
	SkipMixedChannelCheck bool
}

type BulkUpdateAccountFilters struct {
	Platform    string
	Type        string
	Status      string
	Group       string
	Search      string
	PrivacyMode string
}

// BulkUpdateAccountResult captures the result for a single account update.
type BulkUpdateAccountResult struct {
	AccountID int64  `json:"account_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// AdminUpdateAPIKeyGroupIDResult is the result of AdminUpdateAPIKeyGroupID.
type AdminUpdateAPIKeyGroupIDResult struct {
	APIKey                 *APIKey
	AutoGrantedGroupAccess bool   // true if a new exclusive group permission was auto-added
	GrantedGroupID         *int64 // the group ID that was auto-granted
	GrantedGroupName       string // the group name that was auto-granted
}

// ReplaceUserGroupResult 分组替换操作的结果
type ReplaceUserGroupResult struct {
	MigratedKeys int64 // 迁移的 Key 数量
}

// UserRPMStatus describes a user's current per-minute RPM usage.
type UserRPMStatus struct {
	UserRPMUsed  int                  `json:"user_rpm_used"`
	UserRPMLimit int                  `json:"user_rpm_limit"`
	PerGroup     []UserGroupRPMStatus `json:"per_group"`
}

// UserGroupRPMStatus describes current per-minute RPM usage for one user/group pair.
type UserGroupRPMStatus struct {
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name"`
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	Source    string `json:"source"` // "group" | "override"
}

// BulkUpdateAccountsResult is the aggregated response for bulk updates.
type BulkUpdateAccountsResult struct {
	Success                   int                       `json:"success"`
	Failed                    int                       `json:"failed"`
	SuccessIDs                []int64                   `json:"success_ids"`
	FailedIDs                 []int64                   `json:"failed_ids"`
	Results                   []BulkUpdateAccountResult `json:"results"`
	LongContextInheritedCount int                       `json:"long_context_inherited_count,omitempty"`
}

type CreateProxyInput struct {
	Name           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
}

// UpdateProxyInput preserves omitted expiry/backup values; Clear flags explicitly
// remove them. A nil ExpiryWarnDays preserves the current warning period.
type UpdateProxyInput struct {
	Name           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	Status         string
	ExpiresAt      *time.Time
	ClearExpiresAt bool
	FallbackMode   string
	BackupProxyID  *int64
	ClearBackupID  bool
	ExpiryWarnDays *int
}

type GenerateRedeemCodesInput struct {
	Count        int
	Type         string
	Value        float64
	GroupID      *int64 // 订阅类型专用：关联的分组ID
	ValidityDays int    // 订阅类型专用：有效天数
	ExpiresAt    *time.Time
}

type ProxyBatchDeleteResult struct {
	DeletedIDs []int64                   `json:"deleted_ids"`
	Skipped    []ProxyBatchDeleteSkipped `json:"skipped"`
}

type ProxyBatchDeleteSkipped struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// ProxyTestResult represents the result of testing a proxy
type ProxyTestResult struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	LatencyMs   int64  `json:"latency_ms,omitempty"`
	IPAddress   string `json:"ip_address,omitempty"`
	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
}

type ProxyQualityCheckResult struct {
	ProxyID        int64                   `json:"proxy_id"`
	Score          int                     `json:"score"`
	Grade          string                  `json:"grade"`
	Summary        string                  `json:"summary"`
	ExitIP         string                  `json:"exit_ip,omitempty"`
	Country        string                  `json:"country,omitempty"`
	CountryCode    string                  `json:"country_code,omitempty"`
	BaseLatencyMs  int64                   `json:"base_latency_ms,omitempty"`
	PassedCount    int                     `json:"passed_count"`
	WarnCount      int                     `json:"warn_count"`
	FailedCount    int                     `json:"failed_count"`
	ChallengeCount int                     `json:"challenge_count"`
	CheckedAt      int64                   `json:"checked_at"`
	Items          []ProxyQualityCheckItem `json:"items"`
}

type ProxyQualityCheckItem struct {
	Target     string `json:"target"`
	Status     string `json:"status"` // pass/warn/fail/challenge
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMs  int64  `json:"latency_ms,omitempty"`
	Message    string `json:"message,omitempty"`
	CFRay      string `json:"cf_ray,omitempty"`
}

// ProxyExitInfo represents proxy exit information from ip-api.com
type ProxyExitInfo struct {
	IP          string
	City        string
	Region      string
	Country     string
	CountryCode string
}

// ProxyExitInfoProber tests proxy connectivity and retrieves exit information
type ProxyExitInfoProber interface {
	ProbeProxy(ctx context.Context, proxyURL string) (*ProxyExitInfo, int64, error)
}

type groupExistenceBatchReader interface {
	ExistsByIDs(ctx context.Context, ids []int64) (map[int64]bool, error)
}

type proxyQualityTarget struct {
	Target          string
	URL             string
	Method          string
	AllowedStatuses map[int]struct{}
}

var proxyQualityTargets = []proxyQualityTarget{
	{
		Target: "openai",
		URL:    "https://api.openai.com/v1/models",
		Method: http.MethodGet,
		AllowedStatuses: map[int]struct{}{
			http.StatusUnauthorized: {},
		},
	},
	{
		Target: "anthropic",
		URL:    "https://api.anthropic.com/v1/messages",
		Method: http.MethodGet,
		AllowedStatuses: map[int]struct{}{
			http.StatusUnauthorized:     {},
			http.StatusMethodNotAllowed: {},
			http.StatusNotFound:         {},
			http.StatusBadRequest:       {},
		},
	},
	{
		Target: "gemini",
		URL:    "https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta",
		Method: http.MethodGet,
		AllowedStatuses: map[int]struct{}{
			http.StatusOK: {},
		},
	},
}

const (
	proxyQualityRequestTimeout        = 15 * time.Second
	proxyQualityResponseHeaderTimeout = 10 * time.Second
	proxyQualityMaxBodyBytes          = int64(8 * 1024)
	proxyQualityClientUserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

var ErrRPMStatusUnavailable = infraerrors.New(http.StatusNotImplemented, "RPM_STATUS_UNAVAILABLE", "RPM cache not available")

// adminServiceImpl implements AdminService
type adminServiceImpl struct {
	cfg                  *config.Config
	userRepo             UserRepository
	groupRepo            GroupRepository
	groupDuplicateRepo   GroupDuplicateRepository
	emptyGroupDeleteRepo EmptyGroupDeleteRepository
	accountRepo          AccountRepository
	accountDuplicateRepo AccountDuplicateRepository
	proxyRepo            ProxyRepository
	apiKeyRepo           APIKeyRepository
	redeemCodeRepo       RedeemCodeRepository
	userGroupRateRepo    UserGroupRateRepository
	userRPMCache         UserRPMCache
	billingCacheService  *BillingCacheService
	proxyProber          ProxyExitInfoProber
	proxyLatencyCache    ProxyLatencyCache
	authCacheInvalidator APIKeyAuthCacheInvalidator
	entClient            *dbent.Client // 用于开启数据库事务
	settingService       *SettingService
	defaultSubAssigner   DefaultSubscriptionAssigner
	userSubRepo          UserSubscriptionRepository
	privacyClientFactory PrivacyClientFactory
	runtimeBlocker       AccountRuntimeBlocker
	affiliateService     adminRechargeAffiliateAccruer
	compositeRouteRepo   CompositeModelRouteRepository
	compositeResolver    *CompositeRouteResolver
	// 分组平台变更后用来失效渠道缓存；可为 nil（缓存会在 TTL 到期后自然重建）
	channelCacheInvalidator ChannelCacheInvalidator
}

// ChannelCacheInvalidator 失效渠道缓存。
// 窄接口，避免 admin 服务依赖整个 ChannelService——与 APIKeyAuthCacheInvalidator 同一思路。
type ChannelCacheInvalidator interface {
	InvalidateCache()
}

type adminRechargeAffiliateAccruer interface {
	AccrueInviteRebate(ctx context.Context, inviteeUserID int64, baseRechargeAmount float64) (float64, error)
}

type userGroupRateBatchReader interface {
	GetByUserIDs(ctx context.Context, userIDs []int64) (map[int64]map[int64]float64, error)
}

// NewAdminService creates a new AdminService
func NewAdminService(
	cfg *config.Config,
	userRepo UserRepository,
	groupRepo GroupRepository,
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	apiKeyRepo APIKeyRepository,
	redeemCodeRepo RedeemCodeRepository,
	userGroupRateRepo UserGroupRateRepository,
	userRPMCache UserRPMCache,
	billingCacheService *BillingCacheService,
	proxyProber ProxyExitInfoProber,
	proxyLatencyCache ProxyLatencyCache,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
	entClient *dbent.Client,
	settingService *SettingService,
	defaultSubAssigner DefaultSubscriptionAssigner,
	userSubRepo UserSubscriptionRepository,
	privacyClientFactory PrivacyClientFactory,
	runtimeBlocker AccountRuntimeBlocker,
	affiliateService *AffiliateService,
	compositeRouteRepo CompositeModelRouteRepository,
	compositeResolver *CompositeRouteResolver,
	channelCacheInvalidator ChannelCacheInvalidator,
) AdminService {
	groupDuplicateRepo, _ := groupRepo.(GroupDuplicateRepository)
	emptyGroupDeleteRepo, _ := groupRepo.(EmptyGroupDeleteRepository)
	accountDuplicateRepo, _ := accountRepo.(AccountDuplicateRepository)
	return &adminServiceImpl{
		cfg:                  cfg,
		userRepo:             userRepo,
		groupRepo:            groupRepo,
		groupDuplicateRepo:   groupDuplicateRepo,
		emptyGroupDeleteRepo: emptyGroupDeleteRepo,
		accountRepo:          accountRepo,
		accountDuplicateRepo: accountDuplicateRepo,
		proxyRepo:            proxyRepo,
		apiKeyRepo:           apiKeyRepo,
		redeemCodeRepo:       redeemCodeRepo,
		userGroupRateRepo:    userGroupRateRepo,
		userRPMCache:         userRPMCache,
		billingCacheService:  billingCacheService,
		proxyProber:          proxyProber,
		proxyLatencyCache:    proxyLatencyCache,
		authCacheInvalidator: authCacheInvalidator,
		entClient:            entClient,
		settingService:       settingService,
		defaultSubAssigner:   defaultSubAssigner,
		userSubRepo:          userSubRepo,
		privacyClientFactory: privacyClientFactory,
		runtimeBlocker:       runtimeBlocker,
		affiliateService:     affiliateService,
		compositeRouteRepo:   compositeRouteRepo,
		compositeResolver:    compositeResolver,

		channelCacheInvalidator: channelCacheInvalidator,
	}
}

// User management implementations
func (s *adminServiceImpl) ListUsers(ctx context.Context, page, pageSize int, filters UserListFilters, sortBy, sortOrder string) ([]User, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	users, result, err := s.userRepo.ListWithFilters(ctx, params, filters)
	if err != nil {
		return nil, 0, err
	}
	if len(users) > 0 {
		userIDs := make([]int64, 0, len(users))
		for i := range users {
			userIDs = append(userIDs, users[i].ID)
		}
		lastUsedByUserID, latestErr := s.userRepo.GetLatestUsedAtByUserIDs(ctx, userIDs)
		if latestErr != nil {
			logger.LegacyPrintf("service.admin", "failed to load user last_used_at in batch: err=%v", latestErr)
		} else {
			for i := range users {
				users[i].LastUsedAt = lastUsedByUserID[users[i].ID]
			}
		}
	}
	// 批量加载用户专属分组倍率
	if s.userGroupRateRepo != nil && len(users) > 0 {
		if batchRepo, ok := s.userGroupRateRepo.(userGroupRateBatchReader); ok {
			userIDs := make([]int64, 0, len(users))
			for i := range users {
				userIDs = append(userIDs, users[i].ID)
			}
			ratesByUser, err := batchRepo.GetByUserIDs(ctx, userIDs)
			if err != nil {
				logger.LegacyPrintf("service.admin", "failed to load user group rates in batch: err=%v", err)
				s.loadUserGroupRatesOneByOne(ctx, users)
			} else {
				for i := range users {
					if rates, ok := ratesByUser[users[i].ID]; ok {
						users[i].GroupRates = rates
					}
				}
			}
		} else {
			s.loadUserGroupRatesOneByOne(ctx, users)
		}
	}
	return users, result.Total, nil
}

func (s *adminServiceImpl) ImportAPIRequestIPBlocklistFromDisabledUsers(ctx context.Context) (*ImportAPIRequestIPBlocklistResult, error) {
	if s.userRepo == nil || s.settingService == nil || s.entClient == nil {
		return nil, fmt.Errorf("admin service dependencies are not initialized")
	}

	const pageSize = 500
	var disabledUserIDs []int64
	for page := 1; ; page++ {
		users, result, err := s.userRepo.ListWithFilters(ctx, pagination.PaginationParams{
			Page:      page,
			PageSize:  pageSize,
			SortBy:    "id",
			SortOrder: "asc",
		}, UserListFilters{
			Status:               StatusDisabled,
			IncludeSubscriptions: boolPtr(false),
		})
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			disabledUserIDs = append(disabledUserIDs, user.ID)
		}
		if result == nil || page >= result.Pages || len(users) == 0 {
			break
		}
	}

	settings, err := s.settingService.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(settings.APIRequestIPBlocklist))
	for _, rule := range settings.APIRequestIPBlocklist {
		rule = strings.TrimSpace(rule)
		if rule != "" {
			existing[rule] = struct{}{}
		}
	}

	result := &ImportAPIRequestIPBlocklistResult{
		DisabledUserCount: len(disabledUserIDs),
	}
	if len(disabledUserIDs) == 0 {
		result.TotalIPCount = len(existing)
		return result, nil
	}

	scanned := make(map[string]struct{})
	const chunkSize = 500
	for start := 0; start < len(disabledUserIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(disabledUserIDs) {
			end = len(disabledUserIDs)
		}
		var rows []struct {
			IPAddress *string `json:"ip_address"`
		}
		err := s.entClient.UsageLog.Query().
			Unique(true).
			Where(
				usagelog.UserIDIn(disabledUserIDs[start:end]...),
				usagelog.IPAddressNotNil(),
				usagelog.IPAddressNEQ(""),
			).
			Select(usagelog.FieldIPAddress).
			Scan(ctx, &rows)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.IPAddress == nil {
				continue
			}
			raw := strings.TrimSpace(*row.IPAddress)
			if raw == "" || !ip.ValidateIPPattern(raw) {
				continue
			}
			scanned[raw] = struct{}{}
			if _, ok := existing[raw]; ok {
				continue
			}
			existing[raw] = struct{}{}
			result.AddedIPs = append(result.AddedIPs, raw)
		}
	}

	sort.Strings(result.AddedIPs)
	merged := make([]string, 0, len(existing))
	for rule := range existing {
		merged = append(merged, rule)
	}
	sort.Strings(merged)
	result.ScannedIPCount = len(scanned)
	result.AddedIPCount = len(result.AddedIPs)
	result.TotalIPCount = len(merged)
	if result.AddedIPCount == 0 {
		return result, nil
	}
	settings.APIRequestIPBlocklist = merged
	if err := s.settingService.UpdateSettings(ctx, settings); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *adminServiceImpl) loadUserGroupRatesOneByOne(ctx context.Context, users []User) {
	if s.userGroupRateRepo == nil {
		return
	}
	for i := range users {
		rates, err := s.userGroupRateRepo.GetByUserID(ctx, users[i].ID)
		if err != nil {
			logger.LegacyPrintf("service.admin", "failed to load user group rates: user_id=%d err=%v", users[i].ID, err)
			continue
		}
		users[i].GroupRates = rates
	}
}

func (s *adminServiceImpl) GetUser(ctx context.Context, id int64) (*User, error) {
	user, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	lastUsedAt, latestErr := s.userRepo.GetLatestUsedAtByUserID(ctx, id)
	if latestErr != nil {
		logger.LegacyPrintf("service.admin", "failed to load user last_used_at: user_id=%d err=%v", id, latestErr)
	} else {
		user.LastUsedAt = lastUsedAt
	}
	// 加载用户专属分组倍率
	if s.userGroupRateRepo != nil {
		rates, err := s.userGroupRateRepo.GetByUserID(ctx, id)
		if err != nil {
			logger.LegacyPrintf("service.admin", "failed to load user group rates: user_id=%d err=%v", id, err)
		} else {
			user.GroupRates = rates
		}
	}
	return user, nil
}

func (s *adminServiceImpl) GetUserIncludeDeleted(ctx context.Context, id int64) (*User, error) {
	return s.userRepo.GetByIDIncludeDeleted(ctx, id)
}

func (s *adminServiceImpl) CreateUser(ctx context.Context, input *CreateUserInput) (*User, error) {
	balance := 0.0
	if input.Balance != nil {
		balance = *input.Balance
	} else if s.settingService != nil {
		balance = s.settingService.GetDefaultBalance(ctx)
	}

	user := &User{
		Email:         input.Email,
		Username:      input.Username,
		Notes:         input.Notes,
		Role:          RoleUser, // Always create as regular user, never admin
		Balance:       balance,
		Concurrency:   input.Concurrency,
		RPMLimit:      input.RPMLimit,
		Status:        StatusActive,
		AllowedGroups: input.AllowedGroups,
	}
	if err := user.SetPassword(input.Password); err != nil {
		return nil, err
	}
	if err := s.userRepo.Create(ctx, user); err != nil {
		return nil, err
	}
	s.assignDefaultSubscriptions(ctx, user.ID)
	return user, nil
}

func (s *adminServiceImpl) assignDefaultSubscriptions(ctx context.Context, userID int64) {
	if s.settingService == nil || s.defaultSubAssigner == nil || userID <= 0 {
		return
	}
	items := s.settingService.GetDefaultSubscriptions(ctx)
	for _, item := range items {
		if _, _, err := s.defaultSubAssigner.AssignOrExtendSubscription(ctx, &AssignSubscriptionInput{
			UserID:       userID,
			GroupID:      item.GroupID,
			ValidityDays: item.ValidityDays,
			Notes:        "auto assigned by default user subscriptions setting",
		}); err != nil {
			logger.LegacyPrintf("service.admin", "failed to assign default subscription: user_id=%d group_id=%d err=%v", userID, item.GroupID, err)
		}
	}
}

func (s *adminServiceImpl) UpdateUser(ctx context.Context, id int64, input *UpdateUserInput) (*User, error) {
	// 校验用户专属分组倍率：必须 > 0（nil 合法，表示清除专属倍率）
	if input.GroupRates != nil {
		for groupID, rate := range input.GroupRates {
			if rate != nil && *rate <= 0 {
				return nil, fmt.Errorf("rate_multiplier must be > 0 (group_id=%d)", groupID)
			}
		}
	}

	user, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Protect admin users: cannot disable admin accounts
	if user.Role == "admin" && input.Status == "disabled" {
		return nil, errors.New("cannot disable admin user")
	}

	oldConcurrency := user.Concurrency
	oldStatus := user.Status
	oldRole := user.Role
	oldRPMLimit := user.RPMLimit
	oldAllowedGroups := append([]int64(nil), user.AllowedGroups...)
	var fields UserUpdateFields

	if input.Email != "" {
		user.Email = input.Email
		fields.Email = true
	}
	if input.Password != "" {
		if err := user.SetPassword(input.Password); err != nil {
			return nil, err
		}
		fields.PasswordHash = true
	}

	if input.Username != nil {
		user.Username = *input.Username
		fields.Username = true
	}
	if input.Notes != nil {
		user.Notes = *input.Notes
		fields.Notes = true
	}

	if input.Status != "" {
		user.Status = input.Status
		fields.Status = true
	}

	if input.Concurrency != nil {
		user.Concurrency = *input.Concurrency
		fields.Concurrency = true
	}

	if input.RPMLimit != nil {
		user.RPMLimit = *input.RPMLimit
		fields.RPMLimit = true
	}

	if input.AllowedGroups != nil {
		user.AllowedGroups = *input.AllowedGroups
		fields.AllowedGroups = true
	}

	if err := s.userRepo.Update(ctx, user, fields); err != nil {
		return nil, err
	}

	// 同步用户专属分组倍率
	if input.GroupRates != nil && s.userGroupRateRepo != nil {
		if err := s.userGroupRateRepo.SyncUserGroupRates(ctx, user.ID, input.GroupRates); err != nil {
			logger.LegacyPrintf("service.admin", "failed to sync user group rates: user_id=%d err=%v", user.ID, err)
		}
	}

	if s.authCacheInvalidator != nil {
		// RPMLimit 直接参与 billing_cache_service.checkRPM 的三级级联，
		// allowed_groups 参与 API Key 专属分组授权判断；不失效缓存会让修改在一个 L2 TTL 内失去效果。
		if user.Concurrency != oldConcurrency || user.Status != oldStatus || user.Role != oldRole || user.RPMLimit != oldRPMLimit || !sameInt64Set(user.AllowedGroups, oldAllowedGroups) {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, user.ID)
		}
	}

	concurrencyDiff := user.Concurrency - oldConcurrency
	if concurrencyDiff != 0 {
		code, err := GenerateRedeemCode()
		if err != nil {
			logger.LegacyPrintf("service.admin", "failed to generate adjustment redeem code: %v", err)
			return user, nil
		}
		adjustmentRecord := &RedeemCode{
			Code:   code,
			Type:   AdjustmentTypeAdminConcurrency,
			Value:  float64(concurrencyDiff),
			Status: StatusUsed,
			UsedBy: &user.ID,
		}
		now := time.Now()
		adjustmentRecord.UsedAt = &now
		if err := s.redeemCodeRepo.Create(ctx, adjustmentRecord); err != nil {
			logger.LegacyPrintf("service.admin", "failed to create concurrency adjustment redeem code: %v", err)
		}
	}

	return user, nil
}

func (s *adminServiceImpl) BatchUpdateConcurrency(ctx context.Context, userIDs []int64, value int, mode string) (int, error) {
	cleaned := make([]int64, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, uid := range userIDs {
		if uid <= 0 {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		cleaned = append(cleaned, uid)
	}
	if len(cleaned) == 0 {
		return 0, nil
	}

	switch mode {
	case "set":
		return s.userRepo.BatchSetConcurrency(ctx, cleaned, value)
	case "add":
		return s.userRepo.BatchAddConcurrency(ctx, cleaned, value)
	default:
		return 0, errors.New("invalid mode: must be 'set' or 'add'")
	}
}

func (s *adminServiceImpl) BatchUpdateLimits(ctx context.Context, userIDs []int64, concurrency, rpmLimit *int) (int, error) {
	if concurrency == nil && rpmLimit == nil {
		return 0, fmt.Errorf("at least one of concurrency or rpm_limit is required")
	}

	cleaned := make([]int64, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, userID := range userIDs {
		if userID <= 0 {
			continue
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		cleaned = append(cleaned, userID)
	}
	if len(cleaned) == 0 {
		return 0, nil
	}

	affected, err := s.userRepo.BatchUpdateLimits(ctx, cleaned, concurrency, rpmLimit)
	if err != nil {
		return 0, err
	}
	if s.authCacheInvalidator != nil {
		for _, userID := range cleaned {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
		}
	}
	return affected, nil
}

func sameInt64Set(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	counts := make(map[int64]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v] == 0 {
			return false
		}
		counts[v]--
	}
	return true
}

func (s *adminServiceImpl) DeleteUser(ctx context.Context, id int64) error {
	// Protect admin users: cannot delete admin accounts
	user, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if user.Role == "admin" {
		return errors.New("cannot delete admin user")
	}

	apiKeys, err := s.listUserAPIKeysForDeletion(ctx, id)
	if err != nil {
		return err
	}

	if s.entClient != nil {
		tx, err := s.entClient.Tx(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		opCtx := dbent.NewTxContext(ctx, tx)
		if err := s.deleteUserWithAPIKeys(opCtx, id, apiKeys); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	} else {
		if err := s.deleteUserWithAPIKeys(ctx, id, apiKeys); err != nil {
			return err
		}
	}

	if s.authCacheInvalidator != nil {
		for _, key := range apiKeys {
			if keyValue := strings.TrimSpace(key.Key); keyValue != "" {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, keyValue)
			}
		}
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, id)
	}
	return nil
}

func (s *adminServiceImpl) listUserAPIKeysForDeletion(ctx context.Context, userID int64) ([]APIKey, error) {
	if s.apiKeyRepo == nil {
		return nil, nil
	}

	const pageSize = 1000
	keys := make([]APIKey, 0)
	for page := 1; ; page++ {
		batch, result, err := s.apiKeyRepo.ListByUserID(ctx, userID, pagination.PaginationParams{
			Page:      page,
			PageSize:  pageSize,
			SortBy:    "id",
			SortOrder: pagination.SortOrderAsc,
		}, APIKeyListFilters{})
		if err != nil {
			return nil, fmt.Errorf("list user api keys: %w", err)
		}
		keys = append(keys, batch...)
		if len(batch) == 0 || len(batch) < pageSize || result == nil || int64(len(keys)) >= result.Total {
			break
		}
	}
	return keys, nil
}

func (s *adminServiceImpl) deleteUserWithAPIKeys(ctx context.Context, userID int64, apiKeys []APIKey) error {
	if s.apiKeyRepo != nil {
		for _, key := range apiKeys {
			if key.ID <= 0 {
				continue
			}
			if err := s.apiKeyRepo.DeleteWithAudit(ctx, key.ID); err != nil {
				logger.LegacyPrintf("service.admin", "delete user api key failed: user_id=%d api_key_id=%d err=%v", userID, key.ID, err)
				return fmt.Errorf("delete user api key %d: %w", key.ID, err)
			}
		}
	}

	if err := s.userRepo.Delete(ctx, userID); err != nil {
		logger.LegacyPrintf("service.admin", "delete user failed: user_id=%d err=%v", userID, err)
		return err
	}
	return nil
}

func (s *adminServiceImpl) UpdateUserBalance(ctx context.Context, userID int64, balance float64, operation string, notes string) (*User, error) {
	var (
		change BalanceChange
		err    error
	)
	switch operation {
	case "set":
		change, err = s.userRepo.SetBalance(ctx, userID, balance)
	case "add":
		change, err = s.userRepo.AdjustBalance(ctx, userID, balance)
	case "subtract":
		change, err = s.userRepo.AdjustBalance(ctx, userID, -balance)
	default:
		return nil, fmt.Errorf("unsupported balance operation: %q", operation)
	}
	if errors.Is(err, ErrBalanceNegative) {
		return nil, fmt.Errorf("balance cannot be negative, current balance: %.2f, requested operation would result in: %.2f", change.Old, change.New)
	}
	if err != nil {
		return nil, err
	}

	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	balanceDiff := change.New - change.Old
	if s.authCacheInvalidator != nil && balanceDiff != 0 {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	s.tryAccrueAffiliateRebateForAdminRecharge(ctx, userID, operation, balance)

	if s.billingCacheService != nil {
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.billingCacheService.InvalidateUserBalance(cacheCtx, userID); err != nil {
				logger.LegacyPrintf("service.admin", "invalidate user balance cache failed: user_id=%d err=%v", userID, err)
			}
		}()
	}

	if balanceDiff != 0 {
		code, err := GenerateRedeemCode()
		if err != nil {
			logger.LegacyPrintf("service.admin", "failed to generate adjustment redeem code: %v", err)
			return user, nil
		}

		adjustmentRecord := &RedeemCode{
			Code:   code,
			Type:   AdjustmentTypeAdminBalance,
			Value:  balanceDiff,
			Status: StatusUsed,
			UsedBy: &user.ID,
			Notes:  notes,
		}
		now := time.Now()
		adjustmentRecord.UsedAt = &now

		if err := s.redeemCodeRepo.Create(ctx, adjustmentRecord); err != nil {
			logger.LegacyPrintf("service.admin", "failed to create balance adjustment redeem code: %v", err)
		}
	}

	return user, nil
}

func (s *adminServiceImpl) tryAccrueAffiliateRebateForAdminRecharge(ctx context.Context, userID int64, operation string, amount float64) {
	if operation != "add" || amount <= 0 || s.settingService == nil || s.affiliateService == nil {
		return
	}
	if !s.settingService.IsAffiliateAdminRechargeEnabled(ctx) {
		return
	}

	rebate, err := s.affiliateService.AccrueInviteRebate(ctx, userID, amount)
	if err != nil {
		logger.LegacyPrintf("service.admin", "affiliate rebate failed for admin recharge: user_id=%d amount=%.8f err=%v", userID, amount, err)
		return
	}
	if rebate > 0 {
		logger.LegacyPrintf("service.admin", "affiliate rebate accrued for admin recharge: user_id=%d amount=%.8f rebate=%.8f", userID, amount, rebate)
	}
}

func (s *adminServiceImpl) GetUserAPIKeys(ctx context.Context, userID int64, page, pageSize int, sortBy, sortOrder string) ([]APIKey, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	keys, result, err := s.apiKeyRepo.ListByUserID(ctx, userID, params, APIKeyListFilters{})
	if err != nil {
		return nil, 0, err
	}
	return keys, result.Total, nil
}

func (s *adminServiceImpl) GetUserRPMStatus(ctx context.Context, userID int64) (*UserRPMStatus, error) {
	if s.userRPMCache == nil {
		return nil, ErrRPMStatusUnavailable
	}

	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	userRPMUsed, err := s.userRPMCache.GetUserRPM(ctx, userID)
	if err != nil {
		logger.LegacyPrintf("service.admin", "failed to get user rpm: user_id=%d err=%v", userID, err)
	}

	keys, _, err := s.GetUserAPIKeys(ctx, userID, 1, 1000, "", "")
	if err != nil {
		return nil, err
	}

	groupIDSet := make(map[int64]struct{})
	for _, key := range keys {
		if key.GroupID != nil && *key.GroupID > 0 {
			groupIDSet[*key.GroupID] = struct{}{}
		}
	}

	groupIDs := make([]int64, 0, len(groupIDSet))
	for groupID := range groupIDSet {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })

	var perGroup []UserGroupRPMStatus
	for _, groupID := range groupIDs {
		used, getErr := s.userRPMCache.GetUserGroupRPM(ctx, userID, groupID)
		if getErr != nil {
			logger.LegacyPrintf("service.admin", "failed to get user group rpm: user_id=%d group_id=%d err=%v", userID, groupID, getErr)
		}

		entry := UserGroupRPMStatus{
			GroupID: groupID,
			Used:    used,
		}

		if s.groupRepo != nil {
			if group, groupErr := s.groupRepo.GetByIDLite(ctx, groupID); groupErr == nil && group != nil {
				entry.GroupName = group.Name
				entry.Limit = group.RPMLimit
				entry.Source = "group"
			} else if groupErr != nil {
				logger.LegacyPrintf("service.admin", "failed to get group rpm status metadata: group_id=%d err=%v", groupID, groupErr)
			}
		}

		if s.userGroupRateRepo != nil {
			override, overrideErr := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
			if overrideErr != nil {
				logger.LegacyPrintf("service.admin", "failed to get rpm override: user_id=%d group_id=%d err=%v", userID, groupID, overrideErr)
			} else if override != nil {
				entry.Limit = *override
				entry.Source = "override"
			}
		}

		perGroup = append(perGroup, entry)
	}

	return &UserRPMStatus{
		UserRPMUsed:  userRPMUsed,
		UserRPMLimit: user.RPMLimit,
		PerGroup:     perGroup,
	}, nil
}

func (s *adminServiceImpl) GetUserUsageStats(ctx context.Context, userID int64, period string) (any, error) {
	// Return mock data for now
	return map[string]any{
		"period":          period,
		"total_requests":  0,
		"total_cost":      0.0,
		"total_tokens":    0,
		"avg_duration_ms": 0,
	}, nil
}

// GetUserBalanceHistory returns paginated balance/concurrency change records for a user.
func (s *adminServiceImpl) GetUserBalanceHistory(ctx context.Context, userID int64, page, pageSize int, codeType string) ([]RedeemCode, int64, float64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	if codeType == RedeemTypeAffiliateBalance {
		codes, total, err := s.listAffiliateBalanceHistory(ctx, userID, params)
		if err != nil {
			return nil, 0, 0, err
		}
		totalRecharged, err := s.redeemCodeRepo.SumPositiveBalanceByUser(ctx, userID)
		if err != nil {
			return nil, 0, 0, err
		}
		return codes, total, totalRecharged, nil
	}

	if codeType == "" {
		return s.getAllUserBalanceHistory(ctx, userID, params)
	}

	codes, result, err := s.redeemCodeRepo.ListByUserPaginated(ctx, userID, params, codeType)
	if err != nil {
		return nil, 0, 0, err
	}
	total := result.Total
	// Aggregate total recharged amount (only once, regardless of type filter)
	totalRecharged, err := s.redeemCodeRepo.SumPositiveBalanceByUser(ctx, userID)
	if err != nil {
		return nil, 0, 0, err
	}
	return codes, total, totalRecharged, nil
}

func (s *adminServiceImpl) getAllUserBalanceHistory(ctx context.Context, userID int64, params pagination.PaginationParams) ([]RedeemCode, int64, float64, error) {
	needed := params.Offset() + params.Limit()
	if needed < params.Limit() {
		needed = params.Limit()
	}

	redeemCodes, redeemTotal, err := s.listRedeemBalanceHistoryForMerge(ctx, userID, needed)
	if err != nil {
		return nil, 0, 0, err
	}
	affiliateCodes, affiliateTotal, err := s.listAffiliateBalanceHistoryForMerge(ctx, userID, needed)
	if err != nil {
		return nil, 0, 0, err
	}
	codes := mergeBalanceHistoryCodes(redeemCodes, affiliateCodes, params)

	totalRecharged, err := s.redeemCodeRepo.SumPositiveBalanceByUser(ctx, userID)
	if err != nil {
		return nil, 0, 0, err
	}
	return codes, redeemTotal + affiliateTotal, totalRecharged, nil
}

func (s *adminServiceImpl) listRedeemBalanceHistoryForMerge(ctx context.Context, userID int64, needed int) ([]RedeemCode, int64, error) {
	if needed <= 0 {
		return nil, 0, nil
	}

	var (
		out   []RedeemCode
		total int64
	)
	for page := 1; len(out) < needed; page++ {
		params := pagination.PaginationParams{Page: page, PageSize: 1000}
		codes, result, err := s.redeemCodeRepo.ListByUserPaginated(ctx, userID, params, "")
		if err != nil {
			return nil, 0, err
		}
		if result != nil {
			total = result.Total
		}
		out = append(out, codes...)
		if len(codes) < params.Limit() || int64(len(out)) >= total {
			break
		}
	}
	if len(out) > needed {
		out = out[:needed]
	}
	return out, total, nil
}

func (s *adminServiceImpl) listAffiliateBalanceHistoryForMerge(ctx context.Context, userID int64, needed int) ([]RedeemCode, int64, error) {
	if needed <= 0 {
		return nil, 0, nil
	}

	var (
		out   []RedeemCode
		total int64
	)
	for page := 1; len(out) < needed; page++ {
		params := pagination.PaginationParams{Page: page, PageSize: 1000}
		codes, currentTotal, err := s.listAffiliateBalanceHistory(ctx, userID, params)
		if err != nil {
			return nil, 0, err
		}
		total = currentTotal
		out = append(out, codes...)
		if len(codes) < params.Limit() || int64(len(out)) >= total {
			break
		}
	}
	if len(out) > needed {
		out = out[:needed]
	}
	return out, total, nil
}

func (s *adminServiceImpl) listAffiliateBalanceHistory(ctx context.Context, userID int64, params pagination.PaginationParams) ([]RedeemCode, int64, error) {
	if s == nil || s.entClient == nil || userID <= 0 {
		return nil, 0, nil
	}

	rows, err := s.entClient.QueryContext(ctx, `
SELECT id,
       amount::double precision,
       created_at
FROM user_affiliate_ledger
WHERE user_id = $1
  AND action = 'transfer'
ORDER BY created_at DESC, id DESC
OFFSET $2
LIMIT $3`, userID, params.Offset(), params.Limit())
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	codes := make([]RedeemCode, 0, params.Limit())
	for rows.Next() {
		var id int64
		var amount float64
		var createdAt time.Time
		if err := rows.Scan(&id, &amount, &createdAt); err != nil {
			return nil, 0, err
		}
		usedBy := userID
		usedAt := createdAt
		codes = append(codes, RedeemCode{
			ID:        -id,
			Code:      fmt.Sprintf("AFF-%d", id),
			Type:      RedeemTypeAffiliateBalance,
			Value:     amount,
			Status:    StatusUsed,
			UsedBy:    &usedBy,
			UsedAt:    &usedAt,
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	total, err := countAffiliateBalanceHistory(ctx, s.entClient, userID)
	if err != nil {
		return nil, 0, err
	}
	return codes, total, nil
}

func countAffiliateBalanceHistory(ctx context.Context, client *dbent.Client, userID int64) (int64, error) {
	rows, err := client.QueryContext(ctx, `
SELECT COUNT(*)
FROM user_affiliate_ledger
WHERE user_id = $1
  AND action = 'transfer'`, userID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var total sql.NullInt64
	if rows.Next() {
		if err := rows.Scan(&total); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

func mergeBalanceHistoryCodes(redeemCodes, affiliateCodes []RedeemCode, params pagination.PaginationParams) []RedeemCode {
	combined := append(append([]RedeemCode{}, redeemCodes...), affiliateCodes...)
	sort.SliceStable(combined, func(i, j int) bool {
		return redeemCodeHistoryTime(combined[i]).After(redeemCodeHistoryTime(combined[j]))
	})
	offset := params.Offset()
	if offset >= len(combined) {
		return []RedeemCode{}
	}
	end := offset + params.Limit()
	if end > len(combined) {
		end = len(combined)
	}
	return combined[offset:end]
}

func redeemCodeHistoryTime(code RedeemCode) time.Time {
	if code.UsedAt != nil {
		return *code.UsedAt
	}
	return code.CreatedAt
}

func (s *adminServiceImpl) BindUserAuthIdentity(ctx context.Context, userID int64, input AdminBindAuthIdentityInput) (*AdminBoundAuthIdentity, error) {
	if userID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "user_id must be greater than 0")
	}
	if s == nil || s.entClient == nil || s.userRepo == nil {
		return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_UNAVAILABLE", "auth identity binding service is unavailable")
	}
	if _, err := s.userRepo.GetByID(ctx, userID); err != nil {
		return nil, err
	}

	providerType := normalizeAdminAuthIdentityProviderType(input.ProviderType)
	providerKey := strings.TrimSpace(input.ProviderKey)
	providerSubject := strings.TrimSpace(input.ProviderSubject)
	if providerType == "" {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "provider_type must be one of email, linuxdo, oidc, wechat, or dingtalk")
	}
	if providerKey == "" || providerSubject == "" {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "provider_type, provider_key, and provider_subject are required")
	}
	canonicalProviderKey := canonicalAdminAuthIdentityProviderKey(providerType, "", providerKey)
	compatibleProviderKeys := compatibleAdminAuthIdentityProviderKeys(providerType, providerKey)

	var issuer *string
	if input.Issuer != nil {
		trimmed := strings.TrimSpace(*input.Issuer)
		if trimmed != "" {
			issuer = &trimmed
		}
	}

	channelInput := normalizeAdminBindChannelInput(input.Channel)
	if input.Channel != nil && channelInput == nil {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "channel, channel_app_id, and channel_subject are required when channel binding is provided")
	}

	verifiedAt := time.Now().UTC()
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_TX_FAILED", "failed to start auth identity bind transaction").WithCause(err)
	}
	defer func() { _ = tx.Rollback() }()

	identityRecords, err := tx.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ(providerType),
			authidentity.ProviderKeyIn(compatibleProviderKeys...),
			authidentity.ProviderSubjectEQ(providerSubject),
		).
		All(ctx)
	if err != nil {
		return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_LOOKUP_FAILED", "failed to inspect auth identity ownership").WithCause(err)
	}
	if hasAdminAuthIdentityOwnershipConflict(identityRecords, userID) {
		return nil, infraerrors.Conflict("AUTH_IDENTITY_OWNERSHIP_CONFLICT", "auth identity already belongs to another user")
	}
	identity := selectOwnedAdminAuthIdentity(identityRecords, userID)

	if identity == nil {
		create := tx.AuthIdentity.Create().
			SetUserID(userID).
			SetProviderType(providerType).
			SetProviderKey(canonicalProviderKey).
			SetProviderSubject(providerSubject).
			SetVerifiedAt(verifiedAt)
		if issuer != nil {
			create = create.SetIssuer(*issuer)
		}
		if input.Metadata != nil {
			create = create.SetMetadata(cloneAdminAuthIdentityMetadata(input.Metadata))
		}
		identity, err = create.Save(ctx)
		if err != nil {
			return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_SAVE_FAILED", "failed to save auth identity").WithCause(err)
		}
	} else {
		update := tx.AuthIdentity.UpdateOneID(identity.ID).
			SetVerifiedAt(verifiedAt).
			SetProviderKey(canonicalProviderKey)
		if issuer != nil {
			update = update.SetIssuer(*issuer)
		}
		if input.Metadata != nil {
			update = update.SetMetadata(cloneAdminAuthIdentityMetadata(input.Metadata))
		}
		identity, err = update.Save(ctx)
		if err != nil {
			return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_SAVE_FAILED", "failed to save auth identity").WithCause(err)
		}
	}

	var channel *dbent.AuthIdentityChannel
	if channelInput != nil {
		channelRecords, err := tx.AuthIdentityChannel.Query().
			Where(
				authidentitychannel.ProviderTypeEQ(providerType),
				authidentitychannel.ProviderKeyIn(compatibleProviderKeys...),
				authidentitychannel.ChannelEQ(channelInput.Channel),
				authidentitychannel.ChannelAppIDEQ(channelInput.ChannelAppID),
				authidentitychannel.ChannelSubjectEQ(channelInput.ChannelSubject),
			).
			WithIdentity().
			All(ctx)
		if err != nil {
			return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_CHANNEL_LOOKUP_FAILED", "failed to inspect auth identity channel ownership").WithCause(err)
		}
		if hasAdminAuthIdentityChannelOwnershipConflict(channelRecords, userID) {
			return nil, infraerrors.Conflict("AUTH_IDENTITY_CHANNEL_OWNERSHIP_CONFLICT", "auth identity channel already belongs to another user")
		}
		channel = selectOwnedAdminAuthIdentityChannel(channelRecords, userID)
		if channel == nil {
			create := tx.AuthIdentityChannel.Create().
				SetIdentityID(identity.ID).
				SetProviderType(providerType).
				SetProviderKey(canonicalProviderKey).
				SetChannel(channelInput.Channel).
				SetChannelAppID(channelInput.ChannelAppID).
				SetChannelSubject(channelInput.ChannelSubject)
			if channelInput.Metadata != nil {
				create = create.SetMetadata(cloneAdminAuthIdentityMetadata(channelInput.Metadata))
			}
			channel, err = create.Save(ctx)
			if err != nil {
				return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_CHANNEL_SAVE_FAILED", "failed to save auth identity channel").WithCause(err)
			}
		} else {
			update := tx.AuthIdentityChannel.UpdateOneID(channel.ID).
				SetIdentityID(identity.ID).
				SetProviderKey(canonicalProviderKey)
			if channelInput.Metadata != nil {
				update = update.SetMetadata(cloneAdminAuthIdentityMetadata(channelInput.Metadata))
			}
			channel, err = update.Save(ctx)
			if err != nil {
				return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_CHANNEL_SAVE_FAILED", "failed to save auth identity channel").WithCause(err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, infraerrors.InternalServer("ADMIN_AUTH_IDENTITY_BIND_COMMIT_FAILED", "failed to commit auth identity bind").WithCause(err)
	}
	return buildAdminBoundAuthIdentity(identity, channel), nil
}

func compatibleAdminAuthIdentityProviderKeys(providerType, providerKey string) []string {
	providerType = strings.TrimSpace(strings.ToLower(providerType))
	providerKey = strings.TrimSpace(providerKey)
	if providerKey == "" {
		return []string{providerKey}
	}
	if providerType != "wechat" {
		return []string{providerKey}
	}

	keys := []string{providerKey}
	if !strings.EqualFold(providerKey, "wechat-main") {
		keys = append(keys, "wechat-main")
	}
	if !strings.EqualFold(providerKey, "wechat") {
		keys = append(keys, "wechat")
	}
	return keys
}

func canonicalAdminAuthIdentityProviderKey(providerType, existingKey, requestedKey string) string {
	providerType = strings.TrimSpace(strings.ToLower(providerType))
	existingKey = strings.TrimSpace(existingKey)
	requestedKey = strings.TrimSpace(requestedKey)
	if providerType != "wechat" {
		if requestedKey != "" {
			return requestedKey
		}
		return existingKey
	}
	if strings.EqualFold(existingKey, "wechat") || strings.EqualFold(existingKey, "wechat-main") || strings.EqualFold(requestedKey, "wechat-main") {
		return "wechat-main"
	}
	if requestedKey != "" {
		return requestedKey
	}
	return existingKey
}

func adminAuthIdentityProviderKeyRank(providerType, providerKey string) int {
	providerType = strings.TrimSpace(strings.ToLower(providerType))
	providerKey = strings.TrimSpace(providerKey)
	if providerType != "wechat" {
		return 0
	}
	switch {
	case strings.EqualFold(providerKey, "wechat-main"):
		return 0
	case strings.EqualFold(providerKey, "wechat"):
		return 2
	default:
		return 1
	}
}

func selectOwnedAdminAuthIdentity(records []*dbent.AuthIdentity, userID int64) *dbent.AuthIdentity {
	var selected *dbent.AuthIdentity
	for _, record := range records {
		if record.UserID != userID {
			continue
		}
		if selected == nil || adminAuthIdentityProviderKeyRank(record.ProviderType, record.ProviderKey) < adminAuthIdentityProviderKeyRank(selected.ProviderType, selected.ProviderKey) {
			selected = record
		}
	}
	return selected
}

func hasAdminAuthIdentityOwnershipConflict(records []*dbent.AuthIdentity, userID int64) bool {
	for _, record := range records {
		if record.UserID != userID {
			return true
		}
	}
	return false
}

func selectOwnedAdminAuthIdentityChannel(records []*dbent.AuthIdentityChannel, userID int64) *dbent.AuthIdentityChannel {
	var selected *dbent.AuthIdentityChannel
	for _, record := range records {
		if record.Edges.Identity == nil || record.Edges.Identity.UserID != userID {
			continue
		}
		if selected == nil || adminAuthIdentityProviderKeyRank(record.ProviderType, record.ProviderKey) < adminAuthIdentityProviderKeyRank(selected.ProviderType, selected.ProviderKey) {
			selected = record
		}
	}
	return selected
}

func hasAdminAuthIdentityChannelOwnershipConflict(records []*dbent.AuthIdentityChannel, userID int64) bool {
	for _, record := range records {
		if record.Edges.Identity != nil && record.Edges.Identity.UserID != userID {
			return true
		}
	}
	return false
}

func normalizeAdminBindChannelInput(input *AdminBindAuthIdentityChannelInput) *AdminBindAuthIdentityChannelInput {
	if input == nil {
		return nil
	}
	channel := &AdminBindAuthIdentityChannelInput{
		Channel:        strings.TrimSpace(input.Channel),
		ChannelAppID:   strings.TrimSpace(input.ChannelAppID),
		ChannelSubject: strings.TrimSpace(input.ChannelSubject),
		Metadata:       cloneAdminAuthIdentityMetadata(input.Metadata),
	}
	if channel.Channel == "" || channel.ChannelAppID == "" || channel.ChannelSubject == "" {
		return nil
	}
	return channel
}

func normalizeAdminAuthIdentityProviderType(input string) string {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "email":
		return "email"
	case "linuxdo":
		return "linuxdo"
	case "oidc":
		return "oidc"
	case "wechat":
		return "wechat"
	case "dingtalk":
		return "dingtalk"
	default:
		return ""
	}
}

func buildAdminBoundAuthIdentity(identity *dbent.AuthIdentity, channel *dbent.AuthIdentityChannel) *AdminBoundAuthIdentity {
	if identity == nil {
		return nil
	}
	result := &AdminBoundAuthIdentity{
		UserID:          identity.UserID,
		ProviderType:    strings.TrimSpace(identity.ProviderType),
		ProviderKey:     strings.TrimSpace(identity.ProviderKey),
		ProviderSubject: strings.TrimSpace(identity.ProviderSubject),
		VerifiedAt:      identity.VerifiedAt,
		Issuer:          identity.Issuer,
		Metadata:        cloneAdminAuthIdentityMetadata(identity.Metadata),
		CreatedAt:       identity.CreatedAt,
		UpdatedAt:       identity.UpdatedAt,
	}
	if channel != nil {
		result.Channel = &AdminBoundAuthIdentityChannel{
			Channel:        strings.TrimSpace(channel.Channel),
			ChannelAppID:   strings.TrimSpace(channel.ChannelAppID),
			ChannelSubject: strings.TrimSpace(channel.ChannelSubject),
			Metadata:       cloneAdminAuthIdentityMetadata(channel.Metadata),
			CreatedAt:      channel.CreatedAt,
			UpdatedAt:      channel.UpdatedAt,
		}
	}
	return result
}

func cloneAdminAuthIdentityMetadata(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	if len(input) == 0 {
		return map[string]any{}
	}
	data, err := json.Marshal(input)
	if err != nil {
		out := make(map[string]any, len(input))
		for key, value := range input {
			out[key] = value
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		out = make(map[string]any, len(input))
		for key, value := range input {
			out[key] = value
		}
	}
	return out
}

type accountGroupAllStatusesReader interface {
	ListByGroupAllStatuses(ctx context.Context, groupID int64) ([]Account, error)
}

func (s *adminServiceImpl) GetGroupQuotaSummary(ctx context.Context, groupID int64) (*GroupQuotaSummary, error) {
	group, err := s.groupRepo.GetByID(ctx, groupID)
	if err != nil {
		return nil, err
	}

	summary := &GroupQuotaSummary{
		GroupID:   groupID,
		Platform:  group.Platform,
		Supported: group.Platform == PlatformOpenAI,
	}
	if !summary.Supported {
		return summary, nil
	}

	accounts, err := s.accountRepo.ListByGroup(ctx, groupID)
	if reader, ok := s.accountRepo.(accountGroupAllStatusesReader); ok {
		accounts, err = reader.ListByGroupAllStatuses(ctx, groupID)
	}
	if err != nil {
		return nil, err
	}

	summary.Tabs = []GroupQuotaTabSummary{
		buildGroupQuotaTabSummary(accounts, "5h", "prefer_5h", "codex_5h_used_percent"),
		buildGroupQuotaTabSummary(accounts, "7d", "prefer_7d", "codex_7d_used_percent"),
		buildGroupQuotaRefreshTabSummary(accounts, time.Now()),
	}
	return summary, nil
}

func buildGroupQuotaTabSummary(accounts []Account, window, strategy, usedKey string) GroupQuotaTabSummary {
	counts := make(map[int]int)
	matched := 0
	skipped := 0

	for _, account := range accounts {
		if account.Extra == nil {
			if getOpenAIQuotaStrategyForSummary(account.Extra) == strategy {
				skipped++
			}
			continue
		}

		raw, ok := account.Extra[usedKey]
		if ok && raw != nil {
			used := parseExtraFloat64(raw)
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			bucket := bucketOpenAIQuotaUsedPercent(used)
			counts[bucket]++
			matched++
			continue
		}

		if getOpenAIQuotaStrategyForSummary(account.Extra) == strategy {
			skipped++
		}
	}

	bucketCounts := make([]GroupQuotaBucket, 0, len(counts))
	for usedPercent, accountCount := range counts {
		bucketCounts = append(bucketCounts, GroupQuotaBucket{
			UsedPercent:  usedPercent,
			AccountCount: accountCount,
		})
	}
	sort.Slice(bucketCounts, func(i, j int) bool {
		return bucketCounts[i].UsedPercent > bucketCounts[j].UsedPercent
	})

	return GroupQuotaTabSummary{
		Window:              window,
		BucketCounts:        bucketCounts,
		MatchedAccountCount: matched,
		SkippedAccountCount: skipped,
	}
}

func bucketOpenAIQuotaUsedPercent(used float64) int {
	if used >= 100 {
		return 100
	}
	if used <= 0 {
		return 0
	}
	return int(math.Floor(used/10) * 10)
}

func getOpenAIQuotaStrategyForSummary(extra map[string]any) string {
	value, ok := extra["openai_quota_strategy"].(string)
	if !ok {
		return ""
	}
	switch strings.TrimSpace(value) {
	case "prefer_5h", "prefer_7d":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func buildGroupQuotaRefreshTabSummary(accounts []Account, now time.Time) GroupQuotaTabSummary {
	loc := groupQuotaBeijingLocation()
	today := groupQuotaStartOfDay(now.In(loc))
	counts := make([]int, 8)
	matched := 0
	skipped := 0

	for _, account := range accounts {
		resetAt, ok := openAISevenDayResetAt(account.Extra, now)
		if !ok {
			skipped++
			continue
		}

		resetDate := groupQuotaStartOfDay(resetAt.In(loc))
		daysFromNow := int(resetDate.Sub(today).Hours() / 24)
		if daysFromNow < 0 || daysFromNow >= len(counts) {
			skipped++
			continue
		}

		counts[daysFromNow]++
		matched++
	}

	refreshCounts := make([]GroupQuotaRefreshCount, 0, len(counts))
	for dayOffset, accountCount := range counts {
		date := today.AddDate(0, 0, dayOffset)
		refreshCounts = append(refreshCounts, GroupQuotaRefreshCount{
			Date:         date.Format("2006-01-02"),
			DateLabel:    formatChineseDate(date),
			DaysFromNow:  dayOffset,
			AccountCount: accountCount,
		})
	}

	return GroupQuotaTabSummary{
		Window:              "refresh",
		BucketCounts:        []GroupQuotaBucket{},
		RefreshCounts:       refreshCounts,
		MatchedAccountCount: matched,
		SkippedAccountCount: skipped,
	}
}

func openAISevenDayResetAt(extra map[string]any, now time.Time) (time.Time, bool) {
	if len(extra) == 0 {
		return time.Time{}, false
	}

	if raw, ok := extra["codex_7d_reset_at"]; ok && raw != nil {
		resetAt, err := parseTime(fmt.Sprint(raw))
		if err == nil {
			return resetAt, true
		}
	}

	resetAfterSeconds := parseExtraInt(extra["codex_7d_reset_after_seconds"])
	if resetAfterSeconds <= 0 {
		return time.Time{}, false
	}

	base := now
	if updatedAtRaw, ok := extra["codex_usage_updated_at"]; ok && updatedAtRaw != nil {
		if updatedAt, err := parseTime(fmt.Sprint(updatedAtRaw)); err == nil {
			base = updatedAt
		}
	}
	return base.Add(time.Duration(resetAfterSeconds) * time.Second), true
}

func groupQuotaBeijingLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}

func groupQuotaStartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func formatChineseDate(t time.Time) string {
	return fmt.Sprintf("%d年%d月%d日", t.Year(), int(t.Month()), t.Day())
}

func cloneDuplicateAccountValuePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// Redeem code management implementations
func (s *adminServiceImpl) ListRedeemCodes(ctx context.Context, page, pageSize int, codeType, status, search string, sortBy, sortOrder string) ([]RedeemCode, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	codes, result, err := s.redeemCodeRepo.ListWithFilters(ctx, params, codeType, status, search)
	if err != nil {
		return nil, 0, err
	}
	return codes, result.Total, nil
}

func (s *adminServiceImpl) GetRedeemCode(ctx context.Context, id int64) (*RedeemCode, error) {
	return s.redeemCodeRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GenerateRedeemCodes(ctx context.Context, input *GenerateRedeemCodesInput) ([]RedeemCode, error) {
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return nil, ErrRedeemCodeExpired
	}

	// 如果是订阅类型，验证必须有 GroupID
	if input.Type == RedeemTypeSubscription {
		if input.GroupID == nil {
			return nil, errors.New("group_id is required for subscription type")
		}
		// 验证分组存在且为订阅类型
		group, err := s.groupRepo.GetByID(ctx, *input.GroupID)
		if err != nil {
			return nil, fmt.Errorf("group not found: %w", err)
		}
		if !group.IsSubscriptionType() {
			return nil, errors.New("group must be subscription type")
		}
	}

	codes := make([]RedeemCode, 0, input.Count)
	for i := 0; i < input.Count; i++ {
		codeValue, err := GenerateRedeemCode()
		if err != nil {
			return nil, err
		}
		code := RedeemCode{
			Code:      codeValue,
			Type:      input.Type,
			Value:     input.Value,
			Status:    StatusUnused,
			ExpiresAt: input.ExpiresAt,
		}
		// 订阅类型专用字段
		if input.Type == RedeemTypeSubscription {
			code.GroupID = input.GroupID
			code.ValidityDays = input.ValidityDays
			if code.ValidityDays <= 0 {
				code.ValidityDays = 30 // 默认30天
			}
		}
		if err := s.redeemCodeRepo.Create(ctx, &code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func (s *adminServiceImpl) DeleteRedeemCode(ctx context.Context, id int64) error {
	return s.redeemCodeRepo.Delete(ctx, id)
}

func (s *adminServiceImpl) BatchDeleteRedeemCodes(ctx context.Context, ids []int64) (int64, error) {
	var deleted int64
	for _, id := range ids {
		if err := s.redeemCodeRepo.Delete(ctx, id); err == nil {
			deleted++
		}
	}
	return deleted, nil
}

func (s *adminServiceImpl) ExpireRedeemCode(ctx context.Context, id int64) (*RedeemCode, error) {
	code, err := s.redeemCodeRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	code.Status = StatusExpired
	if err := s.redeemCodeRepo.Update(ctx, code); err != nil {
		return nil, err
	}
	return code, nil
}
