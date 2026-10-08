package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLiveSecurityAuditNormalizesOnlyLegacyInstructions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const session = `{"model":"gpt-live-test","instructions":"blocked-live-canary","delegation":{"type":"client"},"input":[{"role":"user","content":"full-transcript-canary"}],"extra_context":{"canary":"raw-context"}}`
	for _, mode := range []string{"no_coordinator", string(securityaudit.ModeOff), string(securityaudit.ModeAsync), string(securityaudit.ModeBlocking)} {
		t.Run(mode, func(t *testing.T) {
			moderationConfig, err := json.Marshal(service.ContentModerationConfig{
				Enabled: true, Mode: service.ContentModerationModePreBlock, AllGroups: true,
				BlockedKeywords:     []string{"blocked-live-canary"},
				KeywordBlockingMode: service.ContentModerationKeywordModeKeywordOnly,
				BlockStatus:         http.StatusForbidden, BlockMessage: "live-block-canary",
			})
			require.NoError(t, err)
			settings := &contentModerationHandlerSettingRepo{values: map[string]string{
				service.SettingKeyRiskControlEnabled:      "true",
				service.SettingKeyContentModerationConfig: string(moderationConfig),
			}}
			legacy := service.NewContentModerationService(settings, &contentModerationHandlerTestRepo{}, nil, nil, nil, nil, nil, nil)
			engine := &handlerPromptEngine{mode: securityaudit.Mode(mode), decision: &securityaudit.PromptDecision{
				Kind: securityaudit.DecisionAllow, AllowNextStage: true,
			}}
			h := &OpenAIGatewayHandler{contentModerationService: legacy}
			if mode != "no_coordinator" {
				h.securityAuditCoordinator = securityaudit.NewCoordinator(securityaudit.NewLegacyModerationAdapter(legacy), engine)
			}
			router := gin.New()
			router.Use(securityAuditMediaTestMiddleware)
			router.Use(func(c *gin.Context) {
				apiKey, ok := middleware2.GetAPIKeyFromContext(c)
				require.True(t, ok)
				apiKey.Group.AllowLive = true
				c.Next()
			})
			router.POST("/v1/live", h.Live)
			request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"sdp":"v=0\r\n","session":`+session+`}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusForbidden, recorder.Code, "legacy instructions must block before billing/upstream work")
			require.Contains(t, recorder.Body.String(), "content_policy_violation")
			require.Contains(t, recorder.Body.String(), "live-block-canary")
			evaluated, enqueued, audited := engine.snapshot()
			if mode == string(securityaudit.ModeAsync) || mode == string(securityaudit.ModeBlocking) {
				require.Len(t, audited, 1)
				require.JSONEq(t, session, string(audited[0].Body), "prompt audit must retain the complete original session")
				require.JSONEq(t, `{"input":"blocked-live-canary"}`, string(audited[0].LegacyBody))
				snapshot, err := securityaudit.ExtractPromptSnapshot(audited[0])
				require.NoError(t, err)
				require.Contains(t, snapshot.ScanText, "blocked-live-canary")
				require.Contains(t, snapshot.ScanText, "full-transcript-canary", "legacy normalization must not narrow prompt audit")
				if mode == string(securityaudit.ModeAsync) {
					require.Equal(t, 1, enqueued)
					require.Zero(t, evaluated)
				} else {
					require.Equal(t, 1, evaluated)
					require.Zero(t, enqueued)
				}
			} else {
				require.Empty(t, audited)
				require.Zero(t, evaluated)
				require.Zero(t, enqueued)
			}
		})
	}
}
