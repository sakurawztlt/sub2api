package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	defaultGrokOAuthReconcilePageSize = 50
	maxGrokOAuthReconcilePageSize     = 500
	maxGrokOAuthReconcileWindow       = 24 * time.Hour

	GrokOAuthReconcileReasonMissingRefreshToken = "missing_refresh_token"
	GrokOAuthReconcileReasonMissingAccessToken  = "missing_access_token"
	GrokOAuthReconcileReasonMissingExpiry       = "missing_expiry"
	GrokOAuthReconcileReasonInvalidExpiry       = "invalid_expiry"
	GrokOAuthReconcileReasonNearExpiry          = "near_expiry"
	GrokOAuthReconcileReasonCredentialRejected  = "credential_rejected"

	GrokOAuthReconcileActionBlock   = "block_account"
	GrokOAuthReconcileActionRefresh = "refresh_credentials"

	GrokOAuthReconcileOutcomePlanned = "planned"
	GrokOAuthReconcileOutcomeApplied = "applied"
	GrokOAuthReconcileOutcomeSkipped = "skipped"
	GrokOAuthReconcileOutcomeFailed  = "failed"
	GrokOAuthReconcileOutcomePartial = "partial"
)

var (
	ErrGrokOAuthReconcileMode = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_MODE_INVALID",
		"apply requires dry_run=false and apply=true",
	)
	ErrGrokOAuthReconcileCursor = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_CURSOR_INVALID",
		"after_id must be non-negative",
	)
	ErrGrokOAuthReconcileLimit = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_LIMIT_INVALID",
		"limit is outside the allowed reconciliation page range",
	)
	ErrGrokOAuthReconcileWindow = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_WINDOW_INVALID",
		"refresh_window_seconds is outside the allowed range",
	)
)

// GrokOAuthReconciler is the narrow admin-facing reconciliation port.
type GrokOAuthReconciler interface {
	ReconcileGrokOAuth(ctx context.Context, input GrokOAuthReconcileInput) (*GrokOAuthReconcileResult, error)
}

// GrokOAuthConditionalErrorRepository is the compare-and-set quarantine used
// for structurally invalid credentials. It must match the complete credential
// document observed immediately before mutation.
type GrokOAuthConditionalErrorRepository interface {
	SetGrokOAuthErrorIfCredentialsUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, errorMsg string) (bool, error)
}

type GrokOAuthReconcileInput struct {
	DryRun        bool
	Apply         bool
	AfterID       int64
	Limit         int
	RefreshWindow time.Duration
}

// GrokOAuthReconcileItem is deliberately metadata-only. Credentials, account
// identity fields, provider response bodies, and raw errors never cross this API.
type GrokOAuthReconcileItem struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
	Action    string `json:"action"`
	Outcome   string `json:"outcome"`
}

type GrokOAuthReconcileResult struct {
	DryRun       bool                     `json:"dry_run"`
	Scanned      int                      `json:"scanned"`
	Actionable   int                      `json:"actionable"`
	WouldBlock   int                      `json:"would_block"`
	WouldRefresh int                      `json:"would_refresh"`
	Blocked      int                      `json:"blocked"`
	Refreshed    int                      `json:"refreshed"`
	Skipped      int                      `json:"skipped"`
	Failed       int                      `json:"failed"`
	Partial      int                      `json:"partial"`
	Items        []GrokOAuthReconcileItem `json:"items"`
	NextAfterID  int64                    `json:"next_after_id"`
	HasMore      bool                     `json:"has_more"`
}

