package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	defaultCodexAccountsPerProxy = 4
	defaultCodexConcurrency      = 3
	defaultCodexPriority         = 50
	defaultCodexNameTemplate     = "codex-{batch}-{index}"
	codexImportSource            = "codex_bulk"
)

type CodexBulkImportRequest struct {
	BatchID              string   `json:"batch_id"`
	NameTemplate         string   `json:"name_template"`
	RefreshTokens        []string `json:"refresh_tokens" binding:"required,min=1"`
	ProxyPoolIDs         []int64  `json:"proxy_pool_ids"`
	AccountsPerProxy     int      `json:"accounts_per_proxy"`
	GroupIDs             []int64  `json:"group_ids"`
	Concurrency          int      `json:"concurrency"`
	Priority             *int     `json:"priority"`
	Notes                *string  `json:"notes"`
	RateMultiplier       *float64 `json:"rate_multiplier"`
	LoadFactor           *int     `json:"load_factor"`
	SkipDefaultGroupBind bool     `json:"skip_default_group_bind"`
}

type codexParsedToken struct {
	LineNo int
	Token  string
	Hint   string
}

type codexProxyCandidate struct {
	Proxy             service.ProxyWithAccountCount
	RemainingCapacity int
	AssignedCount     int
}

type CodexBulkImportSummary struct {
	RequestedCount     int `json:"requested_count"`
	ParsedCount        int `json:"parsed_count"`
	CreatedCount       int `json:"created_count,omitempty"`
	FailedCount        int `json:"failed_count"`
	SelectedProxyCount int `json:"selected_proxy_count"`
	EligibleProxyCount int `json:"eligible_proxy_count"`
	AccountsPerProxy   int `json:"accounts_per_proxy"`
	TotalCapacity      int `json:"total_capacity"`
	RemainingCapacity  int `json:"remaining_capacity"`
}

type CodexBulkImportItemResult struct {
	LineNo    int    `json:"line_no"`
	TokenHint string `json:"token_hint"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	ProxyID   *int64 `json:"proxy_id,omitempty"`
	ProxyName string `json:"proxy_name,omitempty"`
	AccountID *int64 `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
}

type CodexBulkImportProxyAllocation struct {
	ProxyID             int64  `json:"proxy_id"`
	ProxyName           string `json:"proxy_name"`
	AccountCount        int64  `json:"account_count"`
	AllocatableCapacity int    `json:"allocatable_capacity"`
	AssignedCount       int    `json:"assigned_count"`
	TotalAfterImport    int64  `json:"total_after_import"`
	QualityStatus       string `json:"quality_status,omitempty"`
	QualityGrade        string `json:"quality_grade,omitempty"`
	QualityScore        *int   `json:"quality_score,omitempty"`
	LatencyStatus       string `json:"latency_status,omitempty"`
}

type CodexBulkImportResult struct {
	BatchID          string                           `json:"batch_id"`
	Summary          CodexBulkImportSummary           `json:"summary"`
	Items            []CodexBulkImportItemResult      `json:"items"`
	ProxyAllocations []CodexBulkImportProxyAllocation `json:"proxy_allocations"`
	Accounts         []*dto.Account                   `json:"accounts,omitempty"`
}

func (h *OpenAIOAuthHandler) CodexBulkImport(c *gin.Context) {
	var req CodexBulkImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	executeAdminIdempotentJSON(c, "admin.openai.codex.bulk_import", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.executeCodexBulkImport(ctx, req)
	})
}

