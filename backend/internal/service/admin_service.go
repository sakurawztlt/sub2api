package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/ent/authidentitychannel"
	"github.com/Wei-Shaw/sub2api/ent/usagelog"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/httputil"
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
	Name                      string
	Description               string
	Platform                  string
	RateMultiplier            float64
	IsExclusive               bool
	OperatorUserID            *int64
	SubscriptionType          string   // standard/subscription
	DailyLimitUSD             *float64 // 日限额 (USD)
	WeeklyLimitUSD            *float64 // 周限额 (USD)
	MonthlyLimitUSD           *float64 // 月限额 (USD)
	LongContextPricingEnabled bool
	ModelPricing              []ChannelModelPricing
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
	DefaultMappedModel          string
	RequireOAuthOnly            bool
	RequirePrivacySet           bool
	MessagesDispatchModelConfig OpenAIMessagesDispatchModelConfig
	ModelsListConfig            GroupModelsListConfig
	// RPMLimit 分组 RPM 上限（0 = 不限制）
	RPMLimit int
	// MaxReasoningEffort OpenAI/Codex 请求的推理强度上限，空字符串表示不限制。
	MaxReasoningEffort string
	// ReasoningEffortMappings OpenAI/Codex 推理强度精确映射。
	ReasoningEffortMappings []ReasoningEffortMapping
	// 从指定分组复制账号（创建分组后在同一事务内绑定）
	CopyAccountsFromGroupIDs []int64
}

type UpdateGroupInput struct {
	Name                      string
	Description               *string
	Platform                  string
	RateMultiplier            *float64 // 使用指针以支持设置为0
	IsExclusive               *bool
	Status                    string
	SubscriptionType          string   // standard/subscription
	DailyLimitUSD             *float64 // 日限额 (USD)
	WeeklyLimitUSD            *float64 // 周限额 (USD)
	MonthlyLimitUSD           *float64 // 月限额 (USD)
	LongContextPricingEnabled *bool
	ModelPricing              *[]ChannelModelPricing
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
	DefaultMappedModel          *string
	RequireOAuthOnly            *bool
	RequirePrivacySet           *bool
	MessagesDispatchModelConfig *OpenAIMessagesDispatchModelConfig
	ModelsListConfig            *GroupModelsListConfig
	// RPMLimit 分组 RPM 上限（0 = 不限制），nil 表示未提供不改动。
	RPMLimit *int
	// MaxReasoningEffort 空字符串表示清除上限；nil 表示未提供不改动。
	MaxReasoningEffort *string
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

type UpdateProxyInput struct {
	Name           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	Status         string
	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
}

type GenerateRedeemCodesInput struct {
	Count        int
	Type         string
	Value        float64
	GroupID      *int64 // 订阅类型专用：关联的分组ID
	ValidityDays int    // 订阅类型专用：有效天数
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
	userRepo             UserRepository
	groupRepo            GroupRepository
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
	accountDuplicateRepo, _ := accountRepo.(AccountDuplicateRepository)
	return &adminServiceImpl{
		userRepo:             userRepo,
		groupRepo:            groupRepo,
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

// Group management implementations
func (s *adminServiceImpl) ListGroups(ctx context.Context, page, pageSize int, platform, status, search string, isExclusive *bool, sortBy, sortOrder string) ([]Group, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	groups, result, err := s.groupRepo.ListWithFilters(ctx, params, platform, status, search, isExclusive)
	if err != nil {
		return nil, 0, err
	}
	return groups, result.Total, nil
}

func (s *adminServiceImpl) GetAllGroups(ctx context.Context) ([]Group, error) {
	return s.groupRepo.ListActive(ctx)
}

func (s *adminServiceImpl) GetAllGroupsByPlatform(ctx context.Context, platform string) ([]Group, error) {
	return s.groupRepo.ListActiveByPlatform(ctx, platform)
}

func (s *adminServiceImpl) GetAllGroupsIncludingInactive(ctx context.Context) ([]Group, error) {
	// ListWithFilters with empty status = no status filter, so active + disabled groups are returned.
	// PageSize 10000 is intentionally large; group count is O(dozens) in practice.
	groups, _, err := s.groupRepo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10000}, "", "", "", nil)
	return groups, err
}

func (s *adminServiceImpl) GetGroup(ctx context.Context, id int64) (*Group, error) {
	return s.groupRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error) {
	platform = strings.TrimSpace(platform)
	if id > 0 {
		group, err := s.groupRepo.GetByIDLite(ctx, id)
		if err != nil {
			return nil, err
		}
		if platform == "" {
			platform = group.Platform
		}
	}
	if platform == "" {
		platform = PlatformAnthropic
	}

	candidates := defaultModelsListCandidateIDs(platform)
	if id <= 0 || s.accountRepo == nil {
		return candidates, nil
	}

	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, id)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, model := range candidates {
		seen[model] = struct{}{}
	}
	for _, acc := range accounts {
		if platform == PlatformComposite {
			if !isConcreteRequestPlatform(acc.Platform) {
				continue
			}
		} else if acc.Platform != platform {
			continue
		}
		for model := range acc.GetModelMapping() {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			candidates = append(candidates, model)
		}
	}
	return candidates, nil
}

func (s *adminServiceImpl) ListCompositeRoutes(ctx context.Context, groupID int64) ([]CompositeModelRoute, error) {
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	return s.compositeRouteRepo.ListByGroup(ctx, groupID, true)
}

func (s *adminServiceImpl) CreateCompositeRoute(ctx context.Context, groupID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	route, err := compositeRouteFromInput(groupID, input)
	if err != nil {
		return nil, err
	}
	if err := s.compositeRouteRepo.Create(ctx, route); err != nil {
		return nil, err
	}
	return route, nil
}

func (s *adminServiceImpl) UpdateCompositeRoute(ctx context.Context, groupID, routeID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	if ok, err := s.compositeRouteBelongsToGroup(ctx, groupID, routeID); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrCompositeRouteNotFound
	}
	route, err := compositeRouteFromInput(groupID, input)
	if err != nil {
		return nil, err
	}
	route.ID = routeID
	if err := s.compositeRouteRepo.Update(ctx, route); err != nil {
		return nil, err
	}
	return route, nil
}

func (s *adminServiceImpl) DeleteCompositeRoute(ctx context.Context, groupID, routeID int64) error {
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return err
	}
	if s.compositeRouteRepo == nil {
		return fmt.Errorf("composite route repository is not configured")
	}
	if ok, err := s.compositeRouteBelongsToGroup(ctx, groupID, routeID); err != nil {
		return err
	} else if !ok {
		return ErrCompositeRouteNotFound
	}
	return s.compositeRouteRepo.Delete(ctx, routeID)
}