func (s *TokenRefreshService) ReconcileGrokOAuth(ctx context.Context, input GrokOAuthReconcileInput) (*GrokOAuthReconcileResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("grok OAuth reconciliation service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if input.Apply && input.DryRun {
		return nil, ErrGrokOAuthReconcileMode
	}
	if input.AfterID < 0 {
		return nil, ErrGrokOAuthReconcileCursor
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultGrokOAuthReconcilePageSize
	}
	if limit < 1 || limit > maxGrokOAuthReconcilePageSize {
		return nil, ErrGrokOAuthReconcileLimit
	}
	refreshWindow := input.RefreshWindow
	if refreshWindow == 0 {
		refreshWindow = grokTokenRefreshSkew
	}
	if refreshWindow < 0 || refreshWindow > maxGrokOAuthReconcileWindow {
		return nil, ErrGrokOAuthReconcileWindow
	}
	if refreshWindow < grokTokenRefreshSkew {
		refreshWindow = grokTokenRefreshSkew
	}
	dryRun := !input.Apply

	pager, ok := s.accountRepo.(OAuthRefreshCandidatePager)
	if !ok {
		return nil, errors.New("OAuth refresh candidate pager is not configured")
	}
	page, err := pager.ListOAuthRefreshCandidatePage(ctx, OAuthRefreshPageOptions{
		Platforms:  []string{PlatformGrok},
		AfterID:    input.AfterID,
		Limit:      limit,
		ActiveOnly: true,
		// Missing refresh tokens must remain visible so reconciliation can
		// quarantine them through the exact-credential CAS below.
		IncludeSetupToken:   false,
		RequireRefreshToken: false,
	})
	if err != nil {
		return nil, err
	}
	if page == nil {
		return nil, errors.New("OAuth reconciliation repository returned a nil cursor page")
	}
	if !isStrictlyIncreasingGrokOAuthPage(page.Accounts, input.AfterID) {
		return nil, errors.New("OAuth reconciliation repository returned an invalid cursor page")
	}

	result := &GrokOAuthReconcileResult{
		DryRun:  dryRun,
		Scanned: len(page.Accounts),
		Items:   make([]GrokOAuthReconcileItem, 0, len(page.Accounts)),
		HasMore: page.HasMore,
	}
	if page.HasMore {
		if page.NextAfterID <= input.AfterID {
			return nil, errors.New("OAuth reconciliation repository returned invalid cursor metadata")
		}
		result.NextAfterID = page.NextAfterID
	}

	refresher, executor, ok := s.grokOAuthRefreshPair()
	if !ok {
		return nil, errors.New("grok OAuth refresher is not registered")
	}
	conditionalErrorRepo, supportsConditionalError := s.accountRepo.(GrokOAuthConditionalErrorRepository)
	if input.Apply && !supportsConditionalError {
		return nil, errors.New("grok OAuth conditional error mutation is not configured")
	}

	for i := range page.Accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		account := &page.Accounts[i]
		reason, action, actionable := classifyGrokOAuthReconcileAccount(account, refreshWindow)
		if !actionable {
			result.Skipped++
			continue
		}
		result.Actionable++
		item := GrokOAuthReconcileItem{
			AccountID: account.ID,
			Reason:    reason,
			Action:    action,
			Outcome:   GrokOAuthReconcileOutcomePlanned,
		}
		if action == GrokOAuthReconcileActionBlock {
			result.WouldBlock++
		} else {
			result.WouldRefresh++
		}
		if dryRun {
			result.Items = append(result.Items, item)
			continue
		}

		switch action {
		case GrokOAuthReconcileActionBlock:
			latest, readErr := s.accountRepo.GetByID(ctx, account.ID)
			if readErr != nil || latest == nil {
				item.Outcome = GrokOAuthReconcileOutcomeFailed
				result.Failed++
				break
			}
			latestReason, latestAction, stillActionable := classifyGrokOAuthReconcileAccount(latest, refreshWindow)
			if !stillActionable || latestAction != GrokOAuthReconcileActionBlock {
				item.Outcome = GrokOAuthReconcileOutcomeSkipped
				result.Skipped++
				break
			}
			account = latest
			item.Reason = latestReason
			applied, setErr := conditionalErrorRepo.SetGrokOAuthErrorIfCredentialsUnchanged(
				ctx,
				account.ID,
				account.Credentials,
				"Grok OAuth credential reconciliation: missing refresh token",
			)
			if setErr != nil {
				item.Outcome = GrokOAuthReconcileOutcomeFailed
				result.Failed++
				break
			}
			if !applied {
				// A concurrent reauthorization won after the final read. Do not
				// install a runtime block or invalidate its fresh token cache.
				item.Outcome = GrokOAuthReconcileOutcomeSkipped
				result.Skipped++
				break
			}
			s.notifyAccountSchedulingBlocked(account, time.Time{}, "grok_oauth_reconcile_invalid")
			account.Status = StatusError
			account.Schedulable = false
			cacheInvalidationFailed := s.cacheInvalidator == nil
			if s.cacheInvalidator != nil {
				if invalidateErr := s.cacheInvalidator.InvalidateToken(ctx, account); invalidateErr != nil {
					cacheInvalidationFailed = true
				}
			}
			result.Blocked++
			if cacheInvalidationFailed {
				item.Outcome = GrokOAuthReconcileOutcomePartial
				result.Partial++
			} else {
				item.Outcome = GrokOAuthReconcileOutcomeApplied
			}
		case GrokOAuthReconcileActionRefresh:
			refreshErr := s.refreshWithRetry(ctx, account, refresher, executor, refreshWindow)
			switch {
			case refreshErr == nil:
				item.Outcome = GrokOAuthReconcileOutcomeApplied
				result.Refreshed++
			case errors.Is(refreshErr, errRefreshSkipped):
				item.Outcome = GrokOAuthReconcileOutcomeSkipped
				result.Skipped++
			case isNonRetryableRefreshError(refreshErr):
				// The established refresh pool quarantines non-retryable
				// credentials and invalidates their token cache before returning
				// the provider error. Reflect that durable action in the admin
				// result instead of misreporting an applied block as a failure.
				item.Reason = GrokOAuthReconcileReasonCredentialRejected
				item.Action = GrokOAuthReconcileActionBlock
				item.Outcome = GrokOAuthReconcileOutcomeApplied
				result.Blocked++
			default:
				item.Outcome = GrokOAuthReconcileOutcomeFailed
				result.Failed++
			}
		default:
			return nil, fmt.Errorf("unsupported Grok OAuth reconciliation action")
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// grokOAuthRefreshPair adapts upstream's provider registry to this branch's
// established parallel refresher/executor slices. The synthetic account keeps
// platform selection explicit without changing the existing pool machinery.
func (s *TokenRefreshService) grokOAuthRefreshPair() (TokenRefresher, OAuthRefreshExecutor, bool) {
	probe := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "reconciliation-registration-probe",
		},
	}
	for i, refresher := range s.refreshers {
		if refresher == nil || !refresher.CanRefresh(probe) {
			continue
		}
		if i >= len(s.executors) || s.executors[i] == nil {
			return nil, nil, false
		}
		return refresher, s.executors[i], true
	}
	return nil, nil, false
}