func (h *OpenAIOAuthHandler) executeCodexBulkImport(ctx context.Context, req CodexBulkImportRequest) (*CodexBulkImportResult, error) {
	if h.openaiOAuthService == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "OPENAI_OAUTH_SERVICE_UNAVAILABLE", "openai oauth service unavailable")
	}

	normalizedReq, parsedTokens, err := normalizeCodexBulkImportRequest(req)
	if err != nil {
		return nil, err
	}

	proxyPool, candidates, err := h.prepareCodexProxyPool(ctx, normalizedReq.ProxyPoolIDs, normalizedReq.AccountsPerProxy)
	if err != nil {
		return nil, err
	}

	result := &CodexBulkImportResult{
		BatchID:  normalizedReq.BatchID,
		Items:    make([]CodexBulkImportItemResult, 0, len(parsedTokens)),
		Accounts: make([]*dto.Account, 0, len(parsedTokens)),
	}

	clientID, _ := openai.OAuthClientConfigByPlatform(service.PlatformOpenAI)
	seenTokens := make(map[string]struct{}, len(parsedTokens))
	createdCount := 0
	failedCount := 0

	for index, parsed := range parsedTokens {
		accountName := renderCodexAccountName(normalizedReq.NameTemplate, normalizedReq.BatchID, index+1, len(parsedTokens))
		item := CodexBulkImportItemResult{
			LineNo:    parsed.LineNo,
			TokenHint: parsed.Hint,
			Name:      accountName,
			Status:    "failed",
		}

		if _, exists := seenTokens[parsed.Token]; exists {
			item.Reason = "duplicate refresh token in request"
			failedCount++
			result.Items = append(result.Items, item)
			continue
		}
		seenTokens[parsed.Token] = struct{}{}

		candidateIdx, proxyURL, proxyRef, err := pickCodexProxyCandidate(candidates)
		if err != nil {
			item.Reason = err.Error()
			failedCount++
			result.Items = append(result.Items, item)
			continue
		}

		tokenInfo, err := h.openaiOAuthService.RefreshTokenWithClientID(ctx, parsed.Token, proxyURL, clientID)
		if err != nil {
			item.ProxyID = &proxyRef.ID
			item.ProxyName = proxyRef.Name
			item.Reason = codexImportErrorMessage(err)
			failedCount++
			result.Items = append(result.Items, item)
			continue
		}

		item.ProxyID = &proxyRef.ID
		item.ProxyName = proxyRef.Name
		item.Email = tokenInfo.Email
		item.PlanType = tokenInfo.PlanType

		account, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
			Name:                 accountName,
			Notes:                normalizedReq.Notes,
			Platform:             service.PlatformOpenAI,
			Type:                 service.AccountTypeOAuth,
			Credentials:          buildCodexBulkCredentials(h.openaiOAuthService.BuildAccountCredentials(tokenInfo), parsed.Token),
			Extra:                buildCodexBulkExtra(tokenInfo, normalizedReq.BatchID),
			ProxyID:              &proxyRef.ID,
			Concurrency:          normalizedReq.Concurrency,
			Priority:             *normalizedReq.Priority,
			RateMultiplier:       normalizedReq.RateMultiplier,
			LoadFactor:           normalizedReq.LoadFactor,
			GroupIDs:             normalizedReq.GroupIDs,
			SkipDefaultGroupBind: normalizedReq.SkipDefaultGroupBind,
		})
		if err != nil {
			item.Reason = codexImportErrorMessage(err)
			failedCount++
			result.Items = append(result.Items, item)
			continue
		}

		candidates[candidateIdx].RemainingCapacity--
		candidates[candidateIdx].AssignedCount++
		item.Status = "created"
		item.AccountID = &account.ID
		createdCount++
		result.Items = append(result.Items, item)
		result.Accounts = append(result.Accounts, dto.AccountFromService(account))
	}

	totalCapacity := 0
	remainingCapacity := 0
	proxyAllocations := make([]CodexBulkImportProxyAllocation, 0, len(candidates))
	for i := range candidates {
		candidate := candidates[i]
		totalCapacity += candidate.RemainingCapacity + candidate.AssignedCount
		remainingCapacity += candidate.RemainingCapacity
		proxyAllocations = append(proxyAllocations, CodexBulkImportProxyAllocation{
			ProxyID:             candidate.Proxy.ID,
			ProxyName:           candidate.Proxy.Name,
			AccountCount:        candidate.Proxy.AccountCount,
			AllocatableCapacity: candidate.RemainingCapacity + candidate.AssignedCount,
			AssignedCount:       candidate.AssignedCount,
			TotalAfterImport:    candidate.Proxy.AccountCount + int64(candidate.AssignedCount),
			QualityStatus:       candidate.Proxy.QualityStatus,
			QualityGrade:        candidate.Proxy.QualityGrade,
			QualityScore:        candidate.Proxy.QualityScore,
			LatencyStatus:       candidate.Proxy.LatencyStatus,
		})
	}

	result.ProxyAllocations = proxyAllocations
	result.Summary = CodexBulkImportSummary{
		RequestedCount:     len(req.RefreshTokens),
		ParsedCount:        len(parsedTokens),
		CreatedCount:       createdCount,
		FailedCount:        failedCount,
		SelectedProxyCount: len(proxyPool),
		EligibleProxyCount: len(candidates),
		AccountsPerProxy:   normalizedReq.AccountsPerProxy,
		TotalCapacity:      totalCapacity,
		RemainingCapacity:  remainingCapacity,
	}

	return result, nil
}