func (s *adminServiceImpl) PreviewCompositeRoute(ctx context.Context, groupID int64, input CompositeRoutePreviewRequest) (*CompositeRouteDecision, error) {
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	resolver := s.compositeResolver
	if resolver == nil {
		resolver = NewCompositeRouteResolver(s.compositeRouteRepo)
	}
	decision, err := resolver.Resolve(ctx, groupID, input.Model, input.Endpoint)
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

func (s *adminServiceImpl) requireCompositeGroup(ctx context.Context, groupID int64) error {
	group, err := s.groupRepo.GetByIDLite(ctx, groupID)
	if err != nil {
		return err
	}
	if group.Platform != PlatformComposite {
		return fmt.Errorf("group %d is not a composite group", groupID)
	}
	return nil
}

func (s *adminServiceImpl) compositeRouteBelongsToGroup(ctx context.Context, groupID, routeID int64) (bool, error) {
	routes, err := s.compositeRouteRepo.ListByGroup(ctx, groupID, true)
	if err != nil {
		return false, err
	}
	for i := range routes {
		if routes[i].ID == routeID {
			return true, nil
		}
	}
	return false, nil
}

func compositeRouteFromInput(groupID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	input = normalizeCompositeRouteInput(input)
	if input.PublicModel == "" {
		return nil, fmt.Errorf("public_model is required")
	}
	if !isConcreteRequestPlatform(input.TargetPlatform) {
		return nil, fmt.Errorf("target_platform must be a concrete provider")
	}
	if input.Priority == 0 {
		input.Priority = 100
	}
	return &CompositeModelRoute{
		GroupID:        groupID,
		PublicModel:    input.PublicModel,
		MatchType:      input.MatchType,
		TargetPlatform: input.TargetPlatform,
		UpstreamModel:  input.UpstreamModel,
		Endpoint:       input.Endpoint,
		Priority:       input.Priority,
		Enabled:        input.Enabled,
		Notes:          input.Notes,
	}, nil
}

func defaultModelsListCandidateIDs(platform string) []string {
	switch platform {
	case PlatformOpenAI:
		return openai.DefaultModelIDs()
	case PlatformGemini:
		ids := make([]string, 0, len(geminicli.DefaultModels))
		for _, model := range geminicli.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformAntigravity:
		models := antigravity.DefaultModels()
		ids := make([]string, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformGrok:
		return xai.DefaultModelIDs()
	case PlatformComposite:
		return compositeDefaultModelsListCandidateIDs()
	default:
		ids := make([]string, 0, len(claude.DefaultModels))
		for _, model := range claude.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	}
}

func defaultAllowImageGenerationForPlatform(platform string) bool {
	// Grok image and video generation routes share the legacy image-generation gate.
	// Older clients send the false zero value, so Grok groups must default enabled.
	return platform == PlatformGrok
}

func compositeDefaultModelsListCandidateIDs() []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek} {
		for _, id := range defaultModelsListCandidateIDs(platform) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

func canCopyAccountsFromGroupPlatform(targetPlatform, sourcePlatform string) bool {
	if targetPlatform == PlatformComposite {
		return sourcePlatform == PlatformComposite || isConcreteRequestPlatform(sourcePlatform)
	}
	return sourcePlatform == targetPlatform
}

func groupSupportsOAuthOnlyFilter(platform string) bool {
	return platform == PlatformOpenAI ||
		platform == PlatformAntigravity ||
		platform == PlatformAnthropic ||
		platform == PlatformGemini ||
		platform == PlatformGrok ||
		platform == PlatformComposite
}

func (s *adminServiceImpl) CreateGroup(ctx context.Context, input *CreateGroupInput) (*Group, error) {
	if input.RateMultiplier <= 0 {
		return nil, errors.New("rate_multiplier must be > 0")
	}

	platform := NormalizeGroupPlatform(input.Platform)
	modelPricing, err := normalizeGroupModelPricing(platform, input.ModelPricing)
	if err != nil {
		return nil, err
	}
	maxReasoningEffort, err := normalizeMaxReasoningEffortForPlatform(platform, input.MaxReasoningEffort)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT", "%v", err)
	}
	reasoningEffortMappings, err := NormalizeReasoningEffortMappings(platform, input.ReasoningEffortMappings)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REASONING_EFFORT_MAPPING", "%v", err)
	}

	subscriptionType := input.SubscriptionType
	if subscriptionType == "" {
		subscriptionType = SubscriptionTypeStandard
	}

	// 限额字段：nil/负数 表示"无限制"，0 表示"不允许用量"，正数表示具体限额
	dailyLimit := normalizeLimit(input.DailyLimitUSD)
	weeklyLimit := normalizeLimit(input.WeeklyLimitUSD)
	monthlyLimit := normalizeLimit(input.MonthlyLimitUSD)

	// 图片价格：负数表示清除（使用默认价格），0 保留（表示免费）
	imagePrice1K := normalizePrice(input.ImagePrice1K)
	imagePrice2K := normalizePrice(input.ImagePrice2K)
	imagePrice4K := normalizePrice(input.ImagePrice4K)
	videoPrice480P := normalizePrice(input.VideoPrice480P)
	videoPrice720P := normalizePrice(input.VideoPrice720P)
	videoPrice1080P := normalizePrice(input.VideoPrice1080P)
	searchPricePer1k := normalizePrice(input.SearchPricePer1k)
	audioRealtimePricePerMin := normalizePrice(input.AudioRealtimePricePerMin)
	audioTTSPricePerMillionChars := normalizePrice(input.AudioTTSPricePerMillionChars)
	audioSTTPricePerHour := normalizePrice(input.AudioSTTPricePerHour)
	imageRateMultiplier := 1.0
	if input.ImageRateMultiplier != nil {
		if *input.ImageRateMultiplier < 0 {
			return nil, errors.New("image_rate_multiplier must be >= 0")
		}
		imageRateMultiplier = *input.ImageRateMultiplier
	}
	videoRateMultiplier := 1.0
	if input.VideoRateMultiplier != nil {
		if *input.VideoRateMultiplier < 0 {
			return nil, errors.New("video_rate_multiplier must be >= 0")
		}
		videoRateMultiplier = *input.VideoRateMultiplier
	}

	peakRateMultiplier := 1.0
	if input.PeakRateMultiplier != nil {
		peakRateMultiplier = *input.PeakRateMultiplier
	}
	// 先归一化（非订阅分组清空高峰配置、清洗停用状态下的脏字段）再校验，与 UpdateGroup 同一收口。
	peakRateEnabled, peakStart, peakEnd, peakRateMultiplier := NormalizePeakRateConfig(subscriptionType, input.PeakRateEnabled, input.PeakStart, input.PeakEnd, peakRateMultiplier)
	if err := ValidatePeakRateConfig(subscriptionType, peakRateEnabled, peakStart, peakEnd, peakRateMultiplier); err != nil {
		return nil, err
	}

	profitMinMargin := 0.0
	if input.ProfitMinMargin != nil {
		profitMinMargin = *input.ProfitMinMargin
	}
	profitSafetyBuffer := 0.0
	if input.ProfitSafetyBuffer != nil {
		profitSafetyBuffer = *input.ProfitSafetyBuffer
	}
	// 利润控制与高峰倍率同一收口顺序：先按平台归一化（不支持的平台重置），再校验。
	profitControlEnabled, profitMinMargin, profitSafetyBuffer := NormalizeProfitControlConfig(platform, input.ProfitControlEnabled, profitMinMargin, profitSafetyBuffer)
	if err := ValidateProfitControlConfig(platform, profitControlEnabled, profitMinMargin, profitSafetyBuffer); err != nil {
		return nil, err
	}

	// 校验降级分组
	if input.FallbackGroupID != nil {
		if err := s.validateFallbackGroup(ctx, 0, *input.FallbackGroupID); err != nil {
			return nil, err
		}
	}
	fallbackOnInvalidRequest := input.FallbackGroupIDOnInvalidRequest
	if fallbackOnInvalidRequest != nil && *fallbackOnInvalidRequest <= 0 {
		fallbackOnInvalidRequest = nil
	}
	// 校验无效请求兜底分组
	if fallbackOnInvalidRequest != nil {
		if err := s.validateFallbackGroupOnInvalidRequest(ctx, 0, platform, subscriptionType, *fallbackOnInvalidRequest); err != nil {
			return nil, err
		}
	}

	// MCPXMLInject：默认为 true，仅当显式传入 false 时关闭
	mcpXMLInject := true
	if input.MCPXMLInject != nil {
		mcpXMLInject = *input.MCPXMLInject
	}

	allowImageGeneration := input.AllowImageGeneration || defaultAllowImageGenerationForPlatform(platform)

	// 如果指定了复制账号的源分组，先获取账号 ID 列表
	var accountIDsToCopy []int64
	if len(input.CopyAccountsFromGroupIDs) > 0 {
		// 去重源分组 IDs
		seen := make(map[int64]struct{})
		uniqueSourceGroupIDs := make([]int64, 0, len(input.CopyAccountsFromGroupIDs))
		for _, srcGroupID := range input.CopyAccountsFromGroupIDs {
			if _, exists := seen[srcGroupID]; !exists {
				seen[srcGroupID] = struct{}{}
				uniqueSourceGroupIDs = append(uniqueSourceGroupIDs, srcGroupID)
			}
		}

		// 校验源分组的平台是否与新分组一致
		for _, srcGroupID := range uniqueSourceGroupIDs {
			srcGroup, err := s.groupRepo.GetByIDLite(ctx, srcGroupID)
			if err != nil {
				return nil, fmt.Errorf("source group %d not found: %w", srcGroupID, err)
			}
			if !canCopyAccountsFromGroupPlatform(platform, srcGroup.Platform) {
				return nil, fmt.Errorf("source group %d platform mismatch: expected %s, got %s", srcGroupID, platform, srcGroup.Platform)
			}
		}

		// 获取所有源分组的账号（去重）
		var err error
		accountIDsToCopy, err = s.groupRepo.GetAccountIDsByGroupIDs(ctx, uniqueSourceGroupIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get accounts from source groups: %w", err)
		}
	}

	autoGrantCreator := input.IsExclusive &&
		subscriptionType == SubscriptionTypeStandard &&
		input.OperatorUserID != nil &&
		*input.OperatorUserID > 0

	opCtx := ctx
	var tx *dbent.Tx
	if autoGrantCreator && s.entClient != nil {
		var err error
		tx, err = s.entClient.Tx(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		opCtx = dbent.NewTxContext(ctx, tx)
	}

	group := &Group{
		Name:                            input.Name,
		Description:                     input.Description,
		Platform:                        platform,
		RateMultiplier:                  input.RateMultiplier,
		IsExclusive:                     input.IsExclusive,
		Status:                          StatusActive,
		SubscriptionType:                subscriptionType,
		DailyLimitUSD:                   dailyLimit,
		WeeklyLimitUSD:                  weeklyLimit,
		MonthlyLimitUSD:                 monthlyLimit,
		LongContextPricingEnabled:       input.LongContextPricingEnabled,
		ModelPricing:                    modelPricing,
		AllowImageGeneration:            allowImageGeneration,
		ImageRateIndependent:            input.ImageRateIndependent,
		ImageRateMultiplier:             imageRateMultiplier,
		VideoRateIndependent:            input.VideoRateIndependent,
		VideoRateMultiplier:             videoRateMultiplier,
		PeakRateEnabled:                 peakRateEnabled,
		PeakStart:                       peakStart,
		PeakEnd:                         peakEnd,
		PeakRateMultiplier:              peakRateMultiplier,
		ProfitControlEnabled:            profitControlEnabled,
		ProfitMinMargin:                 profitMinMargin,
		ProfitSafetyBuffer:              profitSafetyBuffer,
		ImagePrice1K:                    imagePrice1K,
		ImagePrice2K:                    imagePrice2K,
		ImagePrice4K:                    imagePrice4K,
		VideoPrice480P:                  videoPrice480P,
		VideoPrice720P:                  videoPrice720P,
		VideoPrice1080P:                 videoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(input.VideoModelPrices),
		SearchPricePer1k:                searchPricePer1k,
		AudioRealtimePricePerMin:        audioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    audioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            audioSTTPricePerHour,
		ClaudeCodeOnly:                  input.ClaudeCodeOnly,
		FallbackGroupID:                 input.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: fallbackOnInvalidRequest,
		ModelRouting:                    input.ModelRouting,
		MCPXMLInject:                    mcpXMLInject,
		SupportedModelScopes:            input.SupportedModelScopes,
		AllowMessagesDispatch:           input.AllowMessagesDispatch,
		AllowLive:                       input.AllowLive,
		RequireOAuthOnly:                input.RequireOAuthOnly,
		RequirePrivacySet:               input.RequirePrivacySet,
		DefaultMappedModel:              input.DefaultMappedModel,
		MessagesDispatchModelConfig:     normalizeOpenAIMessagesDispatchModelConfig(input.MessagesDispatchModelConfig),
		ModelsListConfig:                normalizeGroupModelsListConfig(input.ModelsListConfig),
		RPMLimit:                        input.RPMLimit,
		MaxReasoningEffort:              maxReasoningEffort,
		ReasoningEffortMappings:         reasoningEffortMappings,
	}
	sanitizeGroupMessagesDispatchFields(group)
	if group.Platform != PlatformOpenAI && group.Platform != PlatformComposite {
		group.AllowLive = false
	}
	sanitizeGroupReasoningEffortPolicy(group)
	if err := s.groupRepo.Create(opCtx, group); err != nil {
		return nil, err
	}
	if autoGrantCreator {
		if err := s.userRepo.AddGroupToAllowedGroups(opCtx, *input.OperatorUserID, group.ID); err != nil {
			return nil, fmt.Errorf("grant creator access to exclusive group: %w", err)
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit transaction: %w", err)
		}
	}

	// require_oauth_only: 过滤掉 apikey 类型账号
	if group.RequireOAuthOnly && groupSupportsOAuthOnlyFilter(group.Platform) && len(accountIDsToCopy) > 0 {
		accounts, err := s.accountRepo.GetByIDs(ctx, accountIDsToCopy)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch accounts for oauth filter: %w", err)
		}
		oauthIDs := make(map[int64]struct{}, len(accounts))
		for _, acc := range accounts {
			if acc.Type != AccountTypeAPIKey {
				oauthIDs[acc.ID] = struct{}{}
			}
		}
		var filtered []int64
		for _, aid := range accountIDsToCopy {
			if _, ok := oauthIDs[aid]; ok {
				filtered = append(filtered, aid)
			}
		}
		accountIDsToCopy = filtered
	}

	// 如果有需要复制的账号，绑定到新分组
	if len(accountIDsToCopy) > 0 {
		if err := s.groupRepo.BindAccountsToGroup(ctx, group.ID, accountIDsToCopy); err != nil {
			return nil, fmt.Errorf("failed to bind accounts to new group: %w", err)
		}
		group.AccountCount = int64(len(accountIDsToCopy))
	}

	return group, nil
}

// normalizeLimit 将负数转换为 nil（表示无限制），0 保留（表示限额为零）
func normalizeLimit(limit *float64) *float64 {
	if limit == nil || *limit < 0 {
		return nil
	}
	return limit
}

// normalizePrice 将负数转换为 nil（表示使用默认价格），0 保留（表示免费）
func normalizePrice(price *float64) *float64 {
	if price == nil || *price < 0 {
		return nil
	}
	return price
}

// validateFallbackGroup 校验降级分组的有效性
// currentGroupID: 当前分组 ID（新建时为 0）
// fallbackGroupID: 降级分组 ID
func (s *adminServiceImpl) validateFallbackGroup(ctx context.Context, currentGroupID, fallbackGroupID int64) error {
	// 不能将自己设置为降级分组
	if currentGroupID > 0 && currentGroupID == fallbackGroupID {
		return fmt.Errorf("cannot set self as fallback group")
	}

	visited := map[int64]struct{}{}
	nextID := fallbackGroupID
	for {
		if _, seen := visited[nextID]; seen {
			return fmt.Errorf("fallback group cycle detected")
		}
		visited[nextID] = struct{}{}
		if currentGroupID > 0 && nextID == currentGroupID {
			return fmt.Errorf("fallback group cycle detected")
		}

		// 检查降级分组是否存在
		fallbackGroup, err := s.groupRepo.GetByIDLite(ctx, nextID)
		if err != nil {
			return fmt.Errorf("fallback group not found: %w", err)
		}

		// 降级分组不能启用 claude_code_only，否则会造成死循环
		if nextID == fallbackGroupID && fallbackGroup.ClaudeCodeOnly {
			return fmt.Errorf("fallback group cannot have claude_code_only enabled")
		}

		if fallbackGroup.FallbackGroupID == nil {
			return nil
		}
		nextID = *fallbackGroup.FallbackGroupID
	}
}

// validateFallbackGroupOnInvalidRequest 校验无效请求兜底分组的有效性
// currentGroupID: 当前分组 ID（新建时为 0）
// platform/subscriptionType: 当前分组的有效平台/订阅类型
// fallbackGroupID: 兜底分组 ID
func (s *adminServiceImpl) validateFallbackGroupOnInvalidRequest(ctx context.Context, currentGroupID int64, platform, subscriptionType string, fallbackGroupID int64) error {
	if platform != PlatformAnthropic && platform != PlatformAntigravity {
		return fmt.Errorf("invalid request fallback only supported for anthropic or antigravity groups")
	}
	if subscriptionType == SubscriptionTypeSubscription {
		return fmt.Errorf("subscription groups cannot set invalid request fallback")
	}
	if currentGroupID > 0 && currentGroupID == fallbackGroupID {
		return fmt.Errorf("cannot set self as invalid request fallback group")
	}

	fallbackGroup, err := s.groupRepo.GetByIDLite(ctx, fallbackGroupID)
	if err != nil {
		return fmt.Errorf("fallback group not found: %w", err)
	}
	if fallbackGroup.Platform != PlatformAnthropic {
		return fmt.Errorf("fallback group must be anthropic platform")
	}
	if fallbackGroup.SubscriptionType == SubscriptionTypeSubscription {
		return fmt.Errorf("fallback group cannot be subscription type")
	}
	if fallbackGroup.FallbackGroupIDOnInvalidRequest != nil {
		return fmt.Errorf("fallback group cannot have invalid request fallback configured")
	}
	return nil
}

