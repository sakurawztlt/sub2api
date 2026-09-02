package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

const (
	// ChatGPT internal API for OAuth accounts
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	// OpenAI Platform API for API Key accounts (fallback)
	openaiPlatformAPIURL            = "https://api.openai.com/v1/responses"
	openaiPlatformAPIInputTokensURL = "https://api.openai.com/v1/responses/input_tokens"
	openaiStickySessionTTL          = time.Hour // 粘性会话TTL
	// 与真实 Codex CLI 的 User-Agent 结构对齐：
	// {originator}/{version} ({OS} {OS_version}; {arch}) {terminal}
	// 缺少 OS/架构/终端后缀的形态易被上游指纹识别为非官方客户端。
	// 该后缀是 UA 形态的唯一定义处，buildCodexCLIUserAgent 按运行时版本号复用它。
	codexCLIUserAgentSuffix = " (Ubuntu 22.4.0; x86_64) xterm-256color"
	// codexCLIUserAgent 是编译期兜底 UA；运行时优先使用由后台版本号拼出的规范 UA。
	// 版本段必须来自 codexCLIVersion：UA 与 version 头是同一个版本声明的两个出口，
	// 各自硬编码会漂移成互相矛盾的身份。
	codexCLIUserAgent = "codex_cli_rs/" + codexCLIVersion + codexCLIUserAgentSuffix
	// codex_cli_only 拒绝时单个请求头日志长度上限（字符）
	codexCLIOnlyHeaderValueMaxBytes = 256

	// OpenAI WS Mode 失败后的重连次数上限（不含首次尝试）。
	// 与 Codex 客户端保持一致：失败后最多重连 5 次。
	openAIWSReconnectRetryLimit = 5
	// 上游错误体只需要提取错误 JSON/日志摘要，默认 512KiB 避免错误风暴叠加大请求体。
	openAIUpstreamErrorBodyReadLimit int64 = 512 << 10
	// OpenAI WS Mode 重连退避默认值（可由配置覆盖）。
	openAIWSRetryBackoffInitialDefault = 120 * time.Millisecond
	openAIWSRetryBackoffMaxDefault     = 2 * time.Second
	openAIWSRetryJitterRatioDefault    = 0.2
	openAICompactSessionSeedKey        = "openai_compact_session_seed"
	openAIUpstreamEndpointContextKey   = "openai_actual_upstream_endpoint"
	// codexCLIVersion 是网关对上游声明的 Codex 客户端版本，同时供 codexCLIUserAgent
	// 与 version 头使用。上游 /backend-api/codex 在容量紧张时按客户端身份分优先级降载，
	// 陈旧版本会被优先丢弃（HTTP 200 + 流内 server_is_overloaded）；非官方客户端配不出
	// 官方身份时整体回退到本常量，因此它必须跟随官方 CLI 的当前发布版本，
	// 落后多个版本会让这些请求稳定落在被优先丢弃的一侧。
	codexCLIVersion = "0.146.0"
	// Codex 限额快照仅用于后台展示/诊断，不需要每个成功请求都立即落库。
	openAICodexSnapshotPersistMinInterval = 30 * time.Second
	openAICodexRecoverySnapshotMaxSkew    = 2 * time.Minute
	// 配额自动暂停时，超过该时长仍未刷新的 used% 快照视为陈旧，不再据此暂停账号。
	// 被暂停的账号收不到流量，其快照永远不会从上游响应头刷新；该兜底让账号在快照
	// 陈旧时放行一次请求，从而通过正常响应头自愈，而无需等待整个窗口（5h/7d）重置。
	openAICodexAutoPauseStaleAfter = 2 * time.Hour
)

func applyOpenAIOAuthCodexUserAgentFallback(headers http.Header, account *Account) {
	if account == nil || account.Type != AccountTypeOAuth {
		return
	}
	if !openai.IsCodexOfficialClientRequest(headers.Get("user-agent")) {
		headers.Set("user-agent", codexCLIUserAgent)
	}
}

// OpenAI allowed headers whitelist (for non-passthrough).
var openaiAllowedHeaders = map[string]bool{
	"accept-language":         true,
	"content-type":            true,
	"conversation_id":         true,
	"user-agent":              true,
	"originator":              true,
	"session_id":              true,
	"x-codex-beta-features":   true,
	"x-codex-installation-id": true,
	"x-codex-turn-state":      true,
	"x-codex-turn-metadata":   true,
	"x-codex-window-id":       true,
	responsesLiteHeaderKey:    true,
}

// OpenAI passthrough allowed headers whitelist.
// 透传模式下仅放行这些低风险请求头，避免将非标准/环境噪声头传给上游触发风控。
var openaiPassthroughAllowedHeaders = map[string]bool{
	"accept":                  true,
	"accept-language":         true,
	"content-type":            true,
	"conversation_id":         true,
	"openai-beta":             true,
	"user-agent":              true,
	"originator":              true,
	"session_id":              true,
	"x-codex-beta-features":   true,
	"x-codex-installation-id": true,
	"x-codex-turn-state":      true,
	"x-codex-turn-metadata":   true,
	"x-codex-window-id":       true,
	responsesLiteHeaderKey:    true,
}

// codex_cli_only 拒绝时记录的请求头白名单（仅用于诊断日志，不参与上游透传）
var codexCLIOnlyDebugHeaderWhitelist = []string{
	"User-Agent",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"OpenAI-Beta",
	"Originator",
	"Session_ID",
	"Conversation_ID",
	"X-Request-ID",
	"X-Client-Request-ID",
	"X-Forwarded-For",
	"X-Real-IP",
}

// openAIPassthroughRollbackError — codex upstream PR#2498 (2026-05-16):
// signals that the passthrough path detected an invalid explicit instructions
// value before any response was written. Caller (Forward) catches
// this via errors.As and falls back to the non-passthrough (full
// transform) path instead of returning an internal-style 403 to the
// client. Replaces the old behavior of writing
// `OpenAI codex passthrough requires a non-empty instructions field`
// directly to the client which exposed fork architecture details.
type openAIPassthroughRollbackError struct {
	Reason string
}

func (e *openAIPassthroughRollbackError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("openai passthrough rollback: %s", strings.TrimSpace(e.Reason))
}

// OpenAICodexUsageSnapshot represents Codex API usage limits from response headers
type OpenAICodexUsageSnapshot struct {
	PrimaryUsedPercent          *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAfterSeconds    *int     `json:"primary_reset_after_seconds,omitempty"`
	PrimaryWindowMinutes        *int     `json:"primary_window_minutes,omitempty"`
	SecondaryUsedPercent        *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAfterSeconds  *int     `json:"secondary_reset_after_seconds,omitempty"`
	SecondaryWindowMinutes      *int     `json:"secondary_window_minutes,omitempty"`
	PrimaryOverSecondaryPercent *float64 `json:"primary_over_secondary_percent,omitempty"`
	UpdatedAt                   string   `json:"updated_at,omitempty"`
}

// NormalizedCodexLimits contains normalized 5h/7d rate limit data
type NormalizedCodexLimits struct {
	Used5hPercent   *float64
	Reset5hSeconds  *int
	Window5hMinutes *int
	Used7dPercent   *float64
	Reset7dSeconds  *int
	Window7dMinutes *int
}

func normalizeCodexUsedPercent(raw *float64) *float64 {
	if raw == nil {
		return nil
	}
	// Current Codex quota headers expose used%, not remaining%. Keep the
	// canonical 5h field on the same used% scale. A previous remaining% flip
	// made fresh 0% snapshots look like exhausted 100% accounts.
	used := *raw
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return &used
}

// Normalize converts primary/secondary fields to canonical 5h/7d fields.
// Strategy: Compare window_minutes to determine which is 5h vs 7d.
// Returns nil if snapshot is nil or has no useful data.
func (s *OpenAICodexUsageSnapshot) Normalize() *NormalizedCodexLimits {
	if s == nil {
		return nil
	}

	result := &NormalizedCodexLimits{}

	primaryMins := 0
	secondaryMins := 0
	hasPrimaryWindow := false
	hasSecondaryWindow := false

	if s.PrimaryWindowMinutes != nil {
		primaryMins = *s.PrimaryWindowMinutes
		hasPrimaryWindow = true
	}
	if s.SecondaryWindowMinutes != nil {
		secondaryMins = *s.SecondaryWindowMinutes
		hasSecondaryWindow = true
	}

	// Determine mapping based on window_minutes
	use5hFromPrimary := false
	use7dFromPrimary := false

	if hasPrimaryWindow && hasSecondaryWindow {
		// Both known: smaller window is 5h, larger is 7d
		if primaryMins < secondaryMins {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasPrimaryWindow {
		// Only primary known: classify by threshold (<=360 min = 6h -> 5h window)
		if primaryMins <= 360 {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasSecondaryWindow {
		// Only secondary known: classify by threshold
		if secondaryMins <= 360 {
			// 5h from secondary, so primary (if any data) is 7d
			use7dFromPrimary = true
		} else {
			// 7d from secondary, so primary (if any data) is 5h
			use5hFromPrimary = true
		}
	} else {
		// No window_minutes: fall back to legacy assumption (primary=7d, secondary=5h)
		use7dFromPrimary = true
	}

	// Assign values
	if use5hFromPrimary {
		result.Used5hPercent = normalizeCodexUsedPercent(s.PrimaryUsedPercent)
		result.Reset5hSeconds = s.PrimaryResetAfterSeconds
		result.Window5hMinutes = s.PrimaryWindowMinutes
		result.Used7dPercent = normalizeCodexUsedPercent(s.SecondaryUsedPercent)
		result.Reset7dSeconds = s.SecondaryResetAfterSeconds
		result.Window7dMinutes = s.SecondaryWindowMinutes
	} else if use7dFromPrimary {
		result.Used7dPercent = normalizeCodexUsedPercent(s.PrimaryUsedPercent)
		result.Reset7dSeconds = s.PrimaryResetAfterSeconds
		result.Window7dMinutes = s.PrimaryWindowMinutes
		result.Used5hPercent = normalizeCodexUsedPercent(s.SecondaryUsedPercent)
		result.Reset5hSeconds = s.SecondaryResetAfterSeconds
		result.Window5hMinutes = s.SecondaryWindowMinutes
	}

	return result
}

// OpenAIUsage represents OpenAI API response usage
type OpenAIUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ImageInputTokens         int `json:"image_input_tokens,omitempty"`
	ImageOutputTokens        int `json:"image_output_tokens,omitempty"`
}

// OpenAIForwardResult represents the result of forwarding
// streamReqMeta — codex round 11al (2026-05-15): forensics 字段集中传给
// handleAnthropicStreamingResponse / handleAnthropicBufferedStreamingResponse,
// 让 timeout / 大上下文 summary log 写出 (account_id/type / 上游 HTTP headers
// 耗时 / messages 数 / prompt cache key / continuation 状态). 客户慢请求
// 排查的核心字段一次到位.
type streamReqMeta struct {
	Account                   *Account
	TimeToHeadersMs           int    // 从 httpUpstream.Do() 到 resp.Header 返回
	MessagesCount             int    // gjson 数 body.messages 长度
	ToolsCount                int    // gjson 数 body.tools 长度
	CodeExecutionFallbackArgs string // request-derived fallback for empty hosted code_execution args
	PromptCacheKeySha256      string // sha256(promptCacheKey) hex 前 16 位
	HasPreviousResponseID     bool

	// HasTurnState — codex round37 fu56 (2026-05-20): clarified semantics.
	// This field reflects whether the INBOUND cache lookup
	// (getOpenAICompatSessionTurnState) returned a non-empty value at
	// request time. It is fed from the compatTurnState local variable in
	// the handler, NOT from the upstream response. fu55 added a
	// misleading "(outbound)" comment that codex round37 flagged —
	// codex round 11al introduced this field as inbound and it has
	// always been inbound.
	//
	// For the upstream-side signal (did the server return a fresh
	// x-codex-turn-state for THIS turn) use UpstreamTurnStateReturned
	// below.
	HasTurnState bool
	ProxyHash    string // proxy URL sha256 前 12 位 (无 proxy=空)

	// codex round36 fu55 (2026-05-20) / round37 fu56 (2026-05-20):
	// observability fields for the fu54 turn_state cache.
	//
	// codex constraint: never log the raw session id. Only enum / bool
	// fields. The raw header value lives in the request context, never
	// in our log lines.
	TurnStateKeySource string // enum: "session_header" | "metadata_session" | "prompt_cache_key" | "none"

	// TurnStateCacheHit — same semantics as HasTurnState above (both
	// reflect the INBOUND cache lookup result). codex round37 kept this
	// alongside HasTurnState for grep compatibility with the new log
	// field name introduced in fu55; the two values are always equal.
	TurnStateCacheHit bool

	HasSessionHeader   bool // X-Claude-Code-Session-Id header was present and non-empty
	HasMetadataSession bool // metadata.user_id session id was present and non-empty (see hasMetadataUserSessionID for the shapes covered)

	// UpstreamTurnStateReturned — codex round37 fu56 (2026-05-20): the
	// OUTBOUND signal. True when the upstream HTTP response carried a
	// non-empty `x-codex-turn-state` header for THIS turn. Distinct
	// from HasTurnState / TurnStateCacheHit (which describe what
	// sub2api had in cache before sending the request).
	//
	// Grep pattern for fu54 effectiveness:
	//   turn_state_hit=true   → our cache fed prior state into the request
	//   upstream_turn_state_returned=true → upstream emitted fresh state we'll cache for the next turn
	UpstreamTurnStateReturned bool
}

type OpenAIForwardResult struct {
	RequestID string
	// ResponseID is the upstream Responses API response id (resp_xxx).
	// Captured from the terminal SSE event so the messages-bridge path can
	// chain the next turn via previous_response_id (058 step 2 continuation).
	ResponseID string
	Usage      OpenAIUsage
	Model      string // 原始模型（用于响应和日志显示）
	// BillingModel is the model used for cost calculation.
	// When non-empty, CalculateCost uses this instead of Model.
	// This is set by the Anthropic Messages conversion path where
	// the mapped upstream model differs from the client-facing model.
	BillingModel string
	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Empty when no mapping was applied (requested model was used as-is).
	UpstreamModel string
	// UpstreamResponseModel is captured from the raw successful upstream
	// response before any client-facing rewrite or protocol conversion.
	UpstreamResponseModel         string
	UpstreamResponseModelConflict bool
	// UpstreamResponseServiceTier is the tier the upstream reports having used
	// (response service_tier: "priority" / "default" / "flex" / ...); "" when not declared.
	UpstreamResponseServiceTier string
	// UpstreamEndpoint is the actual upstream API path used for this request.
	// It avoids guessing when one downstream protocol can use multiple upstream endpoints.
	UpstreamEndpoint string
	// ServiceTier is the final tier sent upstream after policy rewriting.
	// The upstream response declaration remains separate above and is reconciled
	// at usage-recording time, where the credential protocol is available.
	ServiceTier *string
	// ReasoningEffort is extracted from request body (reasoning.effort) or derived from model suffix
	// after group policy rewriting and model-family remapping.
	// Stored for usage records display; nil means not provided / not applicable.
	ReasoningEffort *string
	// RequestedReasoningEffort is client intent before policy and model-family mapping.
	RequestedReasoningEffort *string
	Stream                   bool
	OpenAIWSMode             bool
	// UpstreamTerminalEvent records the normalized terminal Responses event.
	UpstreamTerminalEvent string
	ResponseHeaders       http.Header
	Duration              time.Duration
	FirstTokenMs          *int
	// FirstMeaningfulMs — codex round 11ak (2026-05-15): time-to-first
	// meaningful event (content_block_delta / tool_use / message_delta with
	// usage / message_stop / error). 跟 FirstTokenMs 区分: FirstTokenMs
	// 是首个上游 SSE 字节, FirstMeaningfulMs 是首个 *有效* 业务事件. 大
	// 上下文请求 ratio FirstMeaningfulMs/FirstTokenMs 大说明上游空转累计
	// metadata 久. nil = 没见到 meaningful (timeout 路径).
	FirstMeaningfulMs  *int
	ClientDisconnect   bool
	ImageCount         int
	ImageSize          string
	HasToolCall        bool
	ImageInputSize     string
	ImageOutputSize    string
	ImageOutputSizes   []string
	ImageSizeSource    string
	ImageSizeBreakdown map[string]int
	VideoCount         int
	VideoResolution    string
	// VideoDurationSeconds 是提交时请求的生成时长（xAI 按输出秒数计费），已归一化到 1-15 秒。
	VideoDurationSeconds int
	// WebSearchCalls is the number of Codex alpha/search calls (per-call billing).
	WebSearchCalls int
	// SearchCount is the Grok-native search tool call count (per-1k billing).
	SearchCount int
	// AudioUsage carries Grok Voice billing units when present.
	AudioUsage *AudioUsage

	wsReplayInput                []json.RawMessage
	wsReplayInputExists          bool
	wsAccountFailoverReplayInput []json.RawMessage
}

// SetActualOpenAIUpstreamEndpoint records the endpoint selected by the current
// forwarding attempt. It covers error paths where no OpenAIForwardResult is
// available for usage and operations logging.
func SetActualOpenAIUpstreamEndpoint(c *gin.Context, endpoint string) {
	if c == nil {
		return
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		c.Set(openAIUpstreamEndpointContextKey, endpoint)
	}
}

// ClearActualOpenAIUpstreamEndpoint 清理当前转发尝试记录的端点。
// Handler 会在账号 failover 尝试间复用同一个 Gin context，因此每次尝试
// 都必须从无残留状态开始。
func ClearActualOpenAIUpstreamEndpoint(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openAIUpstreamEndpointContextKey, "")
}

// GetActualOpenAIUpstreamEndpoint returns the endpoint recorded by the latest
// forwarding attempt in this request.
func GetActualOpenAIUpstreamEndpoint(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(openAIUpstreamEndpointContextKey)
	if !exists {
		return ""
	}
	endpoint, _ := value.(string)
	return strings.TrimSpace(endpoint)
}

type OpenAIWSRetryMetricsSnapshot struct {
	RetryAttemptsTotal            int64 `json:"retry_attempts_total"`
	RetryBackoffMsTotal           int64 `json:"retry_backoff_ms_total"`
	RetryExhaustedTotal           int64 `json:"retry_exhausted_total"`
	NonRetryableFastFallbackTotal int64 `json:"non_retryable_fast_fallback_total"`
}

type OpenAICompatibilityFallbackMetricsSnapshot struct {
	SessionHashLegacyReadFallbackTotal int64   `json:"session_hash_legacy_read_fallback_total"`
	SessionHashLegacyReadFallbackHit   int64   `json:"session_hash_legacy_read_fallback_hit"`
	SessionHashLegacyDualWriteTotal    int64   `json:"session_hash_legacy_dual_write_total"`
	SessionHashLegacyReadHitRate       float64 `json:"session_hash_legacy_read_hit_rate"`

	MetadataLegacyFallbackIsMaxTokensOneHaikuTotal int64 `json:"metadata_legacy_fallback_is_max_tokens_one_haiku_total"`
	MetadataLegacyFallbackThinkingEnabledTotal     int64 `json:"metadata_legacy_fallback_thinking_enabled_total"`
	MetadataLegacyFallbackPrefetchedStickyAccount  int64 `json:"metadata_legacy_fallback_prefetched_sticky_account_total"`
	MetadataLegacyFallbackPrefetchedStickyGroup    int64 `json:"metadata_legacy_fallback_prefetched_sticky_group_total"`
	MetadataLegacyFallbackSingleAccountRetryTotal  int64 `json:"metadata_legacy_fallback_single_account_retry_total"`
	MetadataLegacyFallbackAccountSwitchCountTotal  int64 `json:"metadata_legacy_fallback_account_switch_count_total"`
	MetadataLegacyFallbackTotal                    int64 `json:"metadata_legacy_fallback_total"`
}

type openAIWSRetryMetrics struct {
	retryAttempts            atomic.Int64
	retryBackoffMs           atomic.Int64
	retryExhausted           atomic.Int64
	nonRetryableFastFallback atomic.Int64
}

type accountWriteThrottle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastByID    map[int64]time.Time
}

func newAccountWriteThrottle(minInterval time.Duration) *accountWriteThrottle {
	return &accountWriteThrottle{
		minInterval: minInterval,
		lastByID:    make(map[int64]time.Time),
	}
}

func (t *accountWriteThrottle) Allow(id int64, now time.Time) bool {
	if t == nil || id <= 0 || t.minInterval <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.lastByID[id]; ok && now.Sub(last) < t.minInterval {
		return false
	}
	t.lastByID[id] = now

	if len(t.lastByID) > 4096 {
		cutoff := now.Add(-4 * t.minInterval)
		for accountID, writtenAt := range t.lastByID {
			if writtenAt.Before(cutoff) {
				delete(t.lastByID, accountID)
			}
		}
	}

	return true
}

var defaultOpenAICodexSnapshotPersistThrottle = newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval)

// ErrNoAvailableCompactAccounts indicates a legacy /responses/compact request
// needs compact support but no compatible account is available.
var ErrNoAvailableCompactAccounts = errors.New("no available accounts support /responses/compact")

// OpenAIGatewayService handles OpenAI API gateway operations
type OpenAIGatewayService struct {
	accountRepo           AccountRepository
	usageLogRepo          UsageLogRepository
	usageBillingRepo      UsageBillingRepository
	userRepo              UserRepository
	userSubRepo           UserSubscriptionRepository
	cache                 GatewayCache
	cfg                   *config.Config
	codexDetector         CodexClientRestrictionDetector
	schedulerSnapshot     *SchedulerSnapshotService
	concurrencyService    *ConcurrencyService
	billingService        *BillingService
	rateLimitService      *RateLimitService
	billingCacheService   *BillingCacheService
	userGroupRateResolver *userGroupRateResolver
	httpUpstream          HTTPUpstream
	pluginManager         *PluginManager
	deferredService       *DeferredService
	openAITokenProvider   *OpenAITokenProvider
	grokTokenProvider     *GrokTokenProvider
	toolCorrector         *CodexToolCorrector
	openaiWSResolver      OpenAIWSProtocolResolver
	resolver              *ModelPricingResolver
	channelService        *ChannelService
	balanceNotifyService  *BalanceNotifyService
	settingService        *SettingService
	userPlatformQuotaRepo UserPlatformQuotaRepository

	openaiWSPoolOnce              sync.Once
	openaiWSStateStoreOnce        sync.Once
	openaiSchedulerOnce           sync.Once
	openaiProxyStreamCircuitOnce  sync.Once
	openaiWSPassthroughDialerOnce sync.Once
	openaiModelTransientOnce      sync.Once
	agentIdentityTaskMu           sync.Mutex
	// codex 5/9 PR#2290 audit #2: 防并发清同一 rate-limited 账号撞 429.
	// 多个请求同时到 recover 路径时 singleflight 让第一个真正 ClearRateLimit,
	// 后续 caller 拿到第一个 result, 避免账号刚清完又被一波并发撞回限流.
	rateLimitRecoveryFlight        singleflight.Group
	openaiWSPool                   *openAIWSConnPool
	openaiWSStateStore             OpenAIWSStateStore
	openaiScheduler                OpenAIAccountScheduler
	openaiWSPassthroughDialer      openAIWSClientDialer
	openaiWSSessionPreemptions     openAIWSSessionPreemptRegistry
	openaiAccountStats             *openAIAccountRuntimeStats
	openaiModelTransient           *openAIAccountModelTransientState
	openaiProxyStreamCircuit       *openAIProxyStreamCircuit
	codexModelsManifestCache       codexModelsManifestCache
	openaiProxyStreamFailOpenLogAt atomic.Int64

	openaiWSFallbackUntil               sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockUntil      sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockLocks      sync.Map // key: int64(accountID), value: *sync.Mutex
	openaiAccountRuntimeBlockGeneration sync.Map // key: int64(accountID), value: uint64
	openaiAccountRuntimeBlockSequence   atomic.Uint64
	openaiOAuth429RetryStartedAt        sync.Map // key: int64(accountID), value: time.Time
	grokCredentialMutationLocks         sync.Map // key: int64(accountID), value: *sync.Mutex
	openaiOAuth429WindowStartUnixNano   atomic.Int64
	openaiOAuth429WindowCount           atomic.Int64
	openaiWSRetryMetrics                openAIWSRetryMetrics
	responseHeaderFilter                *responseheaders.CompiledHeaderFilter
	codexSnapshotThrottle               *accountWriteThrottle

	// 2026-05-06 partial port of upstream 0584305e (Claude Code compat).
	// openai_messages_continuation/digest_session/replay_guard/todo_guard
	// modules need shared per-process state to track previous_response_id
	// continuation chains and digest sessions for Anthropic→Responses
	// conversions. sync.Map fits the read-heavy / occasional-write pattern.
	openaiCompatSessionResponses        sync.Map // session_hash → previous_response_id continuation state
	openaiCompatAnthropicDigestSessions sync.Map // sessionDigest → anthropic digest session state
	// 下游会话 seed → 最近一次下发 x-codex-turn-state 的铸造账号。
	openaiCodexTurnStateOrigins sync.Map
	openaiCodexTurnStateWrites  atomic.Uint64
}

// NewOpenAIGatewayService creates a new OpenAIGatewayService
func NewOpenAIGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	grokTokenProvider *GrokTokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *OpenAIGatewayService {
	// enforceCodexIdentityHeaders 是 HTTP / 透传 / WS / 探针 等出站路径共用的纯函数收口点，
	// 拿不到配置，故在此发布进程级开关快照。配置取反义，零值即「强制统一出口开启」。
	if cfg != nil {
		SetCodexIdentityEnforcementEnabled(!cfg.Gateway.DisableCodexIdentityEnforcement)
	}
	svc := &OpenAIGatewayService{
		accountRepo:         accountRepo,
		usageLogRepo:        usageLogRepo,
		usageBillingRepo:    usageBillingRepo,
		userRepo:            userRepo,
		userSubRepo:         userSubRepo,
		cache:               cache,
		cfg:                 cfg,
		codexDetector:       NewOpenAICodexClientRestrictionDetector(cfg),
		schedulerSnapshot:   schedulerSnapshot,
		concurrencyService:  concurrencyService,
		billingService:      billingService,
		rateLimitService:    rateLimitService,
		billingCacheService: billingCacheService,
		userGroupRateResolver: newUserGroupRateResolver(
			userGroupRateRepo,
			nil,
			resolveUserGroupRateCacheTTL(cfg),
			nil,
			"service.openai_gateway",
		),
		httpUpstream:          httpUpstream,
		deferredService:       deferredService,
		openAITokenProvider:   openAITokenProvider,
		grokTokenProvider:     grokTokenProvider,
		toolCorrector:         NewCodexToolCorrector(),
		openaiWSResolver:      NewOpenAIWSProtocolResolver(cfg),
		resolver:              resolver,
		channelService:        channelService,
		balanceNotifyService:  balanceNotifyService,
		settingService:        settingService,
		userPlatformQuotaRepo: userPlatformQuotaRepo,
		responseHeaderFilter:  compileResponseHeaderFilter(cfg),
		codexSnapshotThrottle: newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval),
		openaiModelTransient:  newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax),
	}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(svc)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(svc)
	}
	svc.logOpenAIWSModeBootstrap()
	svc.logOpenAICompactNonstreamKeepaliveBootstrap()
	return svc
}

// ResolveChannelMapping 解析渠道级模型映射（代理到 ChannelService）
func (s *OpenAIGatewayService) ResolveChannelMapping(ctx context.Context, groupID int64, model string) ChannelMappingResult {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}
	}
	return s.channelService.ResolveChannelMapping(ctx, groupID, model)
}

// IsModelRestricted 检查模型是否被渠道限制（代理到 ChannelService）
func (s *OpenAIGatewayService) IsModelRestricted(ctx context.Context, groupID int64, model string) bool {
	if s.channelService == nil {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, model)
}

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段，restricted 始终返回 false。
func (s *OpenAIGatewayService) ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (ChannelMappingResult, bool) {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}, false
	}
	return s.channelService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
}

func (s *OpenAIGatewayService) isCodexImageGenerationBridgeEnabled(ctx context.Context, account *Account, apiKey *APIKey) bool {
	if enabled, ok := s.codexImageGenerationBridgeExplicitValue(ctx, account, apiKey); ok {
		return enabled
	}
	return s != nil && s.cfg != nil && s.cfg.Gateway.CodexImageGenerationBridgeEnabled
}

func (s *OpenAIGatewayService) codexImageGenerationBridgeForcedEnabled(ctx context.Context, account *Account, apiKey *APIKey) bool {
	enabled, ok := s.codexImageGenerationBridgeExplicitValue(ctx, account, apiKey)
	return ok && enabled
}

func (s *OpenAIGatewayService) codexImageGenerationBridgeExplicitValue(ctx context.Context, account *Account, apiKey *APIKey) (bool, bool) {
	if account != nil && account.Platform == PlatformOpenAI {
		if enabled, ok := resolveAccountExtraBoolValue(account.Extra, featureKeyCodexImageGenerationBridge); ok {
			return enabled, true
		}
		if enabled, ok := resolveNestedAccountExtraBoolValue(account.Extra, PlatformOpenAI, "codex_image_generation_bridge_enabled"); ok {
			return enabled, true
		}
	}
	if s != nil && s.channelService != nil && apiKey != nil && apiKey.GroupID != nil {
		if ch, err := s.channelService.GetChannelForGroup(ctx, *apiKey.GroupID); err == nil && ch != nil {
			if enabled, ok := ch.CodexImageGenerationBridgeEnabled(PlatformOpenAI); ok {
				return enabled, true
			}
		}
	}
	return false, false
}

func (s *OpenAIGatewayService) checkChannelPricingRestriction(ctx context.Context, groupID *int64, requestedModel string) bool {
	if groupID == nil || s.channelService == nil || requestedModel == "" {
		return false
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, *groupID, requestedModel)
	billingModel := billingModelForRestriction(mapping.BillingModelSource, requestedModel, mapping.MappedModel)
	if billingModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, *groupID, billingModel)
}

func (s *OpenAIGatewayService) isUpstreamModelRestrictedByChannel(ctx context.Context, groupID int64, account *Account, requestedModel string, requireCompact bool) bool {
	if s.channelService == nil {
		return false
	}
	if compactForwardModel, ok := openAIForwardModelFromContext(ctx); ok {
		requestedModel = compactForwardModel.model
		requireCompact = compactForwardModel.useCompactModelMapping
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)
	if upstreamModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, upstreamModel)
}

func (s *OpenAIGatewayService) needsUpstreamChannelRestrictionCheck(ctx context.Context, groupID *int64) bool {
	if groupID == nil || s.channelService == nil {
		return false
	}
	ch, err := s.channelService.GetChannelForGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("failed to check openai channel upstream restriction", "group_id", *groupID, "error", err)
		return false
	}
	if ch == nil || !ch.RestrictModels {
		return false
	}
	return ch.BillingModelSource == BillingModelSourceUpstream
}

// ReplaceModelInBody 替换请求体中的 JSON model 字段（通用 gjson/sjson 实现）。
func (s *OpenAIGatewayService) ReplaceModelInBody(body []byte, newModel string) []byte {
	return ReplaceModelInBody(body, newModel)
}

func (s *OpenAIGatewayService) getCodexSnapshotThrottle() *accountWriteThrottle {
	if s != nil && s.codexSnapshotThrottle != nil {
		return s.codexSnapshotThrottle
	}
	return defaultOpenAICodexSnapshotPersistThrottle
}

func (s *OpenAIGatewayService) billingDeps() *billingDeps {
	return &billingDeps{
		accountRepo:           s.accountRepo,
		userRepo:              s.userRepo,
		userSubRepo:           s.userSubRepo,
		billingCacheService:   s.billingCacheService,
		deferredService:       s.deferredService,
		balanceNotifyService:  s.balanceNotifyService,
		userPlatformQuotaRepo: s.userPlatformQuotaRepo,
	}
}

// CloseOpenAIWSPool 关闭 OpenAI WebSocket 连接池的后台 worker 和空闲连接。
// 应在应用优雅关闭时调用。
func (s *OpenAIGatewayService) CloseOpenAIWSPool() {
	if s != nil && s.openaiWSPool != nil {
		s.openaiWSPool.Close()
	}
}

// InvalidateAgentIdentityWSConnections closes cached WebSocket connections so
// the next request observes a newly rotated Agent Identity credential.
func (s *OpenAIGatewayService) InvalidateAgentIdentityWSConnections(accountID int64) {
	if pool := s.getOpenAIWSConnPool(); pool != nil {
		pool.ClearAccount(accountID)
	}
}

func (s *OpenAIGatewayService) logOpenAIWSModeBootstrap() {
	if s == nil || s.cfg == nil {
		return
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	logOpenAIWSModeInfo(
		"bootstrap enabled=%v oauth_enabled=%v apikey_enabled=%v force_http=%v responses_websockets_v2=%v responses_websockets=%v payload_log_sample_rate=%.3f event_flush_batch_size=%d event_flush_interval_ms=%d prewarm_cooldown_ms=%d retry_backoff_initial_ms=%d retry_backoff_max_ms=%d retry_jitter_ratio=%.3f retry_total_budget_ms=%d ws_read_limit_bytes=%d",
		wsCfg.Enabled,
		wsCfg.OAuthEnabled,
		wsCfg.APIKeyEnabled,
		wsCfg.ForceHTTP,
		wsCfg.ResponsesWebsocketsV2,
		wsCfg.ResponsesWebsockets,
		wsCfg.PayloadLogSampleRate,
		wsCfg.EventFlushBatchSize,
		wsCfg.EventFlushIntervalMS,
		wsCfg.PrewarmCooldownMS,
		wsCfg.RetryBackoffInitialMS,
		wsCfg.RetryBackoffMaxMS,
		wsCfg.RetryJitterRatio,
		wsCfg.RetryTotalBudgetMS,
		openAIWSMessageReadLimitBytes,
	)
}

func (s *OpenAIGatewayService) logOpenAICompactNonstreamKeepaliveBootstrap() {
	interval := s.compactNonstreamKeepaliveInterval()
	if interval <= 0 {
		return
	}
	logger.L().With(
		zap.String("component", "service.openai_gateway"),
		zap.Int("interval_seconds", int(interval.Seconds())),
	).Info("OpenAI compact non-stream keepalive enabled")
}

func (s *OpenAIGatewayService) getCodexClientRestrictionDetector() CodexClientRestrictionDetector {
	if s != nil && s.codexDetector != nil {
		return s.codexDetector
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAICodexClientRestrictionDetector(cfg)
}

func (s *OpenAIGatewayService) getOpenAIWSProtocolResolver() OpenAIWSProtocolResolver {
	if s != nil && s.openaiWSResolver != nil {
		return s.openaiWSResolver
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAIWSProtocolResolver(cfg)
}

func classifyOpenAIWSReconnectReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return "", false
	}
	reason := strings.TrimSpace(fallbackErr.Reason)
	if reason == "" {
		return "", false
	}

	baseReason := strings.TrimPrefix(reason, "prewarm_")

	switch baseReason {
	case "policy_violation",
		"message_too_big",
		"upgrade_required",
		"ws_unsupported",
		"auth_failed",
		"invalid_encrypted_content",
		"previous_response_not_found":
		return reason, false
	}

	switch baseReason {
	case "read_event",
		"write_request",
		"write",
		"acquire_timeout",
		"acquire_conn",
		"conn_queue_full",
		"dial_failed",
		"upstream_5xx",
		"event_error",
		"error_event",
		"upstream_error_event",
		"ws_connection_limit_reached",
		"missing_final_response":
		return reason, true
	default:
		return reason, false
	}
}

func resolveOpenAIWSFallbackErrorResponse(err error) (statusCode int, errType string, clientMessage string, upstreamMessage string, ok bool) {
	if err == nil {
		return 0, "", "", "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return 0, "", "", "", false
	}

	reason := strings.TrimSpace(fallbackErr.Reason)
	reason = strings.TrimPrefix(reason, "prewarm_")
	if reason == "" {
		return 0, "", "", "", false
	}

	var dialErr *openAIWSDialError
	if fallbackErr.Err != nil && errors.As(fallbackErr.Err, &dialErr) && dialErr != nil {
		if dialErr.StatusCode > 0 {
			statusCode = dialErr.StatusCode
		}
		if dialErr.Err != nil {
			upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(dialErr.Err.Error()))
		}
	}

	switch reason {
	case "invalid_encrypted_content":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "encrypted content could not be verified"
		}
	case "previous_response_not_found":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "previous response not found"
		}
	case "upgrade_required":
		if statusCode == 0 {
			statusCode = http.StatusUpgradeRequired
		}
	case "ws_unsupported":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
	case "auth_failed":
		if statusCode == 0 {
			statusCode = http.StatusUnauthorized
		}
	case "upstream_rate_limited":
		if statusCode == 0 {
			statusCode = http.StatusTooManyRequests
		}
	default:
		if statusCode == 0 {
			return 0, "", "", "", false
		}
	}

	if upstreamMessage == "" && fallbackErr.Err != nil {
		upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(fallbackErr.Err.Error()))
	}
	if upstreamMessage == "" {
		switch reason {
		case "upgrade_required":
			upstreamMessage = "upstream websocket upgrade required"
		case "ws_unsupported":
			upstreamMessage = "upstream websocket not supported"
		case "auth_failed":
			upstreamMessage = "upstream authentication failed"
		case "upstream_rate_limited":
			upstreamMessage = "upstream rate limit exceeded, please retry later"
		default:
			upstreamMessage = "Upstream request failed"
		}
	}

	if errType == "" {
		if statusCode == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else {
			errType = "upstream_error"
		}
	}
	clientMessage = upstreamMessage
	return statusCode, errType, clientMessage, upstreamMessage, true
}

func (s *OpenAIGatewayService) writeOpenAIWSFallbackErrorResponse(c *gin.Context, account *Account, wsErr error) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	statusCode, errType, clientMessage, upstreamMessage, ok := resolveOpenAIWSFallbackErrorResponse(wsErr)
	if !ok {
		return false
	}
	if strings.TrimSpace(clientMessage) == "" {
		clientMessage = "Upstream request failed"
	}
	if strings.TrimSpace(upstreamMessage) == "" {
		upstreamMessage = clientMessage
	}

	setOpsUpstreamError(c, statusCode, upstreamMessage, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusCode,
			Kind:               "ws_error",
			Message:            upstreamMessage,
		})
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": clientMessage,
		},
	})
	return true
}

func (s *OpenAIGatewayService) openAIWSRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}

	initial := openAIWSRetryBackoffInitialDefault
	maxBackoff := openAIWSRetryBackoffMaxDefault
	jitterRatio := openAIWSRetryJitterRatioDefault
	if s != nil && s.cfg != nil {
		wsCfg := s.cfg.Gateway.OpenAIWS
		if wsCfg.RetryBackoffInitialMS > 0 {
			initial = time.Duration(wsCfg.RetryBackoffInitialMS) * time.Millisecond
		}
		if wsCfg.RetryBackoffMaxMS > 0 {
			maxBackoff = time.Duration(wsCfg.RetryBackoffMaxMS) * time.Millisecond
		}
		if wsCfg.RetryJitterRatio >= 0 {
			jitterRatio = wsCfg.RetryJitterRatio
		}
	}
	if initial <= 0 {
		return 0
	}
	if maxBackoff <= 0 {
		maxBackoff = initial
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	if jitterRatio > 1 {
		jitterRatio = 1
	}

	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	backoff := initial
	if shift > 0 {
		backoff = initial * time.Duration(1<<shift)
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if jitterRatio <= 0 {
		return backoff
	}
	jitter := time.Duration(float64(backoff) * jitterRatio)
	if jitter <= 0 {
		return backoff
	}
	delta := time.Duration(rand.Int63n(int64(jitter)*2+1)) - jitter
	withJitter := backoff + delta
	if withJitter < 0 {
		return 0
	}
	return withJitter
}

func (s *OpenAIGatewayService) openAIWSRetryTotalBudget() time.Duration {
	if s != nil && s.cfg != nil {
		ms := s.cfg.Gateway.OpenAIWS.RetryTotalBudgetMS
		if ms <= 0 {
			return 0
		}
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryAttempt(backoff time.Duration) {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryAttempts.Add(1)
	if backoff > 0 {
		s.openaiWSRetryMetrics.retryBackoffMs.Add(backoff.Milliseconds())
	}
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryExhausted() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryExhausted.Add(1)
}

func (s *OpenAIGatewayService) recordOpenAIWSNonRetryableFastFallback() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.nonRetryableFastFallback.Add(1)
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSRetryMetrics() OpenAIWSRetryMetricsSnapshot {
	if s == nil {
		return OpenAIWSRetryMetricsSnapshot{}
	}
	return OpenAIWSRetryMetricsSnapshot{
		RetryAttemptsTotal:            s.openaiWSRetryMetrics.retryAttempts.Load(),
		RetryBackoffMsTotal:           s.openaiWSRetryMetrics.retryBackoffMs.Load(),
		RetryExhaustedTotal:           s.openaiWSRetryMetrics.retryExhausted.Load(),
		NonRetryableFastFallbackTotal: s.openaiWSRetryMetrics.nonRetryableFastFallback.Load(),
	}
}

func SnapshotOpenAICompatibilityFallbackMetrics() OpenAICompatibilityFallbackMetricsSnapshot {
	legacyReadFallbackTotal, legacyReadFallbackHit, legacyDualWriteTotal := openAIStickyCompatStats()
	isMaxTokensOneHaiku, thinkingEnabled, prefetchedStickyAccount, prefetchedStickyGroup, singleAccountRetry, accountSwitchCount := RequestMetadataFallbackStats()

	readHitRate := float64(0)
	if legacyReadFallbackTotal > 0 {
		readHitRate = float64(legacyReadFallbackHit) / float64(legacyReadFallbackTotal)
	}
	metadataFallbackTotal := isMaxTokensOneHaiku + thinkingEnabled + prefetchedStickyAccount + prefetchedStickyGroup + singleAccountRetry + accountSwitchCount

	return OpenAICompatibilityFallbackMetricsSnapshot{
		SessionHashLegacyReadFallbackTotal: legacyReadFallbackTotal,
		SessionHashLegacyReadFallbackHit:   legacyReadFallbackHit,
		SessionHashLegacyDualWriteTotal:    legacyDualWriteTotal,
		SessionHashLegacyReadHitRate:       readHitRate,

		MetadataLegacyFallbackIsMaxTokensOneHaikuTotal: isMaxTokensOneHaiku,
		MetadataLegacyFallbackThinkingEnabledTotal:     thinkingEnabled,
		MetadataLegacyFallbackPrefetchedStickyAccount:  prefetchedStickyAccount,
		MetadataLegacyFallbackPrefetchedStickyGroup:    prefetchedStickyGroup,
		MetadataLegacyFallbackSingleAccountRetryTotal:  singleAccountRetry,
		MetadataLegacyFallbackAccountSwitchCountTotal:  accountSwitchCount,
		MetadataLegacyFallbackTotal:                    metadataFallbackTotal,
	}
}

func (s *OpenAIGatewayService) detectCodexClientRestriction(c *gin.Context, account *Account, body []byte) CodexClientRestrictionDetectionResult {
	// 安全默认：即便缺 settingService（仅测试/误配可达）也保持指纹门为默认种子，
	// 避免零值 policy（nil 信号）让指纹门失败开放。有 settingService 时整体覆盖为全局策略。
	policy := CodexRestrictionPolicy{EngineFingerprintSignals: openai.DefaultEngineFingerprintSignals}
	if account != nil && account.IsCodexCLIOnlyEnabled() && s != nil && s.settingService != nil {
		ctx := context.Background()
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		policy = s.settingService.GetCodexRestrictionPolicy(ctx)
	}
	return s.getCodexClientRestrictionDetector().Detect(c, account, policy, body)
}

func getAPIKeyIDFromContext(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	v, exists := c.Get("api_key")
	if !exists {
		return 0
	}
	apiKey, ok := v.(*APIKey)
	if !ok || apiKey == nil {
		return 0
	}
	return apiKey.ID
}

// isolateOpenAISessionID 将 apiKeyID 混入 session 标识符，
// 确保不同 API Key 的用户即使使用相同的原始 session_id/conversation_id，
// 到达上游的标识符也不同，防止跨用户会话碰撞。
func isolateOpenAISessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	h := xxhash.New()
	_, _ = fmt.Fprintf(h, "k%d:", apiKeyID)
	_, _ = h.WriteString(raw)
	return fmt.Sprintf("%016x", h.Sum64())
}

func logCodexCLIOnlyDetection(ctx context.Context, c *gin.Context, account *Account, apiKeyID int64, result CodexClientRestrictionDetectionResult, body []byte) {
	if !result.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.Bool("codex_cli_only_enabled", result.Enabled),
		zap.Bool("codex_official_client_match", result.Matched),
		zap.String("reject_reason", result.Reason),
	}
	if apiKeyID > 0 {
		fields = append(fields, zap.Int64("api_key_id", apiKeyID))
	}
	if !result.Matched {
		fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	}
	log := logger.FromContext(ctx).With(fields...)
	if result.Matched {
		log.Info("OpenAI codex_cli_only 放行请求")
		return
	}
	log.Warn("OpenAI codex_cli_only 拒绝非官方客户端请求")
}

func appendCodexCLIOnlyRejectedRequestFields(fields []zap.Field, c *gin.Context, body []byte) []zap.Field {
	if c == nil || c.Request == nil {
		return fields
	}

	req := c.Request
	requestModel, requestStream, promptCacheKey := extractOpenAIRequestMetaFromBody(body)
	fields = append(fields,
		zap.String("request_method", strings.TrimSpace(req.Method)),
		zap.String("request_path", strings.TrimSpace(req.URL.Path)),
		zap.String("request_query", strings.TrimSpace(req.URL.RawQuery)),
		zap.String("request_host", strings.TrimSpace(req.Host)),
		zap.String("request_client_ip", strings.TrimSpace(ip.GetClientIP(c))),
		zap.String("request_remote_addr", strings.TrimSpace(req.RemoteAddr)),
		zap.String("request_user_agent", strings.TrimSpace(req.Header.Get("User-Agent"))),
		zap.String("request_content_type", strings.TrimSpace(req.Header.Get("Content-Type"))),
		zap.Int64("request_content_length", req.ContentLength),
		zap.Bool("request_stream", requestStream),
	)
	if requestModel != "" {
		fields = append(fields, zap.String("request_model", requestModel))
	}
	if promptCacheKey != "" {
		fields = append(fields, zap.String("request_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)))
	}

	if headers := snapshotCodexCLIOnlyHeaders(req.Header); len(headers) > 0 {
		fields = append(fields, zap.Any("request_headers", headers))
	}
	fields = append(fields, zap.Int("request_body_size", len(body)))
	return fields
}

func snapshotCodexCLIOnlyHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	result := make(map[string]string, len(codexCLIOnlyDebugHeaderWhitelist))
	for _, key := range codexCLIOnlyDebugHeaderWhitelist {
		value := strings.TrimSpace(header.Get(key))
		if value == "" {
			continue
		}
		result[strings.ToLower(key)] = truncateString(value, codexCLIOnlyHeaderValueMaxBytes)
	}
	return result
}

func hashSensitiveValueForLog(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func logOpenAIInstructionsRequiredDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) {
	msg := strings.TrimSpace(upstreamMsg)
	if !isOpenAIInstructionsRequiredError(upstreamStatusCode, msg, upstreamBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_message", msg),
		zap.String("request_user_agent", userAgent),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, requestBody)

	logger.FromContext(ctx).With(fields...).Warn("OpenAI 上游返回 Instructions are required，已记录请求详情用于排查")
}

func isOpenAIInstructionsRequiredError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	hasInstructionRequired := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "instructions are required") {
			return true
		}
		if strings.Contains(lower, "required parameter: 'instructions'") {
			return true
		}
		if strings.Contains(lower, "required parameter: instructions") {
			return true
		}
		if strings.Contains(lower, "missing required parameter") && strings.Contains(lower, "instructions") {
			return true
		}
		return strings.Contains(lower, "instruction") && strings.Contains(lower, "required")
	}

	if hasInstructionRequired(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}

	errMsg := gjson.GetBytes(upstreamBody, "error.message").String()
	errMsgLower := strings.ToLower(strings.TrimSpace(errMsg))
	errCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String()))
	errParam := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.type").String()))

	if errParam == "instructions" {
		return true
	}
	if hasInstructionRequired(errMsg) {
		return true
	}
	if strings.Contains(errCode, "missing_required_parameter") && strings.Contains(errMsgLower, "instructions") {
		return true
	}
	if strings.Contains(errType, "invalid_request") && strings.Contains(errMsgLower, "instructions") && strings.Contains(errMsgLower, "required") {
		return true
	}

	return false
}

func isOpenAITransientProcessingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest && upstreamStatusCode != http.StatusServiceUnavailable {
		return false
	}

	hasOpenAIServerOverloadedCode := func(payload []byte) bool {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
		}
		return code == "server_is_overloaded" || code == "slow_down"
	}

	if len(upstreamBody) > 0 && hasOpenAIServerOverloadedCode(upstreamBody) {
		return true
	}
	// A capacity shed can arrive as 503 plain text or as a structured error.
	// For valid JSON, inspect only authoritative message fields so echoed
	// request content cannot manufacture a transient classification.
	if isOpenAICapacityShedMessage(upstreamMsg) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "error.message").String()) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "response.error.message").String()) ||
		(!gjson.ValidBytes(upstreamBody) && isOpenAICapacityShedMessage(string(upstreamBody))) {
		return true
	}
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "an error occurred while processing your request") {
			return true
		}
		// codex upstream PR#2481 (2026-05-16): "Selected model is at capacity"
		// is OpenAI's transient capacity rejection (returned as 400
		// invalid_request, not 429/5xx). Treat as account/model-temporary
		// unavailable so failover/same-account retry kicks in.
		if strings.Contains(lower, "selected model is at capacity") {
			return true
		}
		return strings.Contains(lower, "you can retry your request") &&
			strings.Contains(lower, "help.openai.com") &&
			strings.Contains(lower, "request id")
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if match(gjson.GetBytes(upstreamBody, "error.message").String()) {
		return true
	}
	if match(gjson.GetBytes(upstreamBody, "response.error.message").String()) ||
		match(gjson.GetBytes(upstreamBody, "message").String()) {
		return true
	}
	// A valid JSON error may echo arbitrary request content. Only its explicit
	// error fields are authoritative; scan the whole body only for non-JSON
	// providers that return a plain-text error response.
	return !gjson.ValidBytes(upstreamBody) && match(string(upstreamBody))
}

func isOpenAIContextWindowError(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") {
			return true
		}
		if strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
			return true
		}
		hasExceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
		if strings.Contains(lower, "context window") && hasExceeded {
			return true
		}
		if strings.Contains(lower, "context length") && hasExceeded {
			return true
		}
		return strings.Contains(lower, "token limit") &&
			strings.Contains(lower, "context") &&
			hasExceeded
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
		"error.code",
		"response.error.code",
		"code",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	// Do not let echoed request content in a structured JSON error change the
	// retry/client-status classification. Plain-text upstream errors remain
	// supported by scanning the whole body only when it is not valid JSON.
	return !gjson.ValidBytes(upstreamBody) && match(string(upstreamBody))
}

type AnthropicMessageSessionContext struct {
	PromptCacheKey string
	SessionHash    string
}

const (
	openCodeSessionIDHeader     = "X-Session-Id"
	openCodeNativeSessionHeader = "X-OpenCode-Session"
	codeBuddyConversationHeader = "X-Conversation-ID"
)

// extractOpenAIExtendedSessionHeader resolves conversation-scoped identifiers
// used by OpenCode and CodeBuddy. Keep the existing conversation_id/session_id
// and prompt_cache_key priority unchanged; these headers only fill the gap
// before the weaker x-session-affinity fallback.
func extractOpenAIExtendedSessionHeader(c *gin.Context) string {
	if c == nil {
		return ""
	}
	for _, header := range []string{
		openCodeSessionIDHeader,
		openCodeNativeSessionHeader,
		codeBuddyConversationHeader,
	} {
		if sessionID := strings.TrimSpace(c.GetHeader(header)); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

// ExtractSessionID extracts the raw session ID from headers or body without hashing.
// Used by ForwardAsAnthropic to pass as prompt_cache_key for upstream cache.
func (s *OpenAIGatewayService) ExtractSessionID(c *gin.Context, body []byte) string {
	sessionID := extractOpenAIStickySessionSignal(c, body)
	if sessionID == "" && c != nil {
		sessionID = strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id"))
	}
	return sessionID
}

func (s *OpenAIGatewayService) ResolveAnthropicMessageSessionContext(c *gin.Context, model string, body []byte) AnthropicMessageSessionContext {
	resolvedSessionID := s.ExtractSessionID(c, body)
	sessionHash := s.GenerateSessionHash(c, body)
	if sessionHash == "" && resolvedSessionID != "" {
		sessionHash = DeriveSessionHashFromSeed(resolvedSessionID)
	}
	sessionCtx := AnthropicMessageSessionContext{
		PromptCacheKey: resolvedSessionID,
		SessionHash:    sessionHash,
	}
	if len(body) == 0 {
		return sessionCtx
	}

	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return sessionCtx
	}

	// 058 step 2: metadata.user_id only seeds the sticky-session signal
	// (account selection). It must NOT pin PromptCacheKey — letting
	// ForwardAsAnthropic derive the upstream prompt cache key from
	// cache_control breakpoints or the message digest is what allows the
	// cached prefix to roll forward across multi-turn conversations.
	// PromptCacheKey is only populated by an *explicit* signal (a literal
	// prompt_cache_key, X-Claude-Code-Session-Id header, etc.).
	if parsedUserID := ParseMetadataUserID(userID); parsedUserID != nil {
		metadataSessionID := strings.TrimSpace(parsedUserID.SessionID)
		if metadataSessionID == "" {
			return sessionCtx
		}
		if sessionCtx.SessionHash == "" {
			sessionCtx.SessionHash = DeriveSessionHashFromSeed(metadataSessionID)
		}
		return sessionCtx
	}

	seed := strings.TrimSpace(model) + "-" + userID
	if strings.TrimSpace(seed) == "-" {
		return sessionCtx
	}
	if sessionCtx.SessionHash == "" {
		sessionCtx.SessionHash = DeriveSessionHashFromSeed(seed)
	}
	return sessionCtx
}

func extractOpenAIStickySessionSignal(c *gin.Context, body []byte) string {
	if c != nil {
		if conversationID := strings.TrimSpace(c.GetHeader("conversation_id")); conversationID != "" {
			return conversationID
		}
		if sessionID := strings.TrimSpace(c.GetHeader("session_id")); sessionID != "" {
			return sessionID
		}
		if extendedSessionID := extractOpenAIExtendedSessionHeader(c); extendedSessionID != "" {
			return extendedSessionID
		}
		// xAI's native conversation header is a sticky/cache signal only for
		// requests resolved to Grok. Ignore it for every other platform so a
		// spoofed header cannot change OpenAI account affinity.
		if isGrokRequestContext(c) {
			if conversationID := strings.TrimSpace(c.GetHeader(grokConversationIDHeader)); conversationID != "" {
				return conversationID
			}
		}
	}
	if len(body) > 0 {
		if promptCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); promptCacheKey != "" {
			return promptCacheKey
		}
		if isGrokRequestContext(c) {
			if previousResponseSeed := grokPreviousResponseSessionSeed(body); previousResponseSeed != "" {
				return previousResponseSeed
			}
		}
	}
	if c != nil {
		if sessionAffinity := strings.TrimSpace(c.GetHeader("x-session-affinity")); sessionAffinity != "" {
			return sessionAffinity
		}
	}
	return ""
}

// explicitOpenAISessionID returns only the explicitly client-supplied session
// signal (no content-derived fallback). Used by stateless endpoints such as
// /v1/images via GenerateExplicitSessionHash. Distinct from
// extractOpenAIStickySessionSignal which is the main fork sticky logic and
// also considers x-session-affinity.
func explicitOpenAISessionID(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}

	sessionID := strings.TrimSpace(c.GetHeader("session_id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("conversation_id"))
	}
	if sessionID == "" {
		sessionID = extractOpenAIExtendedSessionHeader(c)
	}
	if sessionID == "" && len(body) > 0 {
		sessionID = strings.TrimSpace(openAIRequestPayloadView(body).Get("prompt_cache_key").String())
	}
	return sessionID
}

// GenerateExplicitSessionHash generates a sticky-session hash only from explicit
// client session signals. It intentionally skips content-derived fallback and is
// used by stateless endpoints such as /v1/images.
func (s *OpenAIGatewayService) GenerateExplicitSessionHash(c *gin.Context, body []byte) string {
	sessionID := explicitOpenAISessionID(c, body)
	if sessionID == "" {
		return ""
	}
	if isGrokRequestContext(c) {
		sessionID = grokStickyAffinitySeed(sessionID, body)
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// grokStickyAffinitySeed scopes account affinity by requested model without
// changing the separate tenant-isolated upstream prompt-cache identity.
func grokStickyAffinitySeed(sessionID string, body []byte) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	model := ""
	if len(body) > 0 {
		model = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	}
	if model == "" {
		return "grok-affinity:v1:" + sessionID
	}
	return "grok-affinity:v1:" + model + ":" + sessionID
}

// GenerateSessionHash generates a sticky-session hash for OpenAI requests.
//
// Priority:
//  1. Header: conversation_id
//  2. Header: session_id
//  3. Header: x-session-id / x-opencode-session / x-conversation-id
//  4. Body:   prompt_cache_key (opencode)
//  5. Header: x-session-affinity
//  6. Body:   content-based fallback (model + system + tools + first user message)
//
// Why conversation_id comes first:
// Codex clients can keep a long-lived session_id while starting a brand-new
// conversation_id for each fresh thread. If we key sticky state by session_id
// first, a new question can accidentally inherit the previous conversation's
// turn_state / sessionConn / upstream continuation anchor and look like the
// model answered the last prompt again. Using conversation_id when available
// makes the sticky boundary follow the actual conversation switch.
func (s *OpenAIGatewayService) GenerateSessionHash(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}

	sessionID := extractOpenAIStickySessionSignal(c, body)
	if sessionID == "" && len(body) > 0 {
		sessionID = deriveOpenAIContentSessionSeed(body)
	}
	if sessionID == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// GenerateSessionHashWithFallback 先按常规信号生成会话哈希；
// 当未携带 session_id/conversation_id/prompt_cache_key/x-session-affinity 时，使用 fallbackSeed 生成稳定哈希。
// 该方法用于 WS ingress，避免会话信号缺失时发生跨账号漂移。
func (s *OpenAIGatewayService) GenerateSessionHashWithFallback(c *gin.Context, body []byte, fallbackSeed string) string {
	sessionHash := s.GenerateSessionHash(c, body)
	if sessionHash != "" {
		return sessionHash
	}

	seed := strings.TrimSpace(fallbackSeed)
	if seed == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(seed)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

func resolveOpenAIUpstreamOriginator(c *gin.Context, isOfficialClient bool) string {
	if c != nil {
		if originator := strings.TrimSpace(c.GetHeader("originator")); originator != "" {
			return originator
		}
	}
	if isOfficialClient {
		return "codex_cli_rs"
	}
	return "opencode"
}

// BindStickySession sets session -> account binding with standard TTL.
func (s *OpenAIGatewayService) BindStickySession(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 {
		return nil
	}
	ttl := openaiStickySessionTTL
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		ttl = time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return s.setStickySessionAccountID(ctx, groupID, sessionHash, accountID, ttl)
}

// SelectAccount selects an OpenAI account with sticky session support
func (s *OpenAIGatewayService) SelectAccount(ctx context.Context, groupID *int64, sessionHash string) (*Account, error) {
	return s.SelectAccountForModel(ctx, groupID, sessionHash, "")
}

// SelectAccountForModel selects an account supporting the requested model
func (s *OpenAIGatewayService) SelectAccountForModel(ctx context.Context, groupID *int64, sessionHash string, requestedModel string) (*Account, error) {
	return s.SelectAccountForModelWithExclusions(ctx, groupID, sessionHash, requestedModel, nil)
}

// SelectAccountForModelWithExclusions selects an account supporting the requested model while excluding specified accounts.
// SelectAccountForModelWithExclusions 选择支持指定模型的账号，同时排除指定的账号。
func (s *OpenAIGatewayService) SelectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*Account, error) {
	return s.selectAccountForModelWithExclusions(s.withOpenAIQuotaAutoPauseContext(ctx), groupID, sessionHash, requestedModel, excludedIDs, false, 0, "")
}

// SelectAccountForTokenCount applies normal account eligibility without
// acquiring or waiting for a generation slot. Token-count requests are not
// billable and are explicitly outside the profit gate.
func (s *OpenAIGatewayService) SelectAccountForTokenCount(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	requiredCapability OpenAIEndpointCapability,
	platform string,
) (*Account, error) {
	ctx = WithOpenAIProfitControlSuppressed(ctx)
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	return s.selectOpenAICompatibleAccountForModelWithExclusions(
		ctx,
		NormalizeOpenAICompatiblePlatform(platform),
		groupID,
		sessionHash,
		requestedModel,
		nil,
		false,
		0,
		requiredCapability,
		false,
	)
}

// noAvailableOpenAISelectionError builds the standard "no account available" error
// while preserving the compact-specific error when applicable.
func normalizeOpenAICompatiblePlatform(platform string) string {
	return NormalizeOpenAICompatiblePlatform(platform)
}

// NormalizeOpenAICompatiblePlatform preserves OpenAI-compatible providers
// that require exact scheduler matching and normalizes every other value to
// the native OpenAI platform.
func NormalizeOpenAICompatiblePlatform(platform string) string {
	switch platform {
	case PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek:
		return platform
	default:
		return PlatformOpenAI
	}
}

func noAvailableOpenAISelectionError(requestedModel string, compactBlocked bool, details string) error {
	if compactBlocked {
		return ErrNoAvailableCompactAccounts
	}
	message := "no available OpenAI accounts"
	if requestedModel != "" {
		message = fmt.Sprintf("no available OpenAI accounts supporting model: %s", requestedModel)
	}
	if details != "" {
		message += " (" + details + ")"
	}
	return openAINoAvailableSelectionError{message: message}
}

type openAINoAvailableSelectionError struct {
	message string
}

func (e openAINoAvailableSelectionError) Error() string {
	return e.message
}

func (e openAINoAvailableSelectionError) Unwrap() error {
	return ErrNoAvailableAccounts
}

// openAICompactSupportTier classifies an OpenAI-compatible account by compact capability.
// 0 = explicitly unsupported, 1 = unknown / not yet probed, 2 = explicitly supported.
func openAICompactSupportTier(account *Account) int {
	if account == nil {
		return 0
	}
	if account.IsGrok() {
		return 2
	}
	if !account.IsOpenAI() {
		return 0
	}
	supported, known := account.OpenAICompactSupportKnown()
	if !known {
		return 1
	}
	if supported {
		return 2
	}
	return 0
}

func isOpenAICompatibleAccountEligibleForRequest(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) bool {
	return openAICompatibleAccountEligibilityFailureReason(ctx, account, platform, requestedModel, requireCompact, requiredCapability) == ""
}

func openAICompatibleAccountEligibilityFailureReason(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) string {
	if reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, account, platform, requestedModel, requireCompact, requiredCapability); reason != "" {
		return reason
	}
	if vetoed, reason := openAIProfitControlVetoReason(ctx, account); vetoed {
		return reason
	}
	return ""
}

// isOpenAICompatibleAccountEligibleForRequestBeforeProfit applies the ordinary
// account gates before profit is classified. Keeping the phases separate lets
// legacy selection report the real pre-profit exclusion reason.
func isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) bool {
	return openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, account, platform, requestedModel, requireCompact, requiredCapability) == ""
}

func openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) string {
	platform = normalizeOpenAICompatiblePlatform(platform)
	if account == nil {
		return "account_nil"
	}
	if account.Platform != platform || !account.IsOpenAICompatible() {
		return "platform_mismatch"
	}
	if !account.IsSchedulableForModelWithContext(ctx, requestedModel) {
		if account.IsSchedulable() {
			return "model_rate_limited"
		}
		return "not_schedulable"
	}
	if account.IsOpenAI() {
		if paused, reason := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
			// Debug level: this fires per-candidate on the scheduling hot path, so Info
			// would amplify into log spam once several accounts cross the threshold.
			slog.Debug("account_auto_paused_by_quota",
				"account_id", account.ID,
				"window", reason.window,
				"threshold", reason.threshold,
				"utilization", reason.utilization,
			)
			if reason.window != "" {
				return "quota_auto_pause_" + reason.window
			}
			return "quota_auto_pause"
		}
	}
	if account.IsGrok() {
		if paused, reason := shouldAutoPauseGrokAccountByQuota(account); paused {
			slog.Debug("grok_account_auto_paused_by_quota",
				"account_id", account.ID,
				"window", reason.window,
				"threshold", reason.threshold,
				"utilization", reason.utilization,
			)
			if reason.window != "" {
				return "quota_auto_pause_" + reason.window
			}
			return "quota_auto_pause"
		}
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return "model_not_supported"
	}
	if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
		return "capability_mismatch"
	}
	if requireCompact && openAICompactSupportTier(account) == 0 {
		return "compact_unsupported"
	}
	return ""
}

func isOpenAIAccountEligibleForRequest(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) bool {
	return isOpenAICompatibleAccountEligibleForRequest(ctx, account, PlatformOpenAI, requestedModel, requireCompact, requiredCapability)
}

type openAIQuotaAutoPauseDecision struct {
	window      string
	threshold   float64
	utilization float64
	reason      string
}

func shouldAutoPauseGrokAccountByQuota(account *Account) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil || !account.IsGrok() || account.Type != AccountTypeOAuth {
		return false, openAIQuotaAutoPauseDecision{}
	}
	snapshot, err := grokQuotaSnapshotFromExtra(account.Extra)
	if err != nil || snapshot == nil {
		return false, openAIQuotaAutoPauseDecision{}
	}
	now := time.Now()
	if grokQuotaSnapshotStaleForPause(snapshot, now) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	if grokQuotaRetryAfterActive(snapshot, now) {
		return true, openAIQuotaAutoPauseDecision{window: "retry_after", threshold: 1, utilization: 1}
	}
	if paused, decision := shouldAutoPauseGrokQuotaWindow("requests", snapshot.Requests, now); paused {
		return true, decision
	}
	if paused, decision := shouldAutoPauseGrokQuotaWindow("tokens", snapshot.Tokens, now); paused {
		return true, decision
	}
	return false, openAIQuotaAutoPauseDecision{}
}

func grokQuotaRetryAfterActive(snapshot *xai.QuotaSnapshot, now time.Time) bool {
	if snapshot == nil || snapshot.RetryAfterSeconds == nil || *snapshot.RetryAfterSeconds <= 0 {
		return false
	}
	if strings.TrimSpace(snapshot.UpdatedAt) == "" {
		return true
	}
	updatedAt, err := parseTime(snapshot.UpdatedAt)
	if err != nil {
		return true
	}
	retryAfterUntil := updatedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second)
	return now.Before(retryAfterUntil)
}

func shouldAutoPauseGrokQuotaWindow(name string, window *xai.QuotaWindow, now time.Time) (bool, openAIQuotaAutoPauseDecision) {
	if window == nil || window.Limit == nil || window.Remaining == nil || *window.Limit <= 0 {
		return false, openAIQuotaAutoPauseDecision{}
	}
	if window.ResetUnix != nil && *window.ResetUnix > 0 && !now.Before(time.Unix(*window.ResetUnix, 0)) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	utilization := float64(*window.Limit-*window.Remaining) / float64(*window.Limit)
	if *window.Remaining <= 0 || utilization >= 1 {
		return true, openAIQuotaAutoPauseDecision{window: name, threshold: 1, utilization: utilization}
	}
	return false, openAIQuotaAutoPauseDecision{}
}

func grokQuotaSnapshotStaleForPause(snapshot *xai.QuotaSnapshot, now time.Time) bool {
	if snapshot == nil || strings.TrimSpace(snapshot.UpdatedAt) == "" {
		return false
	}
	updatedAt, err := parseTime(snapshot.UpdatedAt)
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) >= openAICodexAutoPauseStaleAfter
}

func shouldAutoPauseOpenAIAccountByQuota(ctx context.Context, account *Account) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil || !account.IsOpenAI() {
		return false, openAIQuotaAutoPauseDecision{}
	}
	// Automatic credit reset has its own consumption thresholds. Once one is
	// reached the account must leave scheduling while the reset workflow runs.
	// At the ordinary pause threshold, a fresh state with available credits is
	// the only condition that keeps the account schedulable.
	if config := ResolveOpenAIAutoResetCreditConfig(account); config.Enabled {
		now := time.Now()
		utilization5h, has5h := resolveOpenAIQuotaUtilization(account.Extra, "5h", now)
		utilization7d, has7d := resolveOpenAIQuotaUtilization(account.Extra, "7d", now)
		if has5h && utilization5h >= config.Threshold5h {
			notifyOpenAIAutoReset(account.ID)
			return true, openAIQuotaAutoPauseDecision{window: "5h", threshold: config.Threshold5h, utilization: utilization5h, reason: "quota_auto_reset_pending_5h"}
		}
		if has7d && utilization7d >= config.Threshold7d {
			notifyOpenAIAutoReset(account.ID)
			return true, openAIQuotaAutoPauseDecision{window: "7d", threshold: config.Threshold7d, utilization: utilization7d, reason: "quota_auto_reset_pending_7d"}
		}

		disabled5h := resolveAccountExtraBool(account.Extra, "auto_pause_5h_disabled")
		disabled7d := resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled")
		pause5h, pause7d := resolveOpenAIQuotaAutoPauseThresholds(ctx, account)
		pauseReached5h := !disabled5h && pause5h > 0 && has5h && utilization5h >= pause5h
		pauseReached7d := !disabled7d && pause7d > 0 && has7d && utilization7d >= pause7d
		if pauseReached5h || pauseReached7d {
			state := openAIAutoResetStateFromExtra(account.Extra)
			if state != nil && state.Status == OpenAIAutoResetStatusAvailable && state.AvailableCount > 0 && !openAIAutoResetStateStale(state, now) {
				return false, openAIQuotaAutoPauseDecision{}
			}
			notifyOpenAIAutoReset(account.ID)
			if pauseReached5h {
				return true, openAIQuotaAutoPauseDecision{window: "5h", threshold: pause5h, utilization: utilization5h, reason: "quota_auto_reset_credit_check_5h"}
			}
			return true, openAIQuotaAutoPauseDecision{window: "7d", threshold: pause7d, utilization: utilization7d, reason: "quota_auto_reset_credit_check_7d"}
		}
	}
	// Per-account explicit-disable flags must take precedence over the global default.
	// Without these, leaving the account threshold blank means "use global default",
	// so an admin has no way to exempt a single account from auto-pause once a global
	// default exists. The disable flag is per-window so an account can opt out of
	// only 5h or only 7d auto-pause.
	disabled5h := resolveAccountExtraBool(account.Extra, "auto_pause_5h_disabled")
	disabled7d := resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled")
	threshold5h, threshold7d := resolveOpenAIQuotaAutoPauseThresholds(ctx, account)
	now := time.Now()
	if !disabled5h && threshold5h > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "5h", now); ok && utilization >= threshold5h {
			return true, openAIQuotaAutoPauseDecision{window: "5h", threshold: threshold5h, utilization: utilization}
		}
	}
	if !disabled7d && threshold7d > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "7d", now); ok && utilization >= threshold7d {
			return true, openAIQuotaAutoPauseDecision{window: "7d", threshold: threshold7d, utilization: utilization}
		}
	}
	return false, openAIQuotaAutoPauseDecision{}
}

// resolveAccountExtraBool reads a bool-like value from account extra, tolerating
// the few shapes JSON unmarshalling may produce (real bool, "true"/"false"
// strings, 0/1 numbers).
func resolveAccountExtraBool(extra map[string]any, key string) bool {
	value, ok := resolveAccountExtraBoolValue(extra, key)
	return ok && value
}

func resolveAccountExtraBoolValue(extra map[string]any, key string) (bool, bool) {
	if len(extra) == 0 {
		return false, false
	}
	value, ok := extra[key]
	if !ok || value == nil {
		return false, false
	}
	return resolveBoolLikeValue(value)
}

func resolveNestedAccountExtraBoolValue(extra map[string]any, namespace string, key string) (bool, bool) {
	if len(extra) == 0 || namespace == "" || key == "" {
		return false, false
	}
	raw, ok := extra[namespace]
	if !ok || raw == nil {
		return false, false
	}
	if nested, ok := raw.(map[string]any); ok {
		return resolveAccountExtraBoolValue(nested, key)
	}
	if nested, ok := raw.(map[string]bool); ok {
		enabled, ok := nested[key]
		return enabled, ok
	}
	return false, false
}

func resolveBoolLikeValue(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		if err == nil {
			return parsed, true
		}
	case float64:
		return v != 0, true
	case float32:
		return v != 0, true
	case int:
		return v != 0, true
	case int64:
		return v != 0, true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i != 0, true
		}
	}
	return false, false
}

func resolveOpenAIQuotaAutoPauseThresholds(ctx context.Context, account *Account) (float64, float64) {
	threshold5h, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_5h_threshold")
	threshold7d, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_7d_threshold")
	threshold5h = clamp01(threshold5h)
	threshold7d = clamp01(threshold7d)
	if threshold5h > 0 && threshold7d > 0 {
		return threshold5h, threshold7d
	}
	settings := openAIQuotaAutoPauseSettingsFromContext(ctx)
	if threshold5h <= 0 {
		threshold5h = clamp01(settings.DefaultThreshold5h)
	}
	if threshold7d <= 0 {
		threshold7d = clamp01(settings.DefaultThreshold7d)
	}
	return threshold5h, threshold7d
}

func resolveAccountExtraNumber(extra map[string]any, keys ...string) (float64, bool) {
	if len(extra) == 0 {
		return 0, false
	}
	for _, key := range keys {
		value, ok := extra[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case json.Number:
			parsed, err := v.Float64()
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// resolveOpenAIQuotaUtilization returns the current utilization ratio (0..1) for the
// given Codex usage window. ok=false means there is no usable signal to pause on:
// either no snapshot exists, or the window has already rolled over so the cached
// percentage is stale. The stale guard matters because a paused account stops
// receiving requests, so its snapshot is never refreshed from upstream headers —
// without this check an old used_percent would keep the account paused forever even
// after the real window reset.
func resolveOpenAIQuotaUtilization(extra map[string]any, window string, now time.Time) (float64, bool) {
	usedPercent := readOpenAIQuotaUsedPercent(extra, window)
	if usedPercent <= 0 {
		return 0, false
	}
	if openAIQuotaWindowReset(extra, window, now) {
		return 0, false
	}
	// 快照过于陈旧（账号长期未收到流量刷新）时，不再据此暂停。放行后下一次响应头
	// 会刷新快照实现自愈，避免账号在错误/过期的 used% 上被永久跳过（issue #2994）。
	if openAICodexSnapshotStaleForPause(extra, now) {
		return 0, false
	}
	return usedPercent / 100, true
}

// openAICodexSnapshotStaleForPause reports whether the Codex usage snapshot is stale
// enough that it should no longer keep an account auto-paused. It anchors on
// codex_usage_updated_at (always written by buildCodexUsageExtraUpdates). A missing or
// unparseable timestamp returns false (treated as fresh, so the account stays paused) —
// this is deliberate: it prevents any snapshot without a write time from silently escaping
// auto-pause, and a genuinely-exhausted account that is actively served refreshes the
// timestamp on every response so it never crosses the staleness bound.
func openAICodexSnapshotStaleForPause(extra map[string]any, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return false
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) >= openAICodexAutoPauseStaleAfter
}

// openAIQuotaWindowReset reports whether the Codex usage window's reset time has
// already passed relative to now. It prefers the absolute codex_<window>_reset_at
// timestamp and falls back to codex_<window>_reset_after_seconds anchored at
// codex_usage_updated_at, mirroring AccountUsageService's window-progress logic.
func openAIQuotaWindowReset(extra map[string]any, window string, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	if resetAtRaw, ok := extra["codex_"+window+"_reset_at"]; ok {
		if resetAt, err := parseTime(fmt.Sprint(resetAtRaw)); err == nil {
			return !now.Before(resetAt)
		}
	}
	resetAfter := parseExtraInt(extra["codex_"+window+"_reset_after_seconds"])
	if resetAfter <= 0 {
		return false
	}
	base := now
	if updatedRaw, ok := extra["codex_usage_updated_at"]; ok {
		if updatedAt, err := parseTime(fmt.Sprint(updatedRaw)); err == nil {
			base = updatedAt
		}
	}
	resetAt := base.Add(time.Duration(resetAfter) * time.Second)
	return !now.Before(resetAt)
}

func readOpenAIQuotaUsedPercent(extra map[string]any, window string) float64 {
	if len(extra) == 0 {
		return 0
	}
	if value, ok := resolveAccountExtraNumber(extra, "codex_"+window+"_used_percent"); ok {
		return value
	}
	return 0
}

type openAIQuotaAutoPauseCtxKey struct{}

func withOpenAIQuotaAutoPauseSettings(ctx context.Context, settings OpsOpenAIAccountQuotaAutoPauseSettings) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIQuotaAutoPauseCtxKey{}, settings)
}

func openAIQuotaAutoPauseSettingsFromContext(ctx context.Context) OpsOpenAIAccountQuotaAutoPauseSettings {
	if ctx == nil {
		return OpsOpenAIAccountQuotaAutoPauseSettings{}
	}
	settings, _ := ctx.Value(openAIQuotaAutoPauseCtxKey{}).(OpsOpenAIAccountQuotaAutoPauseSettings)
	return settings
}

func (s *OpenAIGatewayService) withOpenAIQuotaAutoPauseContext(ctx context.Context) context.Context {
	if s == nil || s.settingService == nil {
		return ctx
	}
	return withOpenAIQuotaAutoPauseSettings(ctx, s.settingService.GetOpenAIQuotaAutoPauseSettings(ctx))
}

// prioritizeOpenAICompactAccounts re-orders a slice so that accounts with known
// compact support are tried first, followed by unknown, then explicitly unsupported.
// The relative order within each tier is preserved.
func prioritizeOpenAICompactAccounts(accounts []*Account) []*Account {
	if len(accounts) == 0 {
		return nil
	}
	supported := make([]*Account, 0, len(accounts))
	unknown := make([]*Account, 0, len(accounts))
	unsupported := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		switch openAICompactSupportTier(account) {
		case 2:
			supported = append(supported, account)
		case 1:
			unknown = append(unknown, account)
		default:
			unsupported = append(unsupported, account)
		}
	}
	out := make([]*Account, 0, len(accounts))
	out = append(out, supported...)
	out = append(out, unknown...)
	out = append(out, unsupported...)
	return out
}

// resolveOpenAIAccountUpstreamModelForRequest resolves the upstream model that
// would be sent for a given request, honoring the legacy compact-only mapping
// when the caller is on the /responses/compact path.
func resolveOpenAIAccountUpstreamModelForRequest(account *Account, requestedModel string, requireCompact bool) string {
	// Forward checks the raw Chat Completions fallback before passthrough.
	// These API-key accounts therefore apply normal account model_mapping and
	// upstream normalization, but never compact_model_mapping.
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		upstreamModel := resolveOpenAIForwardModel(account, requestedModel, "")
		return normalizeOpenAIModelForUpstream(account, upstreamModel)
	}

	// Passthrough accounts only replace authentication. Their Forward path
	// keeps the channel-mapped model in the request body and does not apply the
	// account's normal model_mapping. Legacy /responses/compact is the one
	// exception: forwardOpenAIPassthrough applies compact_model_mapping
	// directly to that channel-mapped model.
	if account != nil && account.IsOpenAIPassthroughEnabled() {
		upstreamModel := strings.TrimSpace(requestedModel)
		if upstreamModel == "" {
			return ""
		}
		if requireCompact {
			return resolveOpenAICompactForwardModel(account, upstreamModel)
		}
		return upstreamModel
	}

	// Compact mappings are keyed by the client-visible model. Prefer an exact
	// compact rule before ordinary account mapping; otherwise a normal alias can
	// hide the compact-specific rule and make scheduling disagree with Forward.
	if requireCompact && account != nil {
		if compactModel, matched := account.ResolveCompactMappedModel(strings.TrimSpace(requestedModel)); matched {
			if compactModel = strings.TrimSpace(compactModel); compactModel != "" {
				return compactModel
			}
		}
	}

	upstreamModel := resolveOpenAIForwardModel(account, requestedModel, "")
	if upstreamModel == "" {
		return ""
	}
	if requireCompact {
		compactModel := resolveOpenAICompactForwardModel(account, upstreamModel)
		if compactModel != upstreamModel {
			return compactModel
		}
	}
	return normalizeOpenAIModelForUpstream(account, upstreamModel)
}

func (s *OpenAIGatewayService) selectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability) (*Account, error) {
	return s.selectOpenAICompatibleAccountForModelWithExclusions(ctx, PlatformOpenAI, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability, false)
}

func (s *OpenAIGatewayService) selectOpenAICompatibleAccountForModelWithExclusions(ctx context.Context, platform string, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability, preferLowUpstreamRate bool) (*Account, error) {
	platform = normalizeOpenAICompatiblePlatform(platform)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	// 1. 尝试粘性会话命中
	// Try sticky session hit
	if account := s.tryOpenAICompatibleStickySessionHit(ctx, platform, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability); account != nil {
		return account, nil
	}

	// 2. 获取可调度的 OpenAI 账号
	// Get schedulable OpenAI accounts
	accounts, err := s.listSchedulableAccountsByPlatform(ctx, groupID, platform)
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}

	// 3. 按优先级 + LRU 选择最佳账号
	// Select by priority + LRU
	selected, compactBlocked, filterStats := s.selectBestOpenAICompatibleAccount(ctx, platform, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, preferLowUpstreamRate)

	if selected == nil {
		if platform == PlatformOpenAI {
			if recovered := s.recoverOpenAIRateLimitedAccountBeforeNoAvailable(ctx, groupID, requestedModel, excludedIDs, requireCompact, requiredCapability); recovered != nil {
				if sessionHash != "" {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, recovered.ID, openaiStickySessionTTL)
				}
				return recovered, nil
			}
		}
		return nil, noAvailableOpenAISelectionError(requestedModel, compactBlocked, filterStats.summary(""))
	}

	hydrated, err := s.hydrateSelectedAccount(ctx, selected)
	if err != nil {
		return nil, err
	}

	// 4. 设置粘性会话绑定
	// Set sticky session binding
	if sessionHash != "" {
		_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, selected.ID, openaiStickySessionTTL)
	}

	return hydrated, nil
}

// tryStickySessionHit 尝试从粘性会话获取账号。
// 如果命中且账号可用则返回账号；如果账号不可用则清理会话并返回 nil。
//
// tryStickySessionHit attempts to get account from sticky session.
// Returns account if hit and usable; clears session and returns nil if account is unavailable.

func (s *OpenAIGatewayService) tryOpenAICompatibleStickySessionHit(ctx context.Context, platform string, groupID *int64, sessionHash, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability) *Account {
	platform = normalizeOpenAICompatiblePlatform(platform)
	if sessionHash == "" {
		return nil
	}

	accountID := stickyAccountID
	if accountID <= 0 {
		var err error
		accountID, err = s.getStickySessionAccountID(ctx, groupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil
		}
	}

	if _, excluded := excludedIDs[accountID]; excluded {
		return nil
	}

	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return nil
	}

	// 检查账号是否需要清理粘性会话
	// Check if sticky session should be cleared
	if shouldClearStickySession(account, requestedModel) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 验证账号是否可用于当前请求
	// Verify account is usable for current request
	if !isOpenAICompatibleAccountEligibleForRequest(ctx, account, platform, requestedModel, false, requiredCapability) {
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(account) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	account = s.recheckSelectedOpenAIAccountFromDB(ctx, account, groupID, platform, requestedModel, requireCompact, requiredCapability)
	if account == nil || !openAIStickyAccountMatchesGroup(account, groupID) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if groupID != nil && s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
		s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 刷新会话 TTL 并返回账号
	// Refresh session TTL and return account
	_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
	return account
}

// selectBestAccount 从候选账号中选择最佳账号（优先级 + LRU）。
// 返回 nil 表示无可用账号。
//
// selectBestAccount selects the best account from candidates (priority + LRU).
// Returns nil if no available account. The second return reports whether at
// least one candidate was filtered out solely because it lacks compact support
// (only meaningful when requireCompact=true).

func (s *OpenAIGatewayService) selectBestOpenAICompatibleAccount(ctx context.Context, platform string, groupID *int64, accounts []Account, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, preferLowUpstreamRate bool) (*Account, bool, openAISelectionFilterStats) {
	platform = normalizeOpenAICompatiblePlatform(platform)
	compactBlocked := false
	filterStats := openAISelectionFilterStats{pool: len(accounts)}
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	eligible := make([]*Account, 0, len(accounts))
	compactTiers := make(map[int64]int, len(accounts))

	for i := range accounts {
		acc := &accounts[i]

		// 跳过被排除的账号
		// Skip excluded accounts
		if _, excluded := excludedIDs[acc.ID]; excluded {
			filterStats.exclude("excluded")
			continue
		}

		fresh := s.resolveFreshSchedulableOpenAICompatibleAccountBeforeProfit(ctx, acc, platform, requestedModel, false, requiredCapability)
		if fresh == nil {
			filterStats.exclude("ineligible")
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, fresh, groupID, platform, requestedModel, false, requiredCapability)
		if fresh == nil {
			filterStats.exclude("ineligible")
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			filterStats.exclude("channel_restricted")
			continue
		}
		if vetoed, reason := openAIProfitControlVetoReason(ctx, fresh); vetoed {
			filterStats.exclude(reason)
			continue
		}
		compactTier := 0
		if requireCompact {
			compactTier = openAICompactSupportTier(fresh)
			if compactTier == 0 {
				compactBlocked = true
				filterStats.exclude("compact_unsupported")
				continue
			}
		}

		eligible = append(eligible, fresh)
		compactTiers[fresh.ID] = compactTier
	}

	if len(eligible) == 0 {
		return nil, compactBlocked, filterStats
	}
	rateOrder := openAILegacyUpstreamRateOrder{}
	if preferLowUpstreamRate {
		rateOrder = newOpenAILegacyUpstreamRateOrder(eligible, time.Now(), s.openAIOAuthSchedulingRateMultiplier(ctx))
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if requireCompact && compactTiers[a.ID] != compactTiers[b.ID] {
			return compactTiers[a.ID] > compactTiers[b.ID]
		}
		if rateCmp := rateOrder.compare(a, b); rateCmp != 0 {
			return rateCmp < 0
		}
		return s.isBetterAccount(a, b)
	})
	return eligible[0], compactBlocked, filterStats
}

func (s *OpenAIGatewayService) recoverOpenAIRateLimitedAccountBeforeNoAvailable(ctx context.Context, groupID *int64, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if s == nil || s.accountRepo == nil {
		return nil
	}

	accounts, err := s.listOpenAIAccountsForRateLimitRecovery(ctx, groupID)
	if err != nil {
		slog.Warn("openai_rate_limit_recovery_list_failed", "group_id", derefGroupID(groupID), "error", err)
		return nil
	}
	if len(accounts) == 0 {
		return nil
	}

	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if _, excluded := excludedIDs[acc.ID]; excluded {
			continue
		}
		if !isRecoverableOpenAIRateLimitedAccount(acc, requestedModel, requireCompact) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, acc, requestedModel, requireCompact) {
			continue
		}
		candidates = append(candidates, acc)
	}
	if len(candidates) == 0 {
		return nil
	}

	sortAccountsByPriorityAndLastUsed(candidates, false)
	for _, candidate := range candidates {
		if err := s.clearOpenAIRateLimitForRecovery(ctx, candidate.ID); err != nil {
			slog.Warn("openai_rate_limit_recovery_clear_failed", "account_id", candidate.ID, "error", err)
			continue
		}

		fresh, err := s.accountRepo.GetByID(ctx, candidate.ID)
		if err != nil || fresh == nil {
			slog.Warn("openai_rate_limit_recovery_hydrate_failed", "account_id", candidate.ID, "error", err)
			continue
		}
		if !isOpenAIAccountEligibleForRequest(ctx, fresh, requestedModel, requireCompact, requiredCapability) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}

		slog.Info("openai_rate_limit_recovery_selected", "account_id", fresh.ID, "group_id", derefGroupID(groupID), "model", requestedModel)
		return fresh
	}

	return nil
}

func (s *OpenAIGatewayService) recoverOpenAIRateLimitedSelectionBeforeNoAvailable(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, cfg config.GatewaySchedulingConfig) (*AccountSelectionResult, bool) {
	account := s.recoverOpenAIRateLimitedAccountBeforeNoAvailable(ctx, groupID, requestedModel, excludedIDs, requireCompact, requiredCapability)
	if account == nil {
		return nil, false
	}
	if sessionHash != "" {
		_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, account.ID, openaiStickySessionTTL)
	}

	result, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err == nil && result != nil && result.Acquired {
		return &AccountSelectionResult{
			Account:     account,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}, true
	}

	return &AccountSelectionResult{
		Account: account,
		WaitPlan: &AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: account.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		},
	}, true
}

func (s *OpenAIGatewayService) listOpenAIAccountsForRateLimitRecovery(ctx context.Context, groupID *int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	}
	if groupID != nil {
		accounts, err := s.accountRepo.ListByGroup(ctx, *groupID)
		if err != nil {
			return nil, err
		}
		return filterOpenAIAccounts(accounts), nil
	}

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(accounts))
	for _, acc := range accounts {
		if len(acc.AccountGroups) == 0 {
			out = append(out, acc)
		}
	}
	return out, nil
}

func filterOpenAIAccounts(accounts []Account) []Account {
	out := make([]Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc.IsOpenAI() {
			out = append(out, acc)
		}
	}
	return out
}

func (s *OpenAIGatewayService) clearOpenAIRateLimitForRecovery(ctx context.Context, accountID int64) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	// codex 5/9 PR#2290 audit #2: singleflight by accountID. 高并发到 recover
	// 路径时只让一个请求真正调 ClearRateLimit, 其它 caller 共享同一结果, 避免
	// 数据库连续写 + 上游账号刚清就被一波并发撞回 429.
	key := strconv.FormatInt(accountID, 10)
	_, err, _ := s.rateLimitRecoveryFlight.Do(key, func() (any, error) {
		return nil, s.accountRepo.ClearRateLimit(ctx, accountID)
	})
	return err
}

func isRecoverableOpenAIRateLimitedAccount(account *Account, requestedModel string, requireCompact bool) bool {
	if account == nil || !account.IsOpenAI() || !account.IsActive() || !account.Schedulable {
		return false
	}
	now := time.Now()
	if account.RateLimitResetAt == nil || !now.Before(*account.RateLimitResetAt) {
		return false
	}
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return false
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return false
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return false
	}
	if account.IsAPIKeyOrBedrock() && account.IsQuotaExceeded() {
		return false
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return false
	}
	if requireCompact && openAICompactSupportTier(account) == 0 {
		return false
	}
	return openAICodexSnapshotShowsNonExhaustedWindows(account)
}

func openAICodexSnapshotShowsNonExhaustedWindows(account *Account) bool {
	used5h, ok5h := accountExtraFloat64(account, "codex_5h_used_percent")
	used7d, ok7d := accountExtraFloat64(account, "codex_7d_used_percent")
	if !ok5h || !ok7d || used5h >= 100 || used7d >= 100 {
		return false
	}
	if account.RateLimitedAt == nil {
		return false
	}
	sampledAt := account.getExtraTime("codex_usage_updated_at")
	if sampledAt.IsZero() {
		return false
	}
	diff := sampledAt.Sub(*account.RateLimitedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff <= openAICodexRecoverySnapshotMaxSkew
}

func accountExtraFloat64(account *Account, key string) (float64, bool) {
	if account == nil || account.Extra == nil {
		return 0, false
	}
	value, ok := account.Extra[key]
	if !ok {
		return 0, false
	}
	return parseExtraFloat64(value), true
}

// isBetterAccount 判断 candidate 是否比 current 更优。
// 规则：优先级更高（数值更小）优先；同优先级时，未使用过的优先，其次是最久未使用的。
//
// isBetterAccount checks if candidate is better than current.
// Rules: higher priority (lower value) wins; same priority: never used > least recently used.
func (s *OpenAIGatewayService) isBetterAccount(candidate, current *Account) bool {
	// 优先级更高（数值更小）
	// Higher priority (lower value)
	if candidate.Priority < current.Priority {
		return true
	}
	if candidate.Priority > current.Priority {
		return false
	}

	// 同优先级，比较最后使用时间
	// Same priority, compare last used time
	switch {
	case candidate.LastUsedAt == nil && current.LastUsedAt != nil:
		// candidate 从未使用，优先
		return true
	case candidate.LastUsedAt != nil && current.LastUsedAt == nil:
		// current 从未使用，保持
		return false
	case candidate.LastUsedAt == nil && current.LastUsedAt == nil:
		// 都未使用，保持
		return false
	default:
		// 都使用过，选择最久未使用的
		return candidate.LastUsedAt.Before(*current.LastUsedAt)
	}
}

// SelectAccountWithLoadAwareness selects an account with load-awareness and wait plan.
func (s *OpenAIGatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*AccountSelectionResult, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	return s.selectAccountWithLoadAwareness(ctx, groupID, sessionHash, requestedModel, excludedIDs, false, "")
}

func (s *OpenAIGatewayService) selectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability) (*AccountSelectionResult, error) {
	return s.selectOpenAICompatibleAccountWithLoadAwareness(ctx, PlatformOpenAI, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, requiredCapability, true)
}

func (s *OpenAIGatewayService) selectOpenAICompatibleAccountWithLoadAwareness(ctx context.Context, platform string, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, useUpstreamTokenCost bool) (*AccountSelectionResult, error) {
	platform = normalizeOpenAICompatiblePlatform(platform)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	cfg := s.schedulingConfig()
	preferLowUpstreamRate := useUpstreamTokenCost && s.isOpenAILowUpstreamRatePriorityEnabled(ctx)
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	var stickyAccountID int64
	if sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil {
			stickyAccountID = accountID
		}
	}
	if s.concurrencyService == nil || !cfg.LoadBatchEnabled {
		account, err := s.selectOpenAICompatibleAccountForModelWithExclusions(ctx, platform, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability, preferLowUpstreamRate)
		if err != nil {
			return nil, err
		}
		result, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if err == nil && result != nil && result.Acquired {
			return s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
		}
		if stickyAccountID > 0 && stickyAccountID == account.ID && s.concurrencyService != nil {
			waitingCount, _ := s.concurrencyService.GetAccountWaitingCount(ctx, account.ID)
			if waitingCount < cfg.StickySessionMaxWaiting {
				return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
					AccountID:      account.ID,
					MaxConcurrency: account.Concurrency,
					Timeout:        cfg.StickySessionWaitTimeout,
					MaxWaiting:     cfg.StickySessionMaxWaiting,
				})
			}
		}
		return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: account.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		})
	}

	accounts, err := s.listSchedulableAccountsByPlatform(ctx, groupID, platform)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		if recovered, ok := s.recoverOpenAIRateLimitedSelectionBeforeNoAvailable(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, requiredCapability, cfg); ok {
			return recovered, nil
		}
		return nil, noAvailableOpenAISelectionError(requestedModel, false, openAISelectionFilterStats{}.summary(""))
	}

	isExcluded := func(accountID int64) bool {
		if excludedIDs == nil {
			return false
		}
		_, excluded := excludedIDs[accountID]
		return excluded
	}

	// ============ Layer 1: Sticky session ============
	// A healthy sticky account whose bounded wait queue is full may spill one
	// request into Layer 2 without migrating the durable conversation binding.
	stickySpillover := false
	if sessionHash != "" {
		accountID := stickyAccountID
		if accountID > 0 && !isExcluded(accountID) {
			account, err := s.getSchedulableAccount(ctx, accountID)
			if err == nil {
				clearSticky := shouldClearStickySession(account, requestedModel)
				if clearSticky {
					_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
				}
				if !clearSticky && isOpenAICompatibleAccountEligibleForRequest(ctx, account, platform, requestedModel, false, requiredCapability) {
					account = s.recheckSelectedOpenAIAccountFromDB(ctx, account, groupID, platform, requestedModel, requireCompact, requiredCapability)
					if account == nil {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					} else if !openAIStickyAccountMatchesGroup(account, groupID) {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					} else if s.isOpenAIAccountRuntimeBlocked(account) {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					} else if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					} else {
						result, err := s.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
						if err == nil && result != nil && result.Acquired {
							selection, selectErr := s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
							if selectErr != nil {
								return nil, selectErr
							}
							_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
							return selection, nil
						}

						waitingCount, _ := s.concurrencyService.GetAccountWaitingCount(ctx, accountID)
						if waitingCount < cfg.StickySessionMaxWaiting {
							return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
								AccountID:      accountID,
								MaxConcurrency: account.Concurrency,
								Timeout:        cfg.StickySessionWaitTimeout,
								MaxWaiting:     cfg.StickySessionMaxWaiting,
							})
						}
						stickySpillover = true
					}
				}
			}
		}
	}

	// ============ Layer 2: Load-aware selection ============
	baseCandidateCount := 0
	filterStats := openAISelectionFilterStats{pool: len(accounts)}
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if isExcluded(acc.ID) {
			filterStats.exclude("excluded")
			continue
		}
		// Scheduler snapshots can be temporarily stale (bucket rebuild is throttled);
		// re-check schedulability here so recently rate-limited/overloaded accounts
		// are not selected again before the bucket is rebuilt.
		if reason := openAICompatibleAccountEligibilityFailureReason(ctx, acc, platform, requestedModel, false, requiredCapability); reason != "" {
			filterStats.exclude(reason)
			continue
		}
		if s.isOpenAIAccountRuntimeBlocked(acc) {
			filterStats.exclude("runtime_blocked")
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, acc, requestedModel, requireCompact) {
			filterStats.exclude("channel_upstream_restricted")
			continue
		}
		baseCandidateCount++
		candidates = append(candidates, acc)
	}

	if len(candidates) == 0 {
		if recovered, ok := s.recoverOpenAIRateLimitedSelectionBeforeNoAvailable(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, requiredCapability, cfg); ok {
			return recovered, nil
		}
		return nil, noAvailableOpenAISelectionError(requestedModel, false, filterStats.summary(""))
	}
	rateOrder := openAILegacyUpstreamRateOrder{}
	if preferLowUpstreamRate {
		rateOrder = newOpenAILegacyUpstreamRateOrder(candidates, time.Now(), s.openAIOAuthSchedulingRateMultiplier(ctx))
	}

	accountLoads := make([]AccountWithConcurrency, 0, len(candidates))
	for _, acc := range candidates {
		accountLoads = append(accountLoads, AccountWithConcurrency{
			ID:             acc.ID,
			MaxConcurrency: acc.EffectiveLoadFactor(),
		})
	}

	tryAcquireFromLoadMap := func(loadMap map[int64]*AccountLoadInfo) (*AccountSelectionResult, bool, error) {
		var available []accountWithLoad
		for _, acc := range candidates {
			loadInfo := loadMap[acc.ID]
			if loadInfo == nil {
				loadInfo = &AccountLoadInfo{AccountID: acc.ID}
			}
			if loadInfo.LoadRate < 100 {
				available = append(available, accountWithLoad{
					account:  acc,
					loadInfo: loadInfo,
				})
			}
		}

		if len(available) == 0 {
			return nil, false, nil
		}

		sort.SliceStable(available, func(i, j int) bool {
			a, b := available[i], available[j]
			if a.account.Priority != b.account.Priority {
				return a.account.Priority < b.account.Priority
			}
			if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
				return a.loadInfo.LoadRate < b.loadInfo.LoadRate
			}
			switch {
			case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
				return true
			case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
				return false
			case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
				return false
			default:
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		})
		shuffleWithinSortGroups(available)
		if rateOrder.enabled {
			sort.SliceStable(available, func(i, j int) bool {
				return rateOrder.compare(available[i].account, available[j].account) < 0
			})
		}

		selectionOrder := make([]accountWithLoad, 0, len(available))
		if requireCompact {
			appendTier := func(out []accountWithLoad, tier int) []accountWithLoad {
				for _, item := range available {
					if openAICompactSupportTier(item.account) == tier {
						out = append(out, item)
					}
				}
				return out
			}
			selectionOrder = appendTier(selectionOrder, 2)
			selectionOrder = appendTier(selectionOrder, 1)
			// tier 0 候选作为兜底追加：DB recheck 时若发现 cache tier 0 实际
			// 已升级为 1/2（探测刚跑完，cache 尚未刷新），仍可正常命中。
			selectionOrder = appendTier(selectionOrder, 0)
		} else {
			selectionOrder = append(selectionOrder, available...)
		}

		for _, item := range selectionOrder {
			fresh := s.resolveFreshSchedulableOpenAICompatibleAccount(ctx, item.account, platform, requestedModel, false, requiredCapability)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, groupID, platform, requestedModel, requireCompact, requiredCapability)
			if fresh == nil {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, true, selectErr
				}
				if sessionHash != "" && !stickySpillover && !gatewayProfitControlGateActive(ctx) {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, openaiStickySessionTTL)
				}
				return selection, true, nil
			}
		}
		return nil, true, nil
	}

	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(ctx, accountLoads)
	if err != nil {
		ordered := append([]*Account(nil), candidates...)
		sortAccountsByPriorityAndLastUsed(ordered, false)
		if rateOrder.enabled {
			sort.SliceStable(ordered, func(i, j int) bool {
				return rateOrder.compare(ordered[i], ordered[j]) < 0
			})
		}
		if requireCompact {
			ordered = prioritizeOpenAICompactAccounts(ordered)
		}
		for _, acc := range ordered {
			fresh := s.resolveFreshSchedulableOpenAICompatibleAccount(ctx, acc, platform, requestedModel, false, requiredCapability)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, groupID, platform, requestedModel, requireCompact, requiredCapability)
			if fresh == nil {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, selectErr
				}
				if sessionHash != "" && !stickySpillover && !gatewayProfitControlGateActive(ctx) {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, openaiStickySessionTTL)
				}
				return selection, nil
			}
		}
	} else {
		if selection, attempted, selectErr := tryAcquireFromLoadMap(loadMap); selectErr != nil {
			return nil, selectErr
		} else if selection != nil {
			return selection, nil
		} else if attempted {
			if freshLoadMap, loadErr := s.concurrencyService.GetAccountsLoadBatchFresh(ctx, accountLoads); loadErr == nil {
				if selection, _, selectErr := tryAcquireFromLoadMap(freshLoadMap); selectErr != nil {
					return nil, selectErr
				} else if selection != nil {
					return selection, nil
				}
			}
		}
	}

	// ============ Layer 3: Fallback wait ============
	sortAccountsByPriorityAndLastUsed(candidates, false)
	if rateOrder.enabled {
		sort.SliceStable(candidates, func(i, j int) bool {
			return rateOrder.compare(candidates[i], candidates[j]) < 0
		})
	}
	if requireCompact {
		candidates = prioritizeOpenAICompactAccounts(candidates)
	}
	for _, acc := range candidates {
		fresh := s.resolveFreshSchedulableOpenAICompatibleAccount(ctx, acc, platform, requestedModel, false, requiredCapability)
		if fresh == nil {
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, groupID, platform, requestedModel, requireCompact, requiredCapability)
		if fresh == nil {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}
		return s.newSelectionResult(ctx, fresh, false, nil, &AccountWaitPlan{
			AccountID:      fresh.ID,
			MaxConcurrency: fresh.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		})
	}

	if platform == PlatformOpenAI {
		if recovered, ok := s.recoverOpenAIRateLimitedSelectionBeforeNoAvailable(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, requiredCapability, cfg); ok {
			return recovered, nil
		}
	}
	if requireCompact && baseCandidateCount > 0 {
		return nil, ErrNoAvailableCompactAccounts
	}
	return nil, noAvailableOpenAISelectionError(requestedModel, false, filterStats.summary(""))
}

func (s *OpenAIGatewayService) listSchedulableAccountsByPlatform(ctx context.Context, groupID *int64, platform string) ([]Account, error) {
	platform = normalizeOpenAICompatiblePlatform(platform)
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, platform, false)
		if err != nil {
			return accounts, err
		}
		accounts = s.filterOpenAIAccountsBySchedulingThreshold(ctx, accounts)
		if platform == PlatformGrok {
			accounts = s.filterGrokFreeQuotaAccountsForOpenAI(ctx, accounts)
		}
		return accounts, nil
	}
	var accounts []Account
	var err error
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, platform)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, platform)
	} else {
		accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, platform)
	}
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}
	accounts = s.filterOpenAIAccountsBySchedulingThreshold(ctx, accounts)
	if platform == PlatformGrok {
		accounts = s.filterGrokFreeQuotaAccountsForOpenAI(ctx, accounts)
	}
	return accounts, nil
}

// filterGrokFreeQuotaAccountsForOpenAI applies the same local free soft-gate as
// GatewayService / advanced scheduler on the legacy OpenAI-compatible path.
func (s *OpenAIGatewayService) filterGrokFreeQuotaAccountsForOpenAI(ctx context.Context, accounts []Account) []Account {
	if s == nil {
		return accounts
	}
	return filterGrokFreeQuotaAccountsCore(ctx, s.cfg, s.usageLogRepo, &openaiGrokFreeQuotaGateCache, accounts)
}

func (s *OpenAIGatewayService) filterOpenAIAccountsBySchedulingThreshold(ctx context.Context, accounts []Account) []Account {
	if len(accounts) == 0 {
		return accounts
	}
	filtered := make([]Account, 0, len(accounts))
	for i := range accounts {
		if s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, &accounts[i]) {
			continue
		}
		filtered = append(filtered, accounts[i])
	}
	return filtered
}

func (s *OpenAIGatewayService) isOpenAIAccountBlockedBySchedulingThreshold(ctx context.Context, account *Account) bool {
	if s == nil || s.rateLimitService == nil || account == nil {
		return false
	}
	return s.rateLimitService.ApplyAccountSchedulingThreshold(ctx, account)
}

func (s *OpenAIGatewayService) tryAcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int) (*AcquireResult, error) {
	if s.concurrencyService == nil {
		return &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil
	}
	return s.concurrencyService.AcquireAccountSlot(ctx, accountID, maxConcurrency)
}

func (s *OpenAIGatewayService) resolveFreshSchedulableOpenAICompatibleAccount(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	fresh := s.resolveFreshSchedulableOpenAICompatibleAccountBeforeProfit(ctx, account, platform, requestedModel, requireCompact, requiredCapability)
	if fresh == nil {
		return nil
	}
	if vetoed, _ := openAIProfitControlVetoReason(ctx, fresh); vetoed {
		return nil
	}
	return fresh
}

func (s *OpenAIGatewayService) resolveFreshSchedulableOpenAICompatibleAccountBeforeProfit(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if account == nil {
		return nil
	}
	platform = normalizeOpenAICompatiblePlatform(platform)

	fresh := account
	if s.schedulerSnapshot != nil {
		current, err := s.getSchedulableAccount(ctx, account.ID)
		if err != nil || current == nil {
			return nil
		}
		fresh = current
	}

	if !isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx, fresh, platform, requestedModel, requireCompact, requiredCapability) {
		return nil
	}
	if !parentHealthyForShadow(fresh, s.parentAccountLookup(ctx)) {
		return nil
	}
	if s.isOpenAIAccountRequestRuntimeBlocked(fresh, requestedModel) {
		return nil
	}
	if s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, fresh) {
		return nil
	}
	if s.isOpenAIProxyStreamQuarantined(ctx, fresh) {
		return nil
	}
	return fresh
}

func (s *OpenAIGatewayService) recheckSelectedOpenAIAccountFromDB(ctx context.Context, account *Account, groupID *int64, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	latest := s.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, account, groupID, platform, requestedModel, requireCompact, requiredCapability)
	if latest == nil {
		return nil
	}
	if vetoed, _ := openAIProfitControlVetoReason(ctx, latest); vetoed {
		return nil
	}
	return latest
}

func (s *OpenAIGatewayService) recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx context.Context, account *Account, groupID *int64, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if account == nil {
		return nil
	}
	platform = normalizeOpenAICompatiblePlatform(platform)
	if s.schedulerSnapshot == nil || s.accountRepo == nil {
		if s.openAIGroupRequiresPrivacySet(ctx, groupID) && !account.IsPrivacySet() {
			return nil
		}
		if !isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx, account, platform, requestedModel, requireCompact, requiredCapability) {
			return nil
		}
		if s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, account) {
			return nil
		}
		if !parentHealthyForShadow(account, s.parentAccountLookup(ctx)) {
			return nil
		}
		if s.isOpenAIProxyStreamQuarantined(ctx, account) {
			return nil
		}
		return account
	}

	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil {
		return nil
	}
	if !s.openAIAccountMatchesSchedulingGroup(latest, groupID) {
		return nil
	}
	if s.openAIGroupRequiresPrivacySet(ctx, groupID) && !latest.IsPrivacySet() {
		return nil
	}
	if !isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx, latest, platform, requestedModel, requireCompact, requiredCapability) {
		return nil
	}
	if !parentHealthyForShadow(latest, s.parentAccountLookup(ctx)) {
		return nil
	}
	if s.isOpenAIAccountRequestRuntimeBlocked(latest, requestedModel) {
		return nil
	}
	if s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, latest) {
		return nil
	}
	if s.isOpenAIProxyStreamQuarantined(ctx, latest) {
		return nil
	}
	return latest
}

func (s *OpenAIGatewayService) openAIAccountMatchesSchedulingGroup(account *Account, groupID *int64) bool {
	if s != nil && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return account != nil
	}
	return openAIStickyAccountMatchesGroup(account, groupID)
}

func (s *OpenAIGatewayService) recheckSelectedOpenAICompatibleAccountFromDB(ctx context.Context, account *Account, platform string, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if account == nil {
		return nil
	}
	if s.schedulerSnapshot == nil || s.accountRepo == nil {
		if !isOpenAICompatibleAccountEligibleForRequest(ctx, account, platform, requestedModel, requireCompact, requiredCapability) {
			return nil
		}
		if s.isOpenAIProxyStreamQuarantined(ctx, account) {
			return nil
		}
		return account
	}

	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil {
		return nil
	}
	if !isOpenAICompatibleAccountEligibleForRequest(ctx, latest, platform, requestedModel, requireCompact, requiredCapability) {
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(latest) {
		return nil
	}
	if s.isOpenAIProxyStreamQuarantined(ctx, latest) {
		return nil
	}
	return latest
}

func (s *OpenAIGatewayService) parentAccountLookup(ctx context.Context) func(int64) *Account {
	return func(id int64) *Account {
		if s.accountRepo == nil {
			return nil
		}
		account, _ := s.accountRepo.GetByID(ctx, id)
		return account
	}
}

func (s *OpenAIGatewayService) getSchedulableAccount(ctx context.Context, accountID int64) (*Account, error) {
	var (
		account *Account
		err     error
	)
	if s.schedulerSnapshot != nil {
		account, err = s.schedulerSnapshot.GetAccount(ctx, accountID)
	} else {
		account, err = s.accountRepo.GetByID(ctx, accountID)
	}
	if err != nil || account == nil {
		return account, err
	}
	if s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, account) {
		return nil, nil
	}
	if account.IsGrok() {
		if gated := s.filterGrokFreeQuotaAccountsForOpenAI(ctx, []Account{*account}); len(gated) == 0 {
			return nil, nil
		}
	}
	return account, nil
}

func (s *OpenAIGatewayService) hydrateSelectedAccount(ctx context.Context, account *Account) (*Account, error) {
	if account == nil || s.schedulerSnapshot == nil {
		return account, nil
	}
	hydrated, err := s.schedulerSnapshot.GetAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if hydrated == nil {
		return nil, fmt.Errorf("selected openai account %d not found during hydration", account.ID)
	}
	return hydrated, nil
}

func (s *OpenAIGatewayService) newSelectionResult(ctx context.Context, account *Account, acquired bool, release func(), waitPlan *AccountWaitPlan) (*AccountSelectionResult, error) {
	hydrated, err := s.hydrateSelectedAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return attachSelectionProfitGate(ctx, &AccountSelectionResult{
		Account:     hydrated,
		Acquired:    acquired,
		ReleaseFunc: release,
		WaitPlan:    waitPlan,
	}), nil
}

func (s *OpenAIGatewayService) newAcquiredSelectionResult(ctx context.Context, account *Account, release func()) (*AccountSelectionResult, error) {
	selection, err := s.newSelectionResult(ctx, account, true, release, nil)
	if err != nil && release != nil {
		release()
	}
	return selection, err
}

func (s *OpenAIGatewayService) schedulingConfig() config.GatewaySchedulingConfig {
	if s.cfg != nil {
		return s.cfg.Gateway.Scheduling
	}
	return config.GatewaySchedulingConfig{
		StickySessionMaxWaiting:  3,
		StickySessionWaitTimeout: 45 * time.Second,
		FallbackWaitTimeout:      30 * time.Second,
		FallbackMaxWaiting:       100,
		LoadBatchEnabled:         true,
		SlotCleanupInterval:      30 * time.Second,
	}
}

// GetAccessToken gets the access token for an OpenAI account
func (s *OpenAIGatewayService) GetAccessToken(ctx context.Context, account *Account) (string, string, error) {
	if account.IsShadow() {
		credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return "", "", err
		}
		account = credentialAccount
	}
	switch account.Type {
	case AccountTypeOAuth:
		if account.IsOpenAIAgentIdentity() {
			return "", OpenAIAuthModeAgentIdentity, nil
		}
		if account.Platform == PlatformGrok {
			if s.grokTokenProvider != nil {
				accessToken, err := s.grokTokenProvider.GetAccessToken(ctx, account)
				if err != nil {
					return "", "", err
				}
				return accessToken, "oauth", nil
			}
			accessToken := strings.TrimSpace(account.GetGrokAccessToken())
			if accessToken == "" {
				return "", "", errors.New("access_token not found in credentials")
			}
			return accessToken, "oauth", nil
		}
		// 使用 TokenProvider 获取缓存的 token
		if s.openAITokenProvider != nil {
			accessToken, err := s.openAITokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return "", "", err
			}
			return accessToken, "oauth", nil
		}
		// 降级：TokenProvider 未配置时直接从账号读取
		accessToken := account.GetOpenAIAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeSetupToken:
		if !account.IsOpenAIOAuthLike() {
			return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
		}
		// OpenAI setup tokens are inference-only bearer credentials. They use the
		// Codex OAuth forwarding protocol but have no refresh-token lifecycle.
		accessToken := account.GetOpenAIAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeAPIKey:
		if account.Platform == PlatformGrok {
			apiKey := strings.TrimSpace(account.GetCredential("api_key"))
			if apiKey == "" {
				return "", "", errors.New("api_key not found in credentials")
			}
			return apiKey, "apikey", nil
		}
		apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
		if apiKey == "" {
			return "", "", errors.New("api_key not found in credentials")
		}
		return apiKey, "apikey", nil
	default:
		return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

func (s *OpenAIGatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 405, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

// shouldFailoverUpstreamErrorForAccount — codex 2026-05-16 post-account-124
// incident: 同上游账号 (e.g. OpenAI OAuth/Codex) 对多个客户 model 同时
// 返 404 ≠ 客户传错, 是该账号 Codex backend 突然不可用 (feature flag /
// 组织绑定失效 / etc). 老 shouldFailoverUpstreamError 没把 404 当 failover
// → cc-api/sub2api 直接把 "Upstream error: 404" 透给客户.
//
// 修法: 404 在 OpenAI OAuth 账号路径 → account-scoped failover. 切下一个
// 账号 retry, 全失败时 handleAnthropicFailoverExhausted 中性化 502 给客户.
//
// 限定 OAuth account: 404 在 API-key 路径仍可能是真客户传错 model,
// 不要 retry 一遍 (避免误烧 quota + 客户期望同一错误).
func (s *OpenAIGatewayService) shouldFailoverUpstreamErrorForAccount(statusCode int, account *Account) bool {
	if s.shouldFailoverUpstreamError(statusCode) {
		return true
	}
	if statusCode == 404 && account != nil && account.Type == AccountTypeOAuth {
		return true
	}
	return false
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponse(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	// cyber_policy is request-scoped even when an intermediary wraps the
	// provider response in a retryable 5xx status. Never punish or rotate the
	// selected credential for it.
	if hit, _, _ := detectOpenAICyberPolicy(upstreamBody); hit {
		return false
	}
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return false
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if s.shouldFailoverUpstreamError(statusCode) {
		return true
	}
	return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
}

// shouldFailoverOpenAIUpstreamResponseForAccount — account-aware variant
// (codex 2026-05-16): adds the 404-on-OAuth-account failover rule on top
// of the existing logic. Callers that have the account in scope pass it
// here so a worker-scoped 404 (Codex backend unavailable for that
// account) triggers cross-account retry instead of leaking to client.
func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponseForAccount(statusCode int, upstreamMsg string, upstreamBody []byte, account *Account) bool {
	if s.shouldFailoverOpenAIUpstreamResponse(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if isOpenAIOAuthSensitiveBackendError(account, statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if statusCode == 404 && account != nil && account.Type == AccountTypeOAuth {
		return true
	}
	return false
}

func marshalOpenAIUpstreamJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func openAIUpstreamErrorBodyReadLimitForConfig(cfg *config.Config) int64 {
	limit := openAIUpstreamErrorBodyReadLimit
	if cfg != nil && cfg.Gateway.LogUpstreamErrorBody && cfg.Gateway.LogUpstreamErrorBodyMaxBytes > int(limit) {
		limit = int64(cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}
	return limit
}

func (s *OpenAIGatewayService) readUpstreamErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	cfg := (*config.Config)(nil)
	if s != nil {
		cfg = s.cfg
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, openAIUpstreamErrorBodyReadLimitForConfig(cfg)))
	return body
}

func (s *OpenAIGatewayService) handleFailoverSideEffects(ctx context.Context, resp *http.Response, account *Account, body []byte, requestedModel ...string) bool {
	if len(requestedModel) > 0 {
		return s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, requestedModel[0])
	}
	return s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body)
}

// Forward forwards request to OpenAI API
func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (forwardResultOut *OpenAIForwardResult, _ error) {
	beginUpstreamResponseModelObservation(c)
	defer func() {
		forwardResultOut = attachObservedOpenAIUpstreamResponseModel(c, forwardResultOut)
	}()
	clearGrokResponsesClientToolMapping(c)
	clearOpenAIResponsesClientToolMapping(c)
	clearOpenAIResponsesNamespaceNames(c)
	setCodexToolNameReverse(c, nil)
	if _, err := s.prepareCodexAccountIdentitySource(ctx, c, account); err != nil {
		return nil, err
	}
	startTime := time.Now()
	// Keep a request-level canonical body: account-local normalization and
	// compact/tool stripping must not rewrite the cross-failover image hint.
	canonicalImageIntentBody := body

	restrictionResult := s.detectCodexClientRestriction(c, account, body)
	apiKeyID := getAPIKeyIDFromContext(c)
	logCodexCLIOnlyDetection(ctx, c, account, apiKeyID, restrictionResult, body)
	if restrictionResult.Enabled && !restrictionResult.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "forbidden_error",
				"message": "This account only allows Codex official clients",
			},
		})
		return nil, errors.New("codex_cli_only restriction: only codex official clients are allowed")
	}

	normalizedCompactBody, compactEffortNormalized, err := normalizeOpenAICodexCompactReasoningEffortForAccount(c, account, body)
	if err != nil {
		return nil, err
	}
	if compactEffortNormalized {
		body = normalizedCompactBody
	}
	legacyIngressBody, legacyIngressChanged, legacyIngressErr := normalizeOpenAIResponsesLegacyIngress(body)
	if legacyIngressErr != nil {
		return nil, legacyIngressErr
	}
	if legacyIngressChanged {
		body = legacyIngressBody
	}

	// Sanitize explicit null tool schema types before selecting passthrough,
	// Codex transform, or ChatCompletions fallback paths. Otherwise the same
	// invalid schema can be retried across the account pool after upstream 400s.
	sanitizedToolBody, toolSchemaSanitized, toolSchemaErr := sanitizeOpenAIResponsesToolSchemasForPlatform(body, account.Platform)
	if toolSchemaErr != nil {
		return nil, toolSchemaErr
	} else if toolSchemaSanitized {
		body = sanitizedToolBody
	}
	if account.IsOpenAIOAuthLike() {
		reasoningBody, reasoningChanged, reasoningErr := normalizeOpenAIResponsesReasoningMode(body)
		if reasoningErr != nil {
			return nil, fmt.Errorf("normalize OpenAI Responses reasoning.mode: %w", reasoningErr)
		}
		if reasoningChanged {
			body = reasoningBody
		}
	}
	responsesLite := account.IsOpenAI() && isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader))
	if responsesLite {
		liteBody, changed, liteErr := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
		if liteErr != nil {
			param := "tools"
			var validationErr *openAIResponsesLiteValidationError
			if errors.As(liteErr, &validationErr) {
				param = validationErr.param
			}
			setOpsUpstreamError(c, http.StatusBadRequest, liteErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": liteErr.Error(), "param": param,
			}})
			return nil, liteErr
		}
		if changed {
			body = liteBody
		}
	}

	originalBody := body
	requestView := newOpenAIRequestView(body)
	reqModel, reqStream, promptCacheKey := requestView.Model, requestView.Stream, requestView.PromptCacheKey
	originalModel := reqModel
	if account.IsOpenAIApiKey() {
		if normalized, changed, normalizeErr := normalizeOpenAIParallelToolCallsWithoutTools(body, responsesLite); normalizeErr != nil {
			return nil, normalizeErr
		} else if changed {
			body = normalized
			originalBody = normalized
		}
		if normalized, changed, normalizeErr := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, isOpenAIResponsesCompactPath(c)); normalizeErr != nil {
			return nil, normalizeErr
		} else if changed {
			body = normalized
			originalBody = normalized
		}
		requestView = newOpenAIRequestView(body)
		reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
		originalModel = reqModel
	}
	// Responses client × native Anthropic upstream is a cross-protocol path;
	// it must not fall through to raw Chat Completions URL construction.
	if account.IsAnthropicProtocol() {
		return s.forwardResponsesViaNativeAnthropic(ctx, c, account, body, reqModel)
	}
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return s.forwardResponsesViaRawChatCompletions(ctx, c, account, body)
	}
	if account.IsOpenAI() && (account.IsOpenAIApiKey() || account.IsOpenAIOAuthLike()) {
		normalizedReasoningBody, reasoningChanged, reasoningErr := normalizeOpenAIResponsesReasoningContentReplay(body)
		if reasoningErr != nil {
			return nil, fmt.Errorf("normalize OpenAI Responses reasoning content replay: %w", reasoningErr)
		}
		if reasoningChanged {
			body = normalizedReasoningBody
			originalBody = normalizedReasoningBody
			requestView = newOpenAIRequestView(normalizedReasoningBody)
			reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
			originalModel = reqModel
		}
		sanitizedBody, changed, sanitizeErr := sanitizeOpenAIResponsesInputItemIDs(body)
		if sanitizeErr != nil {
			return nil, fmt.Errorf("sanitize Responses input item IDs: %w", sanitizeErr)
		}
		if changed {
			body = sanitizedBody
			originalBody = sanitizedBody
			requestView = newOpenAIRequestView(sanitizedBody)
			reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
			originalModel = reqModel
		}
	}
	compatMessagesBridge := isOpenAICompatMessagesBridgeBody(body)
	setOpenAICompatMessagesBridgeContext(c, compatMessagesBridge)

	if account.Platform == PlatformGrok {
		_ = promptCacheKey
		return s.forwardGrokResponses(ctx, c, account, body, originalModel, reqStream, startTime)
	}

	isCodexCLI := openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) || (s.cfg != nil && s.cfg.Gateway.ForceCodexCLI)
	wsDecision := s.getOpenAIWSProtocolResolver().Resolve(account)
	clientTransport := GetOpenAIClientTransport(c)
	// 仅允许 WS 入站请求走 WS 上游，避免出现 HTTP -> WS 协议混用。
	wsDecision = resolveOpenAIWSDecisionByClientTransport(wsDecision, clientTransport)
	if c != nil {
		c.Set("openai_ws_transport_decision", string(wsDecision.Transport))
		c.Set("openai_ws_transport_reason", wsDecision.Reason)
	}
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		logOpenAIWSModeDebug(
			"selected account_id=%d account_type=%s transport=%s reason=%s model=%s stream=%v",
			account.ID,
			account.Type,
			normalizeOpenAIWSLogValue(string(wsDecision.Transport)),
			normalizeOpenAIWSLogValue(wsDecision.Reason),
			reqModel,
			reqStream,
		)
	}
	// 当前仅支持 WSv2；WSv1 命中时直接返回错误，避免出现“配置可开但行为不确定”。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocket {
		if c != nil {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": "OpenAI WSv1 is temporarily unsupported. Please enable responses_websockets_v2.",
				},
			})
		}
		return nil, errors.New("openai ws v1 is temporarily unsupported; use ws v2")
	}
	passthroughEnabled := account.IsOpenAIPassthroughEnabled()
	compactPath := isOpenAIResponsesCompactPath(c)
	if shouldFlattenOpenAIResponsesNamespaces(account, wsDecision.Transport, passthroughEnabled, compactPath) {
		flattenedBody, flattenErr := flattenOpenAIResponsesNamespaces(c, body)
		if flattenErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, flattenErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": flattenErr.Error(), "param": "tools",
			}})
			return nil, flattenErr
		}
		body = flattenedBody
	}
	if shouldStripOpenAIResponsesInputNamespaces(account, wsDecision.Transport, passthroughEnabled) {
		keepToolCallNamespaces := shouldKeepOpenAIResponsesToolCallNamespaces(
			account, wsDecision.Transport, passthroughEnabled, compactPath, body,
		)
		strippedBody, stripErr := stripOpenAIResponsesInputNamespaces(body, keepToolCallNamespaces)
		if stripErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, stripErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": stripErr.Error(), "param": "input",
			}})
			return nil, stripErr
		}
		body = strippedBody
	}
	nativeCNResponses := account.UsesNativeCNResponses()
	nativeDeepSeekResponses := account.Platform == PlatformDeepseek && nativeCNResponses
	if nativeDeepSeekResponses && account.Type == AccountTypeAPIKey && !compactPath &&
		needsOpenAIResponsesClientToolAdaptation(body) {
		adaptedBody, mapping, adaptErr := adaptOpenAIResponsesClientTools(body)
		if adaptErr != nil {
			return nil, fmt.Errorf("adapt DeepSeek Responses client tools: %w", adaptErr)
		}
		body = adaptedBody
		setOpenAIResponsesClientToolMapping(c, mapping)
	}
	if !bytes.Equal(body, originalBody) {
		originalBody = body
		requestView = newOpenAIRequestView(body)
		reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
		originalModel = reqModel
	}
	storeEnabled := account.IsOpenAIStoreEnabled()
	if passthroughEnabled {
		attemptImageIntentInvalidated := false
		codexImageGenerationExplicitToolPolicy := codexImageGenerationExplicitToolPolicyAllow
		if account != nil {
			codexImageGenerationExplicitToolPolicy = account.CodexImageGenerationExplicitToolPolicy()
		}
		if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
			strippedBody, changed, stripErr := stripOpenAIImageGenerationToolsFromRawPayload(body)
			if stripErr != nil {
				return nil, stripErr
			}
			if changed {
				body = strippedBody
				originalBody = strippedBody
				attemptImageIntentInvalidated = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
			}
		}
		// 透传分支只需要轻量提取字段，避免热路径全量 Unmarshal。
		mappedModel := account.GetMappedModel(reqModel)
		reasoningEffort := extractOpenAIReasoningEffortFromBody(body, mappedModel)
		reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, mappedModel)
		// Invalid explicit instructions still roll back to the full transform
		// path. A missing field is synthesized inside forwardOpenAIPassthrough,
		// preserving the native passthrough transport.
		result, err := s.forwardOpenAIPassthrough(
			ctx,
			c,
			account,
			originalBody,
			canonicalImageIntentBody,
			reqModel,
			attemptImageIntentInvalidated,
			reasoningEffort,
			reqStream,
			startTime,
		)
		if err == nil {
			return result, nil
		}
		var rollbackErr *openAIPassthroughRollbackError
		if !errors.As(err, &rollbackErr) {
			return nil, err
		}
		// fall through: continue Forward execution as if passthroughEnabled was false
	}
	textFormatRaw := extractResponsesTextFormatRaw(body)

	bodyModified := false
	var reqBody map[string]any
	ensureReqBody := func() (map[string]any, error) {
		if requestView.HasPatches() {
			patchedBody, patchErr := requestView.ApplyPatches()
			if patchErr != nil {
				return nil, patchErr
			}
			body = patchedBody
			requestView = newOpenAIRequestView(body)
			reqBody = nil
			bodyModified = false
		}
		if reqBody != nil {
			return reqBody, nil
		}
		decoded, decodeErr := requestView.Decode(c)
		if decodeErr != nil {
			return nil, decodeErr
		}
		reqBody = decoded
		return reqBody, nil
	}
	markPatchSet := func(path string, value any) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				setOpenAIRequestMapPath(reqBody, path, value)
			}
			return
		}
		requestView.MarkPatchSet(path, value)
	}
	markPatchDelete := func(path string) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				deleteOpenAIRequestMapPath(reqBody, path)
			}
			return
		}
		requestView.MarkPatchDelete(path)
	}
	disablePatch := func() {
		requestView.DisablePatches()
	}
	markDecodedModified := func() {
		bodyModified = true
		disablePatch()
	}

	apiKey := getAPIKeyFromContext(c)
	imageGenerationAllowed := GroupAllowsImageGeneration(nil)
	if apiKey != nil {
		imageGenerationAllowed = GroupAllowsImageGeneration(apiKey.Group)
	}
	codexImageGenerationExplicitToolPolicy := codexImageGenerationExplicitToolPolicyAllow
	if isCodexCLI {
		codexImageGenerationExplicitToolPolicy = account.CodexImageGenerationExplicitToolPolicy()
	}
	imageIntent := IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body)
	if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripOpenAIImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
		}
		imageIntent = IsImageGenerationIntentMap(openAIResponsesEndpoint, reqModel, decoded)
	}
	codexCompactRequest := isOpenAIResponsesCompactPath(c) || (isCodexCLI && isCodexCompactRequest(body))
	codexImageGenerationBridgeForced := s.codexImageGenerationBridgeForcedEnabled(ctx, account, apiKey)
	if !codexImageGenerationBridgeForced && s != nil && s.cfg != nil && s.cfg.Gateway.CodexImageGenerationBridgeEnabled {
		codexImageGenerationBridgeForced = true
	}
	codexImageGenerationBridgeEnabled := isCodexCLI &&
		codexImageGenerationExplicitToolPolicy != codexImageGenerationExplicitToolPolicyStrip &&
		shouldEnableCodexImageGenerationBridge(c, body, reqModel, account, apiKey) &&
		imageGenerationAllowed &&
		s.isCodexImageGenerationBridgeEnabled(ctx, account, apiKey)
	if codexCompactRequest && isCodexCLI && !imageIntent {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip image_generation bridge for Codex compact request")
	}
	codexImageBridgeShouldApply := false
	if imageIntent && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": ImageGenerationPermissionMessage()}})
		return nil, errors.New("image generation disabled for group")
	}

	instructions := gjson.GetBytes(body, "instructions")
	instructionsEmpty := !instructions.Exists() || instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == ""
	if instructionsEmpty && account.UsesOpenAICodexProtocol() && !compatMessagesBridge && !nativeCNResponses && (!codexCompactRequest || imageIntent) {
		markPatchSet("instructions", defaultCodexSynthInstructions(reqModel))
	}

	isCompactRequest := codexCompactRequest
	requestedModel := reqModel
	billingModel, upstreamModel := resolveOpenAIForwardMappedModels(account, requestedModel, isCompactRequest)
	if isCompactRequest {
		if compactModel := s.resolveOpenAICompactFallbackModel(account, requestedModel); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if billingModel != requestedModel {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", requestedModel, billingModel, account.Name, isCodexCLI)
	}
	reqModel = billingModel
	if upstreamModel != requestedModel {
		markPatchSet("model", upstreamModel)
	}
	if upstreamModel != billingModel {
		if isCompactRequest {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Compact model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", requestedModel, upstreamModel, account.Name, isCodexCLI)
		} else {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Upstream model resolved: %s -> %s (account: %s, type: %s, isCodexCLI: %v)", billingModel, upstreamModel, account.Name, account.Type, isCodexCLI)
		}
	}
	if !isCompactRequest {
		// codex round42 fu60 / round43 fu61 (2026-05-20): native /v1/responses
		// path bottom guard. When the upstream model maps to a gpt-5.x
		// reasoning model, strip top-level temperature/top_p before any
		// transform runs. OAuth path also hits applyCodexOAuthTransform
		// below; the explicit strip here is idempotent for OAuth and
		// load-bearing for the APIKey-backed /v1/responses path.
		//
		// round43 fu61: switched from markPatchSet("...", nil) to
		// markPatchDelete("..."). The Set variant on a single-field
		// request would have run through the fast-patch path and emitted
		// "temperature": null instead of removing the key, which the
		// upstream still rejects as an unsupported parameter. Two-field
		// case falls through to disablePatch via mismatched patchPath, so
		// the marshal path runs and the deleted reqBody map keys are
		// already gone — net effect is correct in both shapes now.
		if apicompat.IsReasoningModel(upstreamModel) {
			if gjson.GetBytes(body, "temperature").Exists() {
				markPatchDelete("temperature")
			}
			if gjson.GetBytes(body, "top_p").Exists() {
				markPatchDelete("top_p")
			}
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()) == "minimal" {
		markPatchSet("reasoning.effort", "none")
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized reasoning.effort: minimal -> none (account: %s)", account.Name)
	}

	imageIntent = imageIntent || IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, nil) || isOpenAIImageGenerationModel(upstreamModel)
	if imageIntent && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": ImageGenerationPermissionMessage()}})
		return nil, errors.New("image generation disabled for group")
	}

	// /responses/compact 是会话压缩请求：上游不接受 tool_choice（400 unknown_parameter），
	// 注入 image_generation 工具也没有意义，整块豁免。
	if codexImageGenerationBridgeEnabled && !isCompactRequest {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		codexImageBridgeShouldApply = codexImageGenerationBridgeShouldFire(decoded) || codexImageGenerationBridgeForced
	}
	if imageGenerationAllowed && !isCompactRequest && (codexImageBridgeShouldApply || isOpenAIImageGenerationModel(requestView.Model) || openAIRequestBodyImageGenerationToolNeedsNormalization(body) || isOpenAIImageGenerationModel(upstreamModel)) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if codexImageBridgeShouldApply && ensureOpenAIResponsesImageGenerationTool(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Injected /responses image_generation tool for Codex client")
		}
		if codexImageGenerationBridgeEnabled && !isCompactRequest && ensureOpenAIResponsesImageGenerationToolChoiceAuto(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Set /responses image_generation tool_choice=auto for Codex client")
		}
		if normalizeOpenAIResponsesImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image_generation tool payload")
		}
		if normalizeOpenAIResponsesImageOnlyModel(decoded) {
			markDecodedModified()
			if model, ok := decoded["model"].(string); ok {
				upstreamModel = strings.TrimSpace(model)
			}
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image-only model request inbound_model=%s image_model=%s upstream_model=%s", requestView.Model, billingModel, upstreamModel)
		}
		if err := validateOpenAIResponsesImageModel(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "model"}})
			return nil, err
		}
		if hasOpenAIImageGenerationTool(decoded) {
			imageIntent = true
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
		}
		if codexImageBridgeShouldApply && applyCodexImageGenerationBridgeInstructions(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Added Codex image_generation bridge instructions")
		}
	} else if imageGenerationAllowed && imageIntent && openAIRequestBodyHasImageGenerationTool(body) {
		// 完整 image_generation tool 只做 raw 计费读取，校验/桥接/旧字段迁移命中时才展开大 input map。
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
	}

	if isCodexSparkModel(upstreamModel) && openAIRequestBodyMayContainImageInput(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if err := validateCodexSparkInput(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "input"}})
			return nil, err
		}
	}

	// gpt-5.3-codex-spark also rejects the image_generation tool (HTTP 400,
	// param=tools). Strip it here so both APIKey and OAuth /responses paths are
	// covered regardless of the image-generation feature gate.
	if isCodexSparkModel(upstreamModel) && openAIRequestBodyHasImageGenerationTool(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripCodexSparkImageGenerationTools(decoded) {
			markDecodedModified()
		}
	}

	if account.UsesOpenAICodexProtocol() {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		codexResult := codexTransformResult{}
		if compatMessagesBridge {
			codexResult = applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{IsCodexCLI: isCodexCLI, IsCompact: isCompactRequest, StoreEnabled: storeEnabled, SkipDefaultInstructions: true, PreserveToolCallIDs: true})
			ensureCodexOAuthInstructionsField(decoded)
			markDecodedModified()
		} else {
			codexResult = applyCodexOAuthTransform(decoded, isCodexCLI, isCompactRequest, storeEnabled)
		}
		if codexResult.Error != nil {
			if c != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"type":    "invalid_request_error",
					"message": "Conflicting tool names are not supported.",
				}})
			}
			return nil, codexResult.Error
		}
		setCodexToolNameReverse(c, codexResult.ToolNameReverse)
		if codexResult.Modified {
			markDecodedModified()
		}
		if !isCompactRequest && applyCodexClientMetadata(decoded, account) {
			markDecodedModified()
		}
		if !isCompactRequest && applyCodexAccountIdentityClientMetadataMap(decoded, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c)) {
			markDecodedModified()
		}
		stageCodexFingerprintIDs(c, nil)
		if !isCompactRequest {
			var clientHeaders http.Header
			if c != nil && c.Request != nil {
				clientHeaders = c.Request.Header
			}
			fingerprintIDs := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
			if fingerprintIDs != nil && applyCodexFingerprintClientMetadata(decoded, fingerprintIDs) {
				markDecodedModified()
			}
			// Store the same snapshot used for body rewriting so HTTP/WS header
			// rewriting cannot drift during this attempt or across failover.
			stageCodexFingerprintIDs(c, fingerprintIDs)
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
		// codex round33 fu52 (2026-05-19): post-transform orphan-output
		// rejection. PR #2523's PreserveReferences=false strips
		// item_reference under store=false; if the request still has a
		// function_call_output without inline function_call/tool_call
		// context (and no previous_response_id), upstream would 502.
		// Emit a local 400 instead to avoid the round-trip + give the
		// client a actionable shape-level error.
		if codexResult.PostTransformRequiresLocalReject {
			logger.L().Warn("openai responses: function_call_output orphan after codex oauth transform",
				zap.String("model", upstreamModel),
				zap.Int64("account_id", account.ID),
				zap.String("account_type", string(account.Type)),
				zap.String("reason", "store_false_dropped_item_reference_no_inline_tool_context"),
			)
			if c != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type": "invalid_request_error",
						// codex round34 fu53 (2026-05-19): neutralized wording.
						// Earlier draft said "under store=false on this OAuth
						// account" which leaks implementation detail (gateway
						// internals, account topology). The client-visible
						// message now only describes WHAT shape they need to
						// send; the WHY stays in the Warn log above for ops.
						"message": "function_call_output requires an inline function_call/tool_call providing call_id, or a supported continuation transport; item_reference continuation is not supported for this request.",
					},
				})
			}
			return nil, errors.New("codex oauth transform left orphan function_call_output without inline tool call context (round33 fu52)")
		}
	}

	if !SupportsVerbosity(upstreamModel) && gjson.GetBytes(body, "text.verbosity").Exists() {
		markPatchDelete("text.verbosity")
	}

	if !isCodexCLI {
		maxOutputTokens := gjson.GetBytes(body, "max_output_tokens")
		if maxOutputTokens.Exists() {
			switch account.Platform {
			case PlatformOpenAI, PlatformDeepseek:
				// Preserve the Responses-native output limit. Compatibility
				// upstreams that reject it are handled by the bounded 400 retry.
			case PlatformAnthropic:
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				delete(decoded, "max_output_tokens")
				if _, hasMaxTokens := decoded["max_tokens"]; !hasMaxTokens {
					decoded["max_tokens"] = maxOutputTokens.Value()
				}
				markDecodedModified()
			case PlatformGemini:
				markPatchDelete("max_output_tokens")
			default:
				markPatchDelete("max_output_tokens")
			}
		}
		// Some clients still send the Chat Completions spelling. Normalize it
		// only for OpenAI Responses; Anthropic legitimately uses max_tokens.
		if account.Platform == PlatformOpenAI {
			if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() {
				if !gjson.GetBytes(body, "max_output_tokens").Exists() {
					markPatchSet("max_output_tokens", maxTokens.Value())
				}
				markPatchDelete("max_tokens")
			}
		}
		if gjson.GetBytes(body, "max_completion_tokens").Exists() && (account.Type == AccountTypeAPIKey || account.Platform != PlatformOpenAI) {
			markPatchDelete("max_completion_tokens")
		}
		for _, unsupportedField := range openAIResponsesUnsupportedFields {
			if gjson.GetBytes(body, unsupportedField).Exists() {
				markPatchDelete(unsupportedField)
			}
		}
	}

	// 仅在 WSv2 模式保留 previous_response_id，其他模式（HTTP/WSv1）统一过滤。
	// 注意：该规则同样适用于 Codex CLI 请求，避免 WSv1 向上游透传不支持字段。
	// 若账号启用 openai_store_enabled，HTTP 路径也保留 previous_response_id，以支持长会话 stored-response 续链。
	if wsDecision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 && !storeEnabled &&
		!account.IsOpenAIApiKey() && gjson.GetBytes(body, "previous_response_id").Exists() {
		markPatchDelete("previous_response_id")
	}

	if openAIRequestBodyMayContainEmptyBase64InputImage(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(decoded) {
			markDecodedModified()
		}
	}

	// Apply OpenAI fast policy (参照 Claude BetaPolicy 的 fast-mode 过滤)：
	// 针对 body 的 service_tier 字段（"priority" 即 fast，"flex"），按策略
	// 执行 filter（删除字段）或 block（拒绝请求）。对 gpt-5.5 等模型屏蔽
	// fast 时在此生效。
	//
	// 注意：
	//   1. 此处统一使用 upstreamModel（已经过 GetMappedModel +
	//      normalizeOpenAIModelForUpstream + Codex OAuth normalize），与
	//      chat-completions / messages 入口保持一致，避免不同入口因为模型
	//      维度不同而出现 whitelist 命中差异。
	//   2. action=pass 时也要把 raw "fast" 归一化为 "priority" 写回 body，
	//      否则 native /responses 入口透传 "fast" 给上游会被拒。chat-
	//      completions 入口由 normalizeResponsesBodyServiceTier 完成同一
	//      行为，这里手工实现等效逻辑。
	// codex 2026-05-16 round9: presence-based strip — any top-level
	// service_tier value (string, number, null, array, object, bool)
	// triggers the policy block. Only a recognized-string-tier with a
	// pass action survives; everything else gets stripped silently.
	// This closes the residual probe surface that previously let callers
	// hitting sub2api directly check whether the upstream validates
	// service_tier types ("is there an OpenAI backend behind us?").
	// Real Anthropic /v1/responses doesn't exist as a public spec; for
	// our native OpenAI surface, Anthropic-shaped clients never send
	// service_tier so this is invisible to legitimate traffic.
	groupForcesFast := openAIGroupForcesFast(ctx, account)
	if groupForcesFast {
		markPatchSet("service_tier", OpenAIFastTierPriority)
	}
	if tierResult := gjson.GetBytes(body, "service_tier"); tierResult.Exists() || groupForcesFast {
		if groupForcesFast || tierResult.Type == gjson.String {
			rawTier := strings.TrimSpace(tierResult.String())
			if groupForcesFast {
				rawTier = OpenAIFastTierPriority
			}
			if normTier := normalizedOpenAIServiceTierValue(rawTier); normTier != "" {
				action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, upstreamModel, normTier)
				switch action {
				case BetaPolicyActionBlock:
					msg := errMsg
					if msg == "" {
						msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, upstreamModel)
					}
					blocked := &OpenAIFastBlockedError{Message: msg}
					writeOpenAIFastPolicyBlockedResponse(c, blocked)
					return nil, blocked
				case BetaPolicyActionFilter:
					markPatchDelete("service_tier")
				case OpenAIFastPolicyActionForcePriority:
					if rawTier != OpenAIFastTierPriority {
						markPatchSet("service_tier", OpenAIFastTierPriority)
					}
				default:
					// pass：若客户端传的是别名 "fast"，归一化为 "priority"
					// 后写回 body，确保上游收到的是其能识别的规范值。
					if normTier != rawTier {
						markPatchSet("service_tier", normTier)
					}
				}
			} else {
				// Unknown / empty string tier.
				markPatchDelete("service_tier")
			}
		} else {
			// Non-string value (number / null / array / object / bool).
			// Round9 hardening: strip silently — see comment above.
			markPatchDelete("service_tier")
		}
	}

	if account.UsesOpenAICodexProtocol() {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if input, ok := decoded["input"].([]any); ok && sanitizeOpenAIResponsesOrphanToolOutputs(
			decoded,
			input,
			strings.TrimSpace(firstNonEmptyString(decoded["previous_response_id"])) != "",
		) {
			markDecodedModified()
		}
	}

	requestBodyChanged := bodyModified
	if bodyModified {
		if requestView.HasPatches() {
			if patchedBody, patchErr := requestView.ApplyPatches(); patchErr == nil {
				body = patchedBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				bodyModified = false
			}
		}
		if bodyModified {
			decoded, decodeErr := ensureReqBody()
			if decodeErr != nil {
				return nil, decodeErr
			}
			var marshalErr error
			body, marshalErr = marshalOpenAIUpstreamJSON(decoded)
			if marshalErr != nil {
				return nil, fmt.Errorf("serialize request body: %w", marshalErr)
			}
			requestView = newOpenAIRequestView(body)
		}
	}
	if requestBodyChanged && account.UsesOpenAICodexProtocol() {
		var formatErr error
		body, formatErr = restoreResponsesTextFormatRaw(body, textFormatRaw)
		if formatErr != nil {
			return nil, fmt.Errorf("restore text.format after request transform: %w", formatErr)
		}
		requestView = newOpenAIRequestView(body)
	}
	// Run after orphan-output filtering and all request-map rebuilds so a
	// compaction trigger cannot remain ahead of surviving history items.
	if normalizedBody, changed, normalizeErr := NormalizeCompactionTriggerInputOrder(body); normalizeErr != nil {
		return nil, fmt.Errorf("normalize compaction trigger order: %w", normalizeErr)
	} else if changed {
		body = normalizedBody
		requestView = newOpenAIRequestView(body)
		reqBody = nil
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if imageIntent {
		var imageCfg OpenAIResponsesImageBillingConfig
		var imageCfgErr error
		if reqBody != nil {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailed(reqBody, billingModel)
		} else {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, billingModel)
		}
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": imageCfgErr.Error(), "param": "size"}})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	// Capture upstream request body for ops retry of this attempt.
	setOpsUpstreamRequestBody(c, body)
	SetOpsUpstreamModel(c, upstreamModel)

	// 命中 WS 时仅走 WebSocket Mode；不再自动回退 HTTP。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		// WS 分支需要结构化 payload 与重连恢复，命中后再触发 full-map decode。
		wsReqBody, err := ensureReqBody()
		if err != nil {
			return nil, err
		}
		_, hasPreviousResponseID := wsReqBody["previous_response_id"]
		logOpenAIWSModeDebug(
			"forward_start account_id=%d account_type=%s model=%s stream=%v has_previous_response_id=%v",
			account.ID,
			account.Type,
			upstreamModel,
			reqStream,
			hasPreviousResponseID,
		)
		maxAttempts := openAIWSReconnectRetryLimit + 1
		wsAttempts := 0
		var wsResult *OpenAIForwardResult
		var wsErr error
		wsLastFailureReason := ""
		wsPrevResponseRecoveryTried := false
		wsInvalidEncryptedContentRecoveryTried := false
		recoverPrevResponseNotFound := func(attempt int) bool {
			if wsPrevResponseRecoveryTried {
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			if previousResponseID == "" {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=missing_previous_response_id previous_response_id_present=false",
					account.ID,
					attempt,
				)
				return false
			}
			if HasFunctionCallOutput(wsReqBody) {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=has_function_call_output previous_response_id_present=true",
					account.ID,
					attempt,
				)
				return false
			}
			delete(wsReqBody, "previous_response_id")
			wsPrevResponseRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_prev_response_recovery account_id=%d attempt=%d action=drop_previous_response_id retry=1 previous_response_id=%s previous_response_id_kind=%s",
				account.ID,
				attempt,
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
			)
			return true
		}
		recoverInvalidEncryptedContent := func(attempt int) bool {
			if wsInvalidEncryptedContentRecoveryTried {
				return false
			}
			removedReasoningItems := trimOpenAIEncryptedReasoningItems(wsReqBody)
			if !removedReasoningItems {
				logOpenAIWSModeInfo(
					"reconnect_invalid_encrypted_content_recovery_skip account_id=%d attempt=%d reason=missing_encrypted_reasoning_items",
					account.ID,
					attempt,
				)
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			hasFunctionCallOutput := HasFunctionCallOutput(wsReqBody)
			if previousResponseID != "" && !hasFunctionCallOutput {
				delete(wsReqBody, "previous_response_id")
			}
			wsInvalidEncryptedContentRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_invalid_encrypted_content_recovery account_id=%d attempt=%d action=drop_encrypted_reasoning_items retry=1 previous_response_id_present=%v previous_response_id=%s previous_response_id_kind=%s has_function_call_output=%v dropped_previous_response_id=%v",
				account.ID,
				attempt,
				previousResponseID != "",
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
				hasFunctionCallOutput,
				previousResponseID != "" && !hasFunctionCallOutput,
			)
			return true
		}
		retryBudget := s.openAIWSRetryTotalBudget()
		retryStartedAt := time.Now()
	wsRetryLoop:
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			wsAttempts = attempt
			wsResult, wsErr = s.forwardOpenAIWSV2(
				ctx,
				c,
				account,
				wsReqBody,
				token,
				wsDecision,
				isCodexCLI,
				reqStream,
				originalModel,
				upstreamModel,
				startTime,
				attempt,
				wsLastFailureReason,
			)
			if wsErr == nil {
				break
			}
			if c != nil && c.Writer != nil && c.Writer.Written() {
				break
			}

			reason, retryable := classifyOpenAIWSReconnectReason(wsErr)
			if reason != "" {
				wsLastFailureReason = reason
			}
			// previous_response_not_found 说明续链锚点不可用：
			// 对非 function_call_output 场景，允许一次“去掉 previous_response_id 后重放”。
			if reason == "previous_response_not_found" && recoverPrevResponseNotFound(attempt) {
				continue
			}
			if reason == "invalid_encrypted_content" && recoverInvalidEncryptedContent(attempt) {
				continue
			}
			if retryable && attempt < maxAttempts {
				backoff := s.openAIWSRetryBackoff(attempt)
				if retryBudget > 0 && time.Since(retryStartedAt)+backoff > retryBudget {
					s.recordOpenAIWSRetryExhausted()
					logOpenAIWSModeInfo(
						"reconnect_budget_exhausted account_id=%d attempts=%d max_retries=%d reason=%s elapsed_ms=%d budget_ms=%d",
						account.ID,
						attempt,
						openAIWSReconnectRetryLimit,
						normalizeOpenAIWSLogValue(reason),
						time.Since(retryStartedAt).Milliseconds(),
						retryBudget.Milliseconds(),
					)
					break
				}
				s.recordOpenAIWSRetryAttempt(backoff)
				logOpenAIWSModeInfo(
					"reconnect_retry account_id=%d retry=%d max_retries=%d reason=%s backoff_ms=%d",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
					backoff.Milliseconds(),
				)
				if backoff > 0 {
					timer := time.NewTimer(backoff)
					select {
					case <-ctx.Done():
						if !timer.Stop() {
							<-timer.C
						}
						wsErr = wrapOpenAIWSFallback("retry_backoff_canceled", ctx.Err())
						break wsRetryLoop
					case <-timer.C:
					}
				}
				continue
			}
			if retryable {
				s.recordOpenAIWSRetryExhausted()
				logOpenAIWSModeInfo(
					"reconnect_exhausted account_id=%d attempts=%d max_retries=%d reason=%s",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
				)
			} else if reason != "" {
				s.recordOpenAIWSNonRetryableFastFallback()
				logOpenAIWSModeInfo(
					"reconnect_stop account_id=%d attempt=%d reason=%s",
					account.ID,
					attempt,
					normalizeOpenAIWSLogValue(reason),
				)
			}
			break
		}
		if wsErr == nil {
			firstTokenMs := int64(0)
			hasFirstTokenMs := wsResult != nil && wsResult.FirstTokenMs != nil
			if hasFirstTokenMs {
				firstTokenMs = int64(*wsResult.FirstTokenMs)
			}
			requestID := ""
			if wsResult != nil {
				requestID = strings.TrimSpace(wsResult.RequestID)
			}
			logOpenAIWSModeDebug(
				"forward_succeeded account_id=%d request_id=%s stream=%v has_first_token_ms=%v first_token_ms=%d ws_attempts=%d",
				account.ID,
				requestID,
				reqStream,
				hasFirstTokenMs,
				firstTokenMs,
				wsAttempts,
			)
			wsResult.UpstreamModel = upstreamModel
			if wsResult.BillingModel == "" {
				wsResult.BillingModel = billingModel
			}
			if wsResult.ImageCount > 0 {
				wsResult.ImageSize = imageSizeTier
				wsResult.ImageInputSize = imageInputSize
				wsResult.BillingModel = imageBillingModel
			}
			return wsResult, nil
		}
		s.writeOpenAIWSFallbackErrorResponse(c, account, wsErr)
		return nil, wsErr
	}

	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	// 国产模型默认 effort 补充：此处 reqModel 已被 mapping 重写为 billingModel。
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, reqModel)
	reasoningEffortValue := ""
	if reasoningEffort != nil {
		reasoningEffortValue = *reasoningEffort
	}
	firstOutputTimeout := time.Duration(0)
	if reqStream && account.Platform == PlatformOpenAI {
		firstOutputTimeout = s.openAIFirstOutputTimeout(reasoningEffortValue)
	}

	httpInvalidEncryptedContentRetryTried := false
	agentTaskRecoveryTried := false
	compactModelFallbackRetried := false
	rejectedFieldRetryState := openAIResponsesRejectedFieldRetryStateForRequest(c, body)
	for {
		// Build upstream request
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		var headerGuard *openAIFirstOutputHeaderGuard
		if firstOutputTimeout > 0 {
			upstreamCtx, headerGuard = newOpenAIFirstOutputHeaderGuard(
				upstreamCtx, releaseUpstreamCtx, startTime.Add(firstOutputTimeout),
			)
		}
		upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, body, token, reqStream, promptCacheKey, isCodexCLI)
		if headerGuard == nil {
			releaseUpstreamCtx()
		}
		if err != nil {
			if headerGuard != nil {
				headerGuard.close()
			}
			return nil, err
		}

		// Get proxy URL
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.ActiveURL()
		}

		// Send request
		stopCompactKeepalive := func() {}
		if !reqStream {
			stopCompactKeepalive = s.startCompactNonstreamKeepalive(ctx, c)
		}
		upstreamStart := time.Now()
		resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		if headerGuard != nil && headerGuard.stopHeaderWait() {
			stopCompactKeepalive()
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			headerGuard.close()
			return nil, s.newOpenAIFirstOutputTimeoutError(
				ctx, c, account, startTime, originalModel, reasoningEffortValue,
				firstOutputTimeout, "response_headers", nil,
			)
		}
		if err != nil {
			stopCompactKeepalive()
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if headerGuard != nil {
				headerGuard.close()
			}
			// Transport-level failure (proxy/DNS/TCP/TLS — no HTTP response). Convert to
			// a failover so the handler switches to a healthy account, and temporarily
			// unschedule the account on durable faults (e.g. rejected proxy credentials).
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
		}
		if headerGuard != nil {
			resp.Body = &openAIRequestContextReadCloser{ReadCloser: resp.Body, cleanup: headerGuard.close}
		}

		// Handle error response
		if resp.StatusCode >= 400 {
			stopCompactKeepalive()
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
			upstreamCode := extractUpstreamErrorCode(respBody)
			if !agentTaskRecoveryTried && s.isAgentIdentityAccount(ctx, account) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, respBody) {
				agentTaskRecoveryTried = true
				expectedTaskID := account.GetCredential("task_id")
				if recoveryErr := s.recoverAgentIdentityTask(ctx, account, expectedTaskID); recoveryErr != nil {
					return nil, fmt.Errorf("agent identity task recovery failed: %w", recoveryErr)
				}
				continue
			}
			respBody = s.redactAgentIdentitySensitiveBody(ctx, account, respBody)
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			if isOpenAIResponsesCompactPath(c) && c != nil && c.Writer != nil && c.Writer.Written() {
				logOpenAICompactKeepaliveCommitted(ctx, c, account, resp)
				return s.handleErrorResponse(ctx, resp, c, account, body, billingModel)
			}
			if !httpInvalidEncryptedContentRetryTried && resp.StatusCode == http.StatusBadRequest && upstreamCode == "invalid_encrypted_content" {
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				if trimOpenAIEncryptedReasoningItems(decoded) {
					body, err = marshalOpenAIUpstreamJSON(decoded)
					if err != nil {
						return nil, fmt.Errorf("serialize invalid_encrypted_content retry body: %w", err)
					}
					setOpsUpstreamRequestBody(c, body)
					httpInvalidEncryptedContentRetryTried = true
					rejectedFieldRetryState.remember(body)
					logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request once after invalid_encrypted_content (account: %s)", account.Name)
					continue
				}
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip non-WSv2 invalid_encrypted_content retry because encrypted reasoning items are missing (account: %s)", account.Name)
			}
			if retryBody, reason, changed, retryErr := normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, body, respBody); retryErr != nil {
				return nil, fmt.Errorf("normalize rejected Responses field retry body: %w", retryErr)
			} else if changed && rejectedFieldRetryState.Allow(retryBody) {
				body = retryBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				setOpsUpstreamRequestBody(c, body)
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request after %s (account: %s)", reason, account.Name)
				continue
			}
			if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
				c, account, requestedModel, body, resp.StatusCode, upstreamMsg, respBody, compactModelFallbackRetried,
			); retry {
				s.appendOpenAICompactFallbackRetryOps(c, account, resp, respBody, upstreamMsg, false)
				fromModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
				body = retryBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				upstreamModel = fallbackModel
				compactModelFallbackRetried = true
				SetOpsUpstreamModel(c, fallbackModel)
				logger.LegacyPrintf(
					"service.openai_gateway",
					"[OpenAI] Retrying explicit compact request once with fallback model (account: %s, from: %s, to: %s, upstream_code: %s)",
					account.Name, fromModel, fallbackModel, upstreamCode,
				)
				continue
			}
			if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
				upstreamDetail := ""
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
					if maxBytes <= 0 {
						maxBytes = 2048
					}
					upstreamDetail = truncateString(string(respBody), maxBytes)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "failover",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				shouldDisable := s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
				return nil, s.newOpenAIAccountFailoverError(
					account,
					resp.StatusCode,
					resp.Header,
					respBody,
					upstreamMsg,
					shouldDisable,
					!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
				)
			}
			return s.handleErrorResponse(ctx, resp, c, account, body, billingModel)
		}
		defer func() { _ = resp.Body.Close() }()

		// Client-only custom/tool_search/namespace tools are lowered before a
		// native DeepSeek Responses request. Streaming restoration is stateful:
		// argument deltas must be buffered and function-call lifecycle events
		// rewritten back to the original client-tool event types.
		if mapping, ok := openAIResponsesClientToolMapping(c); ok && isEventStreamResponse(resp.Header) {
			maxLineSize := defaultMaxLineSize
			if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
				maxLineSize = s.cfg.Gateway.MaxLineSize
			}
			resp.Body = newResponsesClientToolStreamBody(resp.Body, mapping, maxLineSize)
		}

		serviceTier := extractOpenAIServiceTierFromBody(body)
		// 上游接受后只保留计费需要的标量，避免响应处理期间继续保活完整 input/tools map。
		reqBody = nil

		// Handle normal response
		var usage *OpenAIUsage
		var firstTokenMs *int
		responseID := ""
		imageCount := 0
		var imageOutputSizes []string
		if reqStream {
			stopCompactKeepalive()
			streamResult, err := s.handleStreamingResponseWithReasoning(ctx, resp, c, account, startTime, originalModel, upstreamModel, reasoningEffortValue)
			if err != nil {
				if signal, ok := asOpenAICompactFallbackSignal(err); ok {
					if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
						c, account, requestedModel, body, http.StatusBadRequest, signal.message, signal.payload, compactModelFallbackRetried,
					); retry {
						s.appendOpenAICompactFallbackRetryOps(c, account, resp, signal.payload, signal.message, false)
						_ = resp.Body.Close()
						body = retryBody
						requestView = newOpenAIRequestView(body)
						upstreamModel = fallbackModel
						compactModelFallbackRetried = true
						SetOpsUpstreamModel(c, fallbackModel)
						continue
					}
					_ = resp.Body.Close()
					compactResp, compactBody := openAICompactFallbackErrorResponse(resp, signal)
					if s.shouldFailoverOpenAIUpstreamResponse(compactResp.StatusCode, signal.message, compactBody) {
						appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
							Platform:           account.Platform,
							AccountID:          account.ID,
							AccountName:        account.Name,
							UpstreamStatusCode: compactResp.StatusCode,
							UpstreamRequestID:  compactResp.Header.Get("x-request-id"),
							Kind:               "failover",
							Message:            signal.message,
						})
						shouldDisable := s.handleFailoverSideEffects(ctx, compactResp, account, compactBody, upstreamModel)
						return nil, s.newOpenAIAccountFailoverError(
							account, compactResp.StatusCode, compactResp.Header, compactBody, signal.message, shouldDisable,
							!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(compactResp.StatusCode) || isOpenAITransientProcessingError(compactResp.StatusCode, signal.message, compactBody)),
						)
					}
					return s.handleErrorResponse(ctx, compactResp, c, account, body, resolveOpenAIErrorSchedulingModel(billingModel, upstreamModel))
				}
				return nil, err
			}
			usage = streamResult.usage
			firstTokenMs = streamResult.firstTokenMs
			responseID = strings.TrimSpace(streamResult.responseID)
			imageCount = streamResult.imageCount
			imageOutputSizes = streamResult.imageOutputSizes
		} else {
			nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, stopCompactKeepalive)
			if err != nil {
				if signal, ok := asOpenAICompactFallbackSignal(err); ok {
					if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
						c, account, requestedModel, body, http.StatusBadRequest, signal.message, signal.payload, compactModelFallbackRetried,
					); retry {
						s.appendOpenAICompactFallbackRetryOps(c, account, resp, signal.payload, signal.message, false)
						_ = resp.Body.Close()
						body = retryBody
						requestView = newOpenAIRequestView(body)
						upstreamModel = fallbackModel
						compactModelFallbackRetried = true
						SetOpsUpstreamModel(c, fallbackModel)
						continue
					}
				}
				return nil, err
			}
			usage = nonStreamResult.usage
			responseID = strings.TrimSpace(nonStreamResult.responseID)
			imageCount = nonStreamResult.imageCount
			imageOutputSizes = nonStreamResult.imageOutputSizes
		}
		s.bindHTTPResponseAccount(ctx, c, account, responseID)

		// Extract and save Codex usage snapshot from response headers (for OAuth accounts)
		if account.Type == AccountTypeOAuth && !account.IsShadow() {
			if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		}

		if usage == nil {
			usage = &OpenAIUsage{}
		}

		forwardResult := &OpenAIForwardResult{
			RequestID:       resp.Header.Get("x-request-id"),
			ResponseID:      responseID,
			Usage:           *usage,
			Model:           originalModel,
			BillingModel:    billingModel,
			UpstreamModel:   upstreamModel,
			ServiceTier:     serviceTier,
			ReasoningEffort: reasoningEffort,
			Stream:          reqStream,
			OpenAIWSMode:    false,
			Duration:        time.Since(startTime),
			FirstTokenMs:    firstTokenMs,
		}
		if imageCount > 0 {
			imageBillingModel, imageSizeTier = ensureOpenAIImageBillingDefaults(imageCount, imageBillingModel, imageSizeTier)
			forwardResult.ImageCount = imageCount
			forwardResult.ImageSize = imageSizeTier
			forwardResult.ImageInputSize = imageInputSize
			forwardResult.ImageOutputSizes = imageOutputSizes
			forwardResult.BillingModel = imageBillingModel
		}
		return forwardResult, nil
	}
}

func (s *OpenAIGatewayService) forwardOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	canonicalImageIntentBody []byte,
	reqModel string,
	attemptImageIntentInvalidated bool,
	reasoningEffort *string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestedModel := reqModel
	upstreamPassthroughModel := ""
	if isOpenAIResponsesCompactPath(c) {
		compactMappedModel := resolveOpenAICompactForwardModel(account, reqModel)
		if compactMappedModel != "" && compactMappedModel != reqModel {
			nextBody, setErr := sjson.SetBytes(body, "model", compactMappedModel)
			if setErr != nil {
				return nil, fmt.Errorf("set compact passthrough model: %w", setErr)
			}
			body = nextBody
			upstreamPassthroughModel = compactMappedModel
			attemptImageIntentInvalidated = true
		}
	}

	if account != nil && account.UsesOpenAICodexProtocol() {
		if rejectReason := detectOpenAIPassthroughInstructionsRejectReason(reqModel, body); rejectReason != "" {
			// codex upstream PR#2498 (2026-05-16): instead of returning
			// internal-style 403 to client, signal the caller to roll
			// back to the non-passthrough (full transform) path. Old
			// 403 leaked fork architecture; rollback path transparently
			// retries the request via OAuth transform.
			logOpenAIPassthroughInstructionsRejected(ctx, c, account, reqModel, rejectReason, body)
			return nil, &openAIPassthroughRollbackError{Reason: rejectReason}
		}
		if isOpenAICodexModel(reqModel) && !gjson.GetBytes(body, "instructions").Exists() {
			defaultInstructions := defaultCodexSynthInstructions(reqModel)
			nextBody, setErr := sjson.SetBytes(body, "instructions", defaultInstructions)
			if setErr != nil {
				return nil, fmt.Errorf("set passthrough codex instructions: %w", setErr)
			}
			body = nextBody
		}

		normalizedBody, normalized, err := normalizeOpenAIPassthroughOAuthBody(body, isOpenAIResponsesCompactPath(c), account.IsOpenAIStoreEnabled())
		if err != nil {
			return nil, err
		}
		if normalized {
			body = normalizedBody
		}
		reqStream = gjson.GetBytes(body, "stream").Bool()

		accountScopedBody, accountScoped, scopeErr := applyCodexAccountIdentityClientMetadataRaw(body, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))
		if scopeErr != nil {
			return nil, scopeErr
		}
		if accountScoped {
			body = accountScopedBody
		}

		stageCodexFingerprintIDs(c, nil)
		if !isOpenAIResponsesCompactPath(c) {
			var clientHeaders http.Header
			if c != nil && c.Request != nil {
				clientHeaders = c.Request.Header
			}
			fingerprintIDs := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
			if fingerprintIDs != nil {
				fingerprintedBody, changed, fingerprintErr := applyCodexFingerprintClientMetadataRaw(body, fingerprintIDs)
				if fingerprintErr != nil {
					return nil, fingerprintErr
				}
				if changed {
					body = fingerprintedBody
				}
			}
			stageCodexFingerprintIDs(c, fingerprintIDs)
		}
	}
	if account != nil && account.IsOpenAI() {
		responsesLite := isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)) || isOpenAIResponsesLiteWebSocketPayload(body)
		normalizedBody, normalized, normalizeErr := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, responsesLite)
		if normalizeErr != nil {
			return nil, fmt.Errorf("normalize passthrough Responses compatibility: %w", normalizeErr)
		}
		if normalized {
			body = normalizedBody
		}
		if account.IsOpenAIOAuthLike() {
			aliasedBody, reverse, aliased, aliasErr := aliasOpenAIOAuthReservedToolNamesBody(body)
			if aliasErr != nil {
				return nil, aliasErr
			}
			mergeCodexToolNameReverse(c, reverse)
			if aliased {
				body = aliasedBody
			}
		}
	}
	if account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		!isOpenAIResponsesCompactPath(c) && needsOpenAIResponsesClientToolAdaptation(body) {
		adaptedBody, mapping, adaptErr := adaptOpenAIResponsesClientTools(body)
		if adaptErr != nil {
			return nil, adaptErr
		}
		body = adaptedBody
		setOpenAIResponsesClientToolMapping(c, mapping)
	}
	sanitizedBody, sanitized, err := sanitizeEmptyBase64InputImagesInOpenAIBody(body)
	if err != nil {
		return nil, err
	}
	if sanitized {
		body = sanitizedBody
	}

	// Apply OpenAI fast policy to the passthrough body (filter/block by service_tier).
	// 统一使用 upstream 视角的 model：透传路径下 body 已经过 compact 映射 +
	// OAuth normalize，body 中的 model 字段即上游真正会看到的 slug。
	// 这样可以与 chat-completions / messages / native /responses 入口的
	// upstreamModel 保持一致，避免 whitelist 命中差异。当 body 中没有
	// model 字段时退回 reqModel。
	policyModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if policyModel == "" {
		policyModel = reqModel
	}

	// codex round43 fu61 (2026-05-20): passthrough mode forwards the
	// client body raw — none of the non-passthrough paths' strip logic
	// runs. If the upstream model is gpt-5.x, temperature/top_p still
	// reach upstream and trigger 400 "Unsupported parameter". This is
	// codex round43's #2 finding (the missing fourth entry point).
	// Same shared helper as the Cursor branch and the native
	// /v1/responses path so all four entry points stay consistent.
	if stripped, modified, serr := stripSamplingParamsForReasoningModelBody(policyModel, body); serr == nil && modified {
		body = stripped
	}

	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, policyModel, body)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			writeOpenAIFastPolicyBlockedResponse(c, blocked)
		}
		return nil, policyErr
	}
	body = updatedBody

	apiKey := getAPIKeyFromContext(c)
	// Use the canonical request hint across failover, but recompute the current
	// attempt when account-local compact/tool rewrites invalidated that hint.
	imageIntent := resolveOpenAIPassthroughImageIntent(
		c,
		requestedModel,
		canonicalImageIntentBody,
		policyModel,
		body,
		attemptImageIntentInvalidated,
		IsImageGenerationIntent,
	)
	if imageIntent && !GroupAllowsImageGeneration(apiKeyGroup(apiKey)) {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "permission_error",
				"message": ImageGenerationPermissionMessage(),
			},
		})
		return nil, errors.New("image generation disabled for group")
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if imageIntent {
		imageCfg, imageCfgErr := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, requestedModel)
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": imageCfgErr.Error(),
					"param":   "size",
				},
			})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI 自动透传] 命中自动透传分支: account=%d name=%s type=%s model=%s stream=%v",
		account.ID,
		account.Name,
		account.Type,
		reqModel,
		reqStream,
	)
	if reqStream && c != nil && c.Request != nil {
		if timeoutHeaders := collectOpenAIPassthroughTimeoutHeaders(c.Request.Header); len(timeoutHeaders) > 0 {
			streamWarnLogger := logger.FromContext(ctx).With(
				zap.String("component", "service.openai_gateway"),
				zap.Int64("account_id", account.ID),
				zap.Strings("timeout_headers", timeoutHeaders),
			)
			if s.isOpenAIPassthroughTimeoutHeadersAllowed() {
				streamWarnLogger.Warn("OpenAI passthrough 透传请求包含超时相关请求头，且当前配置为放行，可能导致上游提前断流")
			} else {
				streamWarnLogger.Warn("OpenAI passthrough 检测到超时相关请求头，将按配置过滤以降低断流风险")
			}
		}
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.ActiveURL()
	}

	if c != nil {
		c.Set("openai_passthrough", true)
	}

	agentTaskRecoveryTried := false
	compactModelFallbackRetried := false
	rejectedFieldRetryState := openAIResponsesRejectedFieldRetryStateForRequest(c, body)
	var resp *http.Response
	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	imageCount := 0
	var imageOutputSizes []string
	var stopCompactKeepalive func()
	for {
		for {
			actualModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
			if actualModel == "" {
				actualModel = reqModel
			}
			SetOpsUpstreamModel(c, actualModel)
			setOpsUpstreamRequestBody(c, body)
			upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
			upstreamReq, buildErr := s.buildUpstreamRequestOpenAIPassthrough(upstreamCtx, c, account, body, token)
			releaseUpstreamCtx()
			if buildErr != nil {
				return nil, buildErr
			}

			stopCompactKeepalive = func() {}
			if !reqStream {
				stopCompactKeepalive = s.startCompactNonstreamKeepalive(ctx, c)
			}

			upstreamStart := time.Now()
			resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
			SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
			if err != nil {
				stopCompactKeepalive()
				// Transport-level failure (proxy/DNS/TCP/TLS — no HTTP response). Convert to
				// a failover so the handler switches to a healthy account, and temporarily
				// unschedule the account on durable faults (e.g. rejected proxy credentials).
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
			}
			if resp.StatusCode < 400 {
				break
			}

			stopCompactKeepalive()
			if isOpenAIResponsesCompactPath(c) && c != nil && c.Writer != nil && c.Writer.Written() {
				logOpenAICompactKeepaliveCommitted(ctx, c, account, resp)
				handleErr := s.handleErrorResponsePassthrough(ctx, resp, c, account, body)
				_ = resp.Body.Close()
				return nil, handleErr
			}
			// Read once so retry classification, failover classification and the
			// eventual response handler all observe the same bounded payload.
			probeBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(probeBody))
			if retryBody, reason, changed, retryErr := normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, body, probeBody); retryErr != nil {
				return nil, fmt.Errorf("normalize passthrough rejected Responses field retry body: %w", retryErr)
			} else if changed && rejectedFieldRetryState.Allow(retryBody) {
				body = retryBody
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying passthrough request after %s (account: %s)", reason, account.Name)
				continue
			}
			if !agentTaskRecoveryTried && s.isAgentIdentityAccount(ctx, account) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, probeBody) {
				agentTaskRecoveryTried = true
				expectedTaskID := account.GetCredential("task_id")
				if recoveryErr := s.recoverAgentIdentityTask(ctx, account, expectedTaskID); recoveryErr != nil {
					return nil, fmt.Errorf("agent identity task recovery failed: %w", recoveryErr)
				}
				continue
			}
			upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(probeBody)))
			if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
				c, account, requestedModel, body, resp.StatusCode, upstreamMsg, probeBody, compactModelFallbackRetried,
			); retry {
				s.appendOpenAICompactFallbackRetryOps(c, account, resp, probeBody, upstreamMsg, true)
				fromModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
				body = retryBody
				upstreamPassthroughModel = fallbackModel
				compactModelFallbackRetried = true
				SetOpsUpstreamModel(c, fallbackModel)
				logger.LegacyPrintf(
					"service.openai_gateway",
					"[OpenAI passthrough] Retrying explicit compact request once with fallback model (account: %s, from: %s, to: %s, upstream_code: %s)",
					account.Name, fromModel, fallbackModel, extractUpstreamErrorCode(probeBody),
				)
				continue
			}
			if shouldFailoverOpenAIPassthroughResponse(account, resp.StatusCode, probeBody) {
				return nil, s.handleFailoverErrorResponsePassthrough(ctx, resp, c, account, body, probeBody)
			}
			return nil, s.handleErrorResponsePassthrough(ctx, resp, c, account, body, probeBody)
		}
		if mapping, ok := openAIResponsesClientToolMapping(c); ok && isEventStreamResponse(resp.Header) {
			maxLineSize := defaultMaxLineSize
			if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
				maxLineSize = s.cfg.Gateway.MaxLineSize
			}
			resp.Body = newGrokResponsesClientToolStreamBody(resp.Body, mapping, maxLineSize)
		}
		if reqStream {
			stopCompactKeepalive()
			result, handleErr := s.handleStreamingResponsePassthrough(ctx, resp, c, account, startTime, reqModel, upstreamPassthroughModel)
			if handleErr != nil {
				if retryBody, fallbackModel, retry := s.applyOpenAIPassthroughCompactFallbackFromSignal(
					c, account, requestedModel, body, handleErr, compactModelFallbackRetried, resp,
				); retry {
					_ = resp.Body.Close()
					body = retryBody
					upstreamPassthroughModel = fallbackModel
					compactModelFallbackRetried = true
					continue
				}
				if signal, ok := asOpenAICompactFallbackSignal(handleErr); ok {
					_ = resp.Body.Close()
					compactResp, compactBody := openAICompactFallbackErrorResponse(resp, signal)
					if shouldFailoverOpenAIPassthroughResponse(account, compactResp.StatusCode, compactBody) {
						return nil, s.handleFailoverErrorResponsePassthrough(ctx, compactResp, c, account, body, compactBody)
					}
					return nil, s.handleErrorResponsePassthrough(ctx, compactResp, c, account, body, compactBody)
				}
				_ = resp.Body.Close()
				return nil, handleErr
			}
			usage = result.usage
			firstTokenMs = result.firstTokenMs
			responseID = strings.TrimSpace(result.responseID)
			imageCount = result.imageCount
			imageOutputSizes = result.imageOutputSizes
		} else {
			result, handleErr := s.handleNonStreamingResponsePassthroughForAccount(ctx, resp, c, account, reqModel, upstreamPassthroughModel, stopCompactKeepalive)
			if handleErr != nil {
				if retryBody, fallbackModel, retry := s.applyOpenAIPassthroughCompactFallbackFromSignal(
					c, account, requestedModel, body, handleErr, compactModelFallbackRetried, resp,
				); retry {
					_ = resp.Body.Close()
					body = retryBody
					upstreamPassthroughModel = fallbackModel
					compactModelFallbackRetried = true
					continue
				}
				if signal, ok := asOpenAICompactFallbackSignal(handleErr); ok {
					_ = resp.Body.Close()
					compactResp, compactBody := openAICompactFallbackErrorResponse(resp, signal)
					if shouldFailoverOpenAIPassthroughResponse(account, compactResp.StatusCode, compactBody) {
						return nil, s.handleFailoverErrorResponsePassthrough(ctx, compactResp, c, account, body, compactBody)
					}
					return nil, s.handleErrorResponsePassthrough(ctx, compactResp, c, account, body, compactBody)
				}
				_ = resp.Body.Close()
				return nil, handleErr
			}
			usage = result.usage
			responseID = strings.TrimSpace(result.responseID)
			imageCount = result.imageCount
			imageOutputSizes = result.imageOutputSizes
		}
		break
	}
	defer func() { _ = resp.Body.Close() }()
	s.bindHTTPResponseAccount(ctx, c, account, responseID)

	if !account.IsShadow() {
		if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
			s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
		}
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}

	forwardResult := &OpenAIForwardResult{
		RequestID:       resp.Header.Get("x-request-id"),
		ResponseID:      responseID,
		Usage:           *usage,
		Model:           reqModel,
		UpstreamModel:   upstreamPassthroughModel,
		ServiceTier:     extractOpenAIServiceTierFromBody(body),
		ReasoningEffort: reasoningEffort,
		Stream:          reqStream,
		OpenAIWSMode:    false,
		Duration:        time.Since(startTime),
		FirstTokenMs:    firstTokenMs,
	}
	if imageCount > 0 {
		imageBillingModel, imageSizeTier = ensureOpenAIImageBillingDefaults(imageCount, imageBillingModel, imageSizeTier)
		forwardResult.ImageCount = imageCount
		forwardResult.ImageSize = imageSizeTier
		forwardResult.ImageInputSize = imageInputSize
		forwardResult.ImageOutputSizes = imageOutputSizes
		forwardResult.BillingModel = imageBillingModel
	}
	return forwardResult, nil
}

func logOpenAIPassthroughInstructionsRejected(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	reqModel string,
	rejectReason string,
	body []byte,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	accountName := ""
	accountType := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
		accountType = strings.TrimSpace(string(account.Type))
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.String("account_type", accountType),
		zap.String("request_model", strings.TrimSpace(reqModel)),
		zap.String("reject_reason", strings.TrimSpace(rejectReason)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	logger.FromContext(ctx).With(fields...).Warn("OpenAI passthrough 本地拦截：Codex 请求缺少有效 instructions")
}

func (s *OpenAIGatewayService) buildUpstreamRequestOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, error) {
	targetURL := openaiPlatformAPIURL
	switch account.Type {
	case AccountTypeOAuth:
		targetURL = chatgptCodexURL
	case AccountTypeSetupToken:
		if account.IsOpenAIOAuthLike() {
			targetURL = chatgptCodexURL
		}
	case AccountTypeAPIKey:
		baseURL := account.GetOpenAIBaseURL()
		if baseURL != "" {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURLForPlatform(account.Platform, validatedURL)
		}
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// 透传客户端请求头（安全白名单）。
	allowTimeoutHeaders := s.isOpenAIPassthroughTimeoutHeadersAllowed()
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !isOpenAIPassthroughAllowedRequestHeader(lower, allowTimeoutHeaders) {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	// 覆盖入站鉴权残留，并注入上游认证
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, fmt.Errorf("build openai authentication headers: %w", err)
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// OAuth 透传到 ChatGPT internal API 时补齐必要头。
	if account.UsesOpenAICodexProtocol() {
		// Current Codex OAuth HTTP no longer negotiates the legacy Responses
		// experiment. Preserve unrelated beta tokens from compatible clients.
		stripOpenAILegacyResponsesBeta(req.Header)
		sessionSignal := extractOpenAIStickySessionSignal(c, body)
		req.Host = "chatgpt.com"
		if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
			return nil, fmt.Errorf("resolve chatgpt account headers: %w", err)
		}
		apiKeyID := getAPIKeyIDFromContext(c)
		// 先保存客户端原始值，再做 compact 补充，避免后续统一隔离时读到已处理的值。
		clientConversationID := strings.TrimSpace(req.Header.Get("conversation_id"))
		// Keep the upstream session boundary aligned with conversation_id first.
		// This matters when the client reuses one stable session_id across multiple
		// independent conversations: if upstream session_id keeps following that
		// stable value, ChatGPT/OpenAI can continue the old conversation and return
		// the previous question's answer in the new thread.
		clientSessionID := clientConversationID
		if clientSessionID == "" {
			clientSessionID = strings.TrimSpace(req.Header.Get("session_id"))
		}
		if isOpenAIResponsesCompactPath(c) {
			req.Header.Set("accept", "application/json")
			if req.Header.Get("version") == "" {
				req.Header.Set("version", codexCLIVersion)
			}
			if clientSessionID == "" {
				clientSessionID = resolveOpenAICompactSessionID(c, body)
			}
		} else if req.Header.Get("accept") == "" {
			req.Header.Set("accept", "text/event-stream")
		}
		if req.Header.Get("originator") == "" {
			req.Header.Set("originator", "codex_cli_rs")
		}
		// 用隔离后的 session 标识符覆盖客户端透传值，防止跨用户会话碰撞。
		if clientSessionID == "" {
			clientSessionID = sessionSignal
		}
		if clientConversationID == "" {
			clientConversationID = sessionSignal
		}
		if clientSessionID != "" {
			req.Header.Set("session_id", isolateOpenAIUpstreamSessionID(apiKeyID, codexAccountIdentitySource(c, account), clientSessionID))
		}
		if clientConversationID != "" {
			req.Header.Set("conversation_id", isolateOpenAIUpstreamSessionID(apiKeyID, codexAccountIdentitySource(c, account), clientConversationID))
		}
	} else if isOpenAIResponsesCompactPath(c) {
		// Compact upstreams are unary JSON even for API-key accounts. Override a
		// client text/event-stream Accept value so compatible relays do not return
		// SSE to the unary response path.
		req.Header.Set("accept", "application/json")
	}

	// 透传模式也支持账户自定义 User-Agent 与 ForceCodexCLI 兜底。
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", codexCLIUserAgent)
	}
	applyCodexAccountIdentityHeaders(req.Header, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))
	applyStagedCodexFingerprintHeaders(c, account, req.Header)
	// OAuth 终态收口：User-Agent / originator / version 从同一规范版本源重建。
	// 客户端识别与会话字段仍按上面的本地逻辑保留；API Key 不补 originator。
	if account.UsesOpenAICodexProtocol() {
		enforceCodexIdentityHeadersWithUA(req.Header, s.codexIdentityOverrideUA(account))
	}

	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效；OAuth 路径 no-op）
	account.ApplyHeaderOverrides(req.Header)
	setOpenAICodexRoutingHintFromBody(req.Header, account, body)
	logOpenAIRoutingDiagnosticsFromBody(ctx, account, "http_passthrough", req.Header, body, "not_applicable")

	return req, nil
}

func stripOpenAILegacyResponsesBeta(headers http.Header) {
	if headers == nil {
		return
	}

	preserved := make([]string, 0)
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), "OpenAI-Beta") {
			continue
		}
		delete(headers, key)
		for _, value := range values {
			parts := strings.Split(value, ",")
			kept := parts[:0]
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" || strings.EqualFold(part, "responses=experimental") {
					continue
				}
				kept = append(kept, part)
			}
			if len(kept) > 0 {
				preserved = append(preserved, strings.Join(kept, ", "))
			}
		}
	}
	for _, value := range preserved {
		headers.Add("OpenAI-Beta", value)
	}
}

func shouldFailoverOpenAIPassthroughResponse(account *Account, statusCode int, responseBody []byte) bool {
	if hit, _, _ := detectOpenAICyberPolicy(responseBody); hit {
		return false
	}
	if isOpenAIContextWindowError("", responseBody) {
		return false
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, "", responseBody) {
		return true
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, "", responseBody) {
		return true
	}
	if account != nil && account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode) {
		return true
	}
	switch statusCode {
	case http.StatusTooManyRequests, 529:
		return true
	}
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	switch statusCode {
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		520, 521, 522, 523, 524:
		return true
	default:
		return false
	}
}

// writeOpenAIPassthroughErrorHeaders deliberately exposes only the response
// metadata a client can safely act on. Successful passthrough responses retain
// their normal filtered-header behavior; upstream error responses must not leak
// provider identity, cookies, auth challenges, debug headers, or request IDs.
func writeOpenAIPassthroughErrorHeaders(dst, src http.Header) {
	if dst == nil {
		return
	}
	dst.Set("Content-Type", "application/json; charset=utf-8")
	dst.Set("Cache-Control", "no-store")
	dst.Del("Retry-After")
	if src == nil {
		return
	}
	rawRetryAfter := strings.TrimSpace(src.Get("Retry-After"))
	if validOpenAIPassthroughRetryAfter(rawRetryAfter, time.Now()) {
		dst.Set("Retry-After", rawRetryAfter)
	}
}

func validOpenAIPassthroughRetryAfter(raw string, now time.Time) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	delaySeconds := true
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			delaySeconds = false
			break
		}
	}
	if delaySeconds {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		return err == nil && seconds > 0
	}
	parsed, err := http.ParseTime(raw)
	return err == nil && parsed.After(now)
}

func writeSanitizedOpenAIPassthroughError(c *gin.Context, upstreamStatus int, upstreamHeaders http.Header) {
	downstreamStatus := upstreamStatus
	message := "Upstream request failed"
	switch upstreamStatus {
	case http.StatusUnauthorized:
		downstreamStatus = http.StatusBadGateway
		message = "Upstream authentication failed"
	case http.StatusForbidden:
		downstreamStatus = http.StatusBadGateway
		message = "Upstream access denied"
	default:
		if upstreamStatus >= http.StatusInternalServerError {
			message = "Upstream service temporarily unavailable"
		}
	}
	writeOpenAIPassthroughErrorEnvelope(c, downstreamStatus, upstreamHeaders, message)
}

// writeOpenAIPassthroughErrorEnvelope rebuilds an error locally instead of
// forwarding an arbitrary provider-controlled payload to the client.
func writeOpenAIPassthroughErrorEnvelope(c *gin.Context, downstreamStatus int, upstreamHeaders http.Header, message string) {
	if c == nil {
		return
	}
	body, _ := json.Marshal(gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	if writeOpenAICompactSSEBridge(c, downstreamStatus, body) {
		return
	}
	writeOpenAIPassthroughErrorHeaders(c.Writer.Header(), upstreamHeaders)
	c.Data(downstreamStatus, "application/json; charset=utf-8", body)
}

func (s *OpenAIGatewayService) handleFailoverErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	responseBody ...[]byte,
) error {
	body := []byte(nil)
	if len(responseBody) > 0 {
		body = responseBody[0]
	} else {
		body = s.readUpstreamErrorBody(resp)
	}
	body = s.redactAgentIdentitySensitiveBody(ctx, account, body)

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, upstreamDetail)
		writeOpenAIPassthroughErrorHeaders(c.Writer.Header(), resp.Header)
		MarkResponseCommitted(c)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": clientMsg,
			},
		})
		return fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
	canonicalModel := canonicalOpenAIAccountSchedulingModel(account, reqModel)
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, canonicalModel)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "failover",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})
	return s.newOpenAIAccountFailoverError(
		account,
		resp.StatusCode,
		resp.Header,
		body,
		upstreamMsg,
		shouldDisable,
		!shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
	)
}

func (s *OpenAIGatewayService) handleErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	responseBody ...[]byte,
) error {
	MarkResponseCommitted(c)
	body := []byte(nil)
	if len(responseBody) > 0 {
		body = responseBody[0]
	} else {
		body = s.readUpstreamErrorBody(resp)
	}
	body = s.redactAgentIdentitySensitiveBody(ctx, account, body)

	// cyber_policy：透传账号本就把原始 body 回给客户端（下方 c.Data），此处仅打标记，
	// 供 handler 事后写风控/邮件。cyber 是上游网络安全策略拦截，不冷却账号，
	// 故下方跳过 handleOpenAIAccountUpstreamError（避免自定义 temp-unschedulable 规则误冷却）。
	cyberHit, cyberCode, cyberMsg := detectOpenAICyberPolicy(body)
	if cyberHit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           cyberCode,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, upstreamDetail)
		writeOpenAIPassthroughErrorHeaders(c.Writer.Header(), resp.Header)
		MarkResponseCommitted(c)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": clientMsg,
			},
		})
		return fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)

	// codex round 2026-05-16 enhancement to PR#2498: if upstream complained
	// "instructions are required" (or similar 400), the local codex-only
	// gate (detectOpenAIPassthroughInstructionsRejectReason) missed it.
	// Return a rollback error WITHOUT writing the 400 to the client; the
	// outer Forward catches it via errors.As and retries via the non-
	// passthrough (full transform) path. Covers future cases where OpenAI
	// extends the instructions requirement to non-codex models.
	if isOpenAIInstructionsRequiredError(resp.StatusCode, upstreamMsg, body) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:             account.Platform,
			AccountID:            account.ID,
			AccountName:          account.Name,
			UpstreamStatusCode:   resp.StatusCode,
			UpstreamRequestID:    resp.Header.Get("x-request-id"),
			Passthrough:          true,
			Kind:                 "rollback_to_transform",
			Message:              upstreamMsg,
			Detail:               upstreamDetail,
			UpstreamResponseBody: upstreamDetail,
		})
		return &openAIPassthroughRollbackError{Reason: "upstream_instructions_required"}
	}

	// 错误体不会原样透传，但运行态账号状态仍需更新，避免粘性路由
	// 继续复用刚被限流的账号。cyber 例外：不冷却账号。
	if !cyberHit {
		reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
		canonicalModel := canonicalOpenAIAccountSchedulingModel(account, reqModel)
		_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, canonicalModel)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "http_error",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})

	// Context-window messages are actionable (clients may compact and retry),
	// so preserve the already-sanitized message inside the local envelope. A
	// cyber-policy message is likewise useful after the relay redaction pass.
	if isOpenAIContextWindowError(upstreamMsg, body) && upstreamMsg != "" {
		writeOpenAIPassthroughErrorEnvelope(c, resp.StatusCode, resp.Header, upstreamMsg)
	} else if cyberHit {
		writeOpenAIPassthroughErrorEnvelope(c, resp.StatusCode, resp.Header, cyberPolicyClientMessage(cyberMsg, body))
	} else {
		writeSanitizedOpenAIPassthroughError(c, resp.StatusCode, resp.Header)
	}

	return fmt.Errorf("upstream error: %d (client response sanitized)", resp.StatusCode)
}

func isOpenAIPassthroughAllowedRequestHeader(lowerKey string, allowTimeoutHeaders bool) bool {
	if lowerKey == "" {
		return false
	}
	if isOpenAIPassthroughTimeoutHeader(lowerKey) {
		return allowTimeoutHeaders
	}
	return openaiPassthroughAllowedHeaders[lowerKey]
}

func isOpenAIPassthroughTimeoutHeader(lowerKey string) bool {
	switch lowerKey {
	case "x-stainless-timeout", "x-stainless-read-timeout", "x-stainless-connect-timeout", "x-request-timeout", "request-timeout", "grpc-timeout":
		return true
	default:
		return false
	}
}

func (s *OpenAIGatewayService) isOpenAIPassthroughTimeoutHeadersAllowed() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIPassthroughAllowTimeoutHeaders
}

// compactNonstreamKeepaliveInterval 返回 compact 非流式空行 keepalive 间隔；0 表示禁用。
func (s *OpenAIGatewayService) compactNonstreamKeepaliveInterval() time.Duration {
	if s == nil || s.cfg == nil {
		return 0
	}
	seconds := s.cfg.Gateway.OpenAICompactNonstreamKeepaliveInterval
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// startCompactNonstreamKeepalive 为 compact 非流式请求启动下游空行心跳，防止反代空闲断连。
func (s *OpenAIGatewayService) startCompactNonstreamKeepalive(ctx context.Context, c *gin.Context) func() {
	if s == nil || c == nil || c.Writer == nil || !isOpenAIResponsesCompactPath(c) || openAICompactClientWantsStream(c) {
		return func() {}
	}
	interval := s.compactNonstreamKeepaliveInterval()
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	log := logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.String("request_path", path),
		zap.Int("interval_seconds", int(interval.Seconds())),
	)
	log.Info("OpenAI compact non-stream keepalive started")

	headers := c.Writer.Header()
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("X-Accel-Buffering", "no")
	headers.Del("Content-Length")

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		flushedLogged := false
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := c.Writer.Write([]byte("\n")); err != nil {
					log.Warn("OpenAI compact non-stream keepalive write failed", zap.Error(err))
					return
				}
				flusher.Flush()
				if !flushedLogged {
					log.Info("OpenAI compact non-stream keepalive flushed")
					flushedLogged = true
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopCh)
		})
		wg.Wait()
	}
}

// logOpenAICompactKeepaliveCommitted 记录 keepalive 已提交响应后上游返回错误的诊断日志。
func logOpenAICompactKeepaliveCommitted(ctx context.Context, c *gin.Context, account *Account, resp *http.Response) {
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}
	statusCode := 0
	upstreamRequestID := ""
	if resp != nil {
		statusCode = resp.StatusCode
		upstreamRequestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	requestPath := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		requestPath = strings.TrimSpace(c.Request.URL.Path)
	}
	logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.Bool("compact_keepalive_committed", true),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status", statusCode),
		zap.String("upstream_request_id", upstreamRequestID),
		zap.String("request_path", requestPath),
	).Warn("OpenAI compact non-stream keepalive committed response before upstream error; proxying error without failover")
}

func compactStopFunc(stops ...func()) func() {
	if len(stops) == 0 || stops[0] == nil {
		return func() {}
	}
	return stops[0]
}

func collectOpenAIPassthroughTimeoutHeaders(h http.Header) []string {
	if h == nil {
		return nil
	}
	var matched []string
	for key, values := range h {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if isOpenAIPassthroughTimeoutHeader(lowerKey) {
			entry := lowerKey
			if len(values) > 0 {
				entry = fmt.Sprintf("%s=%s", lowerKey, strings.Join(values, "|"))
			}
			matched = append(matched, entry)
		}
	}
	sort.Strings(matched)
	return matched
}

type openaiStreamingResultPassthrough struct {
	usage            *OpenAIUsage
	firstTokenMs     *int
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

type openaiNonStreamingResultPassthrough struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

const openAIStreamSemanticOutputStartedKey = "openai_stream_semantic_output_started"
const openAIStreamMaxPendingPreOutputBytes = 1 << 20

func appendOpenAIStreamPendingLine(lines *[]string, totalBytes *int, line string) bool {
	lineBytes := len(line) + 1
	if lineBytes > openAIStreamMaxPendingPreOutputBytes-*totalBytes {
		return false
	}
	*lines = append(*lines, line)
	*totalBytes += lineBytes
	return true
}

// MarkOpenAIStreamSemanticOutputStarted records that a Responses stream has
// emitted a client-visible business frame. HTTP headers, SSE comments and
// keepalives deliberately do not set this marker: they can commit the writer
// without making an attempt unsafe to retry.
func MarkOpenAIStreamSemanticOutputStarted(c *gin.Context) {
	if c != nil {
		c.Set(openAIStreamSemanticOutputStartedKey, true)
	}
}

// OpenAIStreamSemanticOutputStarted is the handler-level retry guard for
// streamed Responses attempts. Byte counts cannot distinguish semantic SSE
// frames from transport-only comments.
func OpenAIStreamSemanticOutputStarted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	started, ok := c.Get(openAIStreamSemanticOutputStartedKey)
	return ok && started == true
}

const openAIStreamKeepaliveBytesKey = "openai_stream_keepalive_bytes"

func recordOpenAIStreamKeepaliveBytes(c *gin.Context, written int) {
	if c == nil || written <= 0 {
		return
	}
	current := 0
	if value, ok := c.Get(openAIStreamKeepaliveBytesKey); ok {
		current, _ = value.(int)
	}
	c.Set(openAIStreamKeepaliveBytesKey, current+written)
}

func openAIStreamClientOutputStarted(c *gin.Context, localStarted bool) bool {
	if localStarted {
		return true
	}
	if c == nil || c.Writer == nil {
		return false
	}
	// Compact keepalive comments commit HTTP 200 but are not semantic output.
	return OpenAICompactKeepaliveAdjustedWrittenSize(c) >= 0
}

func openAIStreamEventIsPreamble(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func openAIStreamAddedEventStartsClientOutput(payload []byte, eventType string) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return true
	}

	switch strings.TrimSpace(eventType) {
	case "response.output_item.added":
		item := gjson.GetBytes(payload, "item")
		if !item.Exists() || !item.IsObject() {
			return true
		}
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning":
			if item.Get("encrypted_content").String() != "" {
				return true
			}
			summary := item.Get("summary")
			if !summary.IsArray() {
				return false
			}
			for _, part := range summary.Array() {
				if strings.TrimSpace(part.Get("type").String()) != "summary_text" || part.Get("text").String() != "" {
					return true
				}
			}
			return false
		case "message":
			content := item.Get("content")
			if !content.IsArray() {
				return false
			}
			for _, part := range content.Array() {
				switch strings.TrimSpace(part.Get("type").String()) {
				case "output_text":
					if part.Get("text").String() != "" {
						return true
					}
				case "refusal":
					if part.Get("refusal").String() != "" {
						return true
					}
				default:
					return true
				}
			}
			return false
		case "function_call":
			return item.Get("arguments").String() != ""
		case "custom_tool_call":
			return item.Get("input").String() != ""
		case "compaction":
			return item.Get("encrypted_content").String() != ""
		default:
			return true
		}
	case "response.content_part.added":
		part := gjson.GetBytes(payload, "part")
		if !part.Exists() || !part.IsObject() {
			return true
		}
		switch strings.TrimSpace(part.Get("type").String()) {
		case "output_text":
			return part.Get("text").String() != ""
		case "refusal":
			return part.Get("refusal").String() != ""
		default:
			return true
		}
	case "response.reasoning_summary_part.added":
		part := gjson.GetBytes(payload, "part")
		if !part.Exists() || !part.IsObject() || strings.TrimSpace(part.Get("type").String()) != "summary_text" {
			return true
		}
		return part.Get("text").String() != ""
	default:
		return true
	}
}

func openAIStreamDataStartsClientOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	switch strings.TrimSpace(eventType) {
	case "response.failed":
		return false
	case "error":
		// Capacity/transient errors commonly arrive as error followed by
		// response.failed. Keep retryable error frames attempt-local so the
		// terminal event can still trigger a clean pre-output failover.
		payload := []byte(trimmed)
		return !openAIStreamFailedEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload))
	case "response.output_item.added", "response.content_part.added", "response.reasoning_summary_part.added":
		return openAIStreamAddedEventStartsClientOutput([]byte(trimmed), eventType)
	}
	return !openAIStreamEventIsPreamble(eventType)
}

func openAIStreamItemHasVisibleOutput(item gjson.Result) bool {
	if item.Get("arguments").String() != "" || item.Get("input").String() != "" || item.Get("result").String() != "" {
		return true
	}
	for _, path := range []string{"content", "summary"} {
		for _, part := range item.Get(path).Array() {
			if part.Get("text").String() != "" || part.Get("transcript").String() != "" {
				return true
			}
		}
	}
	return false
}

// Structural progress can commit an attempt and disarm first-output failover,
// but TTFT should start only when the stream carries content a client can use.
func openAIStreamDataStartsVisibleOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" || !gjson.Valid(trimmed) {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = strings.TrimSpace(gjson.Get(trimmed, "type").String())
	}
	if strings.HasSuffix(eventType, ".delta") {
		delta := gjson.Get(trimmed, "delta")
		return delta.Exists() && delta.String() != ""
	}
	switch eventType {
	case "response.output_text.done",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.done",
		"response.audio_transcript.done":
		return gjson.Get(trimmed, "text").String() != ""
	case "response.function_call_arguments.done":
		return gjson.Get(trimmed, "arguments").String() != ""
	case "response.custom_tool_call_input.done":
		return gjson.Get(trimmed, "input").String() != ""
	case "response.image_generation_call.partial_image":
		return gjson.Get(trimmed, "partial_image_b64").String() != ""
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		part := gjson.Get(trimmed, "part")
		return part.Get("text").String() != "" || part.Get("transcript").String() != ""
	case "response.output_item.added", "response.output_item.done":
		return openAIStreamItemHasVisibleOutput(gjson.Get(trimmed, "item"))
	case "response.completed", "response.done":
		for _, item := range gjson.Get(trimmed, "response.output").Array() {
			if openAIStreamItemHasVisibleOutput(item) {
				return true
			}
		}
	}
	return false
}

// openAIStreamDataStartsSemanticTTFT preserves the historical first-token
// definition: after Responses preamble, the first semantic SSE event starts
// TTFT even if it does not yet contain user-visible text.
func openAIStreamDataStartsSemanticTTFT(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" && gjson.Valid(trimmed) {
		eventType = strings.TrimSpace(gjson.Get(trimmed, "type").String())
	}
	switch eventType {
	case "response.failed":
		return false
	case "error":
		payload := []byte(trimmed)
		return !openAIStreamFailedEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload))
	default:
		return !openAIStreamEventIsPreamble(eventType)
	}
}

func (s *OpenAIGatewayService) openAITTFTMode(ctx context.Context) string {
	mode := OpenAITTFTModeSemantic
	if s != nil && s.settingService != nil {
		mode = s.settingService.GetOpenAITTFTMode(ctx)
	} else if cached, ok := gatewayForwardingCache.Load().(*cachedGatewayForwardingSettings); ok && cached != nil {
		if cached.expiresAt == 0 || time.Now().UnixNano() < cached.expiresAt {
			mode = normalizeOpenAITTFTMode(cached.openAITTFTMode)
		}
	}
	return normalizeOpenAITTFTMode(mode)
}

func openAIStreamDataStartsTTFT(data, eventType string, forceOutput bool, mode string) bool {
	if mode == OpenAITTFTModeVisible {
		return openAIStreamDataStartsVisibleOutput(data, eventType)
	}
	return forceOutput || openAIStreamDataStartsSemanticTTFT(data, eventType)
}

// openAIStreamFailedEventErrorCode 提取流内 failed 事件的错误码（小写），
// 兼容 response.failed 的嵌套形态与裸 error 形态。
func openAIStreamFailedEventErrorCode(payload []byte) string {
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	return code
}

// isOpenAIUpstreamCapacityShedEvent 判断流内 failed 事件是否为请求级容量降载信号。
func isOpenAIUpstreamCapacityShedEvent(payload []byte) bool {
	switch openAIStreamFailedEventErrorCode(payload) {
	case "server_is_overloaded", "slow_down":
		return true
	default:
		return false
	}
}

const openAICapacityShedRetryableClientCode = "server_error"

// sanitizeOpenAICapacityShedErrorCodeForClient rewrites only the client copy
// of fatal Codex capacity codes. Callers must perform monitoring, account
// state and usage observation against the raw payload before invoking it.
func sanitizeOpenAICapacityShedErrorCodeForClient(payload []byte) ([]byte, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !isOpenAIUpstreamCapacityShedEvent(payload) {
		return payload, false
	}
	updated := payload
	changed := false
	for _, path := range []string{"response.error.code", "error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(updated, path).String())) {
		case "server_is_overloaded", "slow_down":
		default:
			continue
		}
		next, err := sjson.SetBytes(updated, path, openAICapacityShedRetryableClientCode)
		if err != nil {
			return payload, false
		}
		updated = next
		changed = true
	}
	return updated, changed
}

func openAIStreamFailedEventSemanticStatus(payload []byte, message string) int {
	if isOpenAIContextWindowError(message, payload) {
		return http.StatusBadRequest
	}
	for _, path := range []string{"response.error.status_code", "error.status_code", "status_code"} {
		if statusCode := int(gjson.GetBytes(payload, path).Int()); statusCode >= 400 && statusCode <= 599 {
			return statusCode
		}
	}

	code := openAIStreamFailedEventErrorCode(payload)
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	combined := strings.TrimSpace(errType + " " + code + " " + strings.ToLower(strings.TrimSpace(message)))
	switch {
	case strings.Contains(combined, "rate_limit"):
		return http.StatusTooManyRequests
	case strings.Contains(errType, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(combined, "authentication") || strings.Contains(combined, "unauthorized") || strings.Contains(combined, "invalid_api_key"):
		return http.StatusUnauthorized
	case strings.Contains(combined, "permission") || strings.Contains(combined, "forbidden") || strings.Contains(combined, "access denied"):
		return http.StatusForbidden
	case isOpenAIUpstreamAccessStateError(message, payload):
		return http.StatusForbidden
	case isOpenAIUpstreamCapacityShedEvent(payload):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func openAIStreamFailureStatus(payload []byte, message string) int {
	if len(bytes.TrimSpace(payload)) == 0 || !gjson.ValidBytes(payload) {
		return http.StatusBadGateway
	}
	semanticStatus := openAIStreamFailedEventSemanticStatus(payload, message)
	switch semanticStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 529:
		return semanticStatus
	case http.StatusServiceUnavailable:
		if isOpenAIUpstreamCapacityShedEvent(payload) {
			return semanticStatus
		}
	}
	return http.StatusBadGateway
}

func openAIStreamFailedEventShouldFailover(payload []byte, message string) bool {
	if hit, _, _ := detectOpenAICyberPolicy(payload); hit {
		return false
	}
	if isOpenAIContextWindowError(message, payload) {
		return false
	}
	if isOpenAIUpstreamAccessStateError(message, payload) {
		return true
	}
	semanticStatus := openAIStreamFailureStatus(payload, message)
	if semanticStatus == http.StatusForbidden {
		return openAIStream403AccountFailure(payload, message)
	}
	if semanticStatus == http.StatusTooManyRequests {
		return true
	}
	if isOpenAITransientProcessingError(http.StatusBadRequest, message, payload) {
		return true
	}
	if containsOpenAICompatSensitiveBackendTerm(message, payload) {
		return true
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " + code + " " + errType))
	if combined == "" {
		return true
	}
	nonRetryableMarkers := []string{
		"invalid_request",
		"content_policy",
		"policy",
		"safety",
		"high-risk cyber",
		"not allowed",
		"violat",
	}
	for _, marker := range nonRetryableMarkers {
		if strings.Contains(combined, marker) {
			return false
		}
	}
	return true
}

func openAIStreamFailedEventRetryableOnSameAccount(account *Account, payload []byte, message string) bool {
	if account == nil {
		return false
	}
	// 容量降载是请求级信号，与账号健康无关；先在同一账号有界重试，
	// 并由 RequestScopedTransient 阻止后续临时摘号。
	if isOpenAIUpstreamCapacityShedEvent(payload) {
		return true
	}
	if !account.IsPoolMode() {
		return false
	}
	semanticStatus := openAIStreamFailedEventSemanticStatus(payload, message)
	return account.IsPoolModeRetryableStatus(semanticStatus) ||
		isOpenAITransientProcessingError(http.StatusBadRequest, message, payload)
}

func (s *OpenAIGatewayService) recordOpenAIStreamUpstreamError(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	kind string,
	payload []byte,
	message string,
) string {
	contextWindowError := isOpenAIContextWindowError(message, payload)
	maskedSensitiveBackendError := !contextWindowError && containsOpenAICompatSensitiveBackendTerm(message, payload)
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "OpenAI upstream response failed"
	}
	if maskedSensitiveBackendError {
		message = openAICompatSensitiveBackendErrorMessage
	}
	statusCode := openAIStreamFailureStatus(payload, message)
	detail := ""
	if len(payload) > 0 && !maskedSensitiveBackendError && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(string(payload), maxBytes)
	}
	if c != nil {
		setOpsUpstreamError(c, statusCode, message, detail)
		event := OpsUpstreamErrorEvent{
			Platform:           PlatformOpenAI,
			UpstreamStatusCode: statusCode,
			UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
			Passthrough:        passthrough,
			Kind:               kind,
			Message:            message,
			Detail:             detail,
		}
		if maskedSensitiveBackendError {
			event.Kind = "masked_backend_error"
		}
		if account != nil {
			event.Platform = account.Platform
			event.AccountID = account.ID
			event.AccountName = account.Name
		}
		appendOpsUpstreamError(c, event)
	}
	return message
}

func (s *OpenAIGatewayService) newOpenAIStreamFailoverError(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	payload []byte,
	message string,
	responseHeaders ...http.Header,
) *UpstreamFailoverError {
	return s.newOpenAIStreamFailoverErrorWithModel(c, account, passthrough, upstreamRequestID, payload, message, "", responseHeaders...)
}

func (s *OpenAIGatewayService) newOpenAIStreamFailoverErrorWithModel(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	payload []byte,
	message string,
	canonicalModel string,
	responseHeaders ...http.Header,
) *UpstreamFailoverError {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "OpenAI stream disconnected before completion"
	}
	var headers http.Header
	if len(responseHeaders) > 0 && responseHeaders[0] != nil {
		headers = responseHeaders[0].Clone()
	}
	statusCode, shouldDisable := s.handleOpenAIStreamTerminalAccountSideEffects(c, account, payload, message, headers, canonicalModel)
	// A response.failed event arrives inside HTTP 200. Use its semantic status
	// for account health and failover metadata while retaining the protected
	// stream-429 rule (the side-effect helper deliberately ignores quota
	// snapshot headers for an in-stream 429).
	message = s.recordOpenAIStreamUpstreamError(c, account, passthrough, upstreamRequestID, "failover", payload, message)
	errType := "upstream_error"
	if statusCode == http.StatusTooManyRequests {
		errType = "rate_limit_error"
	}
	body, _ := json.Marshal(gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
	retryableOnSameAccount := openAIStreamFailedEventRetryableOnSameAccount(account, payload, message)
	classificationHeaders := headers
	if statusCode == http.StatusTooManyRequests {
		classificationHeaders = nil
	}
	failoverErr := s.newOpenAIAccountFailoverErrorWithClassificationHeaders(
		account, statusCode, headers, classificationHeaders, payload, message, shouldDisable, retryableOnSameAccount,
	)
	if failoverErr.IsCredentialFailure() {
		return failoverErr
	}
	if failoverErr.IsOpenAICapacityShed() {
		// Keep the raw provider payload in ops diagnostics only. The typed reason
		// and client fields retain failover classification without exposing
		// provider-only codes through the outer WS/HTTP failover boundary.
		failoverErr.ResponseBody = body
		return failoverErr
	}
	if failoverErr.RequestScopedTransient {
		return failoverErr
	}
	// Preserve the established sanitized local envelope for unclassified
	// stream failures. Typed credential/capacity failures retain the structured
	// payload internally so the failover engine can inspect their exact code.
	failoverErr.ResponseBody = body
	return failoverErr
}

func (s *OpenAIGatewayService) handleStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	originalModel string,
	mappedModel string,
) (*openaiStreamingResultPassthrough, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	ttftMode := s.openAITTFTMode(ctx)
	clientDisconnected := false
	sawDone := false
	sawTerminalEvent := false
	sawFailedEvent := false
	sawBareError := false
	sawResponseFailed := false
	terminalEventType := ""
	semanticOutputSeen := false
	capacityFailoverSuppressedLogged := false
	failedMessage := ""
	clientOutputStarted := false
	codexFailureTerminal := account != nil && account.Platform == PlatformOpenAI
	failureDelivered := false
	suppressCurrentEvent := false
	responseFailedPending := false
	var bareErrorPayload []byte
	bareErrorAccountSideEffectsPending := false
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	// pendingLines 在首个可见输出前保留前导事件，确保无输出失败仍可安全 failover。
	pendingLines := make([]string, 0, 8)
	pendingLineBytes := 0
	// flushPending 表示已写入但未到 SSE 空行边界的脏状态；defer 兜底函数退出前的残留，断连后不再 Flush。
	flushPending := false
	pendingSSEEventType := ""
	flushPendingOutput := func() {
		if clientDisconnected || !flushPending {
			return
		}
		flusher.Flush()
		flushPending = false
	}
	defer flushPendingOutput()
	writePendingLines := func() bool {
		for _, pending := range pendingLines {
			n, err := fmt.Fprintln(w, pending)
			if err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
				return false
			}
			if isSSECommentLine(pending) {
				recordOpenAIStreamKeepaliveBytes(c, n)
			}
		}
		pendingLines = pendingLines[:0]
		pendingLineBytes = 0
		return true
	}
	writePendingKeepalives := func() {
		if clientDisconnected || len(pendingLines) == 0 {
			return
		}
		wroteKeepalive := false
		for i := 0; i < len(pendingLines); i++ {
			if !isSSECommentLine(pendingLines[i]) {
				continue
			}
			n, err := fmt.Fprintln(w, pendingLines[i])
			if err != nil {
				clientDisconnected = true
				return
			}
			recordOpenAIStreamKeepaliveBytes(c, n)
			wroteKeepalive = true
			if i+1 < len(pendingLines) && pendingLines[i+1] == "" {
				i++
				n, err = fmt.Fprintln(w)
				if err != nil {
					clientDisconnected = true
					return
				}
				recordOpenAIStreamKeepaliveBytes(c, n)
			}
		}
		if wroteKeepalive {
			flusher.Flush()
		}
	}
	ensureResponseFailedTerminal := func() {
		if !sawBareError || sawResponseFailed || failureDelivered {
			return
		}
		if bareErrorAccountSideEffectsPending {
			s.handleOpenAIStreamTerminalAccountSideEffects(c, account, bareErrorPayload, failedMessage, resp.Header)
			bareErrorAccountSideEffectsPending = false
		}
		if clientDisconnected || !writePendingLines() {
			return
		}
		if _, err := fmt.Fprint(w, buildOpenAIResponseFailedSSE(responseID, originalModel, bareErrorPayload, failedMessage)); err != nil {
			clientDisconnected = true
			return
		}
		clientOutputStarted = true
		failureDelivered = true
		flushPending = true
		flushPendingOutput()
	}
	bareCapacityFailover := func() *UpstreamFailoverError {
		if !codexFailureTerminal || !sawBareError || sawResponseFailed ||
			openAIStreamClientOutputStarted(c, clientOutputStarted) ||
			!isOpenAIRequestScopedCapacityShed(failedMessage, bareErrorPayload) {
			return nil
		}
		bareErrorAccountSideEffectsPending = false
		writePendingKeepalives()
		return s.newOpenAIStreamFailoverError(
			c, account, true, upstreamRequestID, bareErrorPayload, failedMessage, resp.Header,
		)
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	defer putSSEScannerBuf64K(scanBuf)
	documentScanner := newOpenAISSEJSONDocumentScanner(scanner)

	needModelReplace := strings.TrimSpace(originalModel) != "" && strings.TrimSpace(mappedModel) != "" && strings.TrimSpace(originalModel) != strings.TrimSpace(mappedModel)
	resultWithUsage := func() *openaiStreamingResultPassthrough {
		return &openaiStreamingResultPassthrough{
			usage:            usage,
			firstTokenMs:     firstTokenMs,
			responseID:       responseID,
			imageCount:       imageCounter.Count(),
			imageOutputSizes: imageCounter.Sizes(),
		}
	}

	for documentScanner.Scan() {
		line := documentScanner.Text()
		if eventType, ok := extractOpenAISSEEventLine(line); ok {
			pendingSSEEventType = eventType
			eventType = strings.TrimSpace(eventType)
			suppressCurrentEvent = codexFailureTerminal && (eventType == "error" || (sawBareError && !sawResponseFailed && eventType != "response.failed"))
		}
		lineStartsClientOutput := false
		forceFlushFailedEvent := false
		if data, ok := extractOpenAISSEDataLine(line); ok {
			dataBytes := []byte(data)
			trimmedData := strings.TrimSpace(data)
			rawEventType := effectiveOpenAISSEEventType(dataBytes, pendingSSEEventType)
			observer.ObserveOpenAI(dataBytes, rawEventType)
			if needModelReplace && strings.Contains(data, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
				if replacedData, replaced := extractOpenAISSEDataLine(line); replaced {
					dataBytes = []byte(replacedData)
					trimmedData = strings.TrimSpace(replacedData)
				}
			}
			if normalizedData, normalized := normalizeOpenAIResponsesFunctionCallArguments(dataBytes); normalized {
				dataBytes = normalizedData
				trimmedData = strings.TrimSpace(string(normalizedData))
				line = "data: " + string(normalizedData)
			}
			if normalizedData, normalized := normalizeCompletedImageGenerationStatus(dataBytes); normalized {
				dataBytes = normalizedData
				trimmedData = strings.TrimSpace(string(normalizedData))
				line = "data: " + string(normalizedData)
			}
			if trimmedData != "[DONE]" {
				restoredData, restoreErr := restoreOpenAIResponsesNamespacePayload(c, dataBytes)
				if restoreErr != nil {
					return resultWithUsage(), fmt.Errorf("restore OpenAI passthrough namespace response: %w", restoreErr)
				}
				restoredData = restoreCodexToolNamesFromSSEContext(c, restoredData, rawEventType)
				if !bytes.Equal(restoredData, dataBytes) {
					dataBytes = restoredData
					trimmedData = strings.TrimSpace(string(restoredData))
					line = "data: " + string(restoredData)
				}
			}
			eventType := effectiveOpenAISSEEventType(dataBytes, rawEventType)
			if codexFailureTerminal && sawBareError && !sawResponseFailed && eventType != "response.failed" {
				suppressCurrentEvent = true
			}
			if !capacityFailoverSuppressedLogged && account != nil && account.Platform == PlatformOpenAI &&
				(eventType == "error" || eventType == "response.failed") &&
				openAIStreamClientOutputStarted(c, clientOutputStarted) &&
				isOpenAIUpstreamCapacityShedEvent(dataBytes) {
				logOpenAICapacityFailoverSuppressed(ctx, account, "passthrough_sse", upstreamRequestID, eventType)
				capacityFailoverSuppressedLogged = true
			}
			cyberHit := false
			if eventType == "response.failed" || eventType == "error" {
				if codexFailureTerminal && eventType == "error" {
					sawBareError = true
					bareErrorPayload = append(bareErrorPayload[:0], dataBytes...)
					suppressCurrentEvent = true
				} else if codexFailureTerminal && eventType == "response.failed" {
					sawResponseFailed = true
				}
				responseFailedPending = !codexFailureTerminal || eventType == "response.failed"
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				if failedMessage == "" {
					failedMessage = "Upstream response failed"
				}
				// response.failed 自带上游已消耗的 usage（input token 通常已扣）；必须先解析
				// 再打 cyber 标记，否则 mark 记到的是解析前的 0，导致流式 cyber 按 0 token 计费
				// 而漏记真实用量。对齐 WS V2 / Chat 流式路径（均先解析 usage 再 Mark）。
				s.parseSSEUsageBytesWithType(dataBytes, eventType, usage)
				if hit, code, msg := detectOpenAICyberPolicy(dataBytes); hit {
					cyberHit = true
					MarkOpsCyberPolicy(c, CyberPolicyMark{
						Code:           code,
						Message:        msg,
						Body:           truncateString(string(dataBytes), 4096),
						UpstreamStatus: http.StatusOK,
						UpstreamInTok:  usage.InputTokens,
						UpstreamOutTok: usage.OutputTokens,
					})
				}
				outputStarted := openAIStreamClientOutputStarted(c, clientOutputStarted)
				if !outputStarted && !cyberHit {
					if compactErr := newOpenAICompactFallbackSignal(c, dataBytes, failedMessage); compactErr != nil {
						return resultWithUsage(), compactErr
					}
				}
				if outputStarted && !cyberHit {
					if codexFailureTerminal && eventType == "error" {
						// Wait for the authoritative response.failed before mutating
						// account health; EOF synthesis applies the pending effect.
						bareErrorAccountSideEffectsPending = true
					} else {
						s.handleOpenAIStreamTerminalAccountSideEffects(c, account, dataBytes, failedMessage, resp.Header)
						bareErrorAccountSideEffectsPending = false
					}
				}
				if !outputStarted {
					shouldFailover := false
					if !cyberHit {
						if eventType == "error" {
							shouldFailover = openAIStreamErrorEventShouldFailover(dataBytes, failedMessage)
						} else {
							shouldFailover = openAIStreamFailedEventShouldFailover(dataBytes, failedMessage)
						}
					}
					if shouldFailover {
						// OpenAI OAuth streams commonly emit a bare error immediately
						// before the authoritative response.failed event. Keep the bare
						// frame attempt-local and drain the pair so response.failed usage
						// is retained before failing over.
						if !codexFailureTerminal || eventType != "error" {
							if isOpenAIRequestScopedCapacityShed(failedMessage, dataBytes) {
								writePendingKeepalives()
							}
							return resultWithUsage(),
								s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, dataBytes, failedMessage, resp.Header)
						}
					}
					if !cyberHit && !sawBareError {
						if status, errType, errMsg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, dataBytes, failedMessage); matched {
							// 命中透传规则也要记录 ops 上游错误事件（对齐 CC/Messages 与
							// antigravity 先例），否则透传命中的 failed 在监控中不可见。
							s.recordOpenAIStreamUpstreamError(c, account, true, upstreamRequestID, "http_error", dataBytes, failedMessage)
							MarkResponseCommitted(c)
							c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
							c.JSON(status, gin.H{
								"error": gin.H{
									"type":    errType,
									"message": errMsg,
								},
							})
							return resultWithUsage(), fmt.Errorf("upstream response failed: passthrough rule matched message=%s", errMsg)
						}
					}
				}
				forceFlushFailedEvent = true
				sawFailedEvent = true
			}
			if trimmedData == "[DONE]" {
				sawDone = true
				terminalEventType = "[DONE]"
			}
			if openAIStreamEventIsTerminalWithType(trimmedData, eventType) {
				sawTerminalEvent = true
				if trimmedData != "[DONE]" {
					terminalEventType = eventType
				}
			}
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			imageCounter.AddSSEData(dataBytes)
			if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(
				dataBytes,
				eventType,
				openAIStreamClientOutputStarted(c, clientOutputStarted),
			); sanitized {
				dataBytes = sanitizedData
				trimmedData = strings.TrimSpace(string(sanitizedData))
				line = "data: " + string(sanitizedData)
			}
			lineStartsClientOutput = forceFlushFailedEvent || openAIStreamDataStartsClientOutput(trimmedData, eventType)
			if lineStartsClientOutput && trimmedData != "[DONE]" && !openAIStreamEventTypeIsTerminal(eventType) {
				semanticOutputSeen = true
			}
			// OpenAI Responses streams that terminate with an empty
			// response.completed (no output, no usage, no error, nothing sent
			// to the client) are silent upstream refusals: fail over instead of
			// recording a successful 0/0 usage turn (issue #5009).
			if (eventType == "response.completed" || eventType == "response.done") &&
				!sawFailedEvent && !semanticOutputSeen && !clientOutputStarted &&
				openAIResponsesCompletedEventIsEmpty(dataBytes, usage) {
				return resultWithUsage(), newOpenAIResponsesEmptyCompletedFailoverError(c, account, upstreamRequestID)
			}
			if firstTokenMs == nil && openAIStreamDataStartsTTFT(trimmedData, eventType, forceFlushFailedEvent, ttftMode) {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			s.parseSSEUsageBytesWithType(dataBytes, eventType, usage)
		}
		if line == "" {
			pendingSSEEventType = ""
			if suppressCurrentEvent {
				suppressCurrentEvent = false
				responseFailedPending = false
				continue
			}
		}
		if !clientDisconnected && !failureDelivered && !suppressCurrentEvent {
			if !clientOutputStarted && !lineStartsClientOutput {
				if !appendOpenAIStreamPendingLine(&pendingLines, &pendingLineBytes, line) {
					return resultWithUsage(), s.newOpenAIStreamFailoverError(
						c, account, true, upstreamRequestID, nil, "OpenAI pre-output buffer limit exceeded", resp.Header,
					)
				}
				continue
			}
			if !clientOutputStarted && len(pendingLines) > 0 {
				if !writePendingLines() {
					continue
				}
			}
			n, err := fmt.Fprintln(w, line)
			if err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			} else {
				if isSSECommentLine(line) {
					recordOpenAIStreamKeepaliveBytes(c, n)
				}
				clientOutputStarted = true
				if lineStartsClientOutput {
					MarkOpenAIStreamSemanticOutputStarted(c)
				}
				flushPending = true
				if line == "" {
					flushPendingOutput()
				}
			}
		}
		if line == "" && responseFailedPending {
			responseFailedPending = false
			failureDelivered = true
		}
	}
	if failoverErr := bareCapacityFailover(); failoverErr != nil {
		return resultWithUsage(), failoverErr
	}
	ensureResponseFailedTerminal()
	if err := documentScanner.Err(); err != nil {
		if (sawDone || sawTerminalEvent) && !sawFailedEvent {
			s.clearOpenAIProxyStreamDisconnect(account)
			return resultWithUsage(), nil
		}
		if sawFailedEvent {
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", err)
		}
		if errors.Is(err, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, err)
			return resultWithUsage(), err
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(err.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, msg)
		}
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", err)
		}
		s.recordOpenAIProxyStreamDisconnect(account, err, upstreamRequestID)
		logger.LegacyPrintf("service.openai_gateway",
			"[OpenAI passthrough] 流读取异常中断: account=%d request_id=%s err=%v",
			account.ID,
			upstreamRequestID,
			err,
		)
		return resultWithUsage(), fmt.Errorf("stream read error: %w", err)
	}
	if sawFailedEvent {
		return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
	}
	if !clientDisconnected && !sawDone && !sawTerminalEvent && ctx.Err() == nil {
		logger.FromContext(ctx).With(
			zap.String("component", "service.openai_gateway"),
			zap.Int64("account_id", account.ID),
			zap.String("upstream_request_id", upstreamRequestID),
		).Info("OpenAI passthrough 上游流在未收到 [DONE] 时结束，疑似断流")
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, "OpenAI stream ended before a terminal event")
		}
		s.recordOpenAIProxyStreamDisconnect(account, errors.New("stream ended before terminal event"), upstreamRequestID)
		return resultWithUsage(), errors.New("stream usage incomplete: missing terminal event")
	}
	if (sawDone || sawTerminalEvent) && !sawFailedEvent {
		s.clearOpenAIProxyStreamDisconnect(account)
	}
	logOpenAISuccessMissingUsage(ctx, c, account, resp, usage, terminalEventType, clientDisconnected)

	return resultWithUsage(), nil
}

// nonStreamingTerminalFailureFailover applies the streaming path's terminal
// event verdict to a buffered stream=false response. No failover is proposed
// after a response commit or without an originating account.
func (s *OpenAIGatewayService) nonStreamingTerminalFailureFailover(
	c *gin.Context,
	resp *http.Response,
	account *Account,
	passthrough bool,
	terminalType string,
	payload []byte,
	message string,
	canonicalModel ...string,
) *UpstreamFailoverError {
	if account == nil || IsResponseCommitted(c) {
		return nil
	}
	shouldFailover := openAIStreamFailedEventShouldFailover(payload, message)
	if terminalType == "error" {
		shouldFailover = openAIStreamErrorEventShouldFailover(payload, message)
	}
	if !shouldFailover {
		return nil
	}
	var headers http.Header
	upstreamRequestID := ""
	if resp != nil {
		headers = resp.Header
		upstreamRequestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	return s.newOpenAIStreamFailoverErrorWithModel(
		c,
		account,
		passthrough,
		upstreamRequestID,
		payload,
		message,
		firstNonEmpty(canonicalModel...),
		headers,
	)
}

func (s *OpenAIGatewayService) handleNonStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	stopBeforeWrite func(),
) (*openaiNonStreamingResultPassthrough, error) {
	return s.handleNonStreamingResponsePassthroughForAccount(ctx, resp, c, nil, originalModel, mappedModel, stopBeforeWrite)
}

func (s *OpenAIGatewayService) handleNonStreamingResponsePassthroughForAccount(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	mappedModel string,
	stopBeforeWrite func(),
) (*openaiNonStreamingResultPassthrough, error) {
	if stopBeforeWrite == nil {
		stopBeforeWrite = func() {}
	}
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, func(c *gin.Context) {
		stopBeforeWrite()
		openAITooLargeError(c)
	})
	if err != nil {
		stopBeforeWrite()
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if bodyHasSSEFraming(body) {
		observeOpenAISSEBody(observer, string(body))
	} else {
		observer.ObserveOpenAI(body, strings.TrimSpace(gjson.GetBytes(body, "type").String()))
	}

	// Detect SSE responses from upstream and convert to JSON.
	// Some upstreams (e.g. other sub2api instances) may return SSE even when
	// stream=false was requested. Without this conversion the client would
	// receive raw SSE text or a terminal event with empty output.
	if isEventStreamResponse(resp.Header) {
		return s.handlePassthroughSSEToJSON(resp, c, account, body, originalModel, mappedModel, stopBeforeWrite)
	}

	usage := &OpenAIUsage{}
	usageParsed := false
	if len(body) > 0 {
		if parsedUsage, ok := extractOpenAIUsageFromJSONBytes(body); ok {
			*usage = parsedUsage
			usageParsed = true
		}
	}
	if !usageParsed {
		// 兜底：尝试从 SSE 文本中解析 usage
		usage = s.parseSSEUsageFromBody(string(body))
	}
	logOpenAISuccessMissingUsage(ctx, c, account, resp, usage, "json", false)

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}
	body, err = restoreOpenAIResponsesNamespacePayload(c, body)
	if err != nil {
		stopBeforeWrite()
		return nil, fmt.Errorf("restore OpenAI passthrough namespace response: %w", err)
	}
	body = restoreCodexToolNamesFromContext(c, body)
	body, err = restoreOpenAIResponsesClientToolPayload(c, body)
	if err != nil {
		stopBeforeWrite()
		return nil, fmt.Errorf("restore OpenAI Responses client tools: %w", err)
	}
	stopBeforeWrite()
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIResponseImageOutputsFromJSONBytes(body),
		imageOutputSizes: collectOpenAIResponseImageOutputSizesFromJSONBytes(body),
	}, nil
}

// handlePassthroughSSEToJSON converts an SSE response body into a JSON
// response for the passthrough path. It mirrors handleSSEToJSON while
// preserving passthrough payloads, except compact-only model remapping may
// rewrite model fields back to the original requested model.
func (s *OpenAIGatewayService) handlePassthroughSSEToJSON(resp *http.Response, c *gin.Context, account *Account, body []byte, originalModel string, mappedModel string, stopBeforeWrite ...func()) (*openaiNonStreamingResultPassthrough, error) {
	stop := compactStopFunc(stopBeforeWrite...)
	bodyText := string(body)
	terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
	if terminalOK && (terminalType == "response.failed" || terminalType == "error") {
		msg := extractOpenAISSEErrorMessage(terminalPayload)
		if msg == "" {
			msg = "Upstream compact response failed"
		}
		if compactErr := newOpenAICompactFallbackSignal(c, terminalPayload, msg); compactErr != nil {
			stop()
			return nil, compactErr
		}
		if failoverErr := s.nonStreamingTerminalFailureFailover(c, resp, account, true, terminalType, terminalPayload, msg, mappedModel); failoverErr != nil {
			stop()
			return nil, failoverErr
		}
		stop()
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg)
	}
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := s.parseSSEUsageFromBody(bodyText)
	if ok {
		if parsedUsage, parsed := extractOpenAIUsageFromJSONBytes(finalResponse); parsed {
			*usage = parsedUsage
		}
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		finalResponse = supplementCompactionItemFromSSE(c, finalResponse, bodyText)
		body = finalResponse
		if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
		restoredBody, restoreErr := restoreOpenAIResponsesNamespacePayload(c, body)
		if restoreErr != nil {
			stop()
			return nil, fmt.Errorf("restore OpenAI passthrough namespace response: %w", restoreErr)
		}
		restoredBody = restoreCodexToolNamesFromContext(c, restoredBody)
		restoredBody, restoreErr = restoreOpenAIResponsesClientToolPayload(c, restoredBody)
		if restoreErr != nil {
			stop()
			return nil, fmt.Errorf("restore OpenAI Responses client tools: %w", restoreErr)
		}
		body = restoredBody
	} else {
		if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
			bodyText = s.replaceModelInSSEBody(bodyText, mappedModel, originalModel)
		}
		body = []byte(bodyText)
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	logOpenAISuccessMissingUsage(c.Request.Context(), c, account, resp, usage, terminalType, false)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	stop()
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
		imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
	}, nil
}

func writeOpenAIPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
	} else {
		// 兜底：尽量保留最基础的 content-type
		if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
			dst.Set("Content-Type", v)
		}
	}
	// 透传模式强制放行 x-codex-* 响应头（若上游返回）。
	// 注意：真实 http.Response.Header 的 key 一般会被 canonicalize；但为了兼容测试/自建响应，
	// 这里用 EqualFold 做一次大小写不敏感的查找。
	getCaseInsensitiveValues := func(h http.Header, want string) []string {
		if h == nil {
			return nil
		}
		for k, vals := range h {
			if strings.EqualFold(k, want) {
				return vals
			}
		}
		return nil
	}

	for _, rawKey := range []string{
		"x-codex-primary-used-percent",
		"x-codex-primary-reset-after-seconds",
		"x-codex-primary-window-minutes",
		"x-codex-secondary-used-percent",
		"x-codex-secondary-reset-after-seconds",
		"x-codex-secondary-window-minutes",
		"x-codex-primary-over-secondary-limit-percent",
	} {
		vals := getCaseInsensitiveValues(src, rawKey)
		if len(vals) == 0 {
			continue
		}
		key := http.CanonicalHeaderKey(rawKey)
		dst.Del(key)
		for _, v := range vals {
			dst.Add(key, v)
		}
	}
	// Turn state is account-scoped. Clear any value left by a failed attempt,
	// then relay the selected upstream value with case-insensitive lookup.
	turnStateKey := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	dst.Del(turnStateKey)
	for _, value := range getCaseInsensitiveValues(src, openAICodexTurnStateHeader) {
		dst.Add(turnStateKey, value)
	}
}

func (s *OpenAIGatewayService) buildUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string, isStream bool, promptCacheKey string, isCodexCLI bool) (*http.Request, error) {
	// Determine target URL based on account type
	var targetURL string
	switch account.Type {
	case AccountTypeOAuth:
		// OAuth accounts use ChatGPT internal API
		targetURL = chatgptCodexURL
	case AccountTypeSetupToken:
		if account.IsOpenAIOAuthLike() {
			targetURL = chatgptCodexURL
		} else {
			targetURL = openaiPlatformAPIURL
		}
	case AccountTypeAPIKey:
		// API Key accounts use Platform API or custom base URL
		baseURL := account.GetOpenAIBaseURL()
		// Adaptive DeepSeek/Kimi expose Responses on a protocol-specific base.
		// Using the generic chat base would append /responses to the wrong root.
		if account.UsesNativeCNResponses() && account.IsAdaptiveAPIProtocol() {
			baseURL = account.GetCNProtocolBaseURL(APIProtocolResponses)
		}
		if baseURL == "" {
			targetURL = openaiPlatformAPIURL
		} else {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURLForPlatform(account.Platform, validatedURL)
		}
	default:
		targetURL = openaiPlatformAPIURL
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	// DeepSeek's native Responses endpoint is stateless: never forward server
	// state flags or a previous response reference that the provider rejects.
	body = normalizeDeepSeekResponsesRequestBody(account, body)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Build a fresh bearer header for OAuth/PAT/API-key accounts. The shared
	// seam rejects unsupported authentication modes explicitly.
	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, fmt.Errorf("build openai authentication headers: %w", err)
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Set headers specific to OAuth accounts (ChatGPT internal API)
	if account.UsesOpenAICodexProtocol() {
		// Required: set Host for ChatGPT API (must use req.Host, not Header.Set)
		req.Host = "chatgpt.com"
		if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
			return nil, fmt.Errorf("resolve chatgpt account headers: %w", err)
		}
	}

	// Whitelist passthrough headers
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiAllowedHeaders[lowerKey] {
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}
	if account.UsesOpenAICodexProtocol() {
		// 清除客户端透传的 session 头，后续用隔离后的值重新设置，防止跨用户会话碰撞。
		req.Header.Del("conversation_id")
		req.Header.Del("session_id")

		compatMessagesBridge := isOpenAICompatMessagesBridgeContext(c)
		if !compatMessagesBridge {
			req.Header.Set("originator", resolveOpenAIUpstreamOriginator(c, isCodexCLI))
		}
		apiKeyID := getAPIKeyIDFromContext(c)
		clientConversationID := strings.TrimSpace(c.Request.Header.Get("conversation_id"))
		clientSessionID := clientConversationID
		if clientSessionID == "" {
			clientSessionID = strings.TrimSpace(c.Request.Header.Get("session_id"))
		}
		if clientSessionID == "" {
			clientSessionID = strings.TrimSpace(promptCacheKey)
		}
		if clientSessionID == "" {
			clientSessionID = extractOpenAIStickySessionSignal(c, body)
		}
		if clientConversationID == "" {
			clientConversationID = extractOpenAIStickySessionSignal(c, body)
		}
		if isOpenAIResponsesCompactPath(c) {
			req.Header.Set("accept", "application/json")
			if req.Header.Get("version") == "" {
				req.Header.Set("version", codexCLIVersion)
			}
			if clientSessionID == "" {
				clientSessionID = resolveOpenAICompactSessionID(c, body)
			}
		} else {
			req.Header.Set("accept", "text/event-stream")
		}
		if clientSessionID != "" {
			req.Header.Set("session_id", isolateOpenAIUpstreamSessionID(apiKeyID, codexAccountIdentitySource(c, account), clientSessionID))
		}
		if clientConversationID != "" {
			req.Header.Set("conversation_id", isolateOpenAIUpstreamSessionID(apiKeyID, codexAccountIdentitySource(c, account), clientConversationID))
		}
	} else if isOpenAIResponsesCompactPath(c) {
		req.Header.Set("accept", "application/json")
	}

	// Apply custom User-Agent if configured
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}

	// 若开启 ForceCodexCLI，则强制将上游 User-Agent 伪装为 Codex CLI。
	// 用于网关未透传/改写 User-Agent 时，仍能命中 Codex 侧识别逻辑。
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", codexCLIUserAgent)
	}
	applyCodexAccountIdentityHeaders(req.Header, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))
	applyStagedCodexFingerprintHeaders(c, account, req.Header)
	// OAuth 终态收口：User-Agent / originator / version 从同一规范版本源重建。
	// compat bridge 会显式不带 originator，收口函数因此不会误补身份。
	if account.UsesOpenAICodexProtocol() {
		enforceCodexIdentityHeadersWithUA(req.Header, s.codexIdentityOverrideUA(account))
	}

	// Ensure required headers exist
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效；OAuth 路径 no-op）
	account.ApplyHeaderOverrides(req.Header)
	setOpenAICodexRoutingHintFromBody(req.Header, account, body)
	logOpenAIRoutingDiagnosticsFromBody(ctx, account, "http", req.Header, body, "not_applicable")

	return req, nil
}

// codexIdentityOverrideUA 返回账号级显式配置的出站 User-Agent，供强制统一身份时
// 贡献 OS / 架构 / 终端指纹。ForceCodexCLI 的语义是使用网关规范身份，优先级保持不变。
func (s *OpenAIGatewayService) codexIdentityOverrideUA(account *Account) string {
	if account == nil {
		return ""
	}
	if s != nil && s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		return ""
	}
	return account.GetOpenAIUserAgent()
}

func (s *OpenAIGatewayService) handleErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)

	// cyber_policy 硬阻断：透传上游原始错误体给客户端（不重包成通用 502），不冷却账号。
	// 当前请求恒透传（需求1）；标记供 handler 事后写风控/邮件。400 cyber 不可 failover
	// （shouldFailoverUpstreamError(400)=false），故走到此处即可安全早返回。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, cyberPolicyClientBody(cyberMsg, body))
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, upstreamDetail)
		MarkResponseCommitted(c)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": clientMsg,
			},
		})
		return nil, fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)

	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		logger.LegacyPrintf("service.openai_gateway",
			"OpenAI upstream error %d (account=%d platform=%s type=%s): %s",
			resp.StatusCode,
			account.ID,
			account.Platform,
			account.Type,
			truncateForLog(body, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
		)
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformOpenAI,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		c.JSON(status, gin.H{
			"error": gin.H{
				"type":    errType,
				"message": errMsg,
			},
		})
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream gateway error",
			},
		})
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Handle upstream error (mark account status)
	var reqModel string
	if len(requestedModel) > 0 {
		reqModel = strings.TrimSpace(requestedModel[0])
	}
	if reqModel == "" {
		reqModel, _, _ = extractOpenAIRequestMetaFromBody(requestBody)
		reqModel = canonicalOpenAIAccountSchedulingModel(account, reqModel)
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		failoverStatus, failoverBody := sanitizeOpenAICompatFailoverError(resp.StatusCode, upstreamMsg, body, account)
		return nil, &UpstreamFailoverError{
			StatusCode:             failoverStatus,
			ResponseBody:           failoverBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)

	// A non-failover 400 is a deterministic request error. Preserve the
	// upstream OpenAI error shape (including code/param) so clients can fix the
	// request instead of seeing a retryable-looking 502 envelope.
	if isOpenAIDeterministicClientError(resp.StatusCode) {
		writeOpenAIUpstreamClientError(c, resp.StatusCode, body, upstreamMsg)
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
	}

	// Return appropriate error response
	var errType, errMsg string
	var statusCode int

	switch resp.StatusCode {
	case 401:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream authentication failed, please contact administrator"
	case 402:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream payment required: insufficient balance or billing issue"
	case 403:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream access forbidden, please contact administrator"
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = "Upstream rate limit exceeded, please retry later"
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream request failed"
	}
	if isOpenAIContextWindowError(upstreamMsg, body) && upstreamMsg != "" {
		errMsg = upstreamMsg
	}

	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": errMsg,
		},
	})

	if upstreamMsg == "" {
		return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).
type compatErrorWriter func(c *gin.Context, statusCode int, errType, message string)

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.
func (s *OpenAIGatewayService) handleCompatErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	writeError compatErrorWriter,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)

	// cyber_policy：兼容路径（Chat Completions / Anthropic）以各自格式回写错误，
	// 不原样透传 responses 格式的 cyber body（否则对下游格式不合法）。cyber 是上游网络
	// 安全策略拦截，不冷却账号，故标记后直接以兼容格式回写错误并返回，跳过下方
	// handleOpenAIAccountUpstreamError（避免自定义 temp-unschedulable 规则误冷却）。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		clientMsg := cyberPolicyClientMessage(cyberMsg, body)
		writeError(c, resp.StatusCode, "invalid_request_error", clientMsg)
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("Upstream error: %d", resp.StatusCode)
	}
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(resp.StatusCode, body) {
		clientMsg := grokContentPolicyClientMessage(body)
		setOpsUpstreamError(c, resp.StatusCode, clientMsg, upstreamDetail)
		MarkResponseCommitted(c)
		writeError(c, http.StatusForbidden, "invalid_request_error", clientMsg)
		return nil, fmt.Errorf("grok content policy rejection: %s", clientMsg)
	}
	if containsOpenAICompatSensitiveBackendTerm(upstreamMsg, body) {
		if isOpenAIOAuthSensitiveBackendError(account, resp.StatusCode, upstreamMsg, body) {
			s.BlockAccountScheduling(account, time.Now().Add(openAISensitiveBackendFallbackCooldown), "sensitive_backend_400")
		}
		setOpsUpstreamError(c, http.StatusBadGateway, openAICompatSensitiveBackendErrorMessage, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "masked_backend_error",
			Message:            openAICompatSensitiveBackendErrorMessage,
		})
		writeError(c, http.StatusBadGateway, "api_error", openAICompatSensitiveBackendErrorMessage)
		return nil, fmt.Errorf("upstream error: %d (sensitive backend message masked)", resp.StatusCode)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, resp.StatusCode, body,
		http.StatusBadGateway, "api_error", "Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		writeError(c, status, errType, errMsg)
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes — if the account does not handle this status,
	// return a generic error without exposing upstream details.
	//
	// Exception: for upstream 400 client-validation errors (invalid request
	// shape, unsupported MIME type, corrupted file, etc.) we pass the upstream
	// message through as a 400 invalid_request_error so the client can fix
	// their payload. These are client-actionable errors; swallowing them
	// behind a generic 500 makes debugging impossible and is not a security
	// leak. Other 4xx/5xx keep the existing generic-500 behavior.
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		if resp.StatusCode == http.StatusBadRequest && upstreamMsg != "" {
			writeError(c, http.StatusBadRequest, "invalid_request_error", upstreamMsg)
		} else {
			writeError(c, http.StatusInternalServerError, "api_error", "Upstream gateway error")
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Track rate limits and decide whether to trigger secondary failover.
	var modelForCooldown string
	if len(requestedModel) > 0 {
		modelForCooldown = requestedModel[0]
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(
		c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
	)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		failoverStatus, failoverBody := sanitizeOpenAICompatFailoverError(resp.StatusCode, upstreamMsg, body, account)
		return nil, &UpstreamFailoverError{
			StatusCode:             failoverStatus,
			ResponseBody:           failoverBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)

	// Map status code to error type and write response
	errType := "api_error"
	switch {
	case resp.StatusCode == 400:
		errType = "invalid_request_error"
	case resp.StatusCode == 404:
		errType = "not_found_error"
	case resp.StatusCode == 429:
		errType = "rate_limit_error"
	case resp.StatusCode >= 500:
		errType = "api_error"
	}

	writeError(c, resp.StatusCode, errType, upstreamMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

// openaiStreamingResult streaming response result
type openaiStreamingResult struct {
	usage            *OpenAIUsage
	firstTokenMs     *int
	responseID       string
	imageCount       int
	imageOutputSizes []string
	searchCount      int
}

type openaiNonStreamingResult struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
	searchCount      int
}

func (s *OpenAIGatewayService) handleStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, startTime time.Time, originalModel, mappedModel string) (*openaiStreamingResult, error) {
	return s.handleStreamingResponseWithReasoning(ctx, resp, c, account, startTime, originalModel, mappedModel, "")
}

func (s *OpenAIGatewayService) handleStreamingResponseWithReasoning(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, startTime time.Time, originalModel, mappedModel, reasoningEffort string) (*openaiStreamingResult, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	firstOutputTimeout := time.Duration(0)
	if account != nil && account.Platform == PlatformOpenAI {
		firstOutputTimeout = s.openAIFirstOutputTimeout(reasoningEffort)
	}
	guardFirstOutput := firstOutputTimeout > 0
	stageFirstOutput := account != nil && account.Platform == PlatformOpenAI
	var attemptResponseHeaders http.Header
	if stageFirstOutput {
		if s.responseHeaderFilter != nil {
			attemptResponseHeaders = responseheaders.FilterHeaders(resp.Header, s.responseHeaderFilter)
		} else if requestID := strings.TrimSpace(resp.Header.Get("x-request-id")); requestID != "" {
			attemptResponseHeaders = http.Header{"X-Request-Id": []string{requestID}}
		}
	} else if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// x-codex-turn-state 不在通用响应头白名单内，按 Codex 协议显式回传：
	// 客户端会在同回合的后续请求中回带（openai_codex_turn_state.go）。
	// OpenAI 首个语义输出前只暂存，溯源在 applyAttemptResponseHeaders 真正提交时记录。
	if stageFirstOutput {
		stageOpenAICodexTurnState(&attemptResponseHeaders, resp.Header)
	} else {
		s.relayOpenAICodexTurnState(c, account, resp.Header)
	}

	// Set SSE response headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Pass through other headers
	if !stageFirstOutput && resp.Header.Get("x-request-id") != "" {
		v := resp.Header.Get("x-request-id")
		c.Header("x-request-id", v)
	}
	applyAttemptResponseHeaders := func() {
		if !stageFirstOutput || len(attemptResponseHeaders) == 0 || c.Writer.Written() {
			return
		}
		for key, values := range attemptResponseHeaders {
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
		// 暂存头此刻才真正写给客户端：turn-state 溯源在这里记录（见
		// noteStagedOpenAICodexTurnStateCommitted 的 failover 说明）。
		s.noteStagedOpenAICodexTurnStateCommitted(c, account, attemptResponseHeaders)
		// These headers describe this gateway's SSE stream and are stable across
		// account attempts. Keep them authoritative over upstream values.
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	var firstTokenMs *int
	ttftMode := s.openAITTFTMode(ctx)
	firstOutputProgressObserved := false
	bufferedWriter := bufio.NewWriterSize(w, 4*1024)
	var firstOutputStage *openAIFirstOutputStage
	if stageFirstOutput {
		firstOutputStage = newDefaultOpenAIFirstOutputStage()
		defer func() {
			if err := firstOutputStage.Close(); err != nil {
				logger.LegacyPrintf("service.openai_gateway", "OpenAI first-output staging cleanup failed: account=%d model=%s error=%v", account.ID, originalModel, err)
			}
		}()
	}
	writePendingString := func(value string) (int, error) {
		if firstOutputStage != nil && !firstOutputStage.closed {
			return firstOutputStage.WriteString(value)
		}
		return bufferedWriter.WriteString(value)
	}
	pendingBytes := func() int64 {
		if firstOutputStage != nil && !firstOutputStage.closed {
			return firstOutputStage.Buffered()
		}
		return int64(bufferedWriter.Buffered())
	}
	flushBuffered := func() error {
		if firstOutputStage != nil && !firstOutputStage.closed {
			if err := firstOutputStage.CommitTo(w); err != nil {
				return err
			}
		} else {
			if err := bufferedWriter.Flush(); err != nil {
				return err
			}
		}
		flusher.Flush()
		return nil
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	responseID := ""
	var firstOutputScanGuard atomic.Bool
	firstOutputScanGuard.Store(stageFirstOutput)
	scanner := bufio.NewScanner(resp.Body)
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	if stageFirstOutput {
		scanner.Split(openAIFirstOutputDynamicScanLines(&firstOutputScanGuard))
	}
	documentScanner := newOpenAISSEJSONDocumentScanner(scanner)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	// Grok: always enforce an upstream-read idle so hung SSE bodies fail over
	// instead of holding the OAuth slot until the client cancels. Prefer the
	// global gateway setting when set; otherwise apply a Grok-only default.
	if account != nil && account.Platform == PlatformGrok {
		cfgSec := 0
		if s.cfg != nil {
			cfgSec = s.cfg.Gateway.StreamDataIntervalTimeout
		}
		streamInterval = resolveGrokStreamIdleTimeout(cfgSec)
	}
	// 仅监控上游数据间隔超时，不被下游写入阻塞影响
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	// 下游 keepalive 仅用于防止代理空闲断开
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}

	var firstOutputTimer *time.Timer
	var firstOutputCh <-chan time.Time
	if firstOutputTimeout > 0 {
		remaining := time.Until(startTime.Add(firstOutputTimeout))
		if remaining <= 0 {
			remaining = time.Nanosecond
		}
		firstOutputTimer = time.NewTimer(remaining)
		firstOutputCh = firstOutputTimer.C
		defer firstOutputTimer.Stop()
	}
	stopFirstOutputTimer := func() {
		if firstOutputTimer == nil {
			return
		}
		if !firstOutputTimer.Stop() {
			select {
			case <-firstOutputTimer.C:
			default:
			}
		}
		firstOutputTimer = nil
		firstOutputCh = nil
	}
	// Track downstream writes separately from upstream reads: pre-output failover
	// can buffer response.created / response.in_progress, so keepalive must be
	// based on downstream idle time.
	lastDownstreamWriteAt := time.Now()

	// 仅发送一次错误事件，避免多次写入导致协议混乱。
	// 注意：OpenAI `/v1/responses` streaming 事件必须符合 OpenAI Responses schema；
	// 否则下游 SDK（例如 OpenCode）会因为类型校验失败而报错。
	errorEventSent := false
	clientDisconnected := false // 客户端断开后继续 drain 上游以收集 usage
	sawTerminalEvent := false
	sawFailedEvent := false
	sawBareError := false
	sawResponseFailed := false
	terminalEventType := ""
	responsesSemanticOutputSeen := false
	capacityFailoverSuppressedLogged := false
	failedMessage := ""
	clientOutputStarted := false
	codexFailureTerminal := account != nil && account.IsOpenAIOAuthLike()
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	var streamEarlyErr error
	terminalFailurePending := false
	failureDelivered := false
	suppressCurrentEvent := false
	var bareErrorPayload []byte
	bareErrorAccountSideEffectsPending := false
	pendingSSEEventType := ""
	eventInProgress := false
	eventStartsClientOutput := false
	eventStartsTTFTOutput := false
	eventShouldFlush := false
	handlePendingWriteError := func(err error) {
		if firstOutputStage != nil && !firstOutputStage.closed {
			message := "OpenAI first-output staging failed"
			if errors.Is(err, errOpenAIFirstOutputStageLimit) {
				message = "OpenAI first-output staging limit exceeded"
			}
			logger.LegacyPrintf("service.openai_gateway", "%s: account=%d model=%s error=%v", message, account.ID, originalModel, err)
			failoverErr := s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, nil, message)
			failoverErr.SafeToFailoverAfterWrite = true
			streamEarlyErr = failoverErr
			_ = resp.Body.Close()
			return
		}
		clientDisconnected = true
		logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
	}
	completeGuardedEvent := func(queueDrained bool) {
		completedProgressEvent := eventStartsClientOutput
		completedTTFTEvent := eventStartsTTFTOutput
		shouldFlush := eventShouldFlush || (queueDrained && clientOutputStarted)
		eventInProgress = false
		if !clientDisconnected {
			if completedProgressEvent {
				applyAttemptResponseHeaders()
			}
			if shouldFlush {
				if err := flushBuffered(); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming flush, continuing to drain upstream for billing")
				} else {
					clientOutputStarted = true
					if completedProgressEvent {
						MarkOpenAIStreamSemanticOutputStarted(c)
					}
					lastDownstreamWriteAt = time.Now()
				}
			}
		}
		if completedProgressEvent && !firstOutputProgressObserved {
			firstOutputScanGuard.Store(false)
			firstOutputProgressObserved = true
			stopFirstOutputTimer()
		}
		if completedTTFTEvent && firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		eventStartsClientOutput = false
		eventStartsTTFTOutput = false
		eventShouldFlush = false
	}
	sendErrorEvent := func(reason string) {
		if errorEventSent || clientDisconnected || failureDelivered {
			return
		}
		errorEventSent = true
		payload := `{"type":"error","sequence_number":0,"error":{"type":"upstream_error","message":` + strconv.Quote(reason) + `,"code":` + strconv.Quote(reason) + `}}`
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		if _, err := writePendingString("data: " + payload + "\n\n"); err != nil {
			clientDisconnected = true
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}

	needModelReplace := originalModel != mappedModel
	streamOutputAccumulator := apicompat.NewBufferedResponseAccumulator()
	streamDoneItems := newResponsesStreamOutputItems()
	streamImageOutputs := make([]json.RawMessage, 0, 1)
	streamSeenImages := make(map[string]struct{})
	searchCounter := 0
	// Dedup search tool calls across SSE events (item.done + response.completed
	// both list the same call_id — counting both would ~2× the surcharge).
	streamSearchSeen := make(map[string]struct{})
	resultWithUsage := func() *openaiStreamingResult {
		return &openaiStreamingResult{
			usage:            usage,
			firstTokenMs:     firstTokenMs,
			responseID:       responseID,
			imageCount:       imageCounter.Count(),
			imageOutputSizes: imageCounter.Sizes(),
			searchCount:      searchCounter,
		}
	}
	flushPending := func(disconnectMessage string) {
		if clientDisconnected || pendingBytes() == 0 {
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.openai_gateway", "%s", disconnectMessage)
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}
	finalizeStream := func() (*openaiStreamingResult, error) {
		if stageFirstOutput && eventInProgress {
			// EOF dispatches the final SSE event even without a trailing blank line.
			completeGuardedEvent(true)
		}
		if codexFailureTerminal && sawBareError && !sawResponseFailed &&
			!openAIStreamClientOutputStarted(c, clientOutputStarted) && !eventShouldFlush &&
			isOpenAIRequestScopedCapacityShed(failedMessage, bareErrorPayload) {
			bareErrorAccountSideEffectsPending = false
			return resultWithUsage(), s.newOpenAIStreamFailoverError(
				c, account, false, upstreamRequestID, bareErrorPayload, failedMessage, resp.Header,
			)
		}
		if codexFailureTerminal && sawBareError && !sawResponseFailed && bareErrorAccountSideEffectsPending {
			s.handleOpenAIStreamTerminalAccountSideEffects(c, account, bareErrorPayload, failedMessage, resp.Header)
			bareErrorAccountSideEffectsPending = false
		}
		if codexFailureTerminal && sawBareError && !sawResponseFailed && !clientDisconnected {
			applyAttemptResponseHeaders()
			if _, err := writePendingString(buildOpenAIResponseFailedSSE(responseID, originalModel, bareErrorPayload, failedMessage)); err != nil {
				handlePendingWriteError(err)
			} else {
				failureDelivered = true
			}
		}
		if sawTerminalEvent && !sawFailedEvent {
			s.clearOpenAIProxyStreamDisconnect(account)
		}
		if !sawTerminalEvent && !openAIStreamClientOutputStarted(c, clientOutputStarted) && !eventShouldFlush {
			return resultWithUsage(), s.newOpenAIStreamFailoverError(
				c,
				account,
				false,
				upstreamRequestID,
				nil,
				"OpenAI stream ended before a terminal event",
			)
		}
		flushPending("Client disconnected during final flush, returning collected usage")
		if !sawTerminalEvent {
			if openAIStreamClientOutputStarted(c, clientOutputStarted) && !clientDisconnected {
				s.recordOpenAIProxyStreamDisconnect(account, errors.New("stream ended before terminal event"), upstreamRequestID)
			}
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: missing terminal event")
		}
		if sawFailedEvent {
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		logOpenAISuccessMissingUsage(ctx, c, account, resp, usage, terminalEventType, clientDisconnected)
		return resultWithUsage(), nil
	}
	handleScanErr := func(scanErr error) (*openaiStreamingResult, error, bool) {
		if scanErr == nil {
			return nil, nil, false
		}
		if errors.Is(scanErr, errOpenAIFirstOutputScannerLimit) && !firstOutputProgressObserved {
			logger.LegacyPrintf("service.openai_gateway", "SSE token exceeded guarded first-output limit: account=%d limit=%d error=%v", account.ID, openAIFirstOutputStageMaxBytes+openAIFirstOutputScannerFramingAllowance, scanErr)
			failoverErr := s.newOpenAIStreamFailoverError(
				c, account, false, upstreamRequestID, nil,
				"OpenAI SSE line exceeds guarded first-output limit",
			)
			failoverErr.SafeToFailoverAfterWrite = true
			return resultWithUsage(), failoverErr, true
		}
		if errors.Is(scanErr, bufio.ErrTooLong) && stageFirstOutput && !firstOutputProgressObserved {
			logger.LegacyPrintf("service.openai_gateway", "SSE line too long before first output: account=%d max_size=%d error=%v", account.ID, maxLineSize, scanErr)
			failoverErr := s.newOpenAIStreamFailoverError(
				c, account, false, upstreamRequestID, nil,
				"OpenAI SSE line exceeds guarded first-output limit",
			)
			failoverErr.SafeToFailoverAfterWrite = true
			return resultWithUsage(), failoverErr, true
		}
		if sawTerminalEvent {
			if !sawFailedEvent {
				s.clearOpenAIProxyStreamDisconnect(account)
				logger.LegacyPrintf("service.openai_gateway", "Upstream scan ended after terminal event: %v", scanErr)
			}
			result, err := finalizeStream()
			return result, err, true
		}
		// 客户端断开/取消请求时，上游读取往往会返回 context canceled。
		// /v1/responses 的 SSE 事件必须符合 OpenAI 协议；这里不注入自定义 error event，避免下游 SDK 解析失败。
		if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
			if eventShouldFlush {
				flushPending("Client disconnected during canceled stream flush, returning collected usage")
			}
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", scanErr), true
		}
		if errors.Is(scanErr, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, scanErr)
			sendErrorEvent("response_too_large")
			return resultWithUsage(), scanErr, true
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) && !eventShouldFlush {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(scanErr.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(), s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, nil, msg), true
		}
		// 客户端已断开时，上游出错仅影响体验，不影响计费；返回已收集 usage
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", scanErr), true
		}
		s.recordOpenAIProxyStreamDisconnect(account, scanErr, upstreamRequestID)
		sendErrorEvent("stream_read_error")
		return resultWithUsage(), fmt.Errorf("stream read error: %w", scanErr), true
	}
	processSSELine := func(line string, queueDrained bool) {
		if streamEarlyErr != nil {
			return
		}
		if eventType, ok := extractOpenAISSEEventLine(line); ok {
			pendingSSEEventType = eventType
			eventType = strings.TrimSpace(eventType)
			suppressCurrentEvent = codexFailureTerminal && (eventType == "error" || (sawBareError && !sawResponseFailed && eventType != "response.failed"))
		}
		// Extract data from SSE line (supports both "data: " and "data:" formats)
		if data, ok := extractOpenAISSEDataLine(line); ok {
			dataBytes := []byte(data)
			eventType := effectiveOpenAISSEEventType(dataBytes, pendingSSEEventType)
			if codexFailureTerminal && sawBareError && !sawResponseFailed &&
				(eventType == "response.completed" || eventType == "response.done") {
				// A later successful terminal is authoritative over a pending bare
				// error. Keep its usage and terminal visible to the client.
				sawBareError = false
				sawFailedEvent = false
				terminalFailurePending = false
				suppressCurrentEvent = false
				bareErrorPayload = nil
				bareErrorAccountSideEffectsPending = false
				failedMessage = ""
			}
			if codexFailureTerminal && sawBareError && !sawResponseFailed && eventType != "response.failed" {
				suppressCurrentEvent = true
			}
			observer.ObserveOpenAI(dataBytes, eventType)
			// 初始上游 data 的 type 只解析一次：原始值保持终止事件的精确匹配，规范化值供后续分支复用。
			if openAIStreamEventIsTerminalWithType(data, eventType) {
				sawTerminalEvent = true
				terminalEventType = eventType
				if strings.TrimSpace(data) == "[DONE]" {
					terminalEventType = "[DONE]"
				}
			}
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			forceFlushFailedEvent := false
			if !capacityFailoverSuppressedLogged && account != nil && account.Platform == PlatformOpenAI &&
				(eventType == "error" || eventType == "response.failed") &&
				openAIStreamClientOutputStarted(c, clientOutputStarted) &&
				isOpenAIUpstreamCapacityShedEvent(dataBytes) {
				logOpenAICapacityFailoverSuppressed(ctx, account, "native_sse", upstreamRequestID, eventType)
				capacityFailoverSuppressedLogged = true
			}
			cyberHit := false
			if eventType == "response.failed" || eventType == "error" {
				if codexFailureTerminal && eventType == "error" {
					sawBareError = true
					bareErrorPayload = append(bareErrorPayload[:0], dataBytes...)
					suppressCurrentEvent = true
				} else if codexFailureTerminal && eventType == "response.failed" {
					sawResponseFailed = true
				}
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				if failedMessage == "" {
					failedMessage = "Upstream response failed"
				}
				// response.failed 自带上游已消耗的 usage（input token 通常已扣）；必须先解析
				// 再打 cyber 标记，否则 mark 记到的是解析前的 0，导致流式 cyber 按 0 token 计费
				// 而漏记真实用量。对齐 WS V2 / Chat 流式路径（均先解析 usage 再 Mark）。
				s.parseSSEUsageBytesWithType(dataBytes, eventType, usage)
				if hit, code, msg := detectOpenAICyberPolicy(dataBytes); hit {
					cyberHit = true
					MarkOpsCyberPolicy(c, CyberPolicyMark{
						Code:           code,
						Message:        msg,
						Body:           truncateString(string(dataBytes), 4096),
						UpstreamStatus: http.StatusOK,
						UpstreamInTok:  usage.InputTokens,
						UpstreamOutTok: usage.OutputTokens,
					})
				}
				outputStarted := openAIStreamClientOutputStarted(c, clientOutputStarted)
				if !outputStarted && !cyberHit {
					if compactErr := newOpenAICompactFallbackSignal(c, dataBytes, failedMessage); compactErr != nil {
						sawFailedEvent = true
						streamEarlyErr = compactErr
						return
					}
				}
				if outputStarted && !cyberHit {
					if codexFailureTerminal && eventType == "error" {
						// OpenAI commonly follows a bare error with response.failed.
						// Defer account health updates so the pair is applied once.
						bareErrorAccountSideEffectsPending = true
					} else {
						s.handleOpenAIStreamTerminalAccountSideEffects(c, account, dataBytes, failedMessage, resp.Header)
						bareErrorAccountSideEffectsPending = false
					}
				}
				if !outputStarted {
					shouldFailover := false
					if !cyberHit {
						if eventType == "error" {
							shouldFailover = openAIStreamErrorEventShouldFailover(dataBytes, failedMessage)
						} else {
							shouldFailover = openAIStreamFailedEventShouldFailover(dataBytes, failedMessage)
						}
					}
					if shouldFailover {
						// Hold an OpenAI OAuth bare error until its authoritative
						// response.failed pair arrives. The terminal event carries the
						// billable usage that must survive the failover decision.
						if !codexFailureTerminal || eventType != "error" {
							sawFailedEvent = true
							streamEarlyErr = s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, dataBytes, failedMessage, resp.Header)
							return
						}
					}
					if !cyberHit && !sawBareError {
						if status, errType, errMsg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, dataBytes, failedMessage); matched {
							sawFailedEvent = true
							// 命中透传规则也要记录 ops 上游错误事件（对齐 CC/Messages 与
							// antigravity 先例），否则透传命中的 failed 在监控中不可见。
							s.recordOpenAIStreamUpstreamError(c, account, false, upstreamRequestID, "http_error", dataBytes, failedMessage)
							MarkResponseCommitted(c)
							c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
							c.JSON(status, gin.H{
								"error": gin.H{
									"type":    errType,
									"message": errMsg,
								},
							})
							streamEarlyErr = fmt.Errorf("upstream response failed: passthrough rule matched message=%s", errMsg)
							return
						}
					}
				}
				forceFlushFailedEvent = true
				sawFailedEvent = true
				terminalFailurePending = !codexFailureTerminal || eventType == "response.failed"
			}
			if normalizedData, normalized := normalizeCompletedImageGenerationStatus(dataBytes); normalized {
				dataBytes = normalizedData
				data = string(normalizedData)
				line = "data: " + data
			}
			imageCounter.AddSSEData(dataBytes)
			searchCounter += countGrokNativeSearchCallsInSSEDataDedup(dataBytes, streamSearchSeen)

			// Correct Codex tool calls if needed (apply_patch -> edit, etc.)
			if correctedData, corrected := s.toolCorrector.CorrectToolCallsInSSEBytes(dataBytes); corrected {
				dataBytes = correctedData
				data = string(correctedData)
				line = "data: " + data
				eventType = effectiveOpenAISSEEventType(dataBytes, eventType)
			}
			if imageOutput, ok := extractImageGenerationOutputFromSSEData(dataBytes, streamSeenImages); ok {
				streamImageOutputs = append(streamImageOutputs, imageOutput)
			}
			streamDoneItems.Observe(dataBytes)
			if responsesStreamEventMayContributeToOutput(eventType) {
				var streamEvent apicompat.ResponsesStreamEvent
				if err := json.Unmarshal(dataBytes, &streamEvent); err == nil {
					streamOutputAccumulator.ProcessEvent(&streamEvent)
				}
			}
			if normalizedData, normalized := normalizeResponsesStreamingTerminalOutput(dataBytes, streamOutputAccumulator, streamDoneItems, streamImageOutputs); normalized {
				dataBytes = normalizedData
				data = string(normalizedData)
				line = "data: " + data
				eventType = effectiveOpenAISSEEventType(dataBytes, eventType)
			}
			restoredData, restoreErr := restoreGrokResponsesClientToolPayload(c, dataBytes)
			if restoreErr != nil {
				streamEarlyErr = fmt.Errorf("restore Grok Responses client tool response: %w", restoreErr)
				return
			}
			restoredData, restoreErr = restoreOpenAIResponsesNamespacePayload(c, restoredData)
			if restoreErr != nil {
				streamEarlyErr = fmt.Errorf("restore OpenAI namespace response: %w", restoreErr)
				return
			}
			restoredData = restoreCodexToolNamesFromSSEContext(c, restoredData, eventType)
			if !bytes.Equal(restoredData, dataBytes) {
				dataBytes = restoredData
				data = string(restoredData)
				line = "data: " + data
				eventType = effectiveOpenAISSEEventType(dataBytes, eventType)
			}
			if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(
				dataBytes,
				eventType,
				openAIStreamClientOutputStarted(c, clientOutputStarted),
			); sanitized {
				dataBytes = sanitizedData
				data = string(sanitizedData)
				line = "data: " + data
			}
			// Replace model in response if needed.
			// Fast path: most events do not contain model field values.
			if needModelReplace && mappedModel != "" && strings.Contains(line, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
			}
			startsClientOutput := forceFlushFailedEvent || openAIStreamDataStartsClientOutput(data, eventType)
			startsVisibleOutput := openAIStreamDataStartsVisibleOutput(data, eventType)
			startsTTFTOutput := openAIStreamDataStartsTTFT(data, eventType, forceFlushFailedEvent, ttftMode)
			if stageFirstOutput {
				eventStartsClientOutput = eventStartsClientOutput || startsClientOutput
				eventStartsTTFTOutput = eventStartsTTFTOutput || startsTTFTOutput
				if startsClientOutput {
					firstOutputScanGuard.Store(false)
				}
			}
			if startsClientOutput && !openAIStreamEventTypeIsTerminal(eventType) {
				responsesSemanticOutputSeen = true
			}
			// OpenAI Responses streams that terminate with an empty
			// response.completed (no output, no usage, no error, nothing sent
			// to the client) are silent upstream refusals: fail over instead of
			// recording a successful 0/0 usage turn (issue #5009).
			if account != nil && account.Platform == PlatformOpenAI &&
				(eventType == "response.completed" || eventType == "response.done") &&
				!sawFailedEvent && !responsesSemanticOutputSeen && !clientOutputStarted &&
				openAIResponsesCompletedEventIsEmpty(dataBytes, usage) {
				sawTerminalEvent = true
				streamEarlyErr = newOpenAIResponsesEmptyCompletedFailoverError(c, account, upstreamRequestID)
				return
			}

			// 写入客户端（客户端断开后继续 drain 上游）
			if !clientDisconnected && !failureDelivered && !suppressCurrentEvent {
				shouldFlush := queueDrained && (clientOutputStarted || startsClientOutput)
				if firstTokenMs == nil && startsVisibleOutput {
					// 保证首个 token 事件尽快出站，避免影响 TTFT。
					shouldFlush = true
				}
				eventShouldFlush = eventShouldFlush || shouldFlush
				if _, err := writePendingString(line); err != nil {
					handlePendingWriteError(err)
				} else if _, err := writePendingString("\n"); err != nil {
					handlePendingWriteError(err)
				} else {
					eventInProgress = true
				}
			}

			// Record first token time
			if !guardFirstOutput && firstTokenMs == nil && startsTTFTOutput {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
				stopFirstOutputTimer()
			}
			s.parseSSEUsageBytesWithType(dataBytes, eventType, usage)
			return
		}

		// A blank line dispatches a guarded event from the attempt-local stage.
		if stageFirstOutput && line == "" {
			pendingSSEEventType = ""
			if suppressCurrentEvent {
				suppressCurrentEvent = false
				terminalFailurePending = false
				eventInProgress = false
				eventStartsClientOutput = false
				eventStartsTTFTOutput = false
				eventShouldFlush = false
				return
			}
			if failureDelivered {
				terminalFailurePending = false
				eventInProgress = false
				eventStartsClientOutput = false
				eventStartsTTFTOutput = false
				eventShouldFlush = false
				return
			}
			if !clientDisconnected {
				if _, err := writePendingString("\n"); err != nil {
					handlePendingWriteError(err)
				}
			}
			if streamEarlyErr == nil {
				completeGuardedEvent(queueDrained)
			}
			if terminalFailurePending && streamEarlyErr == nil {
				terminalFailurePending = false
				failureDelivered = true
			}
			return
		}
		// Non-guarded streams retain upstream's event-boundary flushing: a keepalive
		// or queue-drain flush must never split an open SSE event.
		shouldFlush := false
		if line == "" {
			pendingSSEEventType = ""
			if suppressCurrentEvent {
				suppressCurrentEvent = false
				terminalFailurePending = false
				eventInProgress = false
				eventShouldFlush = false
				return
			}
			shouldFlush = eventShouldFlush || (queueDrained && clientOutputStarted)
			eventShouldFlush = false
			if failureDelivered {
				terminalFailurePending = false
			}
		}
		if !clientDisconnected && !failureDelivered && !suppressCurrentEvent {
			if _, err := writePendingString(line); err != nil {
				handlePendingWriteError(err)
			} else if _, err := writePendingString("\n"); err != nil {
				handlePendingWriteError(err)
			} else {
				eventInProgress = line != ""
				if shouldFlush {
					if err := flushBuffered(); err != nil {
						clientDisconnected = true
						logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming flush, continuing to drain upstream for billing")
					} else {
						clientOutputStarted = true
						lastDownstreamWriteAt = time.Now()
					}
				}
			}
			if line == "" && terminalFailurePending && streamEarlyErr == nil {
				terminalFailurePending = false
				failureDelivered = true
			}
		}
	}

	// 无超时/无 keepalive 的常见路径走同步扫描，减少 goroutine 与 channel 开销。
	if streamInterval <= 0 && keepaliveInterval <= 0 && firstOutputTimeout <= 0 {
		defer putSSEScannerBuf64K(scanBuf)
		for documentScanner.Scan() {
			processSSELine(documentScanner.Text(), true)
			if streamEarlyErr != nil {
				return resultWithUsage(), streamEarlyErr
			}
		}
		if result, err, done := handleScanErr(documentScanner.Err()); done {
			return result, err
		}
		return finalizeStream()
	}

	type scanEvent struct {
		line      string
		err       error
		processed chan struct{}
	}
	// 独立 goroutine 读取上游，避免读取阻塞影响 keepalive/超时处理
	// Guard mode permits one queued token plus the token being processed. With
	// the guarded scanner cap this bounds scanner/channel retention near 16 MiB;
	// the timeout-disabled path preserves the legacy depth of 16.
	events := make(chan scanEvent, openAIFirstOutputEventQueueSize(guardFirstOutput))
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		if firstOutputScanGuard.Load() {
			ev.processed = make(chan struct{})
		}
		select {
		case events <- ev:
		case <-done:
			return false
		}
		if ev.processed == nil {
			return true
		}
		select {
		case <-ev.processed:
			return true
		case <-done:
			return false
		}
	}
	markEventProcessed := func(ev scanEvent) {
		if ev.processed != nil {
			close(ev.processed)
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for documentScanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: documentScanner.Text()}) {
				return
			}
		}
		if err := documentScanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if stageFirstOutput && eventInProgress {
					// EOF dispatches the final SSE event even without a trailing blank
					// line. Do not synthesize extra bytes on the downstream wire.
					completeGuardedEvent(true)
				}
				return finalizeStream()
			}
			if result, err, done := handleScanErr(ev.err); done {
				markEventProcessed(ev)
				return result, err
			}
			processSSELine(ev.line, len(events) == 0)
			markEventProcessed(ev)
			if streamEarlyErr != nil {
				return resultWithUsage(), streamEarlyErr
			}

		case <-intervalCh:
			if failureDelivered {
				return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
			}
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if codexFailureTerminal && sawBareError && !sawResponseFailed {
				_ = resp.Body.Close()
				return finalizeStream()
			}
			if clientDisconnected {
				return resultWithUsage(), fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.LegacyPrintf("service.openai_gateway", "Stream data interval timeout: account=%d model=%s interval=%s", account.ID, originalModel, streamInterval)
			// 处理流超时，可能标记账户为临时不可调度或错误状态
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
			}
			// Grok: short cool + account failover when no client-visible bytes
			// were committed yet (pre-commit). After output started we keep the
			// legacy stream_timeout path so partial SSE is not dual-written.
			if account != nil && account.Platform == PlatformGrok {
				s.tempUnscheduleGrok(ctx, account, grokStreamIdleCooldown, "grok stream idle timeout")
				if !openAIStreamClientOutputStarted(c, clientOutputStarted) && !eventShouldFlush {
					_ = resp.Body.Close()
					return resultWithUsage(), grokStreamIdleFailoverError(account, streamInterval)
				}
			}
			sendErrorEvent("stream_timeout")
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-firstOutputCh:
			if firstOutputProgressObserved {
				stopFirstOutputTimer()
				continue
			}
			if codexFailureTerminal && sawBareError && !sawResponseFailed && len(events) == 0 {
				_ = resp.Body.Close()
				return finalizeStream()
			}
			_ = resp.Body.Close()
			for ev := range events {
				markEventProcessed(ev)
			}
			return resultWithUsage(), s.newOpenAIFirstOutputTimeoutError(
				ctx, c, account, startTime, originalModel, reasoningEffort,
				firstOutputTimeout, "semantic_output", resp.Header,
			)

		case <-keepaliveCh:
			if clientDisconnected || failureDelivered {
				continue
			}
			if eventInProgress {
				continue
			}
			if time.Since(lastDownstreamWriteAt) < keepaliveInterval {
				continue
			}
			if stageFirstOutput {
				// Bypass attempt-local buffered frames. The stable SSE headers may be
				// committed here, but account headers remain private until semantic output.
				n, err := w.Write([]byte(":\n\n"))
				recordOpenAIStreamKeepaliveBytes(c, n)
				if err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
					continue
				}
				flusher.Flush()
				lastDownstreamWriteAt = time.Now()
				continue
			}
			if _, err := writePendingString(":\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				continue
			}
			if err := flushBuffered(); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during keepalive flush, continuing to drain upstream for billing")
			} else {
				lastDownstreamWriteAt = time.Now()
			}
		}
	}

}

// extractOpenAISSEDataLine 低开销提取 SSE `data:` 行内容。
// 兼容 `data: xxx` 与 `data:xxx` 两种格式。
func extractOpenAISSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return line[start:], true
}

func extractOpenAISSEEventLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "event:") {
		return "", false
	}
	start := len("event:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return strings.TrimSpace(line[start:]), true
}

type openAICompatSSEFrame struct {
	EventType string
	Data      string
}

type openAICompatSSEFrameParser struct {
	eventType string
	dataLines []string
}

func (p *openAICompatSSEFrameParser) AddLine(line string) (openAICompatSSEFrame, bool) {
	// codex round 24 fu41 (2026-05-18): trim trailing CR. bufio.Scanner
	// strips both CR and LF when its split is ScanLines, so the streaming
	// paths (handleAnthropic/Chat StreamingResponse) never carried the
	// CR through. BUT the body-level callers (handleSSEToJSON,
	// handlePassthroughSSEToJSON, etc.) walk with strings.Split(body,
	// "\n") which leaves "\r" at the end of each line. Without this
	// trim, "\r" alone fails the `line == ""` blank-line check and the
	// frame never dispatches — the same Form A terminal we were trying
	// to fix gets silently dropped on CRLF bodies. SSE spec accepts
	// LF, CR, or CRLF as line endings; servers do send CRLF.
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if line == "" {
		return p.dispatch()
	}
	// SSE comment lines (": keepalive" etc.) are skipped without
	// flushing — they don't terminate frames.
	if strings.HasPrefix(line, ":") {
		return openAICompatSSEFrame{}, false
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		p.eventType = eventType
		return openAICompatSSEFrame{}, false
	}
	if data, ok := extractOpenAISSEDataLine(line); ok {
		p.dataLines = append(p.dataLines, data)
	}
	return openAICompatSSEFrame{}, false
}

// Finish flushes any pending data/event without requiring a trailing
// blank-line boundary. Used on upstream-close so the final event still
// gets recognized.
func (p *openAICompatSSEFrameParser) Finish() (openAICompatSSEFrame, bool) {
	return p.dispatch()
}

func (p *openAICompatSSEFrameParser) dispatch() (openAICompatSSEFrame, bool) {
	frame := openAICompatSSEFrame{
		EventType: p.eventType,
		Data:      strings.Join(p.dataLines, "\n"),
	}
	p.eventType = ""
	p.dataLines = nil
	return frame, frame.Data != ""
}

// openAICompatLineEventTracker tracks the currently-open event name as
// SSE lines stream past. Used by passthrough/observer loops that forward
// each line raw (so they can't use the line-buffering
// openAICompatSSEFrameParser) but still need to recognize
// `event: response.completed\ndata: {…}` (Form A) terminals.
//
// Update returns (patchedData, isDataLine):
//   - isDataLine=true: the line was a data: line. patchedData is the
//     data payload with type injected from the most recent event: line
//     (no-op when data already carries `type` or no event: was seen).
//   - isDataLine=false: blank line / event: line / comment. The tracker
//     has internalized whatever state change was needed.
//
// codex round 24 fu41 (2026-05-18): added so handleStreamingResponsePassthrough
// (native OpenAI Responses passthrough — clients hitting /v1/responses
// directly) and handleStreamingResponse (Codex passthrough callback)
// recognize Form A terminals on the same single-pass scan they use to
// forward bytes to the client.
type openAICompatLineEventTracker struct {
	eventName string
}

func (t *openAICompatLineEventTracker) Update(line string) (string, bool) {
	if t == nil {
		return "", false
	}
	trimmed := line
	if n := len(trimmed); n > 0 && trimmed[n-1] == '\r' {
		trimmed = trimmed[:n-1]
	}
	if trimmed == "" {
		// blank line = frame boundary; current event name closes.
		t.eventName = ""
		return "", false
	}
	if strings.HasPrefix(trimmed, ":") {
		// comment — no state change.
		return "", false
	}
	if ename, ok := extractOpenAISSEEventLine(trimmed); ok {
		t.eventName = ename
		return "", false
	}
	if data, ok := extractOpenAISSEDataLine(trimmed); ok {
		return openAICompatPayloadWithEventType(data, t.eventName), true
	}
	return "", false
}

// openAICompatPayloadWithEventType patches a `type` field into the data
// payload when the SSE event name carried it but the data didn't. This
// lets every downstream consumer (json.Unmarshal into
// apicompat.ResponsesStreamEvent etc.) keep treating "type" as the
// single source of truth without having to learn about event:/data:
// duality.
//
// No-ops for:
//   - empty event name (form (B) — data already carries type)
//   - empty/whitespace payload
//   - the [DONE] sentinel (not JSON)
//   - payloads that already have a `type` field (upstream sent both;
//     don't overwrite)
func openAICompatPayloadWithEventType(payload, eventType string) string {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
		return payload
	}
	if payloadType := gjson.Get(payload, "type"); payloadType.Exists() && strings.TrimSpace(payloadType.String()) != "" {
		return payload
	}
	patched, err := sjson.Set(payload, "type", eventType)
	if err != nil {
		return payload
	}
	return patched
}

func (s *OpenAIGatewayService) replaceModelInSSELine(line, fromModel, toModel string) string {
	data, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line
	}
	if data == "" || data == "[DONE]" {
		return line
	}

	// 使用 gjson 精确检查 model 字段，避免全量 JSON 反序列化
	if m := gjson.Get(data, "model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	// 检查嵌套的 response.model 字段
	if m := gjson.Get(data, "response.model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "response.model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	return line
}

// correctToolCallsInResponseBody 修正响应体中的工具调用
func (s *OpenAIGatewayService) correctToolCallsInResponseBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	updated := body
	if s != nil && s.toolCorrector != nil {
		if corrected, changed := s.toolCorrector.CorrectToolCallsInSSEBytes(updated); changed {
			updated = corrected
		}
	}
	if normalized, changed := normalizeOpenAIResponsesFunctionCallArguments(updated); changed {
		updated = normalized
	}
	return updated
}

func normalizeOpenAIResponsesFunctionCallArguments(data []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(data)) == 0 || !bytes.Contains(data, []byte(`"arguments"`)) {
		return data, false
	}
	if !gjson.ValidBytes(data) {
		return data, false
	}

	updated := data
	changed := false
	setDedupedArgument := func(path string) {
		arg := gjson.GetBytes(updated, path)
		if !arg.Exists() || arg.Type != gjson.String {
			return
		}
		deduped, ok := dedupeRepeatedJSONArgumentString(arg.Str)
		if !ok {
			return
		}
		next, err := sjson.SetBytes(updated, path, deduped)
		if err != nil {
			return
		}
		updated = next
		changed = true
	}

	eventType := strings.TrimSpace(gjson.GetBytes(updated, "type").String())
	if eventType == "response.function_call_arguments.done" {
		setDedupedArgument("arguments")
	}
	if itemType := strings.TrimSpace(gjson.GetBytes(updated, "item.type").String()); isResponsesFunctionCallItemType(itemType) {
		setDedupedArgument("item.arguments")
	}
	dedupeResponsesFunctionCallOutputArguments(updated, "response.output", setDedupedArgument)
	dedupeResponsesFunctionCallOutputArguments(updated, "output", setDedupedArgument)

	return updated, changed
}

func dedupeResponsesFunctionCallOutputArguments(data []byte, outputPath string, setDedupedArgument func(string)) {
	output := gjson.GetBytes(data, outputPath)
	if !output.Exists() || !output.IsArray() {
		return
	}
	for i, item := range output.Array() {
		if !isResponsesFunctionCallItemType(strings.TrimSpace(item.Get("type").String())) {
			continue
		}
		setDedupedArgument(outputPath + "." + strconv.Itoa(i) + ".arguments")
	}
}

func isResponsesFunctionCallItemType(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func dedupeRepeatedJSONArgumentString(arguments string) (string, bool) {
	if len(arguments) == 0 || len(arguments)%2 != 0 {
		return "", false
	}
	halfLen := len(arguments) / 2
	first := arguments[:halfLen]
	if first != arguments[halfLen:] {
		return "", false
	}
	trimmed := strings.TrimSpace(first)
	if trimmed == "" || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
		return "", false
	}
	if !json.Valid([]byte(first)) {
		return "", false
	}
	return first, true
}

func (s *OpenAIGatewayService) parseSSEUsage(data string, usage *OpenAIUsage) {
	s.parseSSEUsageBytes([]byte(data), usage)
}

func (s *OpenAIGatewayService) parseSSEUsageBytes(data []byte, usage *OpenAIUsage) {
	s.parseSSEUsageBytesWithType(data, "", usage)
}

func effectiveOpenAISSEEventType(payload []byte, eventType string) string {
	if payloadType := strings.TrimSpace(gjson.GetBytes(payload, "type").String()); payloadType != "" {
		return payloadType
	}
	return strings.TrimSpace(eventType)
}

func openAIStreamEventTypeIsTerminal(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return true
	default:
		return false
	}
}

func (s *OpenAIGatewayService) parseSSEUsageBytesWithType(data []byte, eventType string, usage *OpenAIUsage) {
	if usage == nil || len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	parsedUsage, ok := extractOpenAIUsageFromJSONBytes(data)
	if !ok {
		return
	}
	if openAIStreamEventTypeIsTerminal(effectiveOpenAISSEEventType(data, eventType)) {
		if !openAIUsageHasTokens(&parsedUsage) && openAIUsageHasTokens(usage) {
			return
		}
		*usage = parsedUsage
		return
	}
	mergeOpenAIUsageNonZero(usage, parsedUsage)
}

func mergeOpenAIUsageNonZero(dst *OpenAIUsage, src OpenAIUsage) {
	if dst == nil {
		return
	}
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.ImageInputTokens > 0 {
		dst.ImageInputTokens = src.ImageInputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.ImageOutputTokens > 0 {
		dst.ImageOutputTokens = src.ImageOutputTokens
	}
}

func extractOpenAIUsageFromJSONBytes(body []byte) (OpenAIUsage, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return OpenAIUsage{}, false
	}
	candidates := []struct {
		usagePath      string
		imageUsagePath string
	}{
		{usagePath: "usage", imageUsagePath: "tool_usage.image_gen"},
		{usagePath: "response.usage", imageUsagePath: "response.tool_usage.image_gen"},
		{usagePath: "data.usage", imageUsagePath: "data.tool_usage.image_gen"},
		{usagePath: "data.response.usage", imageUsagePath: "data.response.tool_usage.image_gen"},
	}
	for _, candidate := range candidates {
		if usage, ok := openAIUsageFromGJSON(gjson.GetBytes(body, candidate.usagePath)); ok {
			mergeHostedImageGenToolUsage(gjson.GetBytes(body, candidate.imageUsagePath), &usage)
			return usage, true
		}
	}
	return OpenAIUsage{}, false
}

func mergeHostedImageGenToolUsage(imageGen gjson.Result, usage *OpenAIUsage) {
	if usage == nil || !imageGen.Exists() || !imageGen.IsObject() {
		return
	}
	if usage.ImageOutputTokens == 0 {
		if value := imageGen.Get("output_tokens_details.image_tokens").Int(); value > 0 {
			usage.ImageOutputTokens = int(value)
		}
	}
	if usage.ImageInputTokens == 0 {
		if value := imageGen.Get("input_tokens_details.image_tokens").Int(); value > 0 {
			usage.ImageInputTokens = int(value)
		}
	}
}

func logOpenAIHTTP200SuspiciousUsageResponse(ctx context.Context, source string, resp *http.Response, c *gin.Context, body []byte, usage *OpenAIUsage, usageParsed bool) {
	if resp == nil || resp.StatusCode != http.StatusOK || len(body) == 0 {
		return
	}

	reason := detectOpenAIHTTP200SuspiciousUsageReason(body, usage, usageParsed)
	if reason == "" {
		return
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.String("source", source),
		zap.String("reason", reason),
		zap.Int("status_code", resp.StatusCode),
		zap.String("content_type", resp.Header.Get("Content-Type")),
		zap.String("upstream_request_id", strings.TrimSpace(resp.Header.Get("x-request-id"))),
		zap.Int("response_body_bytes", len(body)),
		zap.String("response_body_preview", openAIHTTP200SuspiciousBodyPreview(body)),
	}
	if c != nil && c.Request != nil && c.Request.URL != nil {
		fields = append(fields,
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
		)
	}
	logger.FromContext(ctx).With(fields...).Warn("openai.http_200_suspicious_usage_response")
}

func openAIHTTP200SuspiciousBodyPreview(body []byte) string {
	const maxPreviewBytes = 4096
	if len(body) <= maxPreviewBytes {
		return string(body)
	}
	return string(body[:maxPreviewBytes]) + "...[truncated]"
}

func detectOpenAIHTTP200SuspiciousUsageReason(body []byte, usage *OpenAIUsage, usageParsed bool) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "empty_body"
	}
	if !gjson.ValidBytes(trimmed) {
		return "json_parse_failed"
	}
	if gjson.GetBytes(trimmed, "error").Exists() || strings.EqualFold(strings.TrimSpace(gjson.GetBytes(trimmed, "type").String()), "error") {
		return "error_shape"
	}

	usageValue, usageExists := firstExistingGJSONValue(trimmed, "usage", "response.usage", "useage", "response.useage")
	if !usageExists {
		return "usage_missing"
	}
	if !usageValue.IsObject() {
		return "usage_invalid_type"
	}
	if usageJSONHasPositiveTokenCount(usageValue) {
		return ""
	}
	if usageParsed && usage != nil && openAIUsageHasAnyTokens(*usage) {
		return ""
	}
	return "usage_zero"
}

func firstExistingGJSONValue(body []byte, paths ...string) (gjson.Result, bool) {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if value.Exists() {
			return value, true
		}
	}
	return gjson.Result{}, false
}

func usageJSONHasPositiveTokenCount(usage gjson.Result) bool {
	for _, path := range []string{
		"total_tokens",
		"input_tokens",
		"output_tokens",
		"prompt_tokens",
		"completion_tokens",
		"cache_creation_input_tokens",
		"input_tokens_details.cached_tokens",
		"prompt_tokens_details.cached_tokens",
		"output_tokens_details.image_tokens",
		"completion_tokens_details.image_tokens",
	} {
		if usage.Get(path).Int() > 0 {
			return true
		}
	}
	return false
}

func openAIUsageHasAnyTokens(usage OpenAIUsage) bool {
	return usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.CacheCreationInputTokens > 0 ||
		usage.CacheReadInputTokens > 0 ||
		usage.ImageOutputTokens > 0
}

func extractOpenAIResponseIDFromJSONBytes(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
		return id
	}
	return strings.TrimSpace(gjson.GetBytes(body, "response.id").String())
}

const openAIHTTPResponseOwnerContextKey = "openai_http_response_owner"

type openAIHTTPResponseOwner struct {
	userID   int64
	apiKeyID int64
}

func SetOpenAIHTTPResponseOwner(c *gin.Context, userID, apiKeyID int64) {
	if c == nil || userID <= 0 || apiKeyID <= 0 {
		return
	}
	c.Set(openAIHTTPResponseOwnerContextKey, openAIHTTPResponseOwner{userID: userID, apiKeyID: apiKeyID})
}

func (s *OpenAIGatewayService) ValidateOpenAIHTTPResponseOwner(
	ctx context.Context,
	groupID int64,
	responseID string,
	userID, apiKeyID int64,
) (bool, error) {
	if s == nil || strings.TrimSpace(responseID) == "" || userID <= 0 || apiKeyID <= 0 {
		return false, nil
	}
	ownerUserID, ownerAPIKeyID, found, err := s.getOpenAIWSStateStore().GetHTTPResponseOwner(ctx, groupID, responseID)
	if err != nil || !found {
		return false, err
	}
	return ownerUserID == userID || (ownerUserID <= 0 && ownerAPIKeyID == apiKeyID), nil
}

func (s *OpenAIGatewayService) BindOpenAIHTTPResponseOwner(
	ctx context.Context,
	groupID int64,
	responseID string,
	userID, apiKeyID int64,
) error {
	if s == nil {
		return nil
	}
	return s.getOpenAIWSStateStore().BindHTTPResponseOwner(
		ctx, groupID, responseID, userID, apiKeyID, s.openAIWSResponseStickyTTL(),
	)
}

func (s *OpenAIGatewayService) bindHTTPResponseAccount(ctx context.Context, c *gin.Context, account *Account, responseID string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return
	}
	groupID := getOpenAIGroupIDFromContext(c)
	ttl := s.openAIWSResponseStickyTTL()
	logOpenAIWSBindResponseAccountWarn(groupID, account.ID, responseID, store.BindResponseAccount(ctx, groupID, responseID, account.ID, ttl))
	if rawOwner, ok := c.Get(openAIHTTPResponseOwnerContextKey); ok {
		if owner, ok := rawOwner.(openAIHTTPResponseOwner); ok && owner.userID > 0 && owner.apiKeyID > 0 {
			if err := s.BindOpenAIHTTPResponseOwner(ctx, groupID, responseID, owner.userID, owner.apiKeyID); err != nil {
				logger.L().Warn(
					"openai.http_bind_response_owner_failed",
					zap.Int64("group_id", groupID),
					zap.Int64("account_id", account.ID),
					zap.Int64("user_id", owner.userID),
					zap.Int64("api_key_id", owner.apiKeyID),
					zap.String("response_id", truncateOpenAIWSLogValue(responseID, openAIWSIDValueMaxLen)),
					zap.Error(err),
				)
			}
		}
	}
}

// openAIUsageFromGJSON — codex upstream PR#2505: extract OpenAIUsage from a
// gjson result that may use either Responses-shape or Chat-Completions-
// shape field names. Native Responses fields take precedence; chat-shape
// fills in when canonical is zero.
func openAIUsageFromGJSON(value gjson.Result) (OpenAIUsage, bool) {
	if !value.Exists() || !value.IsObject() {
		return OpenAIUsage{}, false
	}
	inputTokens := value.Get("input_tokens").Int()
	if inputTokens == 0 {
		inputTokens = value.Get("prompt_tokens").Int()
	}
	outputTokens := value.Get("output_tokens").Int()
	if outputTokens == 0 {
		outputTokens = value.Get("completion_tokens").Int()
	}
	// xAI reports visible output separately from reasoning_tokens; OpenAI
	// folds reasoning into completion/output. Use total_tokens to distinguish it.
	reasoningTokens := max(int(firstPositiveGJSONInt(
		value.Get("completion_tokens_details.reasoning_tokens"),
		value.Get("output_tokens_details.reasoning_tokens"),
	)), 0)
	if reasoningTokens > 0 {
		outputTokens = xai.IncludeIndependentReasoningTokens(
			inputTokens, outputTokens, value.Get("total_tokens").Int(), int64(reasoningTokens),
		)
	}
	cacheReadTokens := openAICacheReadTokensFromUsage(value)
	cacheCreationTokens := openAICacheCreationTokensFromUsage(value)
	imageInputTokens := firstPositiveGJSONInt(
		value.Get("input_tokens_details.image_tokens"),
		value.Get("prompt_tokens_details.image_tokens"),
	)
	imageOutputTokens := value.Get("output_tokens_details.image_tokens").Int()
	if imageOutputTokens == 0 {
		imageOutputTokens = value.Get("completion_tokens_details.image_tokens").Int()
	}
	return OpenAIUsage{
		InputTokens:              int(inputTokens),
		OutputTokens:             int(outputTokens),
		CacheCreationInputTokens: cacheCreationTokens,
		CacheReadInputTokens:     cacheReadTokens,
		ImageInputTokens:         imageInputTokens,
		ImageOutputTokens:        int(imageOutputTokens),
	}, true
}

func openAICacheReadTokensFromUsage(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cached_tokens"),
		value.Get("prompt_tokens_details.cached_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveGJSONInt(
		value.Get("cache_read_input_tokens"),
		value.Get("cache_read_tokens"),
		value.Get("cached_tokens"),
	)
}

func openAICacheCreationTokensFromUsage(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cache_write_tokens"),
		value.Get("prompt_tokens_details.cache_write_tokens"),
		value.Get("input_tokens_details.cache_creation_tokens"),
		value.Get("prompt_tokens_details.cache_creation_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveGJSONInt(
		value.Get("cache_write_tokens"),
		value.Get("cache_creation_input_tokens"),
		value.Get("cache_write_input_tokens"),
		value.Get("cache_creation_tokens"),
	)
}

func (s *OpenAIGatewayService) handleNonStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, originalModel, mappedModel string, stopBeforeWrite ...func()) (*openaiNonStreamingResult, error) {
	stop := compactStopFunc(stopBeforeWrite...)
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, func(c *gin.Context) {
		stop()
		openAITooLargeError(c)
	})
	if err != nil {
		stop()
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if bodyHasSSEFraming(body) {
		observeOpenAISSEBody(observer, string(body))
	} else {
		observer.ObserveOpenAI(body, strings.TrimSpace(gjson.GetBytes(body, "type").String()))
	}

	// Detect SSE responses for ALL account types via Content-Type header.
	// Some OpenAI-compatible upstreams (including other sub2api instances)
	// may return SSE even when stream=false was requested.
	if isEventStreamResponse(resp.Header) {
		return s.handleSSEToJSONResult(resp, c, account, body, originalModel, mappedModel, stop)
	}
	// bodyLooksLikeSSE is a line-level heuristic: real SSE framing requires
	// "data:"/"event:" field names at the very start of a physical line. A
	// plain bytes.Contains scan would also match ordinary JSON responses
	// whose string content merely echoes the literal text "data:" or
	// "event:" (e.g. compact tool output), causing those JSON bodies to be
	// misrouted into handleSSEToJSON and lose their usage accounting.
	bodyLooksLikeSSE := bodyHasSSEFraming(body)

	// For OAuth accounts, also fall back to a body-content heuristic because
	// the upstream may omit the Content-Type header while still sending SSE.
	// This heuristic is NOT applied to API-key accounts to avoid false
	// positives on JSON responses that coincidentally contain "data:" or
	// "event:" in their text content.
	if account.Type == AccountTypeOAuth && bodyLooksLikeSSE {
		return s.handleSSEToJSONResult(resp, c, account, body, originalModel, mappedModel, stop)
	}
	if account != nil && account.IsGrok() && isOpenAIResponsesCompactPath(c) {
		body, err = convertGrokResponseToOpenAICompact(body)
		if err != nil {
			stop()
			return nil, fmt.Errorf("convert Grok compact response: %w", err)
		}
	}

	usageValue, usageOK := extractOpenAIUsageFromJSONBytes(body)
	if !usageOK {
		logOpenAIHTTP200SuspiciousUsageResponse(ctx, "openai_non_stream_parse_failed", resp, c, body, nil, false)
		if bodyLooksLikeSSE {
			return s.handleSSEToJSONResult(resp, c, account, body, originalModel, mappedModel, stop)
		}
		stop()
		return nil, fmt.Errorf("parse response: invalid json response")
	}
	usage := &usageValue
	imageCounter := newOpenAIImageOutputCounter()
	imageCounter.AddJSONResponse(body)

	// Replace model in response if needed
	if originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}
	body, err = restoreGrokResponsesClientToolPayload(c, body)
	if err != nil {
		stop()
		return nil, fmt.Errorf("restore Grok Responses client tool response: %w", err)
	}
	body, err = restoreOpenAIResponsesClientToolPayload(c, body)
	if err != nil {
		stop()
		return nil, fmt.Errorf("restore OpenAI Responses client tool response: %w", err)
	}
	body, err = restoreOpenAIResponsesNamespacePayload(c, body)
	if err != nil {
		stop()
		return nil, fmt.Errorf("restore OpenAI namespace response: %w", err)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json"
	if s.cfg != nil && !s.cfg.Security.ResponseHeaders.Enabled {
		if upstreamType := resp.Header.Get("Content-Type"); upstreamType != "" {
			contentType = upstreamType
		}
	}

	logOpenAIHTTP200SuspiciousUsageResponse(ctx, "openai_non_stream", resp, c, body, usage, true)
	stop()
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       imageCounter.Count(),
		imageOutputSizes: imageCounter.Sizes(),
		searchCount:      countGrokNativeSearchCallsFromJSONBytes(body),
	}, nil
}

func isEventStreamResponse(header http.Header) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

// bodyHasSSEFraming reports whether body contains genuine SSE framing by
// scanning for physical lines that begin with the "data:" or "event:"
// field names, per the SSE spec. Unlike a raw substring scan, this does not
// match when those strings only appear embedded inside JSON string values.
func bodyHasSSEFraming(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) handleSSEToJSON(resp *http.Response, c *gin.Context, account *Account, body []byte, originalModel, mappedModel string, stopBeforeWrite ...func()) (*OpenAIUsage, error) {
	stop := compactStopFunc(stopBeforeWrite...)
	bodyText := string(body)
	terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
	if terminalOK && (terminalType == "response.failed" || terminalType == "error") {
		msg := extractOpenAISSEErrorMessage(terminalPayload)
		if msg == "" {
			msg = "Upstream compact response failed"
		}
		if compactErr := newOpenAICompactFallbackSignal(c, terminalPayload, msg); compactErr != nil {
			stop()
			return nil, compactErr
		}
		if failoverErr := s.nonStreamingTerminalFailureFailover(c, resp, account, false, terminalType, terminalPayload, msg, mappedModel); failoverErr != nil {
			stop()
			return nil, failoverErr
		}
		stop()
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg)
	}
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := s.parseSSEUsageFromBody(bodyText)
	if ok {
		if parsedUsage, parsed := extractOpenAIUsageFromJSONBytes(finalResponse); parsed {
			*usage = parsedUsage
		}
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		// gjson Array() returns empty slice for null, missing, or empty arrays.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		finalResponse = supplementCompactionItemFromSSE(c, finalResponse, bodyText)
		body = finalResponse
		if originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
		restoredBody, restoreErr := restoreGrokResponsesClientToolPayload(c, body)
		if restoreErr != nil {
			stop()
			return nil, fmt.Errorf("restore Grok Responses client tool response: %w", restoreErr)
		}
		restoredBody, restoreErr = restoreOpenAIResponsesClientToolPayload(c, restoredBody)
		if restoreErr != nil {
			stop()
			return nil, fmt.Errorf("restore OpenAI Responses client tool response: %w", restoreErr)
		}
		restoredBody, restoreErr = restoreOpenAIResponsesNamespacePayload(c, restoredBody)
		if restoreErr != nil {
			stop()
			return nil, fmt.Errorf("restore OpenAI namespace response: %w", restoreErr)
		}
		body = restoredBody
	} else {
		if originalModel != mappedModel {
			bodyText = s.replaceModelInSSEBody(bodyText, mappedModel, originalModel)
		}
		body = []byte(bodyText)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	logOpenAISuccessMissingUsage(c.Request.Context(), c, account, resp, usage, "sse_to_json", false)
	s.relayOpenAICodexTurnState(c, account, resp.Header)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	logOpenAIHTTP200SuspiciousUsageResponse(context.Background(), "openai_sse_to_json", resp, c, body, usage, true)
	stop()
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return usage, nil
}

func (s *OpenAIGatewayService) handleSSEToJSONResult(resp *http.Response, c *gin.Context, account *Account, body []byte, originalModel, mappedModel string, stopBeforeWrite ...func()) (*openaiNonStreamingResult, error) {
	imageCounter := newOpenAIImageOutputCounter()
	imageCounter.AddSSEBody(string(body))
	responseID := ""
	if finalResponse, ok := extractCodexFinalResponse(string(body)); ok {
		responseID = extractOpenAIResponseIDFromJSONBytes(finalResponse)
	}
	usage, err := s.handleSSEToJSON(resp, c, account, body, originalModel, mappedModel, stopBeforeWrite...)
	if err != nil {
		return nil, err
	}
	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       responseID,
		imageCount:       imageCounter.Count(),
		imageOutputSizes: imageCounter.Sizes(),
		searchCount:      countGrokNativeSearchCallsFromSSEBody(string(body)),
	}, nil
}

func extractOpenAISSETerminalEvent(body string) (string, []byte, bool) {
	var terminalType string
	var terminalPayload []byte
	forEachOpenAISSEFrame(body, func(eventType string, data []byte) {
		switch eventType {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
			terminalType = eventType
			terminalPayload = append([]byte(nil), data...)
		}
	})
	if terminalPayload != nil {
		return terminalType, terminalPayload, true
	}
	return "", nil, false
}

func extractOpenAISSEErrorMessage(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"response.error.message", "error.message", "message"} {
		if msg := strings.TrimSpace(gjson.GetBytes(payload, path).String()); msg != "" {
			return sanitizeUpstreamErrorMessage(msg)
		}
	}
	return sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(payload)))
}

func sanitizeOpenAIResponseFailedEventForClient(payload []byte, eventType string, clientOutputStarted bool) ([]byte, bool) {
	eventType = strings.TrimSpace(eventType)
	isFailedEvent := eventType == "response.failed"
	if (!isFailedEvent && eventType != "error") || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	if rewritten, changed := sanitizeOpenAICapacityShedErrorCodeForClient(updated); changed {
		updated = rewritten
	}
	if !isFailedEvent {
		return updated, !bytes.Equal(updated, payload)
	}
	if clientOutputStarted && isOpenAIContextWindowError(extractOpenAISSEErrorMessage(payload), payload) {
		errorPath := ""
		switch {
		case gjson.GetBytes(updated, "response.error").Exists():
			errorPath = "response.error"
		case gjson.GetBytes(updated, "error").Exists():
			errorPath = "error"
		}
		if errorPath != "" {
			next, err := sjson.SetBytes(updated, errorPath+".type", "invalid_request_error")
			if err != nil {
				return payload, false
			}
			updated = next
			next, err = sjson.SetBytes(updated, errorPath+".code", "context_length_exceeded")
			if err != nil {
				return payload, false
			}
			updated = next
		}
	}
	if !gjson.GetBytes(updated, "response").Exists() {
		return updated, !bytes.Equal(updated, payload)
	}
	for _, path := range []string{
		"response.instructions",
		"response.output",
		"response.usage",
		"response.metadata",
		"response.reasoning",
		"response.tools",
		"response.tool_choice",
		"response.parallel_tool_calls",
		"response.text",
		"response.truncation",
		"response.max_output_tokens",
		"response.incomplete_details",
	} {
		next, err := sjson.DeleteBytes(updated, path)
		if err != nil {
			return payload, false
		}
		updated = next
	}
	if containsOpenAICompatSensitiveBackendTerm("", updated) {
		next, err := sjson.SetBytes(updated, "response.error.message", openAICompatSensitiveBackendErrorMessage)
		if err != nil {
			return payload, false
		}
		updated = next
	}
	return updated, !bytes.Equal(updated, payload)
}

func (s *OpenAIGatewayService) writeOpenAINonStreamingProtocolError(resp *http.Response, c *gin.Context, message string) error {
	maskedSensitiveBackendError := containsOpenAICompatSensitiveBackendTerm(message, nil)
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "Upstream returned an invalid non-streaming response"
	}
	if maskedSensitiveBackendError {
		message = openAICompatSensitiveBackendErrorMessage
	}
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	// A body-signal compact heartbeat may already have committed HTTP 200. In
	// that state an HTTP JSON error would corrupt the SSE stream, so terminate
	// it with a protocol-valid response.failed event instead.
	if openAICompactClientWantsStream(c) && StopOpenAICompactSSEKeepaliveCommitted(c) {
		writeOpenAICompactSSEFailureMessage(c, http.StatusBadGateway, "upstream_error", message)
		return fmt.Errorf("non-streaming openai protocol error: %s", message)
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusBadGateway, gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	return fmt.Errorf("non-streaming openai protocol error: %s", message)
}

func extractCodexFinalResponse(body string) ([]byte, bool) {
	// codex round23 fu40: parser-aware. See extractOpenAISSETerminalEvent
	// for rationale — `event: response.completed\ndata: {...}` form
	// would otherwise leave `type` empty in the JSON payload and we'd
	// miss the final response.
	var parser openAICompatSSEFrameParser
	lines := strings.Split(body, "\n")
	check := func(payload string) ([]byte, bool) {
		if payload == "" || strings.TrimSpace(payload) == "[DONE]" {
			return nil, false
		}
		eventType := gjson.Get(payload, "type").String()
		if eventType == "response.done" || eventType == "response.completed" {
			if response := gjson.Get(payload, "response"); response.Exists() && response.Type == gjson.JSON && response.Raw != "" {
				return []byte(response.Raw), true
			}
		}
		return nil, false
	}
	for _, line := range lines {
		frame, hasFrame := parser.AddLine(line)
		if !hasFrame {
			continue
		}
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		if out, ok := check(payload); ok {
			return out, ok
		}
	}
	if frame, hasFrame := parser.Finish(); hasFrame {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		if out, ok := check(payload); ok {
			return out, ok
		}
	}
	return nil, false
}

// responsesStreamOutputItems retains authoritative output_item.done items by
// output_index so terminal frames cannot silently drop items reported earlier.
type responsesStreamOutputItems struct {
	items map[int]json.RawMessage
}

func newResponsesStreamOutputItems() *responsesStreamOutputItems {
	return &responsesStreamOutputItems{items: make(map[int]json.RawMessage)}
}

func (r *responsesStreamOutputItems) Observe(data []byte) {
	if r == nil || len(data) == 0 || !gjson.ValidBytes(data) {
		return
	}
	if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	index := int(gjson.GetBytes(data, "output_index").Int())
	r.items[index] = json.RawMessage(append([]byte(nil), item.Raw...))
}

func (r *responsesStreamOutputItems) HasItems() bool {
	return r != nil && len(r.items) > 0
}

func (r *responsesStreamOutputItems) Count() int {
	if r == nil {
		return 0
	}
	return len(r.items)
}

func (r *responsesStreamOutputItems) BuildOutput() ([]byte, bool) {
	if !r.HasItems() {
		return nil, false
	}
	indexes := make([]int, 0, len(r.items))
	for index := range r.items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	ordered := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		ordered = append(ordered, r.items[index])
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func normalizeResponsesStreamingTerminalOutput(data []byte, acc *apicompat.BufferedResponseAccumulator, doneItems *responsesStreamOutputItems, imageOutputs []json.RawMessage) ([]byte, bool) {
	eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.cancelled", "response.canceled":
	default:
		return data, false
	}

	output := gjson.GetBytes(data, "response.output")
	hasAccumulatedOutput := (acc != nil && acc.HasContent()) || len(imageOutputs) > 0 || doneItems.HasItems()
	if output.Exists() && output.IsArray() {
		terminalCount := len(output.Array())
		if terminalCount > 0 && terminalCount >= doneItems.Count() {
			return data, false
		}
		if terminalCount == 0 && !hasAccumulatedOutput {
			return data, false
		}
	}

	outputJSON := []byte("[]")
	if reconstructed, ok := doneItems.BuildOutput(); ok {
		outputJSON = reconstructed
	} else if reconstructed, ok := buildResponsesOutputJSON(acc, imageOutputs); ok {
		outputJSON = reconstructed
	}
	updated, err := sjson.SetRawBytes(data, "response.output", outputJSON)
	if err != nil {
		return data, false
	}
	return updated, true
}

func responsesStreamEventMayContributeToOutput(eventType string) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta":
		return true
	default:
		return false
	}
}

// collectRawResponsesOutputItemsFromSSE preserves authoritative
// output_item.done items byte-for-byte so compact-specific and future fields
// are not lost through a narrow response struct. If there are no done items,
// a complete compaction item from output_item.added is used as a fallback.
func collectRawResponsesOutputItemsFromSSE(bodyText string) ([]byte, bool) {
	var items []json.RawMessage
	seen := make(map[string]struct{})
	hasCompactionItem := false
	appendItem := func(item gjson.Result) {
		if !item.Exists() || !item.IsObject() {
			return
		}
		key := strings.TrimSpace(item.Get("id").String())
		if key == "" {
			key = item.Raw
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		if isResponsesCompactionItemType(item.Get("type").String()) {
			hasCompactionItem = true
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
		if strings.TrimSpace(gjson.GetBytes(data, "type").String()) == "response.output_item.done" {
			appendItem(gjson.GetBytes(data, "item"))
		}
	})
	// Some relays emit done for message items but only added for the compact
	// item. Inspect added whenever no compaction item was found in done.
	if !hasCompactionItem {
		forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
			if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "response.output_item.added" {
				return
			}
			item := gjson.GetBytes(data, "item")
			if isResponsesCompactionItemType(item.Get("type").String()) {
				appendItem(item)
			}
		})
	}
	if len(items) == 0 {
		return nil, false
	}
	outputJSON, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	return outputJSON, true
}

func isResponsesCompactionItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

// supplementCompactionItemFromSSE restores a raw compaction item when a
// compact terminal response has other output but omits that item.
func supplementCompactionItemFromSSE(c *gin.Context, finalResponse []byte, bodyText string) []byte {
	if !isOpenAIResponsesCompactPath(c) || len(gjson.GetBytes(finalResponse, "output").Array()) == 0 || responsesOutputHasCompactionItem(finalResponse) {
		return finalResponse
	}
	item, found := findRawCompactionItemFromSSE(bodyText)
	if !found {
		return finalResponse
	}
	patched, err := sjson.SetRawBytes(finalResponse, "output.-1", item)
	if err != nil {
		return finalResponse
	}
	return patched
}

func responsesOutputHasCompactionItem(response []byte) bool {
	for _, item := range gjson.GetBytes(response, "output").Array() {
		if isResponsesCompactionItemType(item.Get("type").String()) {
			return true
		}
	}
	return false
}

func findRawCompactionItemFromSSE(bodyText string) (json.RawMessage, bool) {
	var found json.RawMessage
	pick := func(eventType string) {
		forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
			if found != nil || strings.TrimSpace(gjson.GetBytes(data, "type").String()) != eventType {
				return
			}
			item := gjson.GetBytes(data, "item")
			if item.IsObject() && isResponsesCompactionItemType(item.Get("type").String()) {
				found = json.RawMessage(item.Raw)
			}
		})
	}
	pick("response.output_item.done")
	if found == nil {
		pick("response.output_item.added")
	}
	return found, found != nil
}

// reconstructResponseOutputFromSSE prefers raw terminal items and falls back
// to delta accumulation only when the upstream emitted no complete items.
func reconstructResponseOutputFromSSE(bodyText string) ([]byte, bool) {
	if outputJSON, ok := collectRawResponsesOutputItemsFromSSE(bodyText); ok {
		return outputJSON, true
	}
	acc := apicompat.NewBufferedResponseAccumulator()
	imageOutputs := make([]json.RawMessage, 0, 1)
	seenImages := make(map[string]struct{})
	// codex round23 fu40: parser-aware. Accumulator's per-event delta
	// recording needs the patched payload so events with type only in
	// event: line still feed acc.ProcessEvent.
	var parser openAICompatSSEFrameParser
	lines := strings.Split(bodyText, "\n")
	process := func(frame openAICompatSSEFrame) {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		if payload == "" || strings.TrimSpace(payload) == "[DONE]" {
			return
		}
		if imageOutput, ok := extractImageGenerationOutputFromSSEData([]byte(payload), seenImages); ok {
			imageOutputs = append(imageOutputs, imageOutput)
		}
		eventType := strings.TrimSpace(gjson.Get(payload, "type").String())
		if !responsesStreamEventMayContributeToOutput(eventType) {
			return
		}
		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return
		}
		acc.ProcessEvent(&event)
	}
	for _, line := range lines {
		frame, hasFrame := parser.AddLine(line)
		if !hasFrame {
			continue
		}
		process(frame)
	}
	if frame, hasFrame := parser.Finish(); hasFrame {
		process(frame)
	}
	if outputJSON, ok := buildResponsesOutputJSON(acc, imageOutputs); ok {
		return outputJSON, true
	}

	// Be liberal with relay-produced SSE that omits blank frame separators.
	// The parser above correctly preserves an `event:`-only type, while this
	// fallback can still recover independent consecutive data lines.
	forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
		if imageOutput, ok := extractImageGenerationOutputFromSSEData(data, seenImages); ok {
			imageOutputs = append(imageOutputs, imageOutput)
		}
		eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
		if !responsesStreamEventMayContributeToOutput(eventType) {
			return
		}
		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal(data, &event); err == nil {
			acc.ProcessEvent(&event)
		}
	})
	return buildResponsesOutputJSON(acc, imageOutputs)
}

func buildResponsesOutputJSON(acc *apicompat.BufferedResponseAccumulator, imageOutputs []json.RawMessage) ([]byte, bool) {
	if (acc == nil || !acc.HasContent()) && len(imageOutputs) == 0 {
		return nil, false
	}
	var output []json.RawMessage
	if acc != nil && acc.HasContent() {
		outputJSON, err := json.Marshal(acc.BuildOutput())
		if err == nil {
			_ = json.Unmarshal(outputJSON, &output)
		}
	}
	output = append(output, imageOutputs...)
	if len(output) == 0 {
		return nil, false
	}

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return nil, false
	}
	return outputJSON, true
}

func extractImageGenerationOutputFromSSEData(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("output_format").String()) + "|" + strings.TrimSpace(item.Get("result").String())
	}
	if key != "" && seen != nil {
		if _, exists := seen[key]; exists {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	return json.RawMessage(item.Raw), true
}

func (s *OpenAIGatewayService) parseSSEUsageFromBody(body string) *OpenAIUsage {
	usage := &OpenAIUsage{}
	// codex round23 fu40: parser-aware. parseSSEUsageBytes reads
	// `usage` and `response.usage` paths; both are usually carried by
	// the terminal response.completed frame, so missing the
	// event-named form means missing usage entirely.
	var parser openAICompatSSEFrameParser
	lines := strings.Split(body, "\n")
	consume := func(frame openAICompatSSEFrame) {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		if payload == "" || strings.TrimSpace(payload) == "[DONE]" {
			return
		}
		s.parseSSEUsageBytes([]byte(payload), usage)
	}
	for _, line := range lines {
		frame, hasFrame := parser.AddLine(line)
		if !hasFrame {
			continue
		}
		consume(frame)
	}
	if frame, hasFrame := parser.Finish(); hasFrame {
		consume(frame)
	}
	return usage
}

func (s *OpenAIGatewayService) replaceModelInSSEBody(body, fromModel, toModel string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if _, ok := extractOpenAISSEDataLine(line); !ok {
			continue
		}
		lines[i] = s.replaceModelInSSELine(line, fromModel, toModel)
	}
	return strings.Join(lines, "\n")
}

func (s *OpenAIGatewayService) validateUpstreamBaseURL(raw string) (string, error) {
	if s.cfg != nil && !s.cfg.Security.URLAllowlist.Enabled {
		normalized, err := urlvalidator.ValidateURLFormat(raw, s.cfg.Security.URLAllowlist.AllowInsecureHTTP)
		if err != nil {
			return "", fmt.Errorf("invalid base_url: %w", err)
		}
		return normalized, nil
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     s.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     s.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	return normalized, nil
}

// buildOpenAIResponsesURL 组装 OpenAI Responses 端点。
// - base 以 /v1 结尾：追加 /responses
// - base 以其他版本段结尾（如 /v4）：追加 /responses
// - base 已是 /responses：原样返回
// - 其他情况：追加 /v1/responses
func buildOpenAIResponsesURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/responses")
}

// buildOpenAIResponsesURLForPlatform keeps DeepSeek's Codex-compatible
// Responses endpoint at /responses while other providers use /v1/responses.
func buildOpenAIResponsesURLForPlatform(platform string, base string) string {
	if platform == PlatformDeepseek {
		return buildOpenAIEndpointURL(base, "/responses")
	}
	return buildOpenAIResponsesURL(base)
}

// normalizeDeepSeekResponsesRequestBody adapts DeepSeek's stateless Responses
// endpoint, which rejects server-side state and previous response references.
func normalizeDeepSeekResponsesRequestBody(account *Account, body []byte) []byte {
	if account == nil || !account.UsesNativeCNResponses() {
		return body
	}
	normalized, err := sjson.SetBytes(body, "store", false)
	if err != nil {
		return body
	}
	if stripped, err := sjson.DeleteBytes(normalized, "previous_response_id"); err == nil {
		normalized = stripped
	}
	return normalized
}

func trimOpenAIEncryptedReasoningItems(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}

	inputValue, has := reqBody["input"]
	if !has {
		return false
	}

	switch input := inputValue.(type) {
	case []any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			filtered = append(filtered, nextItem)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case []map[string]any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			nextMap, ok := nextItem.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			filtered = append(filtered, nextMap)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case map[string]any:
		nextItem, changed, keep := sanitizeEncryptedReasoningInputItem(input)
		if !changed {
			return false
		}
		if !keep {
			delete(reqBody, "input")
			return true
		}
		nextMap, ok := nextItem.(map[string]any)
		if !ok {
			return false
		}
		reqBody["input"] = nextMap
		return true
	default:
		return false
	}
}

func sanitizeEncryptedReasoningInputItem(item any) (next any, changed bool, keep bool) {
	inputItem, ok := item.(map[string]any)
	if !ok {
		return item, false, true
	}

	itemType, _ := inputItem["type"].(string)
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		if _, encrypted := inputItem["encrypted_content"]; encrypted {
			return nil, true, false
		}
		return item, false, true
	case "reasoning":
	default:
		return item, false, true
	}

	_, hasEncryptedContent := inputItem["encrypted_content"]
	if !hasEncryptedContent {
		return item, false, true
	}

	delete(inputItem, "encrypted_content")
	if len(inputItem) == 1 {
		return nil, true, false
	}
	return inputItem, true, true
}

// SanitizeOpenAICrossModeFailoverReasoning derives a non-passthrough failover
// body from the immutable canonical request by removing provider-specific
// encrypted reasoning input items in full, including their coupled ID/summary.
func SanitizeOpenAICrossModeFailoverReasoning(body []byte) (sanitized []byte, changed bool, err error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !gjson.GetBytes(body, "input").Exists() {
		return body, false, nil
	}
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return body, false, fmt.Errorf("decode cross-mode failover body: %w", err)
	}
	if !dropOpenAIEncryptedReasoningInputItems(decoded) {
		return body, false, nil
	}
	out, marshalErr := marshalOpenAIUpstreamJSON(decoded)
	if marshalErr != nil {
		return body, false, fmt.Errorf("serialize cross-mode failover body: %w", marshalErr)
	}
	return out, true, nil
}

func dropOpenAIEncryptedReasoningInputItems(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}
	inputValue, has := reqBody["input"]
	if !has {
		return false
	}
	switch input := inputValue.(type) {
	case []any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			if isOpenAIEncryptedReasoningInputItem(item) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case []map[string]any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			if isOpenAIEncryptedReasoningInputItem(item) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case map[string]any:
		if isOpenAIEncryptedReasoningInputItem(input) {
			delete(reqBody, "input")
			return true
		}
		return false
	default:
		return false
	}
}

func isOpenAIEncryptedReasoningInputItem(item any) bool {
	inputItem, ok := item.(map[string]any)
	if !ok {
		return false
	}
	if itemType, _ := inputItem["type"].(string); strings.TrimSpace(itemType) != "reasoning" {
		return false
	}
	_, has := inputItem["encrypted_content"]
	return has
}

func IsOpenAIResponsesCompactPathForTest(c *gin.Context) bool {
	return isOpenAIResponsesCompactPath(c)
}

// IsOpenAIResponsesCompactPath reports whether the request targets the legacy
// /responses/compact endpoint, including its forwardable subpaths.
func IsOpenAIResponsesCompactPath(c *gin.Context) bool {
	return isOpenAIResponsesCompactPath(c)
}

func isCodexCompactRequest(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if IsImageGenerationIntent(openAIResponsesEndpoint, "", body) {
		return false
	}

	fields := make([]string, 0, 4)
	for _, path := range []string{"instructions", "input", "messages"} {
		if value := gjson.GetBytes(body, path); value.Exists() {
			fields = append(fields, strings.ToLower(value.Raw))
		}
	}
	haystack := strings.Join(fields, "\n")
	if haystack == "" {
		return false
	}

	strongCompactSignature := false
	for _, phrase := range []string{
		"remote compact task",
		"remote compact request",
		"compact task",
		"compact request",
	} {
		if strings.Contains(haystack, phrase) {
			strongCompactSignature = true
			break
		}
	}
	if !strongCompactSignature {
		return false
	}

	for _, phrase := range []string{
		"context compaction",
		"compact the context",
		"compacting the context",
		"conversation summary",
		"summarize conversation",
		"summarize the conversation",
	} {
		if strings.Contains(haystack, phrase) {
			return true
		}
	}

	return false
}

func shouldEnableCodexImageGenerationBridge(c *gin.Context, body []byte, reqModel string, _ *Account, _ *APIKey) bool {
	if (isOpenAIResponsesCompactPath(c) || isCodexCompactRequest(body)) &&
		!IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body) {
		return false
	}
	return true
}

func OpenAICompactSessionSeedKeyForTest() string {
	return openAICompactSessionSeedKey
}

func NormalizeOpenAICompactRequestBodyForTest(body []byte) ([]byte, bool, error) {
	return normalizeOpenAICompactRequestBody(body)
}

func isOpenAIResponsesCompactPath(c *gin.Context) bool {
	suffix := strings.TrimSpace(openAIResponsesRequestPathSuffix(c))
	return suffix == "/compact" || strings.HasPrefix(suffix, "/compact/")
}

func normalizeOpenAICompactRequestBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	normalized := []byte(`{}`)
	// Keep the current Codex /compact schema while still dropping request-scoped
	// fields such as prompt_cache_key, store, and stream.
	for _, field := range []string{
		"model",
		"input",
		"instructions",
		"tools",
		"parallel_tool_calls",
		"reasoning",
		"service_tier",
		"text",
		"previous_response_id",
	} {
		value := gjson.GetBytes(body, field)
		if !value.Exists() {
			continue
		}
		next, err := sjson.SetRawBytes(normalized, field, []byte(value.Raw))
		if err != nil {
			return body, false, fmt.Errorf("normalize compact body %s: %w", field, err)
		}
		normalized = next
	}
	if next, removed, err := normalizeOpenAIParallelToolCallsWithoutTools(normalized); err != nil {
		return body, false, err
	} else if removed {
		normalized = next
	}

	if bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(normalized)) {
		return body, false, nil
	}
	return normalized, true, nil
}

func resolveOpenAICompactSessionID(c *gin.Context, body []byte) string {
	if sessionID := extractOpenAIStickySessionSignal(c, body); sessionID != "" {
		return sessionID
	}
	if c != nil {
		if seed, ok := c.Get(openAICompactSessionSeedKey); ok {
			if seedStr, ok := seed.(string); ok && strings.TrimSpace(seedStr) != "" {
				return strings.TrimSpace(seedStr)
			}
		}
	}
	return uuid.NewString()
}

func openAIResponsesRequestPathSuffix(c *gin.Context) string {
	rawSuffix, recognized := rawOpenAIResponsesRequestPathSuffix(c)
	if !recognized {
		return ""
	}
	suffix, ok := sanitizedUpstreamPathSuffix(rawSuffix)
	if !ok {
		return ""
	}
	return suffix
}

// IsForwardableOpenAIResponsesRequestPath lets the route reject malformed
// suffixes before account scheduling or upstream forwarding.
func IsForwardableOpenAIResponsesRequestPath(c *gin.Context) bool {
	rawSuffix, recognized := rawOpenAIResponsesRequestPathSuffix(c)
	if !recognized {
		return false
	}
	_, ok := sanitizedUpstreamPathSuffix(rawSuffix)
	return ok
}

// IsOpenAIResponsesInputTokensRequestPath reports whether the request targets
// the native Responses input-token counting endpoint.
func IsOpenAIResponsesInputTokensRequestPath(c *gin.Context) bool {
	return openAIResponsesRequestPathSuffix(c) == "/input_tokens"
}

func rawOpenAIResponsesRequestPathSuffix(c *gin.Context) (string, bool) {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "", false
	}
	requestPath := c.Request.URL.Path
	if requestPath != strings.TrimSpace(requestPath) || strings.HasSuffix(requestPath, "//") {
		return "", false
	}
	normalizedPath := strings.TrimSuffix(requestPath, "/")
	for _, marker := range []string{
		"/backend-api/codex/responses",
		"/v1/responses",
		"/responses",
	} {
		searchFrom := 0
		for searchFrom < len(normalizedPath) {
			relativeIndex := strings.Index(normalizedPath[searchFrom:], marker)
			if relativeIndex < 0 {
				break
			}
			idx := searchFrom + relativeIndex
			suffixStart := idx + len(marker)
			if suffixStart == len(normalizedPath) {
				return "", true
			}
			if normalizedPath[suffixStart] == '/' {
				return normalizedPath[suffixStart:], true
			}
			searchFrom = idx + 1
		}
	}
	return "", false
}

func appendOpenAIResponsesRequestPathSuffix(baseURL, suffix string) string {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	trimmedSuffix, ok := sanitizedUpstreamPathSuffix(suffix)
	if !ok || trimmedBase == "" || trimmedSuffix == "" {
		return trimmedBase
	}
	return trimmedBase + trimmedSuffix
}

func (s *OpenAIGatewayService) replaceModelInResponseBody(body []byte, fromModel, toModel string) []byte {
	// 使用 gjson/sjson 精确替换 model 字段，避免全量 JSON 反序列化
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Str == fromModel {
		newBody, err := sjson.SetBytes(body, "model", toModel)
		if err != nil {
			return body
		}
		return newBody
	}
	return body
}

// OpenAIRecordUsageInput input for recording usage
type OpenAIRecordUsageInput struct {
	Result             *OpenAIForwardResult
	APIKey             *APIKey
	User               *User
	Account            *Account
	Subscription       *UserSubscription
	InboundEndpoint    string
	UpstreamEndpoint   string
	UserAgent          string // 请求的 User-Agent
	IPAddress          string // 请求的客户端 IP 地址
	SessionID          string // 仅来自校验后的显式入站会话请求头；只用于用量行关联
	RequestPayloadHash string
	APIKeyService      APIKeyQuotaUpdater
	QuotaPlatform      string // user×platform quota platform resolved by the handler before async billing.
	// PricingAt is captured at request admission so peak pricing and profit
	// control use one stable instant for the whole request. Zero falls back to
	// the legacy record-time behavior.
	PricingAt    time.Time
	CyberBlocked bool // 标记 cyber_policy 命中，仅影响 usage request_type，不改变计费。
	// NativeCompactionV2 is orthogonal to the transport request type and is
	// persisted only after the handler positively identifies a native compact
	// request.
	NativeCompactionV2 bool
	ChannelUsageFields
}

// CyberPolicyUsageInput 是 cyber 拒绝、未走正常 RecordUsage 的请求记录用量的入参。
// 用量按上游真实 token 计费，与 WS cyber 及正常请求口径一致（InputTokens/OutputTokens
// 取自上游 response.failed 报告的 usage，即 mark.UpstreamInTok/OutTok）。
type CyberPolicyUsageInput struct {
	APIKey       *APIKey
	Account      *Account
	Subscription *UserSubscription
	RequestID    string
	Model        string
	Stream       bool
	InputTokens  int
	OutputTokens int
	// 渠道归因与请求级 meta，使 cyber 计费行与正常 RecordUsage 行口径一致
	// （否则 cyber 行 channel_id 等为空，渠道维度统计会遗漏 cyber 命中）。
	InboundEndpoint    string
	UpstreamEndpoint   string
	UserAgent          string
	IPAddress          string
	SessionID          string
	RequestPayloadHash string
	APIKeyService      APIKeyQuotaUpdater
	NativeCompactionV2 bool
	ChannelUsageFields
}

// ResolveUserGroupRateMultiplier resolves the same cached multiplier used by OpenAI usage billing.
func (s *OpenAIGatewayService) ResolveUserGroupRateMultiplier(ctx context.Context, userID, groupID int64, groupDefaultMultiplier float64) float64 {
	if s == nil {
		return groupDefaultMultiplier
	}
	resolver := s.userGroupRateResolver
	if resolver == nil {
		resolver = newUserGroupRateResolver(nil, nil, resolveUserGroupRateCacheTTL(s.cfg), nil, "service.openai_gateway")
	}
	return resolver.Resolve(ctx, userID, groupID, groupDefaultMultiplier)
}

func openAIUsagePricingAt(input *OpenAIRecordUsageInput) time.Time {
	if input != nil && !input.PricingAt.IsZero() {
		return input.PricingAt
	}
	return timezone.Now()
}

// RecordCyberPolicyUsageLog 为被上游 cyber_policy 拒绝、未走正常 RecordUsage 的请求
// （HTTP forward 返回错误路径）记录用量并按上游真实 token 计费，使其与 WS cyber 路径、
// 与正常请求的计费口径统一（不再是 tokens=0 免费行）。token 取自上游 response.failed
// 报告的 usage（非流式直接拒通常为 0，cost 随之为 0）。复用 RecordUsage 完成成本计算、
// 扣费与用量行写入（request_type=cyber 由 CyberBlocked 置位）。仅 forward 返回错误的
// 路径由 handler 调用，避免与成功路径的正常 RecordUsage 重复。
func (s *OpenAIGatewayService) RecordCyberPolicyUsageLog(ctx context.Context, in CyberPolicyUsageInput) {
	if s == nil || in.APIKey == nil || in.APIKey.User == nil || in.Account == nil || strings.TrimSpace(in.Model) == "" {
		return
	}
	result := &OpenAIForwardResult{
		RequestID: in.RequestID,
		Model:     in.Model,
		Stream:    in.Stream,
		Usage: OpenAIUsage{
			InputTokens:  in.InputTokens,
			OutputTokens: in.OutputTokens,
		},
	}
	if err := s.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result:             result,
		APIKey:             in.APIKey,
		User:               in.APIKey.User,
		Account:            in.Account,
		Subscription:       in.Subscription,
		InboundEndpoint:    in.InboundEndpoint,
		UpstreamEndpoint:   in.UpstreamEndpoint,
		UserAgent:          in.UserAgent,
		IPAddress:          in.IPAddress,
		SessionID:          in.SessionID,
		RequestPayloadHash: in.RequestPayloadHash,
		APIKeyService:      in.APIKeyService,
		ChannelUsageFields: in.ChannelUsageFields,
		CyberBlocked:       true,
		NativeCompactionV2: in.NativeCompactionV2,
	}); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber usage record failed: request_id=%s err=%v", in.RequestID, err)
	}
}

func groupSupportsOpenAIFast(platform string) bool {
	return platform == PlatformOpenAI || platform == PlatformComposite
}

func groupBillsOpenAIFastAtStandard(apiKey *APIKey, account *Account, serviceTier string) bool {
	if apiKey == nil || apiKey.Group == nil || !apiKey.Group.FreeOpenAIFast {
		return false
	}
	if account == nil || !account.IsOpenAI() || !groupSupportsOpenAIFast(apiKey.Group.Platform) {
		return false
	}
	switch normalizeBillingServiceTier(serviceTier) {
	case "priority", "fast":
		return true
	default:
		return false
	}
}

// RecordUsage records usage and deducts balance
func (s *OpenAIGatewayService) RecordUsage(ctx context.Context, input *OpenAIRecordUsageInput) error {
	if input == nil {
		return errors.New("openai usage input is nil")
	}
	result := input.Result
	if result == nil {
		return errors.New("openai usage result is nil")
	}
	if s.rateLimitService != nil && input.Account != nil && input.Account.Platform == PlatformOpenAI {
		s.rateLimitService.ResetOpenAI403Counter(ctx, input.Account.ID)
	}

	apiKey := input.APIKey
	user := input.User
	account := input.Account
	subscription := input.Subscription
	if !isGrokVideoUsageResult(result, nil) {
		ApplyOpenAIImageBillingResolution(result)
	}

	// OpenAI input_tokens 是总输入，包含缓存读取和缓存写入明细。
	// 将三类 token 拆成互斥桶，避免缓存写入同时按普通输入和 cache_write 重复计费。
	actualInputTokens := result.Usage.InputTokens - result.Usage.CacheReadInputTokens - result.Usage.CacheCreationInputTokens
	if actualInputTokens < 0 {
		actualInputTokens = 0
	}
	actualImageInputTokens := result.Usage.ImageInputTokens
	if actualImageInputTokens < 0 {
		actualImageInputTokens = 0
	}
	if actualImageInputTokens > actualInputTokens {
		actualImageInputTokens = actualInputTokens
	}

	// Calculate cost
	tokens := UsageTokens{
		InputTokens:         actualInputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		ImageInputTokens:    actualImageInputTokens,
		ImageOutputTokens:   result.Usage.ImageOutputTokens,
	}

	// Get rate multiplier
	multiplier := 1.0
	if s.cfg != nil {
		multiplier = s.cfg.Default.RateMultiplier
	}
	if apiKey.GroupID != nil && apiKey.Group != nil {
		resolver := s.userGroupRateResolver
		if resolver == nil {
			resolver = newUserGroupRateResolver(nil, nil, resolveUserGroupRateCacheTTL(s.cfg), nil, "service.openai_gateway")
		}
		multiplier = resolver.Resolve(ctx, user.ID, *apiKey.GroupID, apiKey.Group.RateMultiplier)
	}
	// token 倍率叠加高峰因子（token 计费含图片 token，图片按次倍率不受影响）。高峰因子按请求时刻现算，
	// 不并入上面的 Resolve，以免污染 user:group 倍率缓存。
	baseMultiplier := multiplier
	pricingAt := openAIUsagePricingAt(input)
	multiplier, imageMultiplier := computePeakAwareMultipliers(apiKey, baseMultiplier, pricingAt)
	videoMultiplier := resolveVideoRateMultiplier(apiKey, baseMultiplier)

	var cost *CostBreakdown
	var err error
	billingModel := forwardResultBillingModel(result.Model, result.UpstreamModel)
	if result.BillingModel != "" {
		billingModel = strings.TrimSpace(result.BillingModel)
	}
	if input.BillingModelSource == BillingModelSourceChannelMapped && input.ChannelMappedModel != "" && input.ChannelMappedModel != input.OriginalModel {
		billingModel = input.ChannelMappedModel
	}
	if input.BillingModelSource == BillingModelSourceRequested && input.OriginalModel != "" {
		billingModel = input.OriginalModel
	}
	billingModels := usageBillingModelCandidates(
		billingModel,
		result.BillingModel,
		input.ChannelMappedModel,
		input.OriginalModel,
		result.UpstreamModel,
		result.Model,
	)
	billingModels = s.filterCNProviderBillingModelCandidates(ctx, account, apiKey, billingModels)
	billingAccount := account
	if account != nil && account.IsShadow() {
		billingAccount, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return err
		}
	}
	logServiceTierBillingDowngrade("service.openai_gateway", account, result.RequestID, ApplyOpenAIServiceTierBillingResolution(billingAccount, result))
	serviceTier := ""
	if result.ServiceTier != nil {
		serviceTier = strings.TrimSpace(*result.ServiceTier)
	}
	longContextBillingGate := openAILongContextBillingGate(billingAccount)
	cost, err = s.calculateOpenAIRecordUsageCost(
		ctx,
		result,
		apiKey,
		billingModels,
		multiplier,
		imageMultiplier,
		videoMultiplier,
		baseMultiplier,
		tokens,
		serviceTier,
		longContextBillingGate,
		pricingAt,
	)
	if responseModel := responseModelBillingDeclaration(
		input.BillingModelSource,
		result.UpstreamResponseModel,
		result.UpstreamResponseModelConflict,
		result.ImageCount > 0 || result.VideoCount > 0 || result.WebSearchCalls > 0 || result.AudioUsage != nil || result.SearchCount > 0,
	); err == nil && responseModel != "" && !strings.EqualFold(responseModel, strings.TrimSpace(billingModel)) {
		if identified, responseChannelPriced := s.hasIdentifiedOpenAIResponsePricing(ctx, responseModel, apiKey); identified {
			responseModels := s.filterCNProviderBillingModelCandidates(ctx, account, apiKey, usageBillingModelCandidates(responseModel))
			responseCost, responseErr := s.calculateOpenAIRecordUsageCost(
				ctx,
				result,
				apiKey,
				responseModels,
				multiplier,
				imageMultiplier,
				videoMultiplier,
				baseMultiplier,
				tokens,
				serviceTier,
				longContextBillingGate,
				pricingAt,
			)
			baselineChannelPriced := s.resolveOpenAIChannelPricing(ctx, firstUsageBillingModel(billingModels), apiKey) != nil
			if responseErr == nil && responseModelBillingAdoptable(cost, responseCost, baselineChannelPriced, responseChannelPriced) {
				logResponseModelBillingApplied("service.openai_gateway", account, result.RequestID, billingModel, responseModel, cost, responseCost)
				billingModels = responseModels
				cost = responseCost
			}
		}
	}
	if err != nil {
		if !isUsagePricingUnavailableError(err) {
			return err
		}
		logger.L().With(
			zap.String("component", "service.openai_gateway"),
			zap.Strings("billing_models", billingModels),
			zap.String("requested_model", input.OriginalModel),
			zap.String("mapped_model", input.ChannelMappedModel),
			zap.String("upstream_model", result.UpstreamModel),
			zap.Int64("api_key_id", apiKey.ID),
			zap.Int64("account_id", account.ID),
		).Warn("openai_usage.pricing_missing_record_zero_cost", zap.Error(err))
		cost = &CostBreakdown{BillingMode: string(BillingModeToken)}
	} else if cost == nil {
		cost = &CostBreakdown{ActualCost: 0, BillingMode: string(BillingModeToken)}
	}

	// Free Fast keeps upstream priority cost attribution while charging the
	// customer at the same request's standard-tier price.
	if groupBillsOpenAIFastAtStandard(apiKey, billingAccount, serviceTier) {
		standardCost, standardErr := s.calculateOpenAIRecordUsageCost(
			ctx,
			result,
			apiKey,
			billingModels,
			multiplier,
			imageMultiplier,
			videoMultiplier,
			baseMultiplier,
			tokens,
			"",
			longContextBillingGate,
			pricingAt,
		)
		if standardErr != nil {
			return standardErr
		}
		if cost != nil && standardCost != nil {
			cost.ActualCost = standardCost.ActualCost
		}
	}

	// Determine billing type
	isSubscriptionBilling := subscription != nil && apiKey.Group != nil && apiKey.Group.IsSubscriptionType()
	billingType := BillingTypeBalance
	if isSubscriptionBilling {
		billingType = BillingTypeSubscription
	}

	// Create usage log
	durationMs := int(result.Duration.Milliseconds())
	accountRateMultiplier := account.BillingRateMultiplier()
	requestID := resolveUsageBillingRequestID(ctx, result.RequestID)
	if result.OpenAIWSMode {
		if upstreamRequestID := strings.TrimSpace(result.RequestID); upstreamRequestID != "" {
			requestID = upstreamRequestID
		}
	}
	// Async Grok video status/content polls must converge on one durable billing
	// key even if a transient Redis claim is lost.
	if result.VideoCount > 0 {
		if stable := StableGrokVideoBillingRequestID(firstNonEmpty(
			strings.TrimPrefix(strings.TrimSpace(result.RequestID), "grok-video:"),
			strings.TrimSpace(result.ResponseID),
			strings.TrimPrefix(strings.TrimSpace(requestID), "grok-video:"),
		)); stable != "" {
			requestID = stable
		}
	}

	// 确定 RequestedModel（渠道映射前的原始模型）
	requestedModel := result.Model
	if input.OriginalModel != "" {
		requestedModel = input.OriginalModel
	}
	sentModel := upstreamSentModel(result.Model, result.UpstreamModel)
	if result.UpstreamResponseModelConflict {
		logger.L().Warn("upstream_response_model_conflict",
			zap.String("platform", account.Platform),
			zap.Int64("account_id", account.ID),
			zap.String("request_id", requestID),
			zap.String("sent_model", sentModel),
			zap.String("selected_response_model", strings.TrimSpace(result.UpstreamResponseModel)),
		)
	}

	usageLog := &UsageLog{
		UserID:                   user.ID,
		APIKeyID:                 apiKey.ID,
		AccountID:                account.ID,
		RequestID:                requestID,
		Model:                    result.Model,
		RequestedModel:           requestedModel,
		UpstreamModel:            optionalTrimmedStringPtr(result.UpstreamModel),
		UpstreamResponseModel:    optionalTrimmedStringPtr(result.UpstreamResponseModel),
		UpstreamModelMismatch:    upstreamModelMismatch(sentModel, result.UpstreamResponseModel),
		ServiceTier:              result.ServiceTier,
		ReasoningEffort:          result.ReasoningEffort,
		RequestedReasoningEffort: coalesceRequestedReasoningEffort(result.RequestedReasoningEffort, result.ReasoningEffort),
		InboundEndpoint:          optionalTrimmedStringPtr(input.InboundEndpoint),
		UpstreamEndpoint:         optionalTrimmedStringPtr(input.UpstreamEndpoint),
		InputTokens:              actualInputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheCreationTokens:      result.Usage.CacheCreationInputTokens,
		CacheReadTokens:          result.Usage.CacheReadInputTokens,
		ImageInputTokens:         actualImageInputTokens,
		ImageOutputTokens:        result.Usage.ImageOutputTokens,
		ImageCount:               result.ImageCount,
		ImageSize:                optionalTrimmedStringPtr(result.ImageSize),
		ImageInputSize:           optionalTrimmedStringPtr(result.ImageInputSize),
		ImageOutputSize:          optionalTrimmedStringPtr(result.ImageOutputSize),
		ImageSizeSource:          optionalTrimmedStringPtr(result.ImageSizeSource),
		ImageSizeBreakdown:       result.ImageSizeBreakdown,
		NativeCompactionV2:       input.NativeCompactionV2,
	}
	isVideoUsage := isGrokVideoUsageResult(result, billingModels)
	if isVideoUsage {
		usageLog.VideoCount = result.VideoCount
		usageLog.VideoResolution = optionalTrimmedStringPtr(NormalizeVideoBillingResolutionOrDefault(result.VideoResolution))
		durationSeconds := NormalizeVideoBillingDurationSecondsOrDefault(result.VideoDurationSeconds)
		usageLog.VideoDurationSeconds = &durationSeconds
	}
	if cost != nil {
		usageLog.InputCost = cost.InputCost
		usageLog.ImageInputCost = cost.ImageInputCost
		usageLog.OutputCost = cost.OutputCost
		usageLog.ImageOutputCost = cost.ImageOutputCost
		usageLog.CacheCreationCost = cost.CacheCreationCost
		usageLog.CacheReadCost = cost.CacheReadCost
		usageLog.TotalCost = cost.TotalCost
		usageLog.ActualCost = cost.ActualCost
	}
	usageLog.RateMultiplier = multiplier
	if isVideoUsage && cost != nil && cost.BillingMode == string(BillingModeVideo) {
		usageLog.RateMultiplier = videoMultiplier
	} else if result.ImageCount > 0 && cost != nil && cost.BillingMode == string(BillingModeImage) {
		usageLog.RateMultiplier = imageMultiplier
	}
	usageLog.AccountRateMultiplier = &accountRateMultiplier
	usageLog.BillingType = billingType
	usageLog.Stream = result.Stream
	if input.CyberBlocked {
		usageLog.RequestType = RequestTypeCyberBlocked
	}
	usageLog.OpenAIWSMode = result.OpenAIWSMode
	usageLog.DurationMs = &durationMs
	usageLog.FirstTokenMs = result.FirstTokenMs
	usageLog.CreatedAt = time.Now()
	// 设置渠道信息
	usageLog.ChannelID = optionalInt64Ptr(input.ChannelID)
	usageLog.ModelMappingChain = optionalTrimmedStringPtr(input.ModelMappingChain)
	// 设置计费模式
	if cost != nil && cost.BillingMode != "" {
		billingMode := cost.BillingMode
		usageLog.BillingMode = &billingMode
	} else if isVideoUsage {
		billingMode := string(BillingModeVideo)
		usageLog.BillingMode = &billingMode
	} else if result.ImageCount > 0 {
		billingMode := string(BillingModeImage)
		usageLog.BillingMode = &billingMode
	} else {
		billingMode := string(BillingModeToken)
		usageLog.BillingMode = &billingMode
	}
	usageLog.LongContextBillingApplied = cost != nil && cost.LongContextBillingApplied
	// 添加 UserAgent
	if input.UserAgent != "" {
		usageLog.UserAgent = &input.UserAgent
	}

	// 添加 IPAddress
	if input.IPAddress != "" {
		usageLog.IPAddress = &input.IPAddress
	}
	usageLog.SessionID = optionalTrimmedStringPtr(input.SessionID)

	if apiKey.GroupID != nil {
		usageLog.GroupID = apiKey.GroupID
	}
	if subscription != nil {
		usageLog.SubscriptionID = &subscription.ID
	}

	// 计算账号统计定价费用（使用最终上游模型匹配自定义规则）
	if apiKey.GroupID != nil {
		applyAccountStatsCost(ctx, usageLog, s.channelService, s.billingService,
			account.ID, *apiKey.GroupID, result.UpstreamModel, result.Model,
			tokens, cost.TotalCost,
		)
	}

	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
		logger.LegacyPrintf("service.openai_gateway", "[SIMPLE MODE] Usage recorded (not billed): user=%d, tokens=%d", usageLog.UserID, usageLog.TotalTokens())
		s.deferredService.ScheduleLastUsedUpdate(account.ID)
		return nil
	}

	// Async usage billing runs outside the original request context, so it
	// cannot recover ForcePlatform there. Fall back for internal/test callers.
	quotaPlatform := input.QuotaPlatform
	if quotaPlatform == "" {
		quotaPlatform = PlatformFromAPIKey(apiKey)
	}

	billingErr := func() error {
		_, err := applyUsageBilling(ctx, requestID, usageLog, &postUsageBillingParams{
			Cost:                  cost,
			User:                  user,
			APIKey:                apiKey,
			Account:               account,
			Subscription:          subscription,
			RequestPayloadHash:    resolveUsageBillingPayloadFingerprint(ctx, input.RequestPayloadHash),
			IsSubscriptionBill:    isSubscriptionBilling,
			AccountRateMultiplier: accountRateMultiplier,
			APIKeyService:         input.APIKeyService,
			Platform:              quotaPlatform,
		}, s.billingDeps(), s.usageBillingRepo)
		return err
	}()

	if billingErr != nil {
		usageLog.ActualCost = 0
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
		return billingErr
	}
	writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")

	return nil
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageCost(
	ctx context.Context,
	result *OpenAIForwardResult,
	apiKey *APIKey,
	billingModels []string,
	multiplier float64,
	imageMultiplier float64,
	videoMultiplier float64,
	webSearchMultiplier float64,
	tokens UsageTokens,
	serviceTier string,
	longContextBillingGate *bool,
	pricingAt time.Time,
) (*CostBreakdown, error) {
	billingModel := firstUsageBillingModel(billingModels)
	if result != nil && result.WebSearchCalls > 0 {
		// Alpha search is billed per invocation. The local pricing model stores
		// search price per 1k calls, whose official default ($10/1k) is exactly
		// the upstream $0.01 per-call default.
		return s.billingService.CalculateOpenAIWebSearchCost(result.WebSearchCalls, groupSearchPricePer1kFromAPIKey(apiKey), webSearchMultiplier), nil
	}
	if isGrokVideoUsageResult(result, billingModels) {
		if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved == nil || resolved.Mode != BillingModeToken {
			return s.calculateOpenAIVideoCost(ctx, billingModel, apiKey, result, videoMultiplier), nil
		}
	}
	if result != nil && result.AudioUsage != nil {
		cfg := groupAudioPriceConfigFromAPIKey(apiKey)
		return s.billingService.CalculateAudioCost(result.AudioUsage.Mode, result.AudioUsage.DurationOrUnits, cfg, webSearchMultiplier), nil
	}
	if result != nil && result.ImageCount > 0 {
		resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey)
		if resolved == nil || resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage {
			return s.calculateOpenAIImageCost(ctx, billingModel, apiKey, result, imageMultiplier), nil
		}
	}

	var tokenCost *CostBreakdown
	var lastErr error
	if len(billingModels) > 0 && billingModel != "" {
		for _, candidate := range billingModels {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			candidateCost, candidateErr := s.calculateOpenAIRecordUsageTokenCost(
				ctx,
				apiKey,
				candidate,
				multiplier,
				pricingAt,
				tokens,
				serviceTier,
				longContextBillingGate,
			)
			if candidateErr == nil {
				tokenCost = candidateCost
				break
			}
			lastErr = candidateErr
		}
	}

	var searchCost *CostBreakdown
	if result != nil && result.SearchCount > 0 {
		price := groupSearchPricePer1kFromAPIKey(apiKey)
		if price != nil && *price == 0 {
			logger.L().Info("openai_usage.search_price_per_1k_explicit_free",
				zap.Int("search_count", result.SearchCount),
				zap.String("model", billingModel),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
			)
		}
		searchCost = s.billingService.CalculateSearchCost(result.SearchCount, price, webSearchMultiplier)
	}

	tokenBillingAttempted := len(billingModels) > 0 && billingModel != ""
	if tokenCost == nil {
		if tokenBillingAttempted {
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: no non-empty billing model candidates", ErrModelPricingUnavailable)
			}
			return nil, fmt.Errorf("calculate OpenAI usage cost failed for billing models %s: %w", strings.Join(billingModels, ","), lastErr)
		}
		if searchCost != nil {
			return searchCost, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: openai usage billing model is empty", ErrModelPricingUnavailable)
		}
		return nil, fmt.Errorf("calculate OpenAI usage cost failed for billing models %s: %w", strings.Join(billingModels, ","), lastErr)
	}
	if searchCost == nil || (searchCost.TotalCost == 0 && searchCost.ActualCost == 0) {
		return tokenCost, nil
	}
	tokenCost.TotalCost += searchCost.TotalCost
	tokenCost.ActualCost += searchCost.ActualCost
	return tokenCost, nil
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageTokenCost(
	ctx context.Context,
	apiKey *APIKey,
	billingModel string,
	multiplier float64,
	pricingAt time.Time,
	tokens UsageTokens,
	serviceTier string,
	longContextBillingGate *bool,
) (*CostBreakdown, error) {
	if s.resolver != nil && apiKey.Group != nil {
		gid := apiKey.Group.ID
		return s.billingService.CalculateCostUnified(CostInput{
			Ctx: ctx, Model: billingModel, GroupID: &gid, Group: apiKey.Group,
			Tokens: tokens, RequestCount: 1, RateMultiplier: multiplier, PricingAt: pricingAt,
			ServiceTier: serviceTier, Resolver: s.resolver,
			LongContextBillingEnabled: longContextBillingGate,
		})
	}
	return s.billingService.calculateCostWithServiceTierPolicy(
		billingModel,
		tokens,
		multiplier,
		serviceTier,
		longContextBillingGate == nil || *longContextBillingGate,
	)
}

func isGrokVideoBillingModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "grok-imagine-video")
}

func isGrokVideoUsageResult(result *OpenAIForwardResult, billingModels []string) bool {
	if result == nil || result.VideoCount <= 0 {
		return false
	}
	candidates := append([]string{}, billingModels...)
	candidates = append(candidates, result.BillingModel, result.Model, result.UpstreamModel)
	for _, candidate := range candidates {
		if isGrokVideoBillingModel(candidate) {
			return true
		}
	}
	return true
}

func isUsagePricingUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrModelPricingUnavailable) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no pricing available") || strings.Contains(msg, "pricing not found")
}

func (s *OpenAIGatewayService) calculateOpenAIImageCost(
	ctx context.Context,
	billingModel string,
	apiKey *APIKey,
	result *OpenAIForwardResult,
	multiplier float64,
) *CostBreakdown {
	sizeTier := NormalizeImageBillingTierOrDefault(result.ImageSize)
	groupConfig := imagePriceConfigFromAPIKey(apiKey)
	if apiKeyHasConfiguredImagePrice(apiKey, sizeTier) {
		return s.billingService.CalculateImageCost(billingModel, sizeTier, result.ImageCount, groupConfig, multiplier)
	}
	if refreshed := s.apiKeyWithFreshGroupMediaPricing(ctx, apiKey); refreshed != apiKey {
		apiKey = refreshed
		groupConfig = imagePriceConfigFromAPIKey(apiKey)
		if apiKeyHasConfiguredImagePrice(apiKey, sizeTier) {
			return s.billingService.CalculateImageCost(billingModel, sizeTier, result.ImageCount, groupConfig, multiplier)
		}
	}
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			RequestCount:   result.ImageCount,
			SizeTier:       imageBillingSizeTier(resolved, sizeTier),
			RateMultiplier: multiplier,
			Resolver:       s.resolver,
			Resolved:       resolved,
		})
		if err == nil {
			return cost
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate image channel cost failed: %v", err)
	}

	return s.billingService.CalculateImageCost(billingModel, sizeTier, result.ImageCount, groupConfig, multiplier)
}

func (s *OpenAIGatewayService) calculateOpenAIVideoCost(
	ctx context.Context,
	billingModel string,
	apiKey *APIKey,
	result *OpenAIForwardResult,
	multiplier float64,
) *CostBreakdown {
	videoCount := result.VideoCount
	if videoCount <= 0 {
		videoCount = 1
	}
	resolution := NormalizeVideoBillingResolutionOrDefault(result.VideoResolution)
	durationSeconds := NormalizeVideoBillingDurationSecondsOrDefault(result.VideoDurationSeconds)
	groupConfig := videoPriceConfigFromAPIKey(apiKey)
	if apiKeyHasConfiguredVideoPrice(apiKey, billingModel, resolution) {
		return s.billingService.CalculateVideoCost(billingModel, resolution, videoCount, durationSeconds, groupConfig, multiplier)
	}
	if refreshed := s.apiKeyWithFreshGroupMediaPricing(ctx, apiKey); refreshed != apiKey {
		apiKey = refreshed
		groupConfig = videoPriceConfigFromAPIKey(apiKey)
		if apiKeyHasConfiguredVideoPrice(apiKey, billingModel, resolution) {
			return s.billingService.CalculateVideoCost(billingModel, resolution, videoCount, durationSeconds, groupConfig, multiplier)
		}
	}
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage || resolved.Mode == BillingModeVideo) {
		// 渠道按次定价保持管理员配置的按次口径，不乘视频时长。
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			RequestCount:   videoCount,
			SizeTier:       resolution,
			RateMultiplier: multiplier,
			Resolver:       s.resolver,
			Resolved:       resolved,
		})
		if err == nil {
			cost.BillingMode = string(BillingModeVideo)
			return cost
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate video channel cost failed: %v", err)
	}

	return s.billingService.CalculateVideoCost(billingModel, resolution, videoCount, durationSeconds, groupConfig, multiplier)
}

func (s *OpenAIGatewayService) apiKeyWithFreshGroupMediaPricing(ctx context.Context, apiKey *APIKey) *APIKey {
	if apiKey == nil || apiKey.GroupID == nil || *apiKey.GroupID <= 0 {
		return apiKey
	}
	if !groupMediaPricingLooksIncomplete(apiKey.Group) {
		return apiKey
	}
	if s == nil || s.channelService == nil || s.channelService.groupRepo == nil {
		return apiKey
	}
	group, err := s.channelService.groupRepo.GetByIDLite(ctx, *apiKey.GroupID)
	if err != nil || group == nil {
		return apiKey
	}
	clone := *apiKey
	clone.Group = group
	return &clone
}

func groupMediaPricingLooksIncomplete(group *Group) bool {
	if group == nil {
		return true
	}
	if group.ImageRateIndependent || group.VideoRateIndependent {
		return false
	}
	if group.ImageRateMultiplier != 0 || group.VideoRateMultiplier != 0 {
		return false
	}
	if len(group.VideoModelPrices) > 0 {
		return false
	}
	if group.SearchPricePer1k != nil ||
		group.AudioRealtimePricePerMin != nil ||
		group.AudioTTSPricePerMillionChars != nil ||
		group.AudioSTTPricePerHour != nil {
		return false
	}
	return group.ImagePrice1K == nil && group.ImagePrice2K == nil && group.ImagePrice4K == nil &&
		group.VideoPrice480P == nil && group.VideoPrice720P == nil && group.VideoPrice1080P == nil
}

func (s *OpenAIGatewayService) resolveOpenAIChannelPricing(ctx context.Context, billingModel string, apiKey *APIKey) *ResolvedPricing {
	if s.resolver == nil || apiKey == nil || apiKey.Group == nil {
		return nil
	}
	gid := apiKey.Group.ID
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: billingModel, GroupID: &gid, Group: apiKey.Group})
	if resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel {
		return resolved
	}
	return nil
}

// openAILongContextBillingGate returns the OpenAI per-account long-context
// opt-in. Other platforms have no account flag and follow the group toggle.
func openAILongContextBillingGate(account *Account) *bool {
	if account == nil || !account.IsOpenAI() {
		return nil
	}
	enabled := account.IsOpenAILongContextBillingEnabled()
	return &enabled
}

// filterCNProviderBillingModelCandidates prevents Anthropic-compatible CN
// providers from falling through to Claude catalog pricing unless an operator
// explicitly configured pricing for that candidate.
func (s *OpenAIGatewayService) filterCNProviderBillingModelCandidates(ctx context.Context, account *Account, apiKey *APIKey, candidates []string) []string {
	if account == nil || !account.IsCNProvider() {
		return candidates
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		if strings.Contains(strings.ToLower(trimmed), "claude") &&
			s.resolveOpenAIChannelPricing(ctx, trimmed, apiKey) == nil {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func (s *OpenAIGatewayService) hasIdentifiedOpenAIResponsePricing(ctx context.Context, model string, apiKey *APIKey) (identified bool, channelPriced bool) {
	if strings.TrimSpace(model) == "" {
		return false, false
	}
	if s.resolveOpenAIChannelPricing(ctx, model, apiKey) != nil {
		return true, true
	}
	if s.billingService == nil {
		return false, false
	}
	return s.billingService.HasIdentifiedTokenPricing(model), false
}

// ParseCodexRateLimitHeaders extracts Codex usage limits from response headers.
// Exported for use in ratelimit_service when handling OpenAI 429 responses.
func ParseCodexRateLimitHeaders(headers http.Header) *OpenAICodexUsageSnapshot {
	snapshot := &OpenAICodexUsageSnapshot{}
	hasData := false

	// Helper to parse float64 from header
	parseFloat := func(key string) *float64 {
		if v := headers.Get(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return &f
			}
		}
		return nil
	}

	// Helper to parse int from header
	parseInt := func(key string) *int {
		if v := headers.Get(key); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				return &i
			}
		}
		return nil
	}

	// Primary (weekly) limits
	if v := parseFloat("x-codex-primary-used-percent"); v != nil {
		snapshot.PrimaryUsedPercent = v
		hasData = true
	}
	if v := parseInt("x-codex-primary-reset-after-seconds"); v != nil {
		snapshot.PrimaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt("x-codex-primary-window-minutes"); v != nil {
		snapshot.PrimaryWindowMinutes = v
		hasData = true
	}

	// Secondary (5h) limits
	if v := parseFloat("x-codex-secondary-used-percent"); v != nil {
		snapshot.SecondaryUsedPercent = v
		hasData = true
	}
	if v := parseInt("x-codex-secondary-reset-after-seconds"); v != nil {
		snapshot.SecondaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt("x-codex-secondary-window-minutes"); v != nil {
		snapshot.SecondaryWindowMinutes = v
		hasData = true
	}

	// Overflow ratio
	if v := parseFloat("x-codex-primary-over-secondary-limit-percent"); v != nil {
		snapshot.PrimaryOverSecondaryPercent = v
		hasData = true
	}

	if !hasData {
		return nil
	}

	snapshot.UpdatedAt = time.Now().Format(time.RFC3339)
	return snapshot
}

func codexSnapshotBaseTime(snapshot *OpenAICodexUsageSnapshot, fallback time.Time) time.Time {
	if snapshot == nil {
		return fallback
	}
	if snapshot.UpdatedAt == "" {
		return fallback
	}
	base, err := time.Parse(time.RFC3339, snapshot.UpdatedAt)
	if err != nil {
		return fallback
	}
	return base
}

func codexResetAtRFC3339(base time.Time, resetAfterSeconds *int) *string {
	if resetAfterSeconds == nil {
		return nil
	}
	sec := *resetAfterSeconds
	if sec < 0 {
		sec = 0
	}
	resetAt := base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
	return &resetAt
}

func buildCodexUsageExtraUpdates(snapshot *OpenAICodexUsageSnapshot, fallbackNow time.Time) map[string]any {
	if snapshot == nil {
		return nil
	}

	baseTime := codexSnapshotBaseTime(snapshot, fallbackNow)
	updates := make(map[string]any)

	// 保存原始 primary/secondary 字段，便于排查问题
	if snapshot.PrimaryUsedPercent != nil {
		updates["codex_primary_used_percent"] = *snapshot.PrimaryUsedPercent
	}
	if snapshot.PrimaryResetAfterSeconds != nil {
		updates["codex_primary_reset_after_seconds"] = *snapshot.PrimaryResetAfterSeconds
	}
	if snapshot.PrimaryWindowMinutes != nil {
		updates["codex_primary_window_minutes"] = *snapshot.PrimaryWindowMinutes
	}
	if snapshot.SecondaryUsedPercent != nil {
		updates["codex_secondary_used_percent"] = *snapshot.SecondaryUsedPercent
	}
	if snapshot.SecondaryResetAfterSeconds != nil {
		updates["codex_secondary_reset_after_seconds"] = *snapshot.SecondaryResetAfterSeconds
	}
	if snapshot.SecondaryWindowMinutes != nil {
		updates["codex_secondary_window_minutes"] = *snapshot.SecondaryWindowMinutes
	}
	if snapshot.PrimaryOverSecondaryPercent != nil {
		updates["codex_primary_over_secondary_percent"] = *snapshot.PrimaryOverSecondaryPercent
	}
	updates["codex_usage_updated_at"] = baseTime.Format(time.RFC3339)

	// 归一化到 5h/7d 规范字段
	if normalized := snapshot.Normalize(); normalized != nil {
		if normalized.Used5hPercent != nil {
			updates["codex_5h_used_percent"] = *normalized.Used5hPercent
		}
		if normalized.Reset5hSeconds != nil {
			updates["codex_5h_reset_after_seconds"] = *normalized.Reset5hSeconds
		}
		if normalized.Window5hMinutes != nil {
			updates["codex_5h_window_minutes"] = *normalized.Window5hMinutes
		}
		if normalized.Used7dPercent != nil {
			updates["codex_7d_used_percent"] = *normalized.Used7dPercent
		}
		if normalized.Reset7dSeconds != nil {
			updates["codex_7d_reset_after_seconds"] = *normalized.Reset7dSeconds
		}
		if normalized.Window7dMinutes != nil {
			updates["codex_7d_window_minutes"] = *normalized.Window7dMinutes
		}
		if reset5hAt := codexResetAtRFC3339(baseTime, normalized.Reset5hSeconds); reset5hAt != nil {
			updates["codex_5h_reset_at"] = *reset5hAt
		}
		if reset7dAt := codexResetAtRFC3339(baseTime, normalized.Reset7dSeconds); reset7dAt != nil {
			updates["codex_7d_reset_at"] = *reset7dAt
		}
	}

	return updates
}

// updateCodexUsageSnapshot saves the Codex usage snapshot to account's Extra field
func (s *OpenAIGatewayService) updateCodexUsageSnapshot(ctx context.Context, accountID int64, snapshot *OpenAICodexUsageSnapshot) {
	if snapshot == nil {
		return
	}
	if s == nil || s.accountRepo == nil {
		return
	}

	now := time.Now()
	updates := buildCodexUsageExtraUpdates(snapshot, now)
	if len(updates) == 0 {
		return
	}
	if !s.getCodexSnapshotThrottle().Allow(accountID, now) {
		return
	}

	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.accountRepo.UpdateExtra(updateCtx, accountID, updates)
	}()
}

func (s *OpenAIGatewayService) UpdateCodexUsageSnapshotFromHeaders(ctx context.Context, accountID int64, headers http.Header) {
	if accountID <= 0 || headers == nil {
		return
	}
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, accountID, snapshot)
	}
}

func getOpenAIReasoningEffortFromReqBody(reqBody map[string]any, requestedModel string) (value string, present bool) {
	if reqBody == nil {
		return "", false
	}

	// Primary: reasoning.effort
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
		}
	}

	// Fallback: some clients may use a flat field.
	if effort, ok := reqBody["reasoning_effort"].(string); ok {
		return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
	}

	return "", false
}

func deriveOpenAIReasoningEffortFromModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}

	if _, effort, ok := splitOpenAICompatReasoningModel(model); ok {
		return effort
	}

	modelID := strings.TrimSpace(model)
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}

	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return ""
	}

	return normalizeOpenAIReasoningEffortForModel(parts[len(parts)-1], modelID)
}

func deriveOpenAIReasoningEffortFromModelCandidates(models []string) string {
	for _, model := range models {
		if value := deriveOpenAIReasoningEffortFromModel(model); value != "" {
			return value
		}
	}
	return ""
}

type openAIRequestView struct {
	body               []byte
	Model              string
	Stream             bool
	PromptCacheKey     string
	PreviousResponseID string
	ServiceTier        string
	ReasoningEffort    string
	patches            []openAIRequestPatch
	patchesDisabled    bool
}

type openAIRequestPatch struct {
	path   string
	delete bool
	value  any
}

func newOpenAIRequestView(body []byte) openAIRequestView {
	if len(body) == 0 {
		return openAIRequestView{}
	}
	return openAIRequestView{
		body:               body,
		Model:              strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		Stream:             gjson.GetBytes(body, "stream").Bool(),
		PromptCacheKey:     strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()),
		PreviousResponseID: strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()),
		ServiceTier:        strings.TrimSpace(gjson.GetBytes(body, "service_tier").String()),
		ReasoningEffort:    strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()),
	}
}

// Decode 保留阶段一既有 full-map 行为；后续阶段会把调用点下沉到复杂分支。
func (v openAIRequestView) Decode(c *gin.Context) (map[string]any, error) {
	return getOpenAIRequestBodyMap(c, v.body)
}

func (v *openAIRequestView) MarkPatchSet(path string, value any) {
	if v == nil || v.patchesDisabled {
		return
	}
	path = strings.TrimSpace(path)
	if !isSimpleOpenAIRequestPatchPath(path) {
		v.DisablePatches()
		return
	}
	v.patches = append(v.patches, openAIRequestPatch{path: path, value: value})
}

func (v *openAIRequestView) MarkPatchDelete(path string) {
	if v == nil || v.patchesDisabled {
		return
	}
	path = strings.TrimSpace(path)
	if !isSimpleOpenAIRequestPatchPath(path) {
		v.DisablePatches()
		return
	}
	v.patches = append(v.patches, openAIRequestPatch{path: path, delete: true})
}

func isSimpleOpenAIRequestPatchPath(path string) bool {
	if path == "" || strings.ContainsRune(path, '\\') {
		return false
	}
	for _, part := range strings.Split(path, ".") {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func (v *openAIRequestView) DisablePatches() {
	if v == nil {
		return
	}
	v.patchesDisabled = true
	v.patches = nil
}

func (v openAIRequestView) HasPatches() bool {
	return !v.patchesDisabled && len(v.patches) > 0
}

func (v openAIRequestView) ApplyPatches() ([]byte, error) {
	if v.patchesDisabled || len(v.patches) == 0 {
		return nil, errors.New("openai request patches disabled")
	}
	body := v.body
	for _, patch := range v.patches {
		var err error
		if patch.delete {
			body, err = sjson.DeleteBytes(body, patch.path)
		} else {
			body, err = sjson.SetBytes(body, patch.path, patch.value)
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func setOpenAIRequestMapPath(reqBody map[string]any, path string, value any) {
	path = strings.TrimSpace(path)
	if reqBody == nil || path == "" {
		return
	}
	parts := strings.Split(path, ".")
	current := reqBody
	for _, part := range parts[:len(parts)-1] {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		next, _ := current[part].(map[string]any)
		if next == nil {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last != "" {
		current[last] = value
	}
}

func deleteOpenAIRequestMapPath(reqBody map[string]any, path string) {
	path = strings.TrimSpace(path)
	if reqBody == nil || path == "" {
		return
	}
	parts := strings.Split(path, ".")
	current := reqBody
	for _, part := range parts[:len(parts)-1] {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		next, _ := current[part].(map[string]any)
		if next == nil {
			return
		}
		current = next
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last != "" {
		delete(current, last)
	}
}

func extractOpenAIRequestMetaFromBody(body []byte) (model string, stream bool, promptCacheKey string) {
	view := newOpenAIRequestView(body)
	return view.Model, view.Stream, view.PromptCacheKey
}

// normalizeOpenAIPassthroughOAuthBody 将透传 OAuth 请求体收敛为旧链路关键行为：
//  1. 删除 ChatGPT internal API 不支持的顶层 Responses 参数
//  2. compact: 删除 store 与 stream
//  3. 非 compact: stream=true; storeEnabled=false 时强制 store=false;
//     storeEnabled=true 时保留客户端 store 值以支持 previous_response_id 续链
func normalizeOpenAIPassthroughOAuthBody(body []byte, compact bool, storeEnabled bool) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	normalized, changed, err := normalizeOpenAIOAuthResponsesCompatibilityBody(body)
	if err != nil {
		return body, false, err
	}
	if reasoningBody, reasoningChanged, reasoningErr := normalizeOpenAIResponsesReasoningMode(normalized); reasoningErr != nil {
		return body, false, reasoningErr
	} else if reasoningChanged {
		normalized = reasoningBody
		changed = true
	}

	for _, field := range openAIChatGPTInternalUnsupportedFields {
		if value := gjson.GetBytes(normalized, field); !value.Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(normalized, field)
		if err != nil {
			return body, false, fmt.Errorf("normalize passthrough body delete %s: %w", field, err)
		}
		normalized = next
		changed = true
	}
	if schemaBody, schemaChanged, schemaErr := normalizeOpenAIResponseFormatSchemasBody(normalized); schemaErr != nil {
		return body, false, schemaErr
	} else if schemaChanged {
		normalized = schemaBody
		changed = true
	}

	if inputResult := gjson.GetBytes(normalized, "input"); inputResult.Exists() {
		switch {
		case inputResult.Type == gjson.String:
			text := inputResult.String()
			var inputValue any
			if strings.TrimSpace(text) != "" {
				inputValue = []any{map[string]any{
					"type": "message", "role": "user", "content": text,
				}}
			} else {
				inputValue = []any{}
			}
			next, err := sjson.SetBytes(normalized, "input", inputValue)
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body input string: %w", err)
			}
			normalized = next
			changed = true
		case inputResult.Type == gjson.JSON && !inputResult.IsArray():
			next, err := sjson.SetRawBytes(normalized, "input", []byte("["+inputResult.Raw+"]"))
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body input object: %w", err)
			}
			normalized = next
			changed = true
		}
	}

	if compact {
		if store := gjson.GetBytes(normalized, "store"); store.Exists() {
			next, err := sjson.DeleteBytes(normalized, "store")
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body delete store: %w", err)
			}
			normalized = next
			changed = true
		}
		if stream := gjson.GetBytes(normalized, "stream"); stream.Exists() {
			next, err := sjson.DeleteBytes(normalized, "stream")
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body delete stream: %w", err)
			}
			normalized = next
			changed = true
		}
	} else {
		if !storeEnabled {
			if store := gjson.GetBytes(normalized, "store"); !store.Exists() || store.Type != gjson.False {
				next, err := sjson.SetBytes(normalized, "store", false)
				if err != nil {
					return body, false, fmt.Errorf("normalize passthrough body store=false: %w", err)
				}
				normalized = next
				changed = true
			}
		}
		if stream := gjson.GetBytes(normalized, "stream"); !stream.Exists() || stream.Type != gjson.True {
			next, err := sjson.SetBytes(normalized, "stream", true)
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body stream=true: %w", err)
			}
			normalized = next
			changed = true
		}
	}

	for _, field := range openAIResponsesUnsupportedFields {
		if value := gjson.GetBytes(normalized, field); value.Exists() {
			next, err := sjson.DeleteBytes(normalized, field)
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body delete %s: %w", field, err)
			}
			normalized = next
			changed = true
		}
	}

	if serviceTier := normalizeOpenAIServiceTier(gjson.GetBytes(normalized, "service_tier").String()); serviceTier != nil && *serviceTier == "flex" {
		next, err := sjson.DeleteBytes(normalized, "service_tier")
		if err != nil {
			return body, false, fmt.Errorf("normalize passthrough body delete service_tier: %w", err)
		}
		normalized = next
		changed = true
	}

	return normalized, changed, nil
}

func extractResponsesTextFormatRaw(body []byte) json.RawMessage {
	format := gjson.GetBytes(body, "text.format")
	if !format.Exists() || strings.TrimSpace(format.Raw) == "" {
		return nil
	}
	return json.RawMessage(format.Raw)
}

func restoreResponsesTextFormatRaw(body []byte, format json.RawMessage) ([]byte, error) {
	if len(format) == 0 {
		return body, nil
	}
	return sjson.SetRawBytes(body, "text.format", format)
}

func detectOpenAIPassthroughInstructionsRejectReason(reqModel string, body []byte) string {
	// Keep the codex-only model gate so non-codex OAuth passthrough behavior
	// remains unchanged. Missing instructions are synthesized before this
	// check; malformed explicit values still use the protected rollback path.
	if !isOpenAICodexModel(reqModel) {
		return ""
	}

	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() {
		return ""
	}
	if instructions.Type != gjson.String {
		return "instructions_not_string"
	}
	if strings.TrimSpace(instructions.String()) == "" {
		return "instructions_empty"
	}
	return ""
}

func isOpenAICodexModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "codex")
}

func extractOpenAIReasoningEffortFromBody(body []byte, modelCandidates ...string) *string {
	reasoningEffort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if reasoningEffort == "" {
		reasoningEffort = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if reasoningEffort != "" {
		normalized := normalizeOpenAIReasoningEffortForModel(reasoningEffort, firstNonEmpty(modelCandidates...))
		if normalized == "" {
			return nil
		}
		return &normalized
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func extractOpenAIServiceTier(reqBody map[string]any) *string {
	if reqBody == nil {
		return nil
	}
	raw, ok := reqBody["service_tier"].(string)
	if !ok {
		return nil
	}
	return normalizeOpenAIServiceTier(raw)
}

func stripUnsupportedOpenAIOAuthServiceTier(reqBody map[string]any) bool {
	serviceTier := extractOpenAIServiceTier(reqBody)
	if serviceTier == nil || *serviceTier != "flex" {
		return false
	}
	delete(reqBody, "service_tier")
	return true
}

func extractOpenAIServiceTierFromBody(body []byte) *string {
	if len(body) == 0 {
		return nil
	}
	return normalizeOpenAIServiceTier(gjson.GetBytes(body, "service_tier").String())
}

func normalizeOpenAIServiceTier(raw string) *string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return nil
	}
	if value == "fast" {
		value = "priority"
	}
	// 放过 OpenAI 官方文档定义的所有合法 tier 值：priority/flex/auto/default/scale。
	// 对 Codex 客户端零影响（Codex 只发 priority 或 flex，见 codex-rs/core/src/client.rs），
	// 但能让直连 OpenAI SDK 的用户透传 auto/default/scale 以便抓包/调试。
	// 真未知值仍返回 nil，由 normalizeResponsesBodyServiceTier 从 body 中删除。
	switch value {
	case "priority", "flex", "auto", "default", "scale":
		return &value
	default:
		return nil
	}
}

// OpenAIFastBlockedError indicates a request was rejected by the OpenAI fast
// policy (action=block). Mirrors BetaBlockedError on the Claude side.
type OpenAIFastBlockedError struct {
	Message string
}

func (e *OpenAIFastBlockedError) Error() string { return e.Message }

// evaluateOpenAIFastPolicy returns the action and error message that should be
// applied for a request with the given account/model/service_tier. When the
// policy service is unavailable or no rule matches, it returns
// (BetaPolicyActionPass, "") so callers can short-circuit safely.
//
// Matching rules:
//   - Scope filters by account type (all / oauth / apikey / bedrock)
//   - ServiceTier must be empty (= any), "all", or equal the normalized tier
//   - ModelWhitelist narrows the rule to specific models; FallbackAction
//     handles the non-matching case (default: pass)
//   - User-specific rules take precedence over global rules; each group keeps
//     the configured first-match order
//
// 与 Claude BetaPolicy 的差异（保留首条匹配 short-circuit）：
//   - BetaPolicy 处理的是 anthropic-beta header 中的 token 集合，不同
//     规则可能针对不同 token，filter 需要累加成 set；block 则 first-match。
//   - OpenAI fast policy 操作的是单个字段 service_tier：filter 即删字段，
//     没有可累加的对象。一次请求只携带一个 service_tier，规则的 tier
//     维度天然互斥；同一 (scope, tier) 下若多条规则的 model whitelist
//     发生重叠，admin 可通过规则顺序明确意图。因此采用 first-match 而
//     非 BetaPolicy 那样的"block 覆盖 filter 覆盖 pass"语义。
func (s *OpenAIGatewayService) evaluateOpenAIFastPolicy(ctx context.Context, account *Account, model, serviceTier string) (action, errMsg string) {
	if s == nil || s.settingService == nil {
		return BetaPolicyActionPass, ""
	}
	tier := strings.ToLower(strings.TrimSpace(serviceTier))
	if tier == "" {
		return BetaPolicyActionPass, ""
	}
	settings := openAIFastPolicySettingsFromContext(ctx)
	if settings == nil {
		fetched, err := s.settingService.GetOpenAIFastPolicySettings(ctx)
		if err != nil || fetched == nil {
			return BetaPolicyActionPass, ""
		}
		settings = fetched
	}
	return evaluateOpenAIFastPolicyWithSettings(settings, openAIFastPolicyUserID(ctx), account, model, tier)
}

// evaluateOpenAIFastPolicyWithSettings is the pure-function core extracted so
// long-lived sessions (e.g. WS) can prefetch settings once and avoid hitting
// the settingService on every frame. See WSSession entry and
// openAIFastPolicySettingsFromContext for the caching glue.
func evaluateOpenAIFastPolicyWithSettings(settings *OpenAIFastPolicySettings, userID int64, account *Account, model, tier string) (action, errMsg string) {
	if settings == nil {
		return BetaPolicyActionPass, ""
	}
	isOAuth := account != nil && account.IsOAuth()
	isBedrock := account != nil && account.IsBedrock()

	// 用户专属规则先于全局规则。规则组内仍按配置顺序首条命中，允许
	// 管理员为某位用户配置例外，而不被先出现的全局规则覆盖。
	for _, userScoped := range []bool{true, false} {
		for _, rule := range settings.Rules {
			if (len(rule.UserIDs) > 0) != userScoped || !openAIFastPolicyUserMatches(rule.UserIDs, userID) {
				continue
			}
			if !betaPolicyScopeMatches(rule.Scope, isOAuth, isBedrock) {
				continue
			}
			ruleTier := strings.ToLower(strings.TrimSpace(rule.ServiceTier))
			if ruleTier != "" && ruleTier != OpenAIFastTierAny && ruleTier != tier {
				continue
			}
			eff := BetaPolicyRule{
				Action:               rule.Action,
				ErrorMessage:         rule.ErrorMessage,
				ModelWhitelist:       rule.ModelWhitelist,
				FallbackAction:       rule.FallbackAction,
				FallbackErrorMessage: rule.FallbackErrorMessage,
			}
			return resolveRuleAction(eff, model)
		}
	}
	return BetaPolicyActionPass, ""
}

func openAIFastPolicyUserID(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	userID, _ := ctx.Value(ctxkey.UserID).(int64)
	if userID <= 0 {
		return 0
	}
	return userID
}

func openAIFastPolicyUserMatches(ruleUserIDs []int64, userID int64) bool {
	if len(ruleUserIDs) == 0 {
		return true
	}
	for _, ruleUserID := range ruleUserIDs {
		if ruleUserID == userID {
			return true
		}
	}
	return false
}

// openAIFastPolicyCtxKey 是 context 中预取的 OpenAIFastPolicySettings 缓存
// 键，仅用于 WebSocket 长会话内多帧复用同一份策略快照，避免每帧 DB 命中。
//
// Trade-off：策略变更不会影响当前 WS session（只影响新 session）。这是
// 有意为之 —— 对长会话来说，"策略一致性"比"立刻生效"更重要，且 Claude
// BetaPolicy 的 gin.Context 缓存也是同样取舍。需要 hot-reload 时管理员
// 可以通过踢断 session 强制刷新。
type openAIFastPolicyCtxKeyType struct{}

var openAIFastPolicyCtxKey = openAIFastPolicyCtxKeyType{}

// withOpenAIFastPolicyContext 将一份 settings 快照绑定到 context，供该 ctx
// 衍生 goroutine 中的 evaluateOpenAIFastPolicy 复用。
func withOpenAIFastPolicyContext(ctx context.Context, settings *OpenAIFastPolicySettings) context.Context {
	if ctx == nil || settings == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIFastPolicyCtxKey, settings)
}

func openAIFastPolicySettingsFromContext(ctx context.Context) *OpenAIFastPolicySettings {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(openAIFastPolicyCtxKey).(*OpenAIFastPolicySettings); ok {
		return v
	}
	return nil
}

func openAIGroupForcesFast(ctx context.Context, account *Account) bool {
	if ctx == nil || account == nil || account.Platform != PlatformOpenAI {
		return false
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	return IsGroupContextValid(group) && groupSupportsOpenAIFast(group.Platform) && group.ForceOpenAIFast
}

// applyOpenAIFastPolicyToBody applies the OpenAI fast policy to a raw request
// body. When action=filter it removes the service_tier field; when
// action=block it returns (body, *OpenAIFastBlockedError). On pass it
// normalizes the service_tier value (e.g. client alias "fast" → "priority"),
// rewriting the body so the upstream receives a slug it recognizes.
//
// Rationale for normalize-on-pass: chat-completions / messages 入口在调用本
// 函数之前已经通过 normalizeResponsesBodyServiceTier 把 service_tier 归一化
// 到了上游可识别值；passthrough（OpenAI 自动透传） / native /responses 等
// 入口没有这一前置步骤，pass 路径下若不在此处归一化，"fast" 就会被原样
// 透传到 OpenAI 上游导致 400/拒绝。把归一化收敛到本函数，所有入口行为一致。
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToBody(ctx context.Context, account *Account, model string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	if openAIGroupForcesFast(ctx, account) {
		updated, err := sjson.SetBytes(body, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return body, fmt.Errorf("force group service_tier priority on body: %w", err)
		}
		body = updated
	}
	tierResult := gjson.GetBytes(body, "service_tier")
	if !tierResult.Exists() {
		return body, nil
	}
	// codex 2026-05-16 round9: any top-level service_tier on a non-string
	// type — number / null / array / object / bool — is stripped. The
	// earlier round7 contract that let non-string values pass through to
	// surface upstream JSON errors gave callers hitting sub2api's native
	// OpenAI surface a free residual probe ("does the upstream
	// validate service_tier?") to confirm there's an OpenAI backend
	// behind us. Strip silently — Anthropic spec has no service_tier and
	// real Claude clients never send one, so this is invisible to
	// legitimate traffic; for OpenAI-shaped probers it just makes the
	// field disappear.
	if tierResult.Type != gjson.String {
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip non-string service_tier from body: %w", err)
		}
		return trimmed, nil
	}
	rawTier := tierResult.String()
	if rawTier == "" {
		// Empty string — treat as "field present but no value to police".
		// Strip for consistency with the round9 universal-presence rule:
		// the upstream shouldn't see service_tier="" any more than it
		// should see service_tier=1.
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip empty service_tier from body: %w", err)
		}
		return trimmed, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		// codex 2026-05-16 round7: unknown *string* tier values (e.g.
		// user-supplied "fixel", typo'd "preemium"). Strip — see same
		// rationale as the non-string branch above.
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip unknown service_tier from body: %w", err)
		}
		return trimmed, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return body, &OpenAIFastBlockedError{Message: msg}
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip service_tier from body: %w", err)
		}
		return trimmed, nil
	case OpenAIFastPolicyActionForcePriority:
		updated, err := sjson.SetBytes(body, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return body, fmt.Errorf("force service_tier priority on body: %w", err)
		}
		return updated, nil
	default:
		// pass：把别名（如 "fast"）写回为规范值（"priority"）。
		if normTier == rawTier {
			return body, nil
		}
		updated, err := sjson.SetBytes(body, "service_tier", normTier)
		if err != nil {
			return body, fmt.Errorf("normalize service_tier on pass: %w", err)
		}
		return updated, nil
	}
}

// writeOpenAIFastPolicyBlockedResponse writes a 403 JSON response for a
// request blocked by the OpenAI fast policy.
func writeOpenAIFastPolicyBlockedResponse(c *gin.Context, err *OpenAIFastBlockedError) {
	if c == nil || err == nil {
		return
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	if StopOpenAICompactSSEKeepaliveCommitted(c) {
		writeOpenAICompactSSEFailureMessage(c, http.StatusForbidden, "permission_error", err.Message)
		return
	}
	c.JSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"type":    "permission_error",
			"message": err.Message,
		},
	})
}

// applyOpenAIFastPolicyToWSResponseCreate evaluates the OpenAI fast policy
// against a single client→upstream WebSocket frame whose top-level
// "type"=="response.create". It mirrors the HTTP-side
// applyOpenAIFastPolicyToBody contract but operates on a Realtime/Responses
// WS payload:
//
//   - pass: returns frame unchanged (newBytes == frame, blocked == nil)
//   - filter: returns a copy with top-level service_tier removed
//   - block: returns (frame, *OpenAIFastBlockedError)
//
// Only frames whose "type" field strictly equals "response.create" are
// inspected/mutated. Any other frame type — including the empty string —
// passes through untouched. The OpenAI Realtime client-event spec requires
// "type" to be set, so an empty type is treated as a malformed frame we do
// not police; the upstream is the source of truth for rejecting it.
//
// service_tier lives at the top level of response.create — same as the
// Responses HTTP body shape (see openai_gateway_chat_completions.go:304 +
// extractOpenAIServiceTierFromBody at line 5593, and the test fixture at
// openai_ws_forwarder_ingress_session_test.go:402). We therefore only need
// to inspect / strip the top-level field; there is no nested form in the
// schema today.
//
// The caller is responsible for choosing the upstream model passed in —
// this helper does not re-derive it.
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToWSResponseCreate(
	ctx context.Context,
	account *Account,
	model string,
	frame []byte,
) ([]byte, *OpenAIFastBlockedError, error) {
	if len(frame) == 0 {
		return frame, nil, nil
	}
	if !gjson.ValidBytes(frame) {
		return frame, nil, nil
	}
	frameType := strings.TrimSpace(gjson.GetBytes(frame, "type").String())
	// Strict match: only response.create is policy-checked. Empty / other
	// types pass through untouched so we never accidentally strip fields
	// from response.cancel, conversation.item.create, or any future
	// client-event the spec adds. The Realtime spec requires "type" on
	// every client event, so an empty type is malformed input — let the
	// upstream reject it rather than guessing at our layer.
	if frameType != "response.create" {
		return frame, nil, nil
	}
	if openAIGroupForcesFast(ctx, account) {
		updated, err := sjson.SetBytes(frame, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return frame, nil, fmt.Errorf("force group service_tier priority in ws frame: %w", err)
		}
		frame = updated
	}
	tierResult := gjson.GetBytes(frame, "service_tier")
	if !tierResult.Exists() {
		return frame, nil, nil
	}
	// codex 2026-05-16 round9: any non-string service_tier on a WS
	// response.create frame also gets stripped, same as the HTTP body
	// path. Removes the residual probing surface for callers hitting
	// the Realtime WS endpoint directly.
	if tierResult.Type != gjson.String {
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip non-string service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	}
	rawTier := tierResult.String()
	if rawTier == "" {
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip empty service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		// codex 2026-05-16 round7: unknown *string* tier — strip.
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip unknown service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return frame, &OpenAIFastBlockedError{Message: msg}, nil
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	case OpenAIFastPolicyActionForcePriority:
		updated, err := sjson.SetBytes(frame, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return frame, nil, fmt.Errorf("force service_tier priority in ws frame: %w", err)
		}
		return updated, nil, nil
	default:
		// pass: codex 2026-05-16 round5 #2457 narrow scope — align WS pass
		// path with the HTTP body path so that the alias "fast" is rewritten
		// to its canonical "priority" before going upstream. Previously the
		// WS pass branch returned the frame unchanged, leaving "fast" leaking
		// upstream and forcing whoever bills off the post-policy frame to
		// special-case the alias.
		if normTier == rawTier {
			return frame, nil, nil
		}
		updated, err := sjson.SetBytes(frame, "service_tier", normTier)
		if err != nil {
			return frame, nil, fmt.Errorf("normalize service_tier in ws frame: %w", err)
		}
		return updated, nil, nil
	}
}

// newOpenAIFastPolicyWSEventID returns a Realtime-style event_id for a
// server-emitted error event. Matches the loose "evt_<rand>" convention used
// by upstream Realtime servers; the exact value is not load-bearing and is
// only required for client-side log correlation. We reuse the existing
// google/uuid dependency rather than pulling a new one.
func newOpenAIFastPolicyWSEventID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		// Extremely unlikely; fall back to a fixed prefix so the field is
		// still non-empty and the schema stays self-consistent.
		return "evt_openai_fast_policy"
	}
	// Strip dashes so it visually matches "evt_<hex>" rather than UUID v4
	// canonical form, mirroring what real Realtime traces look like.
	return "evt_" + strings.ReplaceAll(id.String(), "-", "")
}

// buildOpenAIFastPolicyBlockedWSEvent renders an OpenAI Realtime/Responses
// style "error" event payload for a request blocked by the OpenAI fast
// policy. The shape mirrors Realtime error events as observed in upstream
// traces and per the spec's server "error" event:
//
//	{
//	  "event_id": "evt_<random>",
//	  "type": "error",
//	  "error": {
//	    "type": "invalid_request_error",
//	    "code": "policy_violation",
//	    "message": "..."
//	  }
//	}
//
// event_id lets clients correlate the rejection in their logs; "code" gives
// programmatic clients a stable identifier (HTTP-side equivalent is the
// 403 permission_error JSON body).
func buildOpenAIFastPolicyBlockedWSEvent(err *OpenAIFastBlockedError) []byte {
	if err == nil {
		return nil
	}
	eventID := newOpenAIFastPolicyWSEventID()
	payload, mErr := json.Marshal(map[string]any{
		"event_id": eventID,
		"type":     "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "policy_violation",
			"message": err.Message,
		},
	})
	if mErr != nil {
		// Fallback to a minimal hand-rolled payload; Marshal of the literal
		// shape above should never fail in practice.
		return []byte(`{"event_id":"` + eventID + `","type":"error","error":{"type":"invalid_request_error","code":"policy_violation","message":"openai fast policy blocked this request"}}`)
	}
	return payload
}

func openAIRequestBodyMayContainImageInput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	input := gjson.GetBytes(body, "input")
	messages := gjson.GetBytes(body, "messages.#-1")
	return openAIJSONValueMayContainImageInput(input) || openAIJSONValueMayContainImageInput(messages)
}

func openAIJSONValueMayContainImageInput(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, item gjson.Result) bool {
			if openAIJSONValueMayContainImageInput(item) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsObject() {
		if strings.TrimSpace(value.Get("type").String()) == "input_image" || value.Get("image_url").Exists() {
			return true
		}
		return openAIJSONValueMayContainImageInput(value.Get("content"))
	}
	return false
}

func openAIRequestBodyMayContainEmptyBase64InputImage(body []byte) bool {
	if len(body) == 0 || !openAIRequestBodyMayContainInputImageToken(body) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	return openAIJSONValueMayContainEmptyBase64InputImage(input)
}

func openAIRequestBodyMayContainInputImageToken(body []byte) bool {
	if bytes.Contains(body, []byte("input_image")) {
		return true
	}
	// JSON 字符串任意字符都可能被 unicode escape，遇到 \u 时交给 gjson 解码后的结构扫描兜底。
	return bytes.Contains(body, []byte("\\u"))
}

func openAIJSONValueMayContainEmptyBase64InputImage(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, item gjson.Result) bool {
			if openAIJSONValueMayContainEmptyBase64InputImage(item) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsObject() {
		if strings.TrimSpace(value.Get("type").String()) == "input_image" && isEmptyBase64DataURI(value.Get("image_url").String()) {
			return true
		}
		return openAIJSONValueMayContainEmptyBase64InputImage(value.Get("content"))
	}
	return false
}

func sanitizeEmptyBase64InputImagesInOpenAIBody(body []byte) ([]byte, bool, error) {
	if !openAIRequestBodyMayContainEmptyBase64InputImage(body) {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("sanitize request body: %w", err)
	}
	if !sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody) {
		return body, false, nil
	}
	normalized, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize sanitized request body: %w", err)
	}
	return normalized, true, nil
}

func sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	input, ok := reqBody["input"]
	if !ok {
		return false
	}
	normalizedInput, changed := sanitizeEmptyBase64InputImagesInOpenAIInput(input)
	if !changed {
		return false
	}
	reqBody["input"] = normalizedInput
	return true
}

func sanitizeEmptyBase64InputImagesInOpenAIInput(input any) (any, bool) {
	items, ok := input.([]any)
	if !ok {
		return input, false
	}

	normalizedItems := make([]any, 0, len(items))
	changed := false
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			normalizedItems = append(normalizedItems, item)
			continue
		}
		if shouldDropEmptyBase64InputImagePart(itemMap) {
			changed = true
			continue
		}
		content, ok := itemMap["content"]
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}
		parts, ok := content.([]any)
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}

		normalizedParts := make([]any, 0, len(parts))
		itemChanged := false
		for _, part := range parts {
			if shouldDropEmptyBase64InputImagePart(part) {
				changed = true
				itemChanged = true
				continue
			}
			normalizedParts = append(normalizedParts, part)
		}
		if itemChanged {
			if len(normalizedParts) == 0 {
				continue
			}
			itemMap["content"] = normalizedParts
		}
		normalizedItems = append(normalizedItems, itemMap)
	}
	if !changed {
		return input, false
	}
	return normalizedItems, true
}

func shouldDropEmptyBase64InputImagePart(part any) bool {
	partMap, ok := part.(map[string]any)
	if !ok {
		return false
	}
	typeValue, _ := partMap["type"].(string)
	if strings.TrimSpace(typeValue) != "input_image" {
		return false
	}
	imageURL, _ := partMap["image_url"].(string)
	return isEmptyBase64DataURI(imageURL)
}

func isEmptyBase64DataURI(raw string) bool {
	if !strings.HasPrefix(raw, "data:") {
		return false
	}
	rest := strings.TrimPrefix(raw, "data:")
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return false
	}
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "base64,")) == ""
}

func getOpenAIRequestBodyMap(_ *gin.Context, body []byte) (map[string]any, error) {
	var reqBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &reqBody); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	return reqBody, nil
}

func extractOpenAIReasoningEffort(reqBody map[string]any, modelCandidates ...string) *string {
	if value, present := getOpenAIReasoningEffortFromReqBody(reqBody, firstNonEmpty(modelCandidates...)); present {
		if value == "" {
			return nil
		}
		return &value
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func normalizeOpenAIReasoningEffort(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}

	// Normalize separators for "x-high"/"x_high" variants.
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)

	switch value {
	case "none", "minimal":
		return ""
	case "low", "medium", "high":
		return value
	case "xhigh", "extrahigh", "max":
		return "xhigh"
	default:
		// Only store known effort levels for now to keep UI consistent.
		return ""
	}
}

func normalizeOpenAIReasoningEffortForModel(raw, model string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "max") && supportsOpenAIReasoningEffortMax(model) {
		return "max"
	}
	return normalizeOpenAIReasoningEffort(raw)
}

func supportsOpenAIReasoningEffortMax(model string) bool {
	if isOpenAIGPT56Model(model) {
		return true
	}
	normalized := strings.ToLower(lastOpenAIModelSegment(model))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch {
	case strings.HasPrefix(normalized, "deepseek-v4"):
		return true
	case strings.HasPrefix(normalized, "glm-"):
		return true
	case strings.HasPrefix(normalized, "kimi-"), strings.HasPrefix(normalized, "moonshot-"):
		return true
	case normalized == "k3" || strings.HasPrefix(normalized, "k3-"):
		return true
	default:
		return false
	}
}
