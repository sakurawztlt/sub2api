package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type bindingTestSettings struct{ SettingRepository }

func (bindingTestSettings) GetValue(_ context.Context, key string) (string, error) {
	if key == SettingKeySessionBindingEnabled {
		return "true", nil
	}
	return "", ErrSettingNotFound
}

type bindingTestUsers struct {
	UserRepository
	user *User
}

func (r bindingTestUsers) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type bindingTestCache struct {
	RefreshTokenCache
	data          map[string]*RefreshTokenData
	revokedFamily string
}

func (r *bindingTestCache) StoreRefreshToken(_ context.Context, hash string, data *RefreshTokenData, _ time.Duration) error {
	copied := *data
	r.data[hash] = &copied
	return nil
}
func (r *bindingTestCache) GetRefreshToken(_ context.Context, hash string) (*RefreshTokenData, error) {
	data := r.data[hash]
	if data == nil {
		return nil, ErrRefreshTokenNotFound
	}
	copied := *data
	return &copied, nil
}
func (r *bindingTestCache) DeleteRefreshToken(_ context.Context, hash string) error {
	delete(r.data, hash)
	return nil
}
func (r *bindingTestCache) DeleteTokenFamily(_ context.Context, family string) error {
	r.revokedFamily = family
	for hash, data := range r.data {
		if data.FamilyID == family {
			delete(r.data, hash)
		}
	}
	return nil
}
func (*bindingTestCache) AddToUserTokenSet(context.Context, int64, string, time.Duration) error {
	return nil
}
func (*bindingTestCache) AddToFamilyTokenSet(context.Context, string, string, time.Duration) error {
	return nil
}

func TestAuthSessionBindingRotationAndMismatchRevocation(t *testing.T) {
	user := &User{ID: 42, Email: "binding@example.com", Status: StatusActive}
	cache := &bindingTestCache{data: map[string]*RefreshTokenData{}}
	svc := &AuthService{cfg: &config.Config{JWT: config.JWTConfig{Secret: "binding-test-secret", ExpireHour: 1, RefreshTokenExpireDays: 1}}, userRepo: bindingTestUsers{user: user}, refreshTokenCache: cache, settingService: NewSettingService(bindingTestSettings{}, &config.Config{})}
	binding := &SessionBinding{IP: "192.0.2.4", UserAgent: "test-client"}
	ctx := WithSessionBinding(context.Background(), binding)
	pair, err := svc.GenerateTokenPair(ctx, user, "")
	require.NoError(t, err)
	claims, err := svc.ValidateToken(pair.AccessToken)
	require.NoError(t, err)
	require.NotEmpty(t, claims.SessionID)
	require.Equal(t, binding.Hash(), claims.BindingHash)
	require.Equal(t, claims.SessionID, cache.data[hashToken(pair.RefreshToken)].FamilyID)

	rotated, err := svc.RefreshTokenPair(ctx, pair.RefreshToken)
	require.NoError(t, err)
	require.NotContains(t, cache.data, hashToken(pair.RefreshToken))
	rotatedClaims, err := svc.ValidateToken(rotated.AccessToken)
	require.NoError(t, err)
	require.Equal(t, claims.SessionID, rotatedClaims.SessionID)

	changed := WithSessionBinding(context.Background(), &SessionBinding{IP: "192.0.2.5", UserAgent: binding.UserAgent})
	_, err = svc.RefreshTokenPair(changed, rotated.RefreshToken)
	require.ErrorIs(t, err, ErrSessionBindingMismatch)
	require.Equal(t, claims.SessionID, cache.revokedFamily)
	require.Empty(t, cache.data)

	// Existing unbound refresh tokens upgrade on successful rotation.
	legacy, err := svc.GenerateTokenPair(context.Background(), user, "")
	require.NoError(t, err)
	upgraded, err := svc.RefreshTokenPair(ctx, legacy.RefreshToken)
	require.NoError(t, err)
	upgradedClaims, err := svc.ValidateToken(upgraded.AccessToken)
	require.NoError(t, err)
	require.Equal(t, binding.Hash(), upgradedClaims.BindingHash)
}
