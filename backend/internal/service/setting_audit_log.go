package service

import (
	"context"
	"strconv"
	"strings"
)

const (
	SettingKeyAuditLogRetentionDays = "audit_log_retention_days"
	defaultAuditLogRetentionDays    = 180
)

// GetAuditLogRetentionDays returns the retention window. Values <= 0 mean
// permanent retention until an administrator clears the audit log manually.
func (s *SettingService) GetAuditLogRetentionDays(ctx context.Context) int {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyAuditLogRetentionDays)
	if err != nil {
		return defaultAuditLogRetentionDays
	}
	return parseAuditLogRetentionDays(value)
}

func parseAuditLogRetentionDays(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultAuditLogRetentionDays
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return defaultAuditLogRetentionDays
	}
	if n < 0 {
		return 0
	}
	return n
}