func (s *adminServiceImpl) UpdateGroup(ctx context.Context, id int64, input *UpdateGroupInput) (*Group, error) {
	group, err := s.groupRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	previousPlatform := group.Platform

	if input.Name != "" {
		group.Name = input.Name
	}
	if input.Description != nil {
		group.Description = *input.Description
	}
	if input.Platform != "" {
		group.Platform = input.Platform
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier <= 0 {
			return nil, errors.New("rate_multiplier must be > 0")
		}
		group.RateMultiplier = *input.RateMultiplier
	}
	if input.IsExclusive != nil {
		group.IsExclusive = *input.IsExclusive
	}
	if input.Status != "" {
		group.Status = input.Status
	}
	if input.LongContextPricingEnabled != nil {
		group.LongContextPricingEnabled = *input.LongContextPricingEnabled
	}
	if input.ModelPricing != nil {
		modelPricing, normalizeErr := normalizeGroupModelPricing(group.Platform, *input.ModelPricing)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		group.ModelPricing = modelPricing
	}

	// 订阅相关字段
	if input.SubscriptionType != "" {
		group.SubscriptionType = input.SubscriptionType
	}
	// 限额字段：nil/负数 表示"无限制"，0 表示"不允许用量"，正数表示具体限额
	// 前端始终发送这三个字段，无需 nil 守卫
	group.DailyLimitUSD = normalizeLimit(input.DailyLimitUSD)
	group.WeeklyLimitUSD = normalizeLimit(input.WeeklyLimitUSD)
	group.MonthlyLimitUSD = normalizeLimit(input.MonthlyLimitUSD)
	// 图片生成计费配置：负数表示清除（使用默认价格）
	if input.ImagePrice1K != nil {
		group.ImagePrice1K = normalizePrice(input.ImagePrice1K)
	}
	if input.ImagePrice2K != nil {
		group.ImagePrice2K = normalizePrice(input.ImagePrice2K)
	}
	if input.ImagePrice4K != nil {
		group.ImagePrice4K = normalizePrice(input.ImagePrice4K)
	}
	if input.AllowImageGeneration != nil {
		group.AllowImageGeneration = *input.AllowImageGeneration
	}
	if input.ImageRateIndependent != nil {
		group.ImageRateIndependent = *input.ImageRateIndependent
	}
	if input.ImageRateMultiplier != nil {
		if *input.ImageRateMultiplier < 0 {
			return nil, errors.New("image_rate_multiplier must be >= 0")
		}
		group.ImageRateMultiplier = *input.ImageRateMultiplier
	}
	if input.VideoRateIndependent != nil {
		group.VideoRateIndependent = *input.VideoRateIndependent
	}
	if input.VideoRateMultiplier != nil {
		if *input.VideoRateMultiplier < 0 {
			return nil, errors.New("video_rate_multiplier must be >= 0")
		}
		group.VideoRateMultiplier = *input.VideoRateMultiplier
	}
	if input.PeakRateEnabled != nil {
		group.PeakRateEnabled = *input.PeakRateEnabled
	}
	if input.PeakStart != nil {
		group.PeakStart = *input.PeakStart
	}
	if input.PeakEnd != nil {
		group.PeakEnd = *input.PeakEnd
	}
	if input.PeakRateMultiplier != nil {
		group.PeakRateMultiplier = *input.PeakRateMultiplier
	}
	// 先归一化（非订阅分组——含本次更新转为非订阅——静默清空高峰配置，清洗停用状态下的脏字段），
	// 再收敛校验：Update 可能只传部分 peak 字段，需对合并后的最终配置统一校验，
	// 防止单独修改 start/end 导致最终 start>=end 等非法配置入库。与 CreateGroup 同一收口。
	group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier = NormalizePeakRateConfig(group.SubscriptionType, group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier)
	if err := ValidatePeakRateConfig(group.SubscriptionType, group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier); err != nil {
		return nil, err
	}
	if input.ProfitControlEnabled != nil {
		group.ProfitControlEnabled = *input.ProfitControlEnabled
	}
	if input.ProfitMinMargin != nil {
		group.ProfitMinMargin = *input.ProfitMinMargin
	}
	if input.ProfitSafetyBuffer != nil {
		group.ProfitSafetyBuffer = *input.ProfitSafetyBuffer
	}
	// 按合并后的最终平台归一化（转到不支持平台时静默重置），
	// 再对最终配置统一校验，防止部分字段更新拼出非法组合入库。
	group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer = NormalizeProfitControlConfig(group.Platform, group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer)
	if err := ValidateProfitControlConfig(group.Platform, group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer); err != nil {
		return nil, err
	}
	if input.VideoPrice480P != nil {
		group.VideoPrice480P = normalizePrice(input.VideoPrice480P)
	}
	if input.VideoPrice720P != nil {
		group.VideoPrice720P = normalizePrice(input.VideoPrice720P)
	}
	if input.VideoPrice1080P != nil {
		group.VideoPrice1080P = normalizePrice(input.VideoPrice1080P)
	}
	if input.VideoModelPrices != nil {
		group.VideoModelPrices = NormalizeVideoModelPrices(input.VideoModelPrices)
	}
	if input.SearchPricePer1k != nil {
		group.SearchPricePer1k = normalizePrice(input.SearchPricePer1k)
	}
	if input.AudioRealtimePricePerMin != nil {
		group.AudioRealtimePricePerMin = normalizePrice(input.AudioRealtimePricePerMin)
	}
	if input.AudioTTSPricePerMillionChars != nil {
		group.AudioTTSPricePerMillionChars = normalizePrice(input.AudioTTSPricePerMillionChars)
	}
	if input.AudioSTTPricePerHour != nil {
		group.AudioSTTPricePerHour = normalizePrice(input.AudioSTTPricePerHour)
	}

	// Claude Code 客户端限制
	if input.ClaudeCodeOnly != nil {
		group.ClaudeCodeOnly = *input.ClaudeCodeOnly
	}
	if input.FallbackGroupID != nil {
		// 校验降级分组
		if *input.FallbackGroupID > 0 {
			if err := s.validateFallbackGroup(ctx, id, *input.FallbackGroupID); err != nil {
				return nil, err
			}
			group.FallbackGroupID = input.FallbackGroupID
		} else {
			// 传入 0 或负数表示清除降级分组
			group.FallbackGroupID = nil
		}
	}
	fallbackOnInvalidRequest := group.FallbackGroupIDOnInvalidRequest
	if input.FallbackGroupIDOnInvalidRequest != nil {
		if *input.FallbackGroupIDOnInvalidRequest > 0 {
			fallbackOnInvalidRequest = input.FallbackGroupIDOnInvalidRequest
		} else {
			fallbackOnInvalidRequest = nil
		}
	}
	if fallbackOnInvalidRequest != nil {
		if err := s.validateFallbackGroupOnInvalidRequest(ctx, id, group.Platform, group.SubscriptionType, *fallbackOnInvalidRequest); err != nil {
			return nil, err
		}
	}
	group.FallbackGroupIDOnInvalidRequest = fallbackOnInvalidRequest

	// 模型路由配置
	if input.ModelRouting != nil {
		group.ModelRouting = input.ModelRouting
	}
	if input.ModelRoutingEnabled != nil {
		group.ModelRoutingEnabled = *input.ModelRoutingEnabled
	}
	if input.MCPXMLInject != nil {
		group.MCPXMLInject = *input.MCPXMLInject
	}

	// 支持的模型系列（仅 antigravity 平台使用）
	if input.SupportedModelScopes != nil {
		group.SupportedModelScopes = *input.SupportedModelScopes
	}

	// OpenAI Messages 调度配置
	if input.AllowMessagesDispatch != nil {
		group.AllowMessagesDispatch = *input.AllowMessagesDispatch
	}
	if input.AllowLive != nil {
		group.AllowLive = *input.AllowLive
	}
	if input.RequireOAuthOnly != nil {
		group.RequireOAuthOnly = *input.RequireOAuthOnly
	}
	if input.RequirePrivacySet != nil {
		group.RequirePrivacySet = *input.RequirePrivacySet
	}
	if input.DefaultMappedModel != nil {
		group.DefaultMappedModel = *input.DefaultMappedModel
	}
	if input.MessagesDispatchModelConfig != nil {
		group.MessagesDispatchModelConfig = normalizeOpenAIMessagesDispatchModelConfig(*input.MessagesDispatchModelConfig)
	}
	if input.ModelsListConfig != nil {
		group.ModelsListConfig = normalizeGroupModelsListConfig(*input.ModelsListConfig)
	}
	if input.RPMLimit != nil {
		group.RPMLimit = *input.RPMLimit
	}
	if input.MaxReasoningEffort != nil {
		maxReasoningEffort, err := normalizeMaxReasoningEffortForPlatform(group.Platform, *input.MaxReasoningEffort)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT", "%v", err)
		}
		group.MaxReasoningEffort = maxReasoningEffort
	}
	if input.ReasoningEffortMappings != nil {
		reasoningEffortMappings, err := NormalizeReasoningEffortMappings(group.Platform, *input.ReasoningEffortMappings)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REASONING_EFFORT_MAPPING", "%v", err)
		}
		group.ReasoningEffortMappings = reasoningEffortMappings
	}
	sanitizeGroupMessagesDispatchFields(group)
	if group.Platform != PlatformOpenAI && group.Platform != PlatformComposite {
		group.AllowLive = false
	}
	sanitizeGroupReasoningEffortPolicy(group)

	if err := s.groupRepo.Update(ctx, group); err != nil {
		return nil, err
	}

	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, id)
	}
	if group.Platform != previousPlatform && s.channelCacheInvalidator != nil {
		s.channelCacheInvalidator.InvalidateCache()
	}

	// 如果指定了复制账号的源分组，同步绑定（替换当前分组的账号）
	if len(input.CopyAccountsFromGroupIDs) > 0 {
		// 去重源分组 IDs
		seen := make(map[int64]struct{})
		uniqueSourceGroupIDs := make([]int64, 0, len(input.CopyAccountsFromGroupIDs))
		for _, srcGroupID := range input.CopyAccountsFromGroupIDs {
			// 校验：源分组不能是自身
			if srcGroupID == id {
				return nil, fmt.Errorf("cannot copy accounts from self")
			}
			// 去重
			if _, exists := seen[srcGroupID]; !exists {
				seen[srcGroupID] = struct{}{}
				uniqueSourceGroupIDs = append(uniqueSourceGroupIDs, srcGroupID)
			}
		}

		// 校验源分组的平台是否与当前分组一致
		for _, srcGroupID := range uniqueSourceGroupIDs {
			srcGroup, err := s.groupRepo.GetByIDLite(ctx, srcGroupID)
			if err != nil {
				return nil, fmt.Errorf("source group %d not found: %w", srcGroupID, err)
			}
			if !canCopyAccountsFromGroupPlatform(group.Platform, srcGroup.Platform) {
				return nil, fmt.Errorf("source group %d platform mismatch: expected %s, got %s", srcGroupID, group.Platform, srcGroup.Platform)
			}
		}

		// 获取所有源分组的账号（去重）
		accountIDsToCopy, err := s.groupRepo.GetAccountIDsByGroupIDs(ctx, uniqueSourceGroupIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get accounts from source groups: %w", err)
		}

		// 先清空当前分组的所有账号绑定
		if _, err := s.groupRepo.DeleteAccountGroupsByGroupID(ctx, id); err != nil {
			return nil, fmt.Errorf("failed to clear existing account bindings: %w", err)
		}

		// require_oauth_only: 过滤掉 apikey 类型账号
		if group.RequireOAuthOnly && groupSupportsOAuthOnlyFilter(group.Platform) && len(accountIDsToCopy) > 0 {
			accounts, err := s.accountRepo.GetByIDs(ctx, accountIDsToCopy)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch accounts for oauth filter: %w", err)
			}
			oauthIDs := make(map[int64]struct{}, len(accounts))
			for _, acc := range accounts {
				if acc.Type != AccountTypeAPIKey {
					oauthIDs[acc.ID] = struct{}{}
				}
			}
			var filtered []int64
			for _, aid := range accountIDsToCopy {
				if _, ok := oauthIDs[aid]; ok {
					filtered = append(filtered, aid)
				}
			}
			accountIDsToCopy = filtered
		}

		// 再绑定源分组的账号
		if len(accountIDsToCopy) > 0 {
			if err := s.groupRepo.BindAccountsToGroup(ctx, id, accountIDsToCopy); err != nil {
				return nil, fmt.Errorf("failed to bind accounts to group: %w", err)
			}
		}
	}

	return group, nil
}

func normalizeGroupModelPricing(platform string, pricing []ChannelModelPricing) ([]ChannelModelPricing, error) {
	out := make([]ChannelModelPricing, len(pricing))
	for i := range pricing {
		out[i] = pricing[i].Clone()
		out[i].ID = 0
		out[i].ChannelID = 0
		if out[i].TimePricing != nil {
			if len(out[i].TimePricing.Periods) > 0 {
				return nil, infraerrors.BadRequest("GROUP_MODEL_TIME_PRICING_UNSUPPORTED", "group model pricing does not support time pricing")
			}
			out[i].TimePricing = nil
		}
		if strings.TrimSpace(out[i].Platform) == "" {
			out[i].Platform = platform
		}
		for j := range out[i].Models {
			out[i].Models[j] = strings.TrimSpace(out[i].Models[j])
		}
		if len(out[i].Models) == 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_PRICING_MODELS_REQUIRED", "group model pricing entry requires at least one model")
		}
	}
	if err := validatePricingEntries(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *adminServiceImpl) DeleteGroup(ctx context.Context, id int64) error {
	var groupKeys []string
	if s.authCacheInvalidator != nil {
		keys, err := s.apiKeyRepo.ListKeysByGroupID(ctx, id)
		if err == nil {
			groupKeys = keys
		}
	}

	affectedUserIDs, err := s.groupRepo.DeleteCascade(ctx, id)
	if err != nil {
		return err
	}
	// 注意：user_group_rate_multipliers 表通过外键 ON DELETE CASCADE 自动清理

	// 事务成功后，异步失效受影响用户的订阅缓存
	if len(affectedUserIDs) > 0 && s.billingCacheService != nil {
		groupID := id
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, userID := range affectedUserIDs {
				if err := s.billingCacheService.InvalidateSubscription(cacheCtx, userID, groupID); err != nil {
					logger.LegacyPrintf("service.admin", "invalidate subscription cache failed: user_id=%d group_id=%d err=%v", userID, groupID, err)
				}
			}
		}()
	}
	if s.authCacheInvalidator != nil {
		for _, key := range groupKeys {
			s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, key)
		}
	}

	return nil
}

