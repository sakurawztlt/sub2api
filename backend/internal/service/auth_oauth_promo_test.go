//go:build unit

package service

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type oauthPromoRepo struct {
	PromoCodeRepository
	code    *PromoCode
	err     error
	lookups []string
	usages  []PromoCodeUsage
}

func (r *oauthPromoRepo) GetByCodeForUpdate(_ context.Context, code string) (*PromoCode, error) {
	r.lookups = append(r.lookups, code)
	return r.code, r.err
}
func (r *oauthPromoRepo) GetUsageByPromoCodeAndUser(context.Context, int64, int64) (*PromoCodeUsage, error) {
	return nil, nil
}
func (r *oauthPromoRepo) CreateUsage(_ context.Context, usage *PromoCodeUsage) error {
	r.usages = append(r.usages, *usage)
	return nil
}
func (r *oauthPromoRepo) IncrementUsedCount(context.Context, int64) error { return nil }

type oauthPromoUserRepo struct {
	userRepoStub
}

func (r *oauthPromoUserRepo) UpdateBalance(_ context.Context, id int64, amount float64) error {
	if r.user.ID != id {
		return ErrUserNotFound
	}
	r.user.Balance += amount
	return nil
}

func TestOAuthSignupPromoNewUserConsumesAndReloadsBalance(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		name := "valid"
		if invalid {
			name = "invalid is fail open"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			users := &oauthPromoUserRepo{userRepoStub: userRepoStub{nextID: 81}}
			promos := &oauthPromoRepo{code: &PromoCode{ID: 9, Code: "WELCOME", Status: PromoCodeStatusActive, BonusAmount: 7}}
			mock.ExpectBegin()
			if invalid {
				promos.err = ErrPromoCodeNotFound
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			svc := newAuthService(&users.userRepoStub, map[string]string{SettingKeyRegistrationEnabled: "true", SettingKeyPromoCodeEnabled: "true"}, nil)
			svc.userRepo = users
			svc.refreshTokenCache = &refreshTokenCacheStub{}
			svc.promoService = NewPromoService(promos, users, nil, client, nil)
			tokens, user, err := svc.LoginOrRegisterOAuthWithTokenPairAndPromoCode(context.Background(), "linuxdo-81@linuxdo-connect.invalid", "new", "", "", " WELCOME ", "linuxdo")
			require.NoError(t, err)
			require.NotNil(t, tokens)
			require.Equal(t, []string{"WELCOME"}, promos.lookups)
			if invalid {
				require.Empty(t, promos.usages)
				require.Equal(t, 3.5, user.Balance)
			} else {
				require.Len(t, promos.usages, 1)
				require.Equal(t, int64(81), promos.usages[0].UserID)
				require.Equal(t, 10.5, user.Balance)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOAuthSignupPromoExistingRaceDisabledAndEmptyNeverConsume(t *testing.T) {
	for _, tc := range []struct {
		name, promo string
		existing    bool
		race        bool
		disabled    bool
	}{
		{"existing login", "WELCOME", true, false, false},
		{"create conflict becomes login", "WELCOME", true, true, false},
		{"disabled promo", "WELCOME", false, false, true},
		{"empty promo", " ", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			users := &userRepoStub{nextID: 81}
			if tc.existing {
				users.user = &User{ID: 30, Email: "linuxdo-81@linuxdo-connect.invalid", Username: "old", Role: RoleUser, Status: StatusActive, Balance: 2}
			}
			if tc.race {
				users.getByEmailMisses = 1
				users.createErr = ErrEmailExists
			}
			settings := map[string]string{SettingKeyRegistrationEnabled: "true", SettingKeyPromoCodeEnabled: "true"}
			if tc.disabled {
				settings[SettingKeyPromoCodeEnabled] = "false"
			}
			svc := newAuthService(users, settings, nil)
			svc.refreshTokenCache = &refreshTokenCacheStub{}
			// Any attempted redemption would panic on its deliberately missing DB.
			svc.promoService = &PromoService{}
			tokens, user, err := svc.LoginOrRegisterOAuthWithTokenPairAndPromoCode(context.Background(), "linuxdo-81@linuxdo-connect.invalid", "new", "", "", tc.promo, "linuxdo")
			require.NoError(t, err)
			require.NotNil(t, tokens)
			require.NotNil(t, user)
			if tc.existing {
				require.Equal(t, int64(30), user.ID)
				require.Equal(t, 2.0, user.Balance)
			}
		})
	}
}

func TestOAuthSignupPromoRetainsRegistrationAndAliasAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, enabled string
		alias         bool
		want          error
	}{
		{"closed registration", "false", false, ErrRegDisabled},
		{"email alias duplicate", "true", true, ErrEmailExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			users := &userRepoStub{nextID: 81, aliasExists: tc.alias}
			svc := newAuthService(users, map[string]string{SettingKeyRegistrationEnabled: tc.enabled, SettingKeyPromoCodeEnabled: "true"}, nil)
			svc.refreshTokenCache = &refreshTokenCacheStub{}
			svc.promoService = &PromoService{}
			_, _, err := svc.LoginOrRegisterOAuthWithTokenPairAndPromoCode(context.Background(), "some.one+oauth@gmail.com", "new", "", "", "WELCOME", "google")
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, users.created)
		})
	}
}

func TestEmailOAuthPromoCreateConflictReturnsNotCreated(t *testing.T) {
	users := &userRepoStub{user: &User{ID: 40, Email: "existing@example.com", Status: StatusActive}, createErr: ErrEmailExists}
	svc := newEmailOAuthAutoAuthService(users, map[string]string{SettingKeyRegistrationEnabled: "true"}, nil)
	user, created, err := svc.createEmailOAuthUserWithCreationResult(context.Background(), "existing@example.com", "oauth", "github", "", "")
	require.NoError(t, err)
	require.Equal(t, int64(40), user.ID)
	require.False(t, created)
}
