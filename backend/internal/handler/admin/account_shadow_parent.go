package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func enrichShadowParentInfo(items []AccountWithConcurrency, parents map[int64]*service.Account) {
	for i := range items {
		account := items[i].Account
		if account == nil || account.ParentAccountID == nil {
			continue
		}
		parent := parents[*account.ParentAccountID]
		if parent == nil {
			continue
		}
		account.ParentEmail = parent.GetCredential("email")
		account.ParentPlanType = parent.GetCredential("plan_type")
		account.ParentSubscriptionExpiresAt = parent.GetCredential("subscription_expires_at")
		account.ParentChatGPTAccountID = parent.GetCredential("chatgpt_account_id")
		account.ParentPrivacyMode = parent.GetExtraString("privacy_mode")
	}
}

func (h *AccountHandler) enrichShadowParents(ctx context.Context, items []AccountWithConcurrency) {
	seen := make(map[int64]struct{})
	for i := range items {
		account := items[i].Account
		if account == nil || account.ParentAccountID == nil {
			continue
		}
		seen[*account.ParentAccountID] = struct{}{}
	}
	if len(seen) == 0 {
		return
	}
	parentIDs := make([]int64, 0, len(seen))
	for parentID := range seen {
		parentIDs = append(parentIDs, parentID)
	}
	parents, err := h.adminService.GetAccountsByIDs(ctx, parentIDs)
	if err != nil {
		return
	}
	byID := make(map[int64]*service.Account, len(parents))
	for _, parent := range parents {
		byID[parent.ID] = parent
	}
	enrichShadowParentInfo(items, byID)
}