func (s *adminServiceImpl) GetGroupAPIKeys(ctx context.Context, groupID int64, page, pageSize int) ([]APIKey, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	keys, result, err := s.apiKeyRepo.ListByGroupID(ctx, groupID, params)
	if err != nil {
		return nil, 0, err
	}
	return keys, result.Total, nil
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

func (s *adminServiceImpl) GetGroupRateMultipliers(ctx context.Context, groupID int64) ([]UserGroupRateEntry, error) {
	if s.userGroupRateRepo == nil {
		return nil, nil
	}
	return s.userGroupRateRepo.GetByGroupID(ctx, groupID)
}

func (s *adminServiceImpl) ClearGroupRateMultipliers(ctx context.Context, groupID int64) error {
	if s.userGroupRateRepo == nil {
		return nil
	}
	return s.userGroupRateRepo.DeleteByGroupID(ctx, groupID)
}

func (s *adminServiceImpl) BatchSetGroupRateMultipliers(ctx context.Context, groupID int64, entries []GroupRateMultiplierInput) error {
	if s.userGroupRateRepo == nil {
		return nil
	}
	for _, e := range entries {
		if e.RateMultiplier <= 0 {
			return fmt.Errorf("rate_multiplier must be > 0 (user_id=%d)", e.UserID)
		}
	}
	return s.userGroupRateRepo.SyncGroupRateMultipliers(ctx, groupID, entries)
}

func (s *adminServiceImpl) ClearGroupRPMOverrides(ctx context.Context, groupID int64) error {
	if s.userGroupRateRepo == nil {
		return nil
	}
	if err := s.userGroupRateRepo.ClearGroupRPMOverrides(ctx, groupID); err != nil {
		return err
	}
	// RPM override 已嵌入 auth cache snapshot (v7)，变更后必须失效相关缓存。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
	return nil
}

func (s *adminServiceImpl) BatchSetGroupRPMOverrides(ctx context.Context, groupID int64, entries []GroupRPMOverrideInput) error {
	if s.userGroupRateRepo == nil {
		return nil
	}
	for _, e := range entries {
		if e.RPMOverride != nil && *e.RPMOverride < 0 {
			return infraerrors.BadRequest("INVALID_RPM_OVERRIDE", fmt.Sprintf("rpm_override must be >= 0 (user_id=%d)", e.UserID))
		}
	}
	if err := s.userGroupRateRepo.SyncGroupRPMOverrides(ctx, groupID, entries); err != nil {
		return err
	}
	// RPM override 已嵌入 auth cache snapshot (v7)，变更后必须失效相关缓存。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
	return nil
}

func (s *adminServiceImpl) UpdateGroupSortOrders(ctx context.Context, updates []GroupSortOrderUpdate) error {
	return s.groupRepo.UpdateSortOrders(ctx, updates)
}

// AdminUpdateAPIKeyGroupID 管理员修改 API Key 分组绑定
// groupID: nil=不修改, 指向0=解绑, 指向正整数=绑定到目标分组
func (s *adminServiceImpl) AdminUpdateAPIKeyGroupID(ctx context.Context, keyID int64, groupID *int64) (*AdminUpdateAPIKeyGroupIDResult, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}

	if groupID == nil {
		// nil 表示不修改，直接返回
		return &AdminUpdateAPIKeyGroupIDResult{APIKey: apiKey}, nil
	}

	if *groupID < 0 {
		return nil, infraerrors.BadRequest("INVALID_GROUP_ID", "group_id must be non-negative")
	}

	result := &AdminUpdateAPIKeyGroupIDResult{}

	if *groupID == 0 {
		// 0 表示解绑分组（不修改 user_allowed_groups，避免影响用户其他 Key）
		apiKey.GroupID = nil
		apiKey.Group = nil
	} else {
		// 验证目标分组存在且状态为 active
		group, err := s.groupRepo.GetByID(ctx, *groupID)
		if err != nil {
			return nil, err
		}
		if group.Status != StatusActive {
			return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", "target group is not active")
		}
		// 订阅类型分组：用户须持有该分组的有效订阅才可绑定
		if group.IsSubscriptionType() {
			if s.userSubRepo == nil {
				return nil, infraerrors.InternalServer("SUBSCRIPTION_REPOSITORY_UNAVAILABLE", "subscription repository is not configured")
			}
			if _, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, apiKey.UserID, *groupID); err != nil {
				if errors.Is(err, ErrSubscriptionNotFound) {
					return nil, infraerrors.BadRequest("SUBSCRIPTION_REQUIRED", "user does not have an active subscription for this group")
				}
				return nil, err
			}
		}

		gid := *groupID
		apiKey.GroupID = &gid
		apiKey.Group = group

		// 专属标准分组：使用事务保证「添加分组权限」与「更新 API Key」的原子性
		if group.IsExclusive && !group.IsSubscriptionType() {
			opCtx := ctx
			var tx *dbent.Tx
			if s.entClient == nil {
				logger.LegacyPrintf("service.admin", "Warning: entClient is nil, skipping transaction protection for exclusive group binding")
			} else {
				var txErr error
				tx, txErr = s.entClient.Tx(ctx)
				if txErr != nil {
					return nil, fmt.Errorf("begin transaction: %w", txErr)
				}
				defer func() { _ = tx.Rollback() }()
				opCtx = dbent.NewTxContext(ctx, tx)
			}

			if addErr := s.userRepo.AddGroupToAllowedGroups(opCtx, apiKey.UserID, gid); addErr != nil {
				return nil, fmt.Errorf("add group to user allowed groups: %w", addErr)
			}
			if err := s.apiKeyRepo.Update(opCtx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
				return nil, fmt.Errorf("update api key: %w", err)
			}
			if tx != nil {
				if err := tx.Commit(); err != nil {
					return nil, fmt.Errorf("commit transaction: %w", err)
				}
			}

			result.AutoGrantedGroupAccess = true
			result.GrantedGroupID = &gid
			result.GrantedGroupName = group.Name

			// 失效认证缓存（在事务提交后执行）
			if s.authCacheInvalidator != nil {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
			}

			result.APIKey = apiKey
			return result, nil
		}
	}

	// 非专属分组 / 解绑：无需事务，单步更新即可
	if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}

	// 失效认证缓存
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}

	result.APIKey = apiKey
	return result, nil
}

// AdminResetAPIKeyRateLimitUsage resets all API key rate-limit usage windows.
func (s *adminServiceImpl) AdminResetAPIKeyRateLimitUsage(ctx context.Context, keyID int64) (*APIKey, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}
	apiKey.Usage5h = 0
	apiKey.Usage1d = 0
	apiKey.Usage7d = 0
	apiKey.Window5hStart = nil
	apiKey.Window1dStart = nil
	apiKey.Window7dStart = nil
	if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{RateLimitUsage: true}); err != nil {
		return nil, fmt.Errorf("reset api key rate limit usage: %w", err)
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}
	if s.billingCacheService != nil {
		_ = s.billingCacheService.InvalidateAPIKeyRateLimit(ctx, apiKey.ID)
	}
	return apiKey, nil
}

// ReplaceUserGroup 替换用户的专属分组
func (s *adminServiceImpl) ReplaceUserGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (*ReplaceUserGroupResult, error) {
	if oldGroupID == newGroupID {
		return nil, infraerrors.BadRequest("SAME_GROUP", "old and new group must be different")
	}

	// 验证新分组存在且为活跃的专属标准分组
	newGroup, err := s.groupRepo.GetByID(ctx, newGroupID)
	if err != nil {
		return nil, err
	}
	if newGroup.Status != StatusActive {
		return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", "target group is not active")
	}
	if !newGroup.IsExclusive {
		return nil, infraerrors.BadRequest("GROUP_NOT_EXCLUSIVE", "target group is not exclusive")
	}
	if newGroup.IsSubscriptionType() {
		return nil, infraerrors.BadRequest("GROUP_IS_SUBSCRIPTION", "subscription groups are not supported for replacement")
	}

	// 事务保证原子性
	if s.entClient == nil {
		return nil, fmt.Errorf("entClient is nil, cannot perform group replacement")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	opCtx := dbent.NewTxContext(ctx, tx)

	// 1. 授予新分组权限
	if err := s.userRepo.AddGroupToAllowedGroups(opCtx, userID, newGroupID); err != nil {
		return nil, fmt.Errorf("add new group to allowed groups: %w", err)
	}

	// 2. 迁移绑定旧分组的 Key 到新分组
	migrated, err := s.apiKeyRepo.UpdateGroupIDByUserAndGroup(opCtx, userID, oldGroupID, newGroupID)
	if err != nil {
		return nil, fmt.Errorf("migrate api keys: %w", err)
	}

	// 3. 移除旧分组权限
	if err := s.userRepo.RemoveGroupFromUserAllowedGroups(opCtx, userID, oldGroupID); err != nil {
		return nil, fmt.Errorf("remove old group from allowed groups: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// 失效该用户所有 Key 的认证缓存
	if s.authCacheInvalidator != nil {
		keys, keyErr := s.apiKeyRepo.ListKeysByUserID(ctx, userID)
		if keyErr == nil {
			for _, k := range keys {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, k)
			}
		}
	}

	return &ReplaceUserGroupResult{MigratedKeys: migrated}, nil
}

// Account management implementations
func (s *adminServiceImpl) ListAccounts(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode string, sortBy, sortOrder string) ([]Account, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	accounts, result, err := s.accountRepo.ListWithFilters(ctx, params, platform, accountType, status, search, groupID, privacyMode)
	if err != nil {
		return nil, 0, err
	}
	return accounts, result.Total, nil
}

func (s *adminServiceImpl) ListAccountsForSchedulerScoreFilter(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	return s.accountRepo.ListAllWithFilters(ctx, platform, accountType, status, search, groupID, privacyMode)
}

func (s *adminServiceImpl) ListOpenAISchedulableAccountsForSchedulerScore(ctx context.Context, groupID *int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenAI)
	}
	return s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformOpenAI)
}

func (s *adminServiceImpl) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetAccountsByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	if len(ids) == 0 {
		return []*Account{}, nil
	}

	accounts, err := s.accountRepo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to get accounts by IDs: %w", err)
	}

	return accounts, nil
}

const maxAccountNameRunes = 100
const duplicateAccountOperationIDExtraKey = "duplicate_operation_id"

func duplicateAccountName(sourceName string) string {
	const suffix = " (Copy)"
	nameRunes := []rune(strings.TrimSpace(sourceName))
	maxBaseRunes := maxAccountNameRunes - len([]rune(suffix))
	if len(nameRunes) > maxBaseRunes {
		nameRunes = nameRunes[:maxBaseRunes]
	}
	return string(nameRunes) + suffix
}

func cloneAccountJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	cloned := make(map[string]any, len(value))
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

var duplicateAccountDiscardedExtraKeys = map[string]struct{}{
	duplicateAccountOperationIDExtraKey:      {},
	"crs_account_id":                         {},
	"crs_kind":                               {},
	"crs_synced_at":                          {},
	"quota_used":                             {},
	"quota_daily_used":                       {},
	"quota_weekly_used":                      {},
	"quota_daily_start":                      {},
	"quota_weekly_start":                     {},
	"quota_daily_reset_at":                   {},
	"quota_weekly_reset_at":                  {},
	"model_rate_limits":                      {},
	"session_window_utilization":             {},
	"passive_usage_7d_utilization":           {},
	"passive_usage_7d_reset":                 {},
	"passive_usage_7d_oi_utilization":        {},
	"passive_usage_7d_oi_reset":              {},
	"passive_usage_sampled_at":               {},
	"grok_usage_snapshot":                    {},
	"grok_billing_snapshot":                  {},
	"openai_responses_supported":             {},
	"openai_compact_supported":               {},
	"openai_compact_checked_at":              {},
	"openai_compact_last_status":             {},
	"openai_compact_last_error":              {},
	"antigravity_credits_overages":           {},
	"antigravity_force_token_refresh":        {},
	"antigravity_force_token_refresh_at":     {},
	"antigravity_force_token_refresh_reason": {},
	"drive_storage_limit":                    {},
	"drive_storage_usage":                    {},
	"drive_tier_updated_at":                  {},
	// A convergence seed is account identity and must never be copied.
	codexFingerprintSeedExtraKey:           {},
	"codex_primary_used_percent":           {},
	"codex_primary_reset_after_seconds":    {},
	"codex_primary_window_minutes":         {},
	"codex_secondary_used_percent":         {},
	"codex_secondary_reset_after_seconds":  {},
	"codex_secondary_window_minutes":       {},
	"codex_primary_over_secondary_percent": {},
	"codex_usage_updated_at":               {},
	"codex_5h_used_percent":                {},
	"codex_5h_reset_after_seconds":         {},
	"codex_5h_window_minutes":              {},
	"codex_5h_reset_at":                    {},
	"codex_7d_used_percent":                {},
	"codex_7d_reset_after_seconds":         {},
	"codex_7d_window_minutes":              {},
	"codex_7d_reset_at":                    {},
}

func duplicateAccountExtra(value map[string]any) (map[string]any, error) {
	cloned, err := cloneAccountJSONMap(value)
	if err != nil {
		return nil, err
	}
	for key := range duplicateAccountDiscardedExtraKeys {
		delete(cloned, key)
	}
	return cloned, nil
}

func canDuplicateAccountType(accountType string) bool {
	switch accountType {
	case AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount:
		return true
	default:
		return false
	}
}

func duplicateAccountGroups(source *Account) ([]AccountGroup, []int64) {
	if len(source.AccountGroups) > 0 {
		groups := make([]AccountGroup, 0, len(source.AccountGroups))
		groupIDs := make([]int64, 0, len(source.AccountGroups))
		for _, sourceGroup := range source.AccountGroups {
			groups = append(groups, AccountGroup{GroupID: sourceGroup.GroupID, Priority: sourceGroup.Priority})
			groupIDs = append(groupIDs, sourceGroup.GroupID)
		}
		return groups, groupIDs
	}

	groups := make([]AccountGroup, 0, len(source.GroupIDs))
	groupIDs := append([]int64(nil), source.GroupIDs...)
	for i, groupID := range groupIDs {
		groups = append(groups, AccountGroup{GroupID: groupID, Priority: i + 1})
	}
	return groups, groupIDs
}

func duplicateAccountOperationID(sourceID int64, actorScope, operationKey string) string {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" {
		return ""
	}
	actorScope = strings.TrimSpace(actorScope)
	if actorScope == "" {
		actorScope = "admin:0"
	}
	payload := "admin.accounts.duplicate\x00" + actorScope + "\x00" + strconv.FormatInt(sourceID, 10) + "\x00" + operationKey
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest)
}

func (s *adminServiceImpl) findDuplicateByOperationID(ctx context.Context, operationID string) (*Account, error) {
	if operationID == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, duplicateAccountOperationIDExtraKey, operationID)
	if err != nil {
		return nil, fmt.Errorf("find duplicate account operation: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	account := accounts[0]
	return &account, nil
}

func (s *adminServiceImpl) RecoverDuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	return s.findDuplicateByOperationID(ctx, duplicateAccountOperationID(id, actorScope, operationKey))
}

func cloneDuplicateAccountValuePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// DuplicateAccount creates a paused copy while discarding runtime and identity state.
func (s *adminServiceImpl) DuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	operationID := duplicateAccountOperationID(id, actorScope, operationKey)
	existing, err := s.RecoverDuplicateAccount(ctx, id, actorScope, operationKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	source, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if source.IsCredentialShadow() {
		return nil, infraerrors.BadRequest("ACCOUNT_DUPLICATE_SHADOW_UNSUPPORTED", "linked credential shadow accounts cannot be duplicated; duplicate the parent account instead")
	}
	if !canDuplicateAccountType(source.Type) {
		return nil, infraerrors.BadRequest("ACCOUNT_DUPLICATE_CREDENTIAL_TYPE_UNSUPPORTED", "accounts with rotating or unsupported credential types cannot be duplicated")
	}

	credentials, err := cloneAccountJSONMap(source.Credentials)
	if err != nil {
		return nil, fmt.Errorf("clone account credentials: %w", err)
	}
	extra, err := duplicateAccountExtra(source.Extra)
	if err != nil {
		return nil, fmt.Errorf("clone account extra configuration: %w", err)
	}
	if operationID != "" {
		if extra == nil {
			extra = make(map[string]any, 1)
		}
		extra[duplicateAccountOperationIDExtraKey] = operationID
	}

	var expiresAt *int64
	if source.ExpiresAt != nil {
		unix := source.ExpiresAt.Unix()
		expiresAt = &unix
	}
	autoPauseOnExpired := source.AutoPauseOnExpired
	groups, groupIDs := duplicateAccountGroups(source)
	proxyID := source.ProxyID
	if source.ProxyFallbackOriginID != nil {
		proxyID = source.ProxyFallbackOriginID
	}
	input := &CreateAccountInput{
		Name:                  duplicateAccountName(source.Name),
		Notes:                 cloneDuplicateAccountValuePointer(source.Notes),
		Platform:              source.Platform,
		Type:                  source.Type,
		Credentials:           credentials,
		Extra:                 extra,
		ProxyID:               cloneDuplicateAccountValuePointer(proxyID),
		Concurrency:           source.Concurrency,
		Priority:              source.Priority,
		RateMultiplier:        cloneDuplicateAccountValuePointer(source.RateMultiplier),
		LoadFactor:            cloneDuplicateAccountValuePointer(source.LoadFactor),
		GroupIDs:              groupIDs,
		ExpiresAt:             expiresAt,
		AutoPauseOnExpired:    &autoPauseOnExpired,
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	}
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, fmt.Errorf("normalize duplicate account extra: %w", err)
	}
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	duplicate, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	duplicate.Schedulable = false
	if s.accountDuplicateRepo == nil {
		return nil, errors.New("account duplicate repository is not configured")
	}
	if err := s.accountDuplicateRepo.CreateWithAccountGroups(ctx, duplicate, groups); err != nil {
		return nil, fmt.Errorf("create duplicate account: %w", err)
	}
	for i := range groups {
		groups[i].AccountID = duplicate.ID
	}
	duplicate.AccountGroups = groups
	duplicate.GroupIDs = groupIDs
	return duplicate, nil
}

// ValidateOpenAILongContextBillingExtra validates the OpenAI billing flag when present.
func ValidateOpenAILongContextBillingExtra(platform string, extra map[string]any) error {
	if platform != PlatformOpenAI {
		return nil
	}
	raw, exists := extra[openAILongContextBillingEnabledKey]
	if !exists {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return infraerrors.BadRequest("OPENAI_LONG_CONTEXT_BILLING_INVALID", "openai_long_context_billing_enabled must be a boolean")
	}
	return nil
}

func normalizeOpenAILongContextBillingExtra(platform string, extra map[string]any) (map[string]any, error) {
	if platform != PlatformOpenAI {
		return extra, nil
	}
	if err := ValidateOpenAILongContextBillingExtra(platform, extra); err != nil {
		return nil, err
	}
	normalized := maps.Clone(extra)
	if normalized == nil {
		normalized = make(map[string]any, 1)
	}
	if _, exists := normalized[openAILongContextBillingEnabledKey]; !exists {
		normalized[openAILongContextBillingEnabledKey] = false
	}
	return normalized, nil
}

func normalizeOpenAILongContextBillingUpdateExtra(account *Account, input *UpdateAccountInput) (map[string]any, error) {
	normalized, err := normalizeOpenAILongContextBillingExtra(account.Platform, input.Extra)
	if err != nil || account.Platform != PlatformOpenAI {
		return normalized, err
	}
	if _, provided := input.Extra[openAILongContextBillingEnabledKey]; !provided {
		if current, ok := account.Extra[openAILongContextBillingEnabledKey].(bool); ok {
			normalized[openAILongContextBillingEnabledKey] = current
		}
	}
	return normalized, nil
}

func normalizeAccountConcurrency(platform, accountType string, concurrency int) int {
	if platform == PlatformGrok && accountType == AccountTypeOAuth {
		if concurrency <= 0 {
			return 1
		}
	}
	return concurrency
}

func buildAccountForCreate(input *CreateAccountInput, accountExtra map[string]any) (*Account, error) {
	extra := cloneWithoutOllamaCloudUsageManagedExtra(accountExtra)
	// Probe state and snapshots are owned by the dedicated billing-probe
	// service. Never trust generic Extra payloads for a newly-created account.
	delete(extra, UpstreamBillingProbeEnabledExtraKey)
	delete(extra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(extra, UpstreamBillingProbeExtraKey)
	extra = prepareCodexFingerprintExtraForCreate(input.Platform, input.Type, extra)
	account := &Account{
		Name:        input.Name,
		Notes:       normalizeAccountNotes(input.Notes),
		Platform:    input.Platform,
		Type:        input.Type,
		Credentials: input.Credentials,
		Extra:       extra,
		ProxyID:     input.ProxyID,
		Concurrency: normalizeAccountConcurrency(input.Platform, input.Type, input.Concurrency),
		Priority:    input.Priority,
		Status:      StatusActive,
		Schedulable: true,
	}
	if input.ProbeEnabled != nil && *input.ProbeEnabled {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
	}
	if err := ValidateQuotaResetConfig(account.Extra); err != nil {
		return nil, err
	}
	ComputeQuotaResetAt(account.Extra)
	NormalizeFixedQuotaWindows(account.Extra)
	if input.ExpiresAt != nil && *input.ExpiresAt > 0 {
		expiresAt := time.Unix(*input.ExpiresAt, 0)
		account.ExpiresAt = &expiresAt
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	} else {
		account.AutoPauseOnExpired = true
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil && *input.LoadFactor > 0 {
		if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		}
		account.LoadFactor = input.LoadFactor
	}
	return account, nil
}

func (s *adminServiceImpl) CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, err
	}
	accountExtra, err = normalizeGrokMediaEligibilityExtra(input.Platform, accountExtra)
	if err != nil {
		return nil, err
	}
	// 绑定分组
	groupIDs := input.GroupIDs
	// 如果没有指定分组,自动绑定对应平台的默认分组
	if len(groupIDs) == 0 && !input.SkipDefaultGroupBind {
		defaultGroupName := input.Platform + "-default"
		groups, err := s.groupRepo.ListActiveByPlatform(ctx, input.Platform)
		if err == nil {
			for _, g := range groups {
				if g.Name == defaultGroupName {
					groupIDs = []int64{g.ID}
					break
				}
			}
		}
	}

	// 检查混合渠道风险（除非用户已确认）
	if len(groupIDs) > 0 && !input.SkipMixedChannelCheck {
		if err := s.checkMixedChannelRisk(ctx, 0, input.Platform, groupIDs); err != nil {
			return nil, err
		}
	}

	// 校验并规范化请求头覆写配置（header 名小写化、格式检查）。
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	input.Credentials = SanitizeStoredCredentials(input.Platform, input.Credentials)

	account, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	if err := s.accountRepo.Create(ctx, account); err != nil {
		return nil, err
	}

	// 绑定分组
	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, account.ID, groupIDs); err != nil {
			return nil, err
		}
	}

	// OAuth 账号：创建后异步设置隐私。
	// 使用 Ensure（幂等）而非 Force：新建账号 Extra 为空时效果相同，但更安全。
	if account.Type == AccountTypeOAuth {
		switch account.Platform {
		case PlatformOpenAI:
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("create_account_openai_privacy_panic", "account_id", account.ID, "recover", r)
					}
				}()
				s.EnsureOpenAIPrivacy(context.Background(), account)
			}()
		case PlatformAntigravity:
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("create_account_antigravity_privacy_panic", "account_id", account.ID, "recover", r)
					}
				}()
				s.EnsureAntigravityPrivacy(context.Background(), account)
			}()
		}
	}

	return account, nil
}