func normalizeCodexBulkImportRequest(req CodexBulkImportRequest) (CodexBulkImportRequest, []codexParsedToken, error) {
	req.BatchID = normalizeCodexBatchID(req.BatchID)
	req.NameTemplate = strings.TrimSpace(req.NameTemplate)
	if req.NameTemplate == "" {
		req.NameTemplate = defaultCodexNameTemplate
	}
	if req.AccountsPerProxy <= 0 {
		req.AccountsPerProxy = defaultCodexAccountsPerProxy
	}
	if req.AccountsPerProxy > 1000 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_ACCOUNTS_PER_PROXY_INVALID", "accounts_per_proxy must be <= 1000")
	}
	if req.Concurrency <= 0 {
		req.Concurrency = defaultCodexConcurrency
	}
	if req.Priority == nil {
		priority := defaultCodexPriority
		req.Priority = &priority
	}
	if req.Concurrency > 10000 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_CONCURRENCY_INVALID", "concurrency must be <= 10000")
	}
	if *req.Priority < 0 || *req.Priority > 100000 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_PRIORITY_INVALID", "priority must be between 0 and 100000")
	}
	if req.RateMultiplier != nil && *req.RateMultiplier < 0 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_RATE_MULTIPLIER_INVALID", "rate_multiplier must be >= 0")
	}
	if req.LoadFactor != nil {
		if *req.LoadFactor <= 0 || *req.LoadFactor > 10000 {
			return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_LOAD_FACTOR_INVALID", "load_factor must be between 1 and 10000")
		}
	}
	if req.Notes != nil {
		trimmed := strings.TrimSpace(*req.Notes)
		req.Notes = &trimmed
		if trimmed == "" {
			req.Notes = nil
		}
	}

	parsedTokens := parseCodexRefreshTokens(req.RefreshTokens)
	if len(parsedTokens) == 0 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_REFRESH_TOKENS_REQUIRED", "refresh_tokens must contain at least one non-empty value")
	}
	if len(parsedTokens) > 1000 {
		return req, nil, infraerrors.New(http.StatusBadRequest, "CODEX_REFRESH_TOKENS_TOO_MANY", "refresh_tokens must be <= 1000 entries")
	}
	return req, parsedTokens, nil
}

func parseCodexRefreshTokens(input []string) []codexParsedToken {
	parsed := make([]codexParsedToken, 0, len(input))
	for idx, raw := range input {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parsed = append(parsed, codexParsedToken{
			LineNo: idx + 1,
			Token:  trimmed,
			Hint:   maskCodexToken(trimmed),
		})
	}
	return parsed
}

func normalizeCodexBatchID(batchID string) string {
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		return time.Now().UTC().Format("20060102-150405")
	}
	var b strings.Builder
	for _, r := range batchID {
		switch {
		case r >= 'a' && r <= 'z':
			_, _ = b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			_, _ = b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			_, _ = b.WriteRune(r)
		case r == '-' || r == '_':
			_, _ = b.WriteRune(r)
		}
	}
	normalized := strings.Trim(strings.ReplaceAll(b.String(), "_", "-"), "-")
	if normalized == "" {
		return time.Now().UTC().Format("20060102-150405")
	}
	return normalized
}

func renderCodexAccountName(template, batchID string, index, total int) string {
	width := len(strconv.Itoa(total))
	if width < 3 {
		width = 3
	}
	indexValue := fmt.Sprintf("%0*d", width, index)
	containsIndexPlaceholder := strings.Contains(template, "{index}")
	rendered := strings.TrimSpace(template)
	if rendered == "" {
		rendered = defaultCodexNameTemplate
		containsIndexPlaceholder = true
	}
	rendered = strings.ReplaceAll(rendered, "{batch}", batchID)
	rendered = strings.ReplaceAll(rendered, "{index}", indexValue)
	if !containsIndexPlaceholder {
		rendered = fmt.Sprintf("%s-%s", strings.TrimRight(rendered, "-"), indexValue)
	}
	return rendered
}

func maskCodexToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 10 {
		return token
	}
	return token[:6] + "..." + token[len(token)-4:]
}

