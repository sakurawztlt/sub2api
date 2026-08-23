package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

func profitControlTestGroup(id int64, margin, buffer float64) *Group {
	return &Group{
		ID:                   id,
		Platform:             PlatformOpenAI,
		Status:               StatusActive,
		Hydrated:             true,
		RateMultiplier:       1.0,
		SubscriptionType:     SubscriptionTypeStandard,
		ProfitControlEnabled: true,
		ProfitMinMargin:      margin,
		ProfitSafetyBuffer:   buffer,
	}
}

func profitControlTestCtx(group *Group) context.Context {
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

func profitControlTestAccountWithRate(account *Account, rate float64) *Account {
	account.RateMultiplier = &rate
	return account
}