func (s *adminServiceImpl) UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if account.IsCredentialShadow() {
		if !isAllowedSparkShadowCredentialsUpdate(input.Credentials) {
			return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_NO_CREDENTIALS",
				"spark shadow accounts do not hold auth credentials; only model mapping can be configured on the shadow account")
		}
		if input.Type != "" && input.Type != account.Type {
			return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_IMMUTABLE_TYPE",
				"spark shadow account type cannot be changed; it must remain an OpenAI OAuth shadow")
		}
	} else if input.Type != "" && input.Type != account.Type && input.Type != AccountTypeOAuth {
		shadows, listErr := s.accountRepo.ListShadowsByParent(ctx, id)
		if listErr != nil {
			return nil, listErr
		}
		if len(shadows) > 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "SPARK_SHADOW_PARENT_IMMUTABLE_TYPE",
				"cannot change account type while it has a spark shadow; delete the shadow first")
		}
	}
	var normalizedExtra map[string]any
	if input.Extra != nil {
		normalizedExtra, err = normalizeOpenAILongContextBillingUpdateExtra(account, input)
		if err != nil {
			return nil, err
		}
		normalizedExtra, err = normalizeGrokMediaEligibilityUpdateExtra(account, input, normalizedExtra)
		if err != nil {
			return nil, err
		}
	}
	wasOveragesEnabled := account.IsOveragesEnabled()
	previousProbeIdentity := upstreamBillingProbeIdentity(account)
	previousOllamaUsageIdentity := ollamaCloudUsageIdentity(account)
	previousExtra := account.Extra
	requestedProbeEnabledUpdate := input.ProbeEnabled
	requestedRateSyncEnabledUpdate := input.RateSyncEnabled
	if input.Extra != nil {
		if rawEnabled, ok := input.Extra[UpstreamBillingProbeEnabledExtraKey]; ok {
			enabled, valid := rawEnabled.(bool)
			if !valid {
				return nil, infraerrors.BadRequest(
					"INVALID_UPSTREAM_BILLING_PROBE_ENABLED",
					"upstream_billing_probe_enabled must be a boolean",
				)
			}
			if requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate != enabled {
				return nil, infraerrors.BadRequest(
					"CONFLICTING_UPSTREAM_BILLING_PROBE_ENABLED",
					"conflicting upstream_billing_probe_enabled values",
				)
			}
			requestedProbeEnabledUpdate = &enabled
		}
	}

	if input.Name != "" {
		account.Name = input.Name
	}
	if input.Type != "" {
		account.Type = input.Type
	}
	if input.Notes != nil {
		account.Notes = normalizeAccountNotes(input.Notes)
	}
	if account.IsCredentialShadow() && input.Credentials != nil {
		account.Credentials = sanitizeSparkShadowCredentials(input.Credentials)
	} else if len(input.Credentials) > 0 {
		// 敏感子键采用"incoming 没提供就保留"的合并语义：前端响应已脱敏，
		// 全对象 PUT 编辑时不会再带回 token，避免覆盖时清空已有凭证。
		account.Credentials = MergePreservingSensitiveCreds(account.Credentials, input.Credentials)
		// 校验并规范化请求头覆写配置（header 名小写化、格式检查）
		if err := NormalizeHeaderOverrideCredentials(account.Credentials); err != nil {
			return nil, err
		}
		account.Credentials = SanitizeStoredCredentials(account.Platform, account.Credentials)
	}
	// Extra 使用 map：需要区分“未提供(nil)”与“显式清空({})”。
	// 关闭配额限制时前端会删除 quota_* 键并提交 extra:{}，此时也必须落库。
	if input.Extra != nil {
		inputExtra := cloneWithoutOllamaCloudUsageManagedExtra(normalizedExtra)
		delete(inputExtra, UpstreamBillingProbeEnabledExtraKey)
		delete(inputExtra, UpstreamBillingRateSyncEnabledExtraKey)
		delete(inputExtra, UpstreamBillingProbeExtraKey)
		// 保留配额用量字段，防止编辑账号时意外重置
		for _, key := range []string{
			"quota_used",
			"quota_daily_used",
			"quota_daily_start",
			"quota_weekly_used",
			"quota_weekly_start",
			UpstreamBillingProbeEnabledExtraKey,
			UpstreamBillingRateSyncEnabledExtraKey,
			UpstreamBillingProbeExtraKey,
		} {
			if v, ok := account.Extra[key]; ok {
				inputExtra[key] = v
			}
		}
		account.Extra = prepareCodexFingerprintExtraForUpdate(account, inputExtra)
		if account.Platform == PlatformAntigravity && wasOveragesEnabled && !account.IsOveragesEnabled() {
			delete(account.Extra, "antigravity_credits_overages") // 清理旧版 overages 运行态
			// 清除 AICredits 限流 key
			if rawLimits, ok := account.Extra[modelRateLimitsKey].(map[string]any); ok {
				delete(rawLimits, creditsExhaustedKey)
			}
		}
		if account.Platform == PlatformAntigravity && !wasOveragesEnabled && account.IsOveragesEnabled() {
			delete(account.Extra, modelRateLimitsKey)
			delete(account.Extra, "antigravity_credits_overages") // 清理旧版 overages 运行态
		}
		// 校验并预计算固定时间重置的下次重置时间
		if err := ValidateQuotaResetConfig(account.Extra); err != nil {
			return nil, err
		}
		ComputeQuotaResetAt(account.Extra)
		NormalizeFixedQuotaWindows(account.Extra)
	}
	if input.Extra == nil {
		account.Extra = prepareCodexFingerprintExtraForUpdate(account, account.Extra)
	}
	if requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate {
		if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
			return nil, infraerrors.BadRequest(
				"UPSTREAM_BILLING_RATE_SYNC_REQUIRES_PROBE",
				"upstream billing rate sync requires upstream billing probe",
			)
		}
		enabled := true
		requestedProbeEnabledUpdate = &enabled
	}
	if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
		disabled := false
		requestedRateSyncEnabledUpdate = &disabled
	}
	if (requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate) ||
		(requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate) {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
	}
	if account.Extra == nil && (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) {
		account.Extra = make(map[string]any)
	}
	if requestedProbeEnabledUpdate != nil {
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
	}
	if requestedRateSyncEnabledUpdate != nil {
		account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
	}
	if input.ProxyID != nil && !account.IsCredentialShadow() {
		// 0 表示清除代理（前端发送 0 而不是 null 来表达清除意图）
		if *input.ProxyID == 0 {
			account.ProxyID = nil
		} else {
			account.ProxyID = input.ProxyID
		}
		account.Proxy = nil // 清除关联对象，防止 GORM Save 时根据 Proxy.ID 覆盖 ProxyID
	}
	if !reflect.DeepEqual(previousProbeIdentity, upstreamBillingProbeIdentity(account)) && account.Extra != nil {
		delete(account.Extra, UpstreamBillingProbeExtraKey)
		if !isUpstreamBillingProbeAccount(account) {
			delete(account.Extra, UpstreamBillingProbeEnabledExtraKey)
			delete(account.Extra, UpstreamBillingRateSyncEnabledExtraKey)
		}
	}
	// 只在指针非 nil 时更新 Concurrency（支持设置为 0）
	if input.Concurrency != nil {
		account.Concurrency = normalizeAccountConcurrency(account.Platform, account.Type, *input.Concurrency)
	}
	// 只在指针非 nil 时更新 Priority（支持设置为 0）
	if input.Priority != nil {
		account.Priority = *input.Priority
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		if upstreamBillingRateSyncEnabled(account) {
			return nil, ErrUpstreamBillingRateSyncConflict
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			account.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			account.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		account.Status = input.Status
	}
	if input.ExpiresAt != nil {
		if *input.ExpiresAt <= 0 {
			account.ExpiresAt = nil
		} else {
			expiresAt := time.Unix(*input.ExpiresAt, 0)
			account.ExpiresAt = &expiresAt
		}
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	if previousOllamaUsageIdentity != nil &&
		reflect.DeepEqual(previousOllamaUsageIdentity, ollamaCloudUsageIdentity(account)) {
		preserveOllamaCloudUsageManagedExtra(previousExtra, account.Extra)
	} else {
		for _, key := range ollamaCloudUsageManagedExtraKeys {
			delete(account.Extra, key)
		}
	}

	// 先验证分组是否存在（在任何写操作之前）
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}

		// 检查混合渠道风险（除非用户已确认）
		if !input.SkipMixedChannelCheck {
			if err := s.checkMixedChannelRisk(ctx, account.ID, account.Platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	billingSettingsAppliedAtomically := false
	if updater, ok := s.accountRepo.(AccountBillingSettingsRepository); ok {
		if err := updater.UpdateWithAccountBillingSettings(
			ctx,
			account,
			requestedProbeEnabledUpdate,
			requestedRateSyncEnabledUpdate,
			input.RateMultiplier,
		); err != nil {
			return nil, err
		}
		billingSettingsAppliedAtomically = true
	}
	if !billingSettingsAppliedAtomically {
		if err := s.accountRepo.Update(ctx, account); err != nil {
			return nil, err
		}
		if (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) &&
			isUpstreamBillingProbeAccount(account) {
			settings := make(map[string]any, 2)
			if requestedProbeEnabledUpdate != nil {
				settings[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
			}
			if requestedRateSyncEnabledUpdate != nil {
				settings[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
			}
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, settings); err != nil {
				return nil, err
			}
		}
	}

	if input.ProxyID != nil && !account.IsCredentialShadow() {
		if err := s.propagateProxyToShadows(ctx, id, account.ProxyID); err != nil {
			return nil, err
		}
	}

	// 绑定分组
	if input.GroupIDs != nil {
		if err := s.accountRepo.BindGroups(ctx, account.ID, *input.GroupIDs); err != nil {
			return nil, err
		}
	}

	// 重新查询以确保返回完整数据（包括正确的 Proxy 关联对象）
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdateAccountExtra 仅对 Extra JSONB 做 key 级合并，避免覆盖其它运行态键
// （如 model_rate_limits / passive_usage_* 等）。
func (s *adminServiceImpl) UpdateAccountExtra(ctx context.Context, id int64, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	filtered := cloneWithoutOllamaCloudUsageManagedExtra(sanitizedCodexFingerprintExtraUpdates(updates))
	delete(filtered, UpstreamBillingProbeEnabledExtraKey)
	delete(filtered, UpstreamBillingRateSyncEnabledExtraKey)
	delete(filtered, UpstreamBillingProbeExtraKey)
	if _, exists := filtered[openAILongContextBillingEnabledKey]; exists {
		account, err := s.accountRepo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if err := ValidateOpenAILongContextBillingExtra(account.Platform, filtered); err != nil {
			return err
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return s.accountRepo.UpdateExtra(ctx, id, filtered)
}

// BulkUpdateAccounts updates multiple accounts in one request.
// It merges credentials/extra keys instead of overwriting the whole object.
func (s *adminServiceImpl) BulkUpdateAccounts(ctx context.Context, input *BulkUpdateAccountsInput) (*BulkUpdateAccountsResult, error) {
	// Generic JSONB updates must not inject service-owned settings or snapshots.
	input.Extra = sanitizedCodexFingerprintExtraUpdates(input.Extra)
	managedExtra := cloneWithoutOllamaCloudUsageManagedExtra(input.Extra)
	delete(managedExtra, UpstreamBillingProbeEnabledExtraKey)
	delete(managedExtra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(managedExtra, UpstreamBillingProbeExtraKey)
	input.Extra = managedExtra

	if len(input.AccountIDs) == 0 && input.Filters != nil {
		accountIDs, err := s.resolveBulkUpdateTargetIDs(ctx, input.Filters)
		if err != nil {
			return nil, err
		}
		input.AccountIDs = accountIDs
	}

	result := &BulkUpdateAccountsResult{
		SuccessIDs: make([]int64, 0, len(input.AccountIDs)),
		FailedIDs:  make([]int64, 0, len(input.AccountIDs)),
		Results:    make([]BulkUpdateAccountResult, 0, len(input.AccountIDs)),
	}

	if len(input.AccountIDs) == 0 {
		return result, nil
	}
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
	}
	openAISettings, err := normalizeBulkOpenAISettings(input)
	if err != nil {
		return nil, err
	}

	needMixedChannelCheck := input.GroupIDs != nil && !input.SkipMixedChannelCheck
	var cachedTargets []*Account
	if len(input.Credentials) > 0 || input.ProxyID != nil || needMixedChannelCheck || openAISettings.any() || input.ProbeEnabled != nil || input.RateMultiplier != nil {
		accounts, err := s.accountRepo.GetByIDs(ctx, input.AccountIDs)
		if err != nil {
			return nil, err
		}
		cachedTargets = accounts
	}
	targetsByID := make(map[int64]*Account, len(cachedTargets))
	for _, account := range cachedTargets {
		if account != nil {
			targetsByID[account.ID] = account
		}
	}
	if openAISettings.any() {
		inheritedCount, err := validateBulkOpenAISettingsTargets(input, openAISettings, targetsByID)
		if err != nil {
			return nil, err
		}
		result.LongContextInheritedCount = inheritedCount
	}
	if input.ProbeEnabled != nil {
		for _, accountID := range input.AccountIDs {
			account, ok := targetsByID[accountID]
			if !ok {
				return nil, ErrAccountNotFound
			}
			if !isUpstreamBillingProbeAccount(account) {
				return nil, ErrUpstreamBillingProbeAccountInvalid
			}
		}
	}
	if len(input.Credentials) > 0 {
		for _, account := range cachedTargets {
			if account != nil && account.IsCredentialShadow() {
				return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_NO_CREDENTIALS",
					"spark shadow account %d cannot hold credentials; manage credentials on the parent account", account.ID)
			}
		}
	}
	if input.ProxyID != nil {
		for _, account := range cachedTargets {
			if account != nil && account.IsCredentialShadow() {
				return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_PROXY_INHERITED",
					"spark shadow account %d proxy is inherited from its parent and cannot be set in bulk; manage it on the parent account", account.ID)
			}
		}
	}

	// 预加载账号平台信息（混合渠道检查需要）。
	platformByID := map[int64]string{}
	if needMixedChannelCheck {
		for _, account := range cachedTargets {
			if account != nil {
				platformByID[account.ID] = account.Platform
			}
		}
	}

	// 预检查混合渠道风险：在任何写操作之前，若发现风险立即返回错误。
	if needMixedChannelCheck {
		for _, accountID := range input.AccountIDs {
			platform := platformByID[accountID]
			if platform == "" {
				continue
			}
			if err := s.checkMixedChannelRisk(ctx, accountID, platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		syncEnabledCount := 0
		for _, account := range cachedTargets {
			if upstreamBillingRateSyncEnabled(account) {
				syncEnabledCount++
			}
		}
		if syncEnabledCount > 0 {
			return nil, ErrUpstreamBillingRateSyncBulkConflict.WithMetadata(map[string]string{
				"count": strconv.Itoa(syncEnabledCount),
			})
		}
	}

	// 校验并规范化请求头覆写配置（批量路径为 JSONB 顶层 key 合并，直接校验增量即可）
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	if input.Credentials != nil {
		input.Credentials = SanitizeStoredCredentials("", input.Credentials)
	}

	// Prepare bulk updates for columns and JSONB fields.
	repoUpdates := AccountBulkUpdate{
		Credentials:                input.Credentials,
		Extra:                      input.Extra,
		ProbeEnabled:               input.ProbeEnabled,
		EnsureCodexFingerprintSeed: ShouldEnsureCodexFingerprintSeedForExtraUpdates(input.Extra),
	}
	if input.ProbeEnabled != nil {
		repoUpdates.Extra[UpstreamBillingProbeEnabledExtraKey] = *input.ProbeEnabled
		if !*input.ProbeEnabled {
			repoUpdates.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
		}
	}
	if updatesUpstreamBillingProbeIdentity(input.Credentials) || input.ProxyID != nil {
		repoUpdates.Extra[UpstreamBillingProbeExtraKey] = nil
	}
	if input.Name != "" {
		repoUpdates.Name = &input.Name
	}
	if input.ProxyID != nil {
		repoUpdates.ProxyID = input.ProxyID
	}
	if input.Concurrency != nil {
		repoUpdates.Concurrency = input.Concurrency
	}
	if input.Priority != nil {
		repoUpdates.Priority = input.Priority
	}
	if input.RateMultiplier != nil {
		repoUpdates.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			repoUpdates.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			repoUpdates.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		repoUpdates.Status = &input.Status
	}
	if input.Schedulable != nil {
		repoUpdates.Schedulable = input.Schedulable
	}

	// Run bulk update for column/jsonb fields first.
	if _, err := s.accountRepo.BulkUpdate(ctx, input.AccountIDs, repoUpdates); err != nil {
		return nil, err
	}

	if repoUpdates.ProxyID != nil {
		var effectiveProxyID *int64
		if *repoUpdates.ProxyID != 0 {
			effectiveProxyID = repoUpdates.ProxyID
		}
		for _, accountID := range input.AccountIDs {
			if err := s.propagateProxyToShadows(ctx, accountID, effectiveProxyID); err != nil {
				return nil, err
			}
		}
	}

	// Handle group bindings per account (requires individual operations).
	for _, accountID := range input.AccountIDs {
		entry := BulkUpdateAccountResult{AccountID: accountID}

		if input.GroupIDs != nil {
			if err := s.accountRepo.BindGroups(ctx, accountID, *input.GroupIDs); err != nil {
				entry.Success = false
				entry.Error = err.Error()
				result.Failed++
				result.FailedIDs = append(result.FailedIDs, accountID)
				result.Results = append(result.Results, entry)
				continue
			}
		}

		entry.Success = true
		result.Success++
		result.SuccessIDs = append(result.SuccessIDs, accountID)
		result.Results = append(result.Results, entry)
	}

	return result, nil
}

func updatesUpstreamBillingProbeIdentity(credentials map[string]any) bool {
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if _, ok := credentials[key]; ok {
			return true
		}
	}
	return false
}

func upstreamBillingProbeIdentity(account *Account) map[string]any {
	if account == nil {
		return nil
	}
	identity := map[string]any{"platform": account.Platform, "type": account.Type, "proxy_id": nil}
	if account.ProxyID != nil {
		identity["proxy_id"] = *account.ProxyID
	}
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if value, ok := account.Credentials[key]; ok {
			identity[key] = value
		}
	}
	return identity
}

func (s *adminServiceImpl) resolveBulkUpdateTargetIDs(ctx context.Context, filters *BulkUpdateAccountFilters) ([]int64, error) {
	if filters == nil {
		return nil, nil
	}

	groupID := int64(0)
	switch strings.TrimSpace(filters.Group) {
	case "":
	case "ungrouped":
		groupID = AccountListGroupUngrouped
	default:
		parsedGroupID, err := strconv.ParseInt(strings.TrimSpace(filters.Group), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid group filter: %w", err)
		}
		groupID = parsedGroupID
	}

	const pageSize = 500
	page := 1
	accountIDs := make([]int64, 0, pageSize)

	for {
		accounts, total, err := s.ListAccounts(
			ctx,
			page,
			pageSize,
			filters.Platform,
			filters.Type,
			filters.Status,
			filters.Search,
			groupID,
			filters.PrivacyMode,
			"",
			"",
		)
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			accountIDs = append(accountIDs, account.ID)
		}
		if int64(len(accountIDs)) >= total || len(accounts) == 0 {
			return accountIDs, nil
		}
		page++
	}
}

func (s *adminServiceImpl) DeleteAccount(ctx context.Context, id int64) error {
	shadows, err := s.accountRepo.ListShadowsByParent(ctx, id)
	if err != nil {
		return fmt.Errorf("list spark shadows for cascade delete: %w", err)
	}
	for _, shadow := range shadows {
		if err := s.accountRepo.Delete(ctx, shadow.ID); err != nil {
			return fmt.Errorf("cascade delete spark shadow %d: %w", shadow.ID, err)
		}
	}
	if err := s.accountRepo.Delete(ctx, id); err != nil {
		return err
	}
	return nil
}

func (s *adminServiceImpl) RefreshAccountCredentials(ctx context.Context, id int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// TODO: Implement refresh logic
	return account, nil
}

func (s *adminServiceImpl) ClearAccountError(ctx context.Context, id int64) (*Account, error) {
	if err := s.accountRepo.ClearError(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearRateLimit(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearAntigravityQuotaScopes(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearModelRateLimits(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearTempUnschedulable(ctx, id); err != nil {
		return nil, err
	}
	if s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(id)
	}
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) SetAccountError(ctx context.Context, id int64, errorMsg string) error {
	return s.accountRepo.SetError(ctx, id, errorMsg)
}

func (s *adminServiceImpl) SetAccountSchedulable(ctx context.Context, id int64, schedulable bool) (*Account, error) {
	if err := s.accountRepo.SetSchedulable(ctx, id, schedulable); err != nil {
		return nil, err
	}
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *adminServiceImpl) RevertAccountProxyFallback(ctx context.Context, id int64) error {
	if err := s.accountRepo.RevertProxyFallback(ctx, id); err != nil {
		return err
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get account after proxy revert: %w", err)
	}
	return s.propagateProxyToShadows(ctx, id, account.ProxyID)
}

// CreateShadow creates the single spark quota-dimension shadow linked to an
// OpenAI OAuth parent. The shadow stores only model mappings; authentication
// and proxy identity always come from the parent.
func (s *adminServiceImpl) CreateShadow(ctx context.Context, parentID int64, opts ShadowOptions) (*Account, error) {
	parent, err := s.accountRepo.GetByID(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("get parent account: %w", err)
	}
	if !parent.IsOpenAIOAuth() {
		return nil, infraerrors.New(http.StatusBadRequest, "SPARK_SHADOW_INVALID_PARENT",
			"spark shadow requires an OpenAI OAuth parent account")
	}
	if parent.IsCredentialShadow() {
		return nil, infraerrors.New(http.StatusBadRequest, "SPARK_SHADOW_PARENT_IS_SHADOW",
			"spark shadow parent must be a real account, not another spark shadow")
	}

	shadows, err := s.accountRepo.ListShadowsByParent(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("check existing spark shadows: %w", err)
	}
	if len(shadows) > 0 {
		return nil, infraerrors.New(http.StatusConflict, "SPARK_SHADOW_ALREADY_EXISTS",
			"parent account already has a spark shadow account")
	}

	groupIDs := append([]int64(nil), opts.GroupIDs...)
	if len(groupIDs) > 0 {
		if s.groupRepo != nil {
			if err := s.validateGroupIDsExist(ctx, groupIDs); err != nil {
				return nil, err
			}
		}
	} else if len(parent.GroupIDs) > 0 {
		groupIDs = append([]int64(nil), parent.GroupIDs...)
	} else if s.groupRepo != nil {
		defaultGroupName := PlatformOpenAI + "-default"
		if groups, listErr := s.groupRepo.ListActiveByPlatform(ctx, PlatformOpenAI); listErr == nil {
			for _, group := range groups {
				if group.Name == defaultGroupName {
					groupIDs = []int64{group.ID}
					break
				}
			}
		}
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = parent.Name + " (Spark)"
	}
	if runes := []rune(name); len(runes) > 100 {
		name = string(runes[:100])
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = parent.Concurrency
	}
	priority := opts.Priority
	if priority <= 0 {
		priority = parent.Priority
	}
	shadow := &Account{
		Name:            name,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		Credentials:     map[string]any{"model_mapping": defaultSparkShadowModelMapping()},
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		ProxyID:         parent.ProxyID,
		Priority:        priority,
		Concurrency:     concurrency,
		Schedulable:     true,
		Extra: map[string]any{
			openAILongContextBillingEnabledKey: parent.IsOpenAILongContextBillingEnabled(),
		},
	}
	if err := s.accountRepo.Create(ctx, shadow); err != nil {
		if existing, listErr := s.accountRepo.ListShadowsByParent(ctx, parentID); listErr == nil && len(existing) > 0 {
			return nil, infraerrors.New(http.StatusConflict, "SPARK_SHADOW_ALREADY_EXISTS",
				"parent account already has a spark shadow account")
		}
		return nil, fmt.Errorf("create spark shadow: %w", err)
	}

	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, shadow.ID, groupIDs); err != nil {
			if deleteErr := s.accountRepo.Delete(context.WithoutCancel(ctx), shadow.ID); deleteErr != nil {
				slog.Error("spark_shadow_bind_groups_rollback_failed",
					"shadow_id", shadow.ID, "parent_id", parentID, "delete_err", deleteErr)
			}
			return nil, fmt.Errorf("bind groups for spark shadow: %w", err)
		}
		shadow.GroupIDs = groupIDs
	}

	return shadow, nil
}

func (s *adminServiceImpl) propagateProxyToShadows(ctx context.Context, parentID int64, proxyID *int64) error {
	return propagateAccountProxyToShadows(ctx, s.accountRepo, parentID, proxyID)
}

func propagateAccountProxyToShadows(ctx context.Context, repo AccountRepository, parentID int64, proxyID *int64) error {
	shadows, err := repo.ListShadowsByParent(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list spark shadows for proxy propagation: %w", err)
	}
	for _, shadow := range shadows {
		shadow.ProxyID = proxyID
		if err := repo.Update(ctx, shadow); err != nil {
			return fmt.Errorf("update spark shadow %d proxy: %w", shadow.ID, err)
		}
	}
	return nil
}

// Proxy management implementations
func (s *adminServiceImpl) ListProxies(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]Proxy, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	proxies, result, err := s.proxyRepo.ListWithFilters(ctx, params, protocol, status, search)
	if err != nil {
		return nil, 0, err
	}
	return proxies, result.Total, nil
}

func (s *adminServiceImpl) ListProxiesWithAccountCount(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]ProxyWithAccountCount, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	proxies, result, err := s.proxyRepo.ListWithFiltersAndAccountCount(ctx, params, protocol, status, search)
	if err != nil {
		return nil, 0, err
	}
	s.attachProxyLatency(ctx, proxies)
	return proxies, result.Total, nil
}

func (s *adminServiceImpl) GetAllProxies(ctx context.Context) ([]Proxy, error) {
	return s.proxyRepo.ListActive(ctx)
}

func (s *adminServiceImpl) GetAllProxiesWithAccountCount(ctx context.Context) ([]ProxyWithAccountCount, error) {
	proxies, err := s.proxyRepo.ListActiveWithAccountCount(ctx)
	if err != nil {
		return nil, err
	}
	s.attachProxyLatency(ctx, proxies)
	return proxies, nil
}

func (s *adminServiceImpl) GetProxy(ctx context.Context, id int64) (*Proxy, error) {
	return s.proxyRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetProxiesByIDs(ctx context.Context, ids []int64) ([]Proxy, error) {
	return s.proxyRepo.ListByIDs(ctx, ids)
}

func (s *adminServiceImpl) CreateProxy(ctx context.Context, input *CreateProxyInput) (*Proxy, error) {
	// 规范化 fallback_mode
	mode := input.FallbackMode
	if mode == "" {
		mode = FallbackModeNone
	}
	// 校验：mode=proxy 必须有 backup
	if mode == FallbackModeProxy && input.BackupProxyID == nil {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_REQUIRED", "backup proxy required when fallback_mode=proxy")
	}
	if input.ExpiryWarnDays < 0 {
		return nil, infraerrors.BadRequest("PROXY_WARN_DAYS_INVALID", "expiry_warn_days must be >= 0")
	}

	proxy := &Proxy{
		Name:           input.Name,
		Protocol:       input.Protocol,
		Host:           input.Host,
		Port:           input.Port,
		Username:       input.Username,
		Password:       input.Password,
		Status:         StatusActive,
		ExpiresAt:      input.ExpiresAt,
		FallbackMode:   mode,
		BackupProxyID:  input.BackupProxyID,
		ExpiryWarnDays: input.ExpiryWarnDays,
	}
	if err := s.proxyRepo.Create(ctx, proxy); err != nil {
		return nil, err
	}
	// Probe latency asynchronously so creation isn't blocked by network timeout.
	go s.probeProxyLatency(context.Background(), proxy)
	return proxy, nil
}

func (s *adminServiceImpl) UpdateProxy(ctx context.Context, id int64, input *UpdateProxyInput) (*Proxy, error) {
	// 校验：backup_proxy_id 不能是自身
	if input.BackupProxyID != nil && *input.BackupProxyID == id {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_SELF", "backup proxy cannot be itself")
	}
	// 规范化 fallback_mode
	mode := input.FallbackMode
	if mode == "" {
		mode = FallbackModeNone
	}
	// 校验：mode=proxy 必须有 backup
	if mode == FallbackModeProxy && input.BackupProxyID == nil {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_REQUIRED", "backup proxy required when fallback_mode=proxy")
	}
	if input.ExpiryWarnDays < 0 {
		return nil, infraerrors.BadRequest("PROXY_WARN_DAYS_INVALID", "expiry_warn_days must be >= 0")
	}

	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if input.Name != "" {
		proxy.Name = input.Name
	}
	if input.Protocol != "" {
		proxy.Protocol = input.Protocol
	}
	if input.Host != "" {
		proxy.Host = input.Host
	}
	if input.Port != 0 {
		proxy.Port = input.Port
	}
	if input.Username != "" {
		proxy.Username = input.Username
	}
	if input.Password != "" {
		proxy.Password = input.Password
	}
	if input.Status != "" {
		proxy.Status = input.Status
	}
	// 透传有效期与回退字段
	proxy.ExpiresAt = input.ExpiresAt
	proxy.FallbackMode = mode
	proxy.BackupProxyID = input.BackupProxyID
	proxy.ExpiryWarnDays = input.ExpiryWarnDays

	if err := s.proxyRepo.Update(ctx, proxy); err != nil {
		return nil, err
	}
	return proxy, nil
}

func (s *adminServiceImpl) DeleteProxy(ctx context.Context, id int64) error {
	count, err := s.proxyRepo.CountAccountsByProxyID(ctx, id)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrProxyInUse
	}
	return s.proxyRepo.Delete(ctx, id)
}

func (s *adminServiceImpl) BatchDeleteProxies(ctx context.Context, ids []int64) (*ProxyBatchDeleteResult, error) {
	result := &ProxyBatchDeleteResult{}
	if len(ids) == 0 {
		return result, nil
	}

	for _, id := range ids {
		count, err := s.proxyRepo.CountAccountsByProxyID(ctx, id)
		if err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: err.Error(),
			})
			continue
		}
		if count > 0 {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: ErrProxyInUse.Error(),
			})
			continue
		}
		if err := s.proxyRepo.Delete(ctx, id); err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: err.Error(),
			})
			continue
		}
		result.DeletedIDs = append(result.DeletedIDs, id)
	}

	return result, nil
}

