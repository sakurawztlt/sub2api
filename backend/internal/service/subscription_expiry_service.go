package service

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
)

const (
	// subscriptionExpiryLeaderLockKey gates the periodic expired-subscription
	// status sweep so only one instance updates rows per cycle.
	subscriptionExpiryLeaderLockKey = "subscription:expiry:leader"
	subscriptionExpiryLeaderLockTTL = 2 * time.Minute
)

// SubscriptionExpiryService periodically updates expired subscription status.
type SubscriptionExpiryService struct {
	userSubRepo UserSubscriptionRepository
	interval    time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup

	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	settingRepo              SettingRepository
	notificationEmailService *NotificationEmailService
}

func NewSubscriptionExpiryService(userSubRepo UserSubscriptionRepository, interval time.Duration) *SubscriptionExpiryService {
	return &SubscriptionExpiryService{
		userSubRepo: userSubRepo,
		interval:    interval,
		stopCh:      make(chan struct{}),
		instanceID:  uuid.NewString(),
	}
}

// SetLeaderLock injects the leader-lock cache and DB used to elect a single
// instance for the periodic expired-subscription sweep. When both are nil it runs
// ungated (single-instance / test behavior).
func (s *SubscriptionExpiryService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

func (s *SubscriptionExpiryService) SetSettingRepository(settingRepo SettingRepository) {
	if s == nil {
		return
	}
	s.settingRepo = settingRepo
}

func (s *SubscriptionExpiryService) SetNotificationEmailService(notificationEmailService *NotificationEmailService) {
	if s == nil {
		return
	}
	s.notificationEmailService = notificationEmailService
}

func (s *SubscriptionExpiryService) expiryReminderEnabled(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return true
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeySubscriptionExpiryNotifyEnabled)
	if err == nil {
		return !isFalseSettingValue(value)
	}
	if err == ErrSettingNotFound {
		return true
	}
	return false
}

func (s *SubscriptionExpiryService) sendExpiryReminders(ctx context.Context) {
	if s == nil || !s.expiryReminderEnabled(ctx) || s.userSubRepo == nil || s.notificationEmailService == nil {
		return
	}
	_, _, err := s.userSubRepo.List(ctx, pagination.PaginationParams{Page: 1, PageSize: 100}, nil, nil, SubscriptionStatusActive, "", "expires_at", "asc")
	if err != nil {
		log.Printf("[SubscriptionExpiry] Scan expiry reminders failed: %v", err)
	}
}

func (s *SubscriptionExpiryService) Start() {
	if s == nil || s.userSubRepo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *SubscriptionExpiryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *SubscriptionExpiryService) runOnce() {
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 2*time.Second)
	release, ok := tryAcquireSingletonLeaderLock(lockCtx, s.lockCache, s.db, subscriptionExpiryLeaderLockKey, s.instanceID, subscriptionExpiryLeaderLockTTL)
	lockCancel()
	if !ok {
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	updated, err := s.userSubRepo.BatchUpdateExpiredStatus(ctx)
	if err != nil {
		log.Printf("[SubscriptionExpiry] Update expired subscriptions failed: %v", err)
		return
	}
	if updated > 0 {
		log.Printf("[SubscriptionExpiry] Updated %d expired subscriptions", updated)
	}
}
