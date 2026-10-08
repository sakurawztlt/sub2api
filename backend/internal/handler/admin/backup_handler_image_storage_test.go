//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type backupImageStorageRepo struct {
	settingHandlerRepoStub
	writes int
}

func (r *backupImageStorageRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	r.writes++
	return nil
}

type backupImageStorageEncryptor struct {
	encryptCalls int
}

func (e *backupImageStorageEncryptor) Encrypt(plain string) (string, error) {
	e.encryptCalls++
	return "encrypted:" + plain, nil
}

func (e *backupImageStorageEncryptor) Decrypt(cipher string) (string, error) {
	if !strings.HasPrefix(cipher, "encrypted:") {
		return "", errors.New("invalid test ciphertext")
	}
	return strings.TrimPrefix(cipher, "encrypted:"), nil
}

func backupImageStorageRouter(s *service.ImageStorageSettingService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewBackupHandler(nil, nil, s)
	r := gin.New()
	r.GET("/image-storage", h.GetImageStorageConfig)
	r.PUT("/image-storage", h.UpdateImageStorageConfig)
	r.POST("/image-storage/test", h.TestImageStorageConnection)
	return r
}

func backupImageStorageRequest(t *testing.T, r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return w, out
}

func TestBackupImageStorageGetRedactsSavedAndFallbackSecrets(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "fallback"
		if saved {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			repo := &backupImageStorageRepo{}
			if saved {
				repo.values = map[string]string{"image_storage_config": `{"enabled":true,"bucket":"saved-bucket","secret_access_key":"encrypted:secret-canary"}`}
			}
			s := service.NewImageStorageSettingService(repo, nil, nil, nil, config.ImageStorageConfig{Enabled: true, Bucket: "fallback-bucket", SecretAccessKey: "fallback-secret-canary"})
			w, out := backupImageStorageRequest(t, backupImageStorageRouter(s), http.MethodGet, "/image-storage", "")
			require.Equal(t, http.StatusOK, w.Code)
			data := out["data"].(map[string]any)
			require.Equal(t, true, data["secret_configured"])
			require.NotContains(t, data["config"].(map[string]any), "secret_access_key")
			require.NotContains(t, w.Body.String(), "secret-canary")
			require.Zero(t, repo.writes)
		})
	}
}

func TestBackupImageStorageUpdateEncryptsPreservesAndRedactsSecret(t *testing.T) {
	repo := &backupImageStorageRepo{}
	encryptor := &backupImageStorageEncryptor{}
	cfg := &config.Config{}
	cfg.Totp.EncryptionKeyConfigured = true
	backup := service.NewBackupService(repo, cfg, encryptor, nil, nil)
	s := service.NewImageStorageSettingService(repo, encryptor, backup, nil, config.ImageStorageConfig{})
	r := backupImageStorageRouter(s)

	w, out := backupImageStorageRequest(t, r, http.MethodPut, "/image-storage", `{"enabled":true,"bucket":" images-bucket ","prefix":" generated ","access_key_id":"access-key","secret_access_key":"secret-canary"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "secret-canary")
	data := out["data"].(map[string]any)
	require.Equal(t, "images-bucket", data["bucket"])
	require.Equal(t, "generated/", data["prefix"])
	require.NotContains(t, data, "secret_access_key")
	var saved service.ImageStorageSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values["image_storage_config"]), &saved))
	require.Equal(t, "encrypted:secret-canary", saved.SecretAccessKey)
	require.Equal(t, 1, encryptor.encryptCalls)

	w, _ = backupImageStorageRequest(t, r, http.MethodPut, "/image-storage", `{"enabled":true,"bucket":"second-bucket","secret_access_key":""}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "secret-canary")
	require.NoError(t, json.Unmarshal([]byte(repo.values["image_storage_config"]), &saved))
	require.Equal(t, "encrypted:secret-canary", saved.SecretAccessKey)
	require.Equal(t, 1, encryptor.encryptCalls)
	require.Equal(t, 2, repo.writes)
}

func TestBackupImageStorageUpdateRequiresFixedEncryptionKey(t *testing.T) {
	repo := &backupImageStorageRepo{}
	encryptor := &backupImageStorageEncryptor{}
	s := service.NewImageStorageSettingService(repo, encryptor, nil, nil, config.ImageStorageConfig{})
	w, out := backupImageStorageRequest(t, backupImageStorageRouter(s), http.MethodPut, "/image-storage", `{"enabled":true,"secret_access_key":"secret-canary"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, "SECRET_ENCRYPTION_KEY_NOT_CONFIGURED", out["reason"])
	require.NotContains(t, w.Body.String(), "secret-canary")
	require.Zero(t, encryptor.encryptCalls)
	require.Zero(t, repo.writes)
}

func TestBackupImageStorageConnectionUsesSavedSecretAndReportsResult(t *testing.T) {
	for _, connectionErr := range []error{nil, errors.New("object storage is unavailable")} {
		name := "success"
		if connectionErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			repo := &backupImageStorageRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{"image_storage_config": `{"secret_access_key":"encrypted:secret-canary"}`}}}
			calls := 0
			factory := func(_ context.Context, cfg *config.ImageStorageConfig) (service.ImageStorage, error) {
				calls++
				require.Equal(t, "secret-canary", cfg.SecretAccessKey)
				require.Equal(t, "images-bucket", cfg.Bucket)
				return nil, connectionErr
			}
			s := service.NewImageStorageSettingService(repo, &backupImageStorageEncryptor{}, nil, factory, config.ImageStorageConfig{})
			w, out := backupImageStorageRequest(t, backupImageStorageRouter(s), http.MethodPost, "/image-storage/test", `{"enabled":true,"bucket":"images-bucket","access_key_id":"access-key","secret_access_key":""}`)
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, connectionErr == nil, out["data"].(map[string]any)["ok"])
			require.Equal(t, 1, calls)
			require.NotContains(t, w.Body.String(), "secret-canary")
			require.Zero(t, repo.writes)
		})
	}
}

func TestBackupImageStorageRejectsInvalidJSON(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			path := "/image-storage"
			if method == http.MethodPost {
				path += "/test"
			}
			w, _ := backupImageStorageRequest(t, backupImageStorageRouter(nil), method, path, "{")
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}