func (h *OpenAIOAuthHandler) prepareCodexProxyPool(ctx context.Context, selectedIDs []int64, accountsPerProxy int) ([]service.ProxyWithAccountCount, []codexProxyCandidate, error) {
	proxies, err := h.adminService.GetAllProxiesWithAccountCount(ctx)
	if err != nil {
		return nil, nil, err
	}

	selectedSet := make(map[int64]struct{}, len(selectedIDs))
	for _, id := range selectedIDs {
		if id > 0 {
			selectedSet[id] = struct{}{}
		}
	}

	selected := make([]service.ProxyWithAccountCount, 0, len(proxies))
	for i := range proxies {
		proxy := proxies[i]
		if len(selectedSet) > 0 {
			if _, ok := selectedSet[proxy.ID]; !ok {
				continue
			}
		}
		selected = append(selected, proxy)
	}

	if len(selected) == 0 {
		return nil, nil, infraerrors.New(http.StatusBadRequest, "CODEX_PROXY_POOL_EMPTY", "no active proxies available in proxy pool")
	}

	candidates := make([]codexProxyCandidate, 0, len(selected))
	for i := range selected {
		proxy := selected[i]
		if !isCodexProxyEligible(proxy) {
			continue
		}
		remaining := accountsPerProxy - int(proxy.AccountCount)
		if remaining <= 0 {
			continue
		}
		candidates = append(candidates, codexProxyCandidate{
			Proxy:             proxy,
			RemainingCapacity: remaining,
		})
	}

	return selected, candidates, nil
}

func isCodexProxyEligible(proxy service.ProxyWithAccountCount) bool {
	if proxy.Status != service.StatusActive {
		return false
	}
	if proxy.LatencyStatus == "failed" {
		return false
	}
	switch proxy.QualityStatus {
	case "failed", "challenge":
		return false
	default:
		return true
	}
}

func pickCodexProxyCandidate(candidates []codexProxyCandidate) (int, string, *service.ProxyWithAccountCount, error) {
	bestIdx := -1
	for idx := range candidates {
		if candidates[idx].RemainingCapacity <= 0 {
			continue
		}
		if bestIdx == -1 || compareCodexProxyCandidates(candidates[idx], candidates[bestIdx]) < 0 {
			bestIdx = idx
		}
	}
	if bestIdx == -1 {
		return -1, "", nil, infraerrors.New(http.StatusBadRequest, "CODEX_PROXY_CAPACITY_EXHAUSTED", "proxy pool capacity exhausted for current accounts_per_proxy setting")
	}
	proxy := candidates[bestIdx].Proxy
	return bestIdx, proxy.URL(), &proxy, nil
}

func compareCodexProxyCandidates(left, right codexProxyCandidate) int {
	leftLoad := left.Proxy.AccountCount + int64(left.AssignedCount)
	rightLoad := right.Proxy.AccountCount + int64(right.AssignedCount)
	if leftLoad != rightLoad {
		if leftLoad < rightLoad {
			return -1
		}
		return 1
	}

	leftScore := -1
	if left.Proxy.QualityScore != nil {
		leftScore = *left.Proxy.QualityScore
	}
	rightScore := -1
	if right.Proxy.QualityScore != nil {
		rightScore = *right.Proxy.QualityScore
	}
	if leftScore != rightScore {
		if leftScore > rightScore {
			return -1
		}
		return 1
	}

	if left.Proxy.ID < right.Proxy.ID {
		return -1
	}
	if left.Proxy.ID > right.Proxy.ID {
		return 1
	}
	return 0
}

func buildCodexBulkExtra(tokenInfo *service.OpenAITokenInfo, batchID string) map[string]any {
	extra := map[string]any{
		"openai_passthrough": true,
		"codex_cli_only":     true,
		"import_source":      codexImportSource,
		"import_batch_id":    batchID,
	}
	if tokenInfo != nil {
		if tokenInfo.Email != "" {
			extra["email_address"] = tokenInfo.Email
		}
		if tokenInfo.PrivacyMode != "" {
			extra["privacy_mode"] = tokenInfo.PrivacyMode
		}
	}
	return extra
}

func buildCodexBulkCredentials(credentials map[string]any, refreshToken string) map[string]any {
	if credentials == nil {
		credentials = make(map[string]any)
	}

	trimmed := strings.TrimSpace(refreshToken)
	if trimmed == "" {
		return credentials
	}

	current, _ := credentials["refresh_token"].(string)
	if strings.TrimSpace(current) == "" {
		credentials["refresh_token"] = trimmed
	}

	return credentials
}

func codexImportErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if msg := strings.TrimSpace(infraerrors.Message(err)); msg != "" {
		return msg
	}
	return err.Error()
}