func (s *adminServiceImpl) GetProxyAccounts(ctx context.Context, proxyID int64) ([]ProxyAccountSummary, error) {
	return s.proxyRepo.ListAccountSummariesByProxyID(ctx, proxyID)
}

func (s *adminServiceImpl) CheckProxyExists(ctx context.Context, host string, port int, username, password string) (bool, error) {
	return s.proxyRepo.ExistsByHostPortAuth(ctx, host, port, username, password)
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
			Code:   codeValue,
			Type:   input.Type,
			Value:  input.Value,
			Status: StatusUnused,
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

func (s *adminServiceImpl) TestProxy(ctx context.Context, id int64) (*ProxyTestResult, error) {
	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	proxyURL := proxy.URL()
	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxyURL)
	if err != nil {
		s.saveProxyLatency(ctx, id, &ProxyLatencyInfo{
			Success:   false,
			Message:   err.Error(),
			UpdatedAt: time.Now(),
		})
		return &ProxyTestResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	latency := latencyMs
	s.saveProxyLatency(ctx, id, &ProxyLatencyInfo{
		Success:     true,
		LatencyMs:   &latency,
		Message:     "Proxy is accessible",
		IPAddress:   exitInfo.IP,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
		Region:      exitInfo.Region,
		City:        exitInfo.City,
		UpdatedAt:   time.Now(),
	})
	return &ProxyTestResult{
		Success:     true,
		Message:     "Proxy is accessible",
		LatencyMs:   latencyMs,
		IPAddress:   exitInfo.IP,
		City:        exitInfo.City,
		Region:      exitInfo.Region,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
	}, nil
}

func (s *adminServiceImpl) CheckProxyQuality(ctx context.Context, id int64) (*ProxyQualityCheckResult, error) {
	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result := &ProxyQualityCheckResult{
		ProxyID:   id,
		Score:     100,
		Grade:     "A",
		CheckedAt: time.Now().Unix(),
		Items:     make([]ProxyQualityCheckItem, 0, len(proxyQualityTargets)+1),
	}

	proxyURL := proxy.URL()
	if s.proxyProber == nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "base_connectivity",
			Status:  "fail",
			Message: "代理探测服务未配置",
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, nil)
		return result, nil
	}

	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxyURL)
	if err != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:    "base_connectivity",
			Status:    "fail",
			LatencyMs: latencyMs,
			Message:   err.Error(),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, nil)
		return result, nil
	}

	result.ExitIP = exitInfo.IP
	result.Country = exitInfo.Country
	result.CountryCode = exitInfo.CountryCode
	result.BaseLatencyMs = latencyMs
	result.Items = append(result.Items, ProxyQualityCheckItem{
		Target:    "base_connectivity",
		Status:    "pass",
		LatencyMs: latencyMs,
		Message:   "代理出口连通正常",
	})
	result.PassedCount++

	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:              proxyURL,
		Timeout:               proxyQualityRequestTimeout,
		ResponseHeaderTimeout: proxyQualityResponseHeaderTimeout,
	})
	if err != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "http_client",
			Status:  "fail",
			Message: fmt.Sprintf("创建检测客户端失败: %v", err),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, exitInfo)
		return result, nil
	}

	for _, target := range proxyQualityTargets {
		item := runProxyQualityTarget(ctx, client, target)
		result.Items = append(result.Items, item)
		switch item.Status {
		case "pass":
			result.PassedCount++
		case "warn":
			result.WarnCount++
		case "challenge":
			result.ChallengeCount++
		default:
			result.FailedCount++
		}
	}

	finalizeProxyQualityResult(result)
	s.saveProxyQualitySnapshot(ctx, id, result, exitInfo)
	return result, nil
}