func isStrictlyIncreasingGrokOAuthPage(accounts []Account, afterID int64) bool {
	previous := afterID
	for i := range accounts {
		if accounts[i].ID <= previous {
			return false
		}
		previous = accounts[i].ID
	}
	return true
}

func classifyGrokOAuthReconcileAccount(account *Account, refreshWindow time.Duration) (reason, action string, actionable bool) {
	if account == nil || !account.IsGrokOAuth() || account.Status != StatusActive {
		return "", "", false
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return GrokOAuthReconcileReasonMissingRefreshToken, GrokOAuthReconcileActionBlock, true
	}
	if strings.TrimSpace(account.GetGrokAccessToken()) == "" {
		return GrokOAuthReconcileReasonMissingAccessToken, GrokOAuthReconcileActionRefresh, true
	}
	rawExpiry := strings.TrimSpace(account.GetCredential("expires_at"))
	if rawExpiry == "" {
		return GrokOAuthReconcileReasonMissingExpiry, GrokOAuthReconcileActionRefresh, true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return GrokOAuthReconcileReasonInvalidExpiry, GrokOAuthReconcileActionRefresh, true
	}
	if time.Until(*expiresAt) <= refreshWindow {
		return GrokOAuthReconcileReasonNearExpiry, GrokOAuthReconcileActionRefresh, true
	}
	return "", "", false
}
