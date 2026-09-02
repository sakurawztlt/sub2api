package middleware

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// StepUpAuthMiddleware is the gate used for sensitive admin operations.
type StepUpAuthMiddleware gin.HandlerFunc

type stepUpGrantChecker interface {
	HasStepUpGrant(ctx context.Context, userID int64, sessionKey string) (bool, error)
}

type stepUpUserReader interface {
	GetByID(ctx context.Context, id int64) (*service.User, error)
}

type stepUpSettingReader interface {
	IsStepUpEnabled(ctx context.Context) bool
}

// StepUpSessionKey binds a grant to the current refresh-token family when one
// is available. Older tokens fall back to a user-scoped key.
func StepUpSessionKey(c *gin.Context, userID int64) string {
	if sid := c.GetString(ContextKeySessionID); sid != "" {
		return sid
	}
	return fmt.Sprintf("u%d", userID)
}

func NewStepUpAuthMiddleware(
	totpService *service.TotpService,
	userService *service.UserService,
	settingService *service.SettingService,
) StepUpAuthMiddleware {
	return StepUpAuthMiddleware(stepUpAuth(totpService, userService, stepUpSettingsOrNil(settingService)))
}

func stepUpSettingsOrNil(settingService *service.SettingService) stepUpSettingReader {
	if settingService == nil {
		return nil
	}
	return settingService
}

func stepUpAuth(grantChecker stepUpGrantChecker, userReader stepUpUserReader, settings stepUpSettingReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enforceStepUp(c, grantChecker, userReader, settings) {
			return
		}
		c.Next()
	}
}

// EnforceStepUp applies the configured gate from handlers that need to decide
// conditionally based on the request body.
func EnforceStepUp(
	c *gin.Context,
	totpService *service.TotpService,
	userService *service.UserService,
	settingService *service.SettingService,
) bool {
	return enforceStepUp(c, totpService, userService, stepUpSettingsOrNil(settingService))
}

// EnforceStepUpAlways applies the gate without re-reading the feature switch.
func EnforceStepUpAlways(
	c *gin.Context,
	totpService *service.TotpService,
	userService *service.UserService,
) bool {
	return enforceStepUp(c, totpService, userService, nil)
}

func enforceStepUp(c *gin.Context, grantChecker stepUpGrantChecker, userReader stepUpUserReader, settings stepUpSettingReader) bool {
	if settings != nil && !settings.IsStepUpEnabled(c.Request.Context()) {
		return true
	}

	if c.GetString("auth_method") == service.AuditAuthMethodAdminAPIKey {
		AbortWithError(c, 403, "STEP_UP_ADMIN_API_KEY_FORBIDDEN",
			"Admin API key cannot access this endpoint; a two-factor verified admin session is required")
		return false
	}

	subject, ok := GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		AbortWithError(c, 401, "UNAUTHORIZED", "Authorization required")
		return false
	}

	user, err := userReader.GetByID(c.Request.Context(), subject.UserID)
	if err != nil {
		AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to load user")
		return false
	}
	if !user.TotpEnabled {
		AbortWithError(c, 403, "STEP_UP_TOTP_NOT_ENABLED",
			"This operation requires two-factor authentication; please enable TOTP first")
		return false
	}

	sessionKey := StepUpSessionKey(c, subject.UserID)
	granted, err := grantChecker.HasStepUpGrant(c.Request.Context(), subject.UserID, sessionKey)
	if err != nil {
		AbortWithError(c, 503, "STEP_UP_UNAVAILABLE", "Step-up verification service unavailable")
		return false
	}
	if !granted {
		AbortWithError(c, 403, "STEP_UP_REQUIRED", "This operation requires recent two-factor verification")
		return false
	}

	return true
}