func runProxyQualityTarget(ctx context.Context, client *http.Client, target proxyQualityTarget) ProxyQualityCheckItem {
	item := ProxyQualityCheckItem{
		Target: target.Target,
	}

	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, nil)
	if err != nil {
		item.Status = "fail"
		item.Message = fmt.Sprintf("构建请求失败: %v", err)
		return item
	}
	req.Header.Set("Accept", "application/json,text/html,*/*")
	req.Header.Set("User-Agent", proxyQualityClientUserAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		item.Status = "fail"
		item.LatencyMs = time.Since(start).Milliseconds()
		item.Message = fmt.Sprintf("请求失败: %v", err)
		return item
	}
	defer func() { _ = resp.Body.Close() }()
	item.LatencyMs = time.Since(start).Milliseconds()
	item.HTTPStatus = resp.StatusCode

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, proxyQualityMaxBodyBytes+1))
	if readErr != nil {
		item.Status = "fail"
		item.Message = fmt.Sprintf("读取响应失败: %v", readErr)
		return item
	}
	if int64(len(body)) > proxyQualityMaxBodyBytes {
		body = body[:proxyQualityMaxBodyBytes]
	}

	// Cloudflare challenge 检测
	if httputil.IsCloudflareChallengeResponse(resp.StatusCode, resp.Header, body) {
		item.Status = "challenge"
		item.CFRay = httputil.ExtractCloudflareRayID(resp.Header, body)
		item.Message = "命中 Cloudflare challenge"
		return item
	}

	if _, ok := target.AllowedStatuses[resp.StatusCode]; ok {
		// 白名单内的状态码均代表目标可达：2xx 表示接口直接可用，
		// 401/405 等是无鉴权探测的预期结果，同样视为连通正常，不再扣分。
		item.Status = "pass"
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			item.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			item.Message = fmt.Sprintf("HTTP %d（目标可达）", resp.StatusCode)
		}
		return item
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		item.Status = "warn"
		item.Message = "目标返回 429，可能存在频控"
		return item
	}

	item.Status = "fail"
	item.Message = fmt.Sprintf("非预期状态码: %d", resp.StatusCode)
	return item
}

func finalizeProxyQualityResult(result *ProxyQualityCheckResult) {
	if result == nil {
		return
	}
	score := 100 - result.WarnCount*10 - result.FailedCount*22 - result.ChallengeCount*30
	if score < 0 {
		score = 0
	}
	result.Score = score
	result.Grade = proxyQualityGrade(score)
	result.Summary = fmt.Sprintf(
		"通过 %d 项，告警 %d 项，失败 %d 项，挑战 %d 项",
		result.PassedCount,
		result.WarnCount,
		result.FailedCount,
		result.ChallengeCount,
	)
}

func proxyQualityGrade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}

func proxyQualityOverallStatus(result *ProxyQualityCheckResult) string {
	if result == nil {
		return ""
	}
	if result.ChallengeCount > 0 {
		return "challenge"
	}
	if result.FailedCount > 0 {
		return "failed"
	}
	if result.WarnCount > 0 {
		return "warn"
	}
	if result.PassedCount > 0 {
		return "healthy"
	}
	return "failed"
}

func proxyQualityFirstCFRay(result *ProxyQualityCheckResult) string {
	if result == nil {
		return ""
	}
	for _, item := range result.Items {
		if item.CFRay != "" {
			return item.CFRay
		}
	}
	return ""
}

func proxyQualityBaseConnectivityPass(result *ProxyQualityCheckResult) bool {
	if result == nil {
		return false
	}
	for _, item := range result.Items {
		if item.Target == "base_connectivity" {
			return item.Status == "pass"
		}
	}
	return false
}

func (s *adminServiceImpl) saveProxyQualitySnapshot(ctx context.Context, proxyID int64, result *ProxyQualityCheckResult, exitInfo *ProxyExitInfo) {
	if result == nil {
		return
	}
	score := result.Score
	checkedAt := result.CheckedAt
	info := &ProxyLatencyInfo{
		Success:          proxyQualityBaseConnectivityPass(result),
		Message:          result.Summary,
		QualityStatus:    proxyQualityOverallStatus(result),
		QualityScore:     &score,
		QualityGrade:     result.Grade,
		QualitySummary:   result.Summary,
		QualityCheckedAt: &checkedAt,
		QualityCFRay:     proxyQualityFirstCFRay(result),
		UpdatedAt:        time.Now(),
	}
	if result.BaseLatencyMs > 0 {
		latency := result.BaseLatencyMs
		info.LatencyMs = &latency
	}
	if exitInfo != nil {
		info.IPAddress = exitInfo.IP
		info.Country = exitInfo.Country
		info.CountryCode = exitInfo.CountryCode
		info.Region = exitInfo.Region
		info.City = exitInfo.City
	}
	s.saveProxyLatency(ctx, proxyID, info)
}

func (s *adminServiceImpl) probeProxyLatency(ctx context.Context, proxy *Proxy) {
	if s.proxyProber == nil || proxy == nil {
		return
	}
	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxy.URL())
	if err != nil {
		s.saveProxyLatency(ctx, proxy.ID, &ProxyLatencyInfo{
			Success:   false,
			Message:   err.Error(),
			UpdatedAt: time.Now(),
		})
		return
	}

	latency := latencyMs
	s.saveProxyLatency(ctx, proxy.ID, &ProxyLatencyInfo{
		Success:     true,
		LatencyMs:   &latency,
		Message:     "Proxy is accessible",
		IPAddress:   exitInfo.IP,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
		Region:      exitInfo.Region,
		City:        exitInfo.City,
		UpdatedAt:   time.Now(),
	})
}

// checkMixedChannelRisk 检查分组中是否存在混合渠道（Antigravity + Anthropic）
// 如果存在混合，返回错误提示用户确认
func (s *adminServiceImpl) checkMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	// 判断当前账号的渠道类型（基于 platform 字段，而不是 type 字段）
	currentPlatform := getAccountPlatform(currentAccountPlatform)
	if currentPlatform == "" {
		// 不是 Antigravity 或 Anthropic，无需检查
		return nil
	}

	// 检查每个分组中的其他账号
	for _, groupID := range groupIDs {
		accounts, err := s.accountRepo.ListByGroup(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get accounts in group %d: %w", groupID, err)
		}

		// 检查是否存在不同渠道的账号
		for _, account := range accounts {
			if currentAccountID > 0 && account.ID == currentAccountID {
				continue // 跳过当前账号
			}

			otherPlatform := getAccountPlatform(account.Platform)
			if otherPlatform == "" {
				continue // 不是 Antigravity 或 Anthropic，跳过
			}

			// 检测混合渠道
			if currentPlatform != otherPlatform {
				group, _ := s.groupRepo.GetByID(ctx, groupID)
				groupName := fmt.Sprintf("Group %d", groupID)
				if group != nil {
					groupName = group.Name
				}

				return &MixedChannelError{
					GroupID:         groupID,
					GroupName:       groupName,
					CurrentPlatform: currentPlatform,
					OtherPlatform:   otherPlatform,
				}
			}
		}
	}

	return nil
}

func (s *adminServiceImpl) validateGroupIDsExist(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 {
		return nil
	}
	if s.groupRepo == nil {
		return errors.New("group repository not configured")
	}

	if batchReader, ok := s.groupRepo.(groupExistenceBatchReader); ok {
		existsByID, err := batchReader.ExistsByIDs(ctx, groupIDs)
		if err != nil {
			return fmt.Errorf("check groups exists: %w", err)
		}
		for _, groupID := range groupIDs {
			if groupID <= 0 || !existsByID[groupID] {
				return fmt.Errorf("get group: %w", ErrGroupNotFound)
			}
		}
		return nil
	}

	for _, groupID := range groupIDs {
		if _, err := s.groupRepo.GetByID(ctx, groupID); err != nil {
			return fmt.Errorf("get group: %w", err)
		}
	}
	return nil
}

// CheckMixedChannelRisk checks whether target groups contain mixed channels for the current account platform.
func (s *adminServiceImpl) CheckMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	return s.checkMixedChannelRisk(ctx, currentAccountID, currentAccountPlatform, groupIDs)
}

func (s *adminServiceImpl) attachProxyLatency(ctx context.Context, proxies []ProxyWithAccountCount) {
	if s.proxyLatencyCache == nil || len(proxies) == 0 {
		return
	}

	ids := make([]int64, 0, len(proxies))
	for i := range proxies {
		ids = append(ids, proxies[i].ID)
	}

	latencies, err := s.proxyLatencyCache.GetProxyLatencies(ctx, ids)
	if err != nil {
		logger.LegacyPrintf("service.admin", "Warning: load proxy latency cache failed: %v", err)
		return
	}

	for i := range proxies {
		info := latencies[proxies[i].ID]
		if info == nil {
			continue
		}
		if info.Success {
			proxies[i].LatencyStatus = "success"
			proxies[i].LatencyMs = info.LatencyMs
		} else {
			proxies[i].LatencyStatus = "failed"
		}
		proxies[i].LatencyMessage = info.Message
		proxies[i].IPAddress = info.IPAddress
		proxies[i].Country = info.Country
		proxies[i].CountryCode = info.CountryCode
		proxies[i].Region = info.Region
		proxies[i].City = info.City
		proxies[i].QualityStatus = info.QualityStatus
		proxies[i].QualityScore = info.QualityScore
		proxies[i].QualityGrade = info.QualityGrade
		proxies[i].QualitySummary = info.QualitySummary
		proxies[i].QualityChecked = info.QualityCheckedAt
	}
}

func (s *adminServiceImpl) saveProxyLatency(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) {
	if s.proxyLatencyCache == nil || info == nil {
		return
	}

	merged := *info
	if latencies, err := s.proxyLatencyCache.GetProxyLatencies(ctx, []int64{proxyID}); err == nil {
		if existing := latencies[proxyID]; existing != nil {
			if merged.QualityCheckedAt == nil &&
				merged.QualityScore == nil &&
				merged.QualityGrade == "" &&
				merged.QualityStatus == "" &&
				merged.QualitySummary == "" &&
				merged.QualityCFRay == "" {
				merged.QualityStatus = existing.QualityStatus
				merged.QualityScore = existing.QualityScore
				merged.QualityGrade = existing.QualityGrade
				merged.QualitySummary = existing.QualitySummary
				merged.QualityCheckedAt = existing.QualityCheckedAt
				merged.QualityCFRay = existing.QualityCFRay
			}
		}
	}

	if err := s.proxyLatencyCache.SetProxyLatency(ctx, proxyID, &merged); err != nil {
		logger.LegacyPrintf("service.admin", "Warning: store proxy latency cache failed: %v", err)
	}
}

// getAccountPlatform 根据账号 platform 判断混合渠道检查用的平台标识
func getAccountPlatform(accountPlatform string) string {
	switch strings.ToLower(strings.TrimSpace(accountPlatform)) {
	case PlatformAntigravity:
		return "Antigravity"
	case PlatformAnthropic, "claude":
		return "Anthropic"
	default:
		return ""
	}
}

// MixedChannelError 混合渠道错误
type MixedChannelError struct {
	GroupID         int64
	GroupName       string
	CurrentPlatform string
	OtherPlatform   string
}

func (e *MixedChannelError) Error() string {
	return fmt.Sprintf("mixed_channel_warning: Group '%s' contains both %s and %s accounts. Using mixed channels in the same context may cause thinking block signature validation issues, which will fallback to non-thinking mode for historical messages.",
		e.GroupName, e.CurrentPlatform, e.OtherPlatform)
}

func (s *adminServiceImpl) ResetAccountQuota(ctx context.Context, id int64) error {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if account.IsCredentialShadow() {
		return infraerrors.New(http.StatusBadRequest, "SPARK_SHADOW_NO_QUOTA_RESET",
			"cannot reset quota for a spark shadow account; manage it on the parent account")
	}
	return s.accountRepo.ResetQuotaUsed(ctx, id)
}

// EnsureOpenAIPrivacy 检查 OpenAI OAuth 账号是否已设置 privacy_mode，
// 未设置则调用 disableOpenAITraining 并持久化到 Extra，返回设置的 mode 值。
func (s *adminServiceImpl) EnsureOpenAIPrivacy(ctx context.Context, account *Account) string {
	if account.IsCredentialShadow() {
		return ""
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}
	if shouldSkipOpenAIPrivacyEnsure(account.Extra) {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.ActiveURL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "update_openai_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra["privacy_mode"] = mode
	return mode
}

// ForceOpenAIPrivacy 强制重新设置 OpenAI OAuth 账号隐私，无论当前状态。
func (s *adminServiceImpl) ForceOpenAIPrivacy(ctx context.Context, account *Account) string {
	if account.IsCredentialShadow() {
		return ""
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.ActiveURL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "force_update_openai_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra["privacy_mode"] = mode
	return mode
}

// EnsureAntigravityPrivacy 检查 Antigravity OAuth 账号隐私状态。
// 仅当 privacy_mode 已成功设置（"privacy_set"）时跳过；
// 未设置或之前失败（"privacy_set_failed"）均会重试。
func (s *adminServiceImpl) EnsureAntigravityPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformAntigravity || account.Type != AccountTypeOAuth {
		return ""
	}
	if account.Extra != nil {
		if existing, ok := account.Extra["privacy_mode"].(string); ok && existing == AntigravityPrivacySet {
			return existing
		}
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	projectID, _ := account.Credentials["project_id"].(string)

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.ActiveURL()
		}
	}

	mode := setAntigravityPrivacy(ctx, token, projectID, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "update_antigravity_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	applyAntigravityPrivacyMode(account, mode)
	return mode
}

// ForceAntigravityPrivacy 强制重新设置 Antigravity OAuth 账号隐私，无论当前状态。
func (s *adminServiceImpl) ForceAntigravityPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformAntigravity || account.Type != AccountTypeOAuth {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	projectID, _ := account.Credentials["project_id"].(string)

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.ActiveURL()
		}
	}

	mode := setAntigravityPrivacy(ctx, token, projectID, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "force_update_antigravity_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	applyAntigravityPrivacyMode(account, mode)
	return mode
}
