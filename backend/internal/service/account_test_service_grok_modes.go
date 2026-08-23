package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func firstAccountTestOptions(opts []AccountTestOptions) AccountTestOptions {
	if len(opts) == 0 {
		return AccountTestOptions{}
	}
	return opts[0]
}

// maxAccountTestMediaBytes caps inbound data-URL payloads for admin tests (~8 MiB).
const maxAccountTestMediaBytes = 8 << 20

const (
	defaultGrokImageTestPrompt = "Generate a cute orange cat astronaut sticker on a clean pastel background."
	defaultGrokVideoTestPrompt = "A red ball bouncing once on a white floor, short simple motion."
	defaultGrokSearchTestQuery = "xAI Grok"
	defaultGrokTTSTestText     = "Hello from Sub2API account connectivity test."

	// Grok account-test modes (admin UI). Empty / default / text = Responses probe.
	// image/video may also be inferred from model_id when mode is default.
	AccountTestModeGrokText     = "text"
	AccountTestModeGrokImage    = "image"
	AccountTestModeGrokVideo    = "video"
	AccountTestModeGrokSearch   = "search"
	AccountTestModeGrokTTS      = "tts"
	AccountTestModeGrokSTT      = "stt"
	AccountTestModeGrokRealtime = "realtime"

	defaultGrokRealtimeTestModel = "grok-voice-latest"
	grokRealtimeProbeTimeout     = DefaultGrokRealtimeDialTimeout
)

func isGrokVideoGenerationModel(model string) bool {
	return isGrokVideoBillingModel(model) ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "grok-video")
}

func normalizeGrokAccountTestMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AccountTestModeGrokText:
		return AccountTestModeGrokText
	case AccountTestModeGrokImage:
		return AccountTestModeGrokImage
	case AccountTestModeGrokVideo:
		return AccountTestModeGrokVideo
	case AccountTestModeGrokSearch:
		return AccountTestModeGrokSearch
	case AccountTestModeGrokTTS:
		return AccountTestModeGrokTTS
	case AccountTestModeGrokSTT:
		return AccountTestModeGrokSTT
	case AccountTestModeGrokRealtime:
		return AccountTestModeGrokRealtime
	default:
		return AccountTestModeDefault
	}
}

func (s *AccountTestService) testGrokAccountConnection(c *gin.Context, account *Account, modelID, prompt, mode string, opts AccountTestOptions) error {
	ctx := c.Request.Context()

	// Realtime is WebSocket-only and does not need HTTP upstream.
	mode = normalizeGrokAccountTestMode(mode)
	if mode != AccountTestModeGrokRealtime && s.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "HTTP upstream not configured")
	}

	authToken, err := s.grokTestAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}

	// Explicit standalone / media modes always win over model id.
	switch mode {
	case AccountTestModeGrokSearch:
		return s.testGrokWebSearch(c, ctx, account, authToken, prompt)
	case AccountTestModeGrokTTS:
		return s.testGrokTTS(c, ctx, account, authToken, prompt)
	case AccountTestModeGrokSTT:
		return s.testGrokSTT(c, ctx, account, authToken, opts.AudioDataURL)
	case AccountTestModeGrokRealtime:
		return s.testGrokRealtime(c, ctx, account, authToken, modelID)
	case AccountTestModeGrokImage:
		return s.testGrokImageGeneration(c, ctx, account, authToken, resolveGrokImageTestModel(account, modelID), resolveGrokImagePrompt(prompt), opts.ImageDataURL)
	case AccountTestModeGrokVideo:
		return s.testGrokVideoGeneration(c, ctx, account, authToken, resolveGrokVideoTestModel(account, modelID), resolveGrokVideoPrompt(prompt), opts)
	case AccountTestModeGrokText:
		// Force text Responses even if model_id looks like media.
		testModelID := strings.TrimSpace(modelID)
		if testModelID == "" {
			testModelID = grokDefaultResponsesModel
		}
		if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
			testModelID = mapped
		}
		return s.testGrokResponsesConnection(c, ctx, account, authToken, testModelID)
	}

	// mode == default: infer from model family (legacy UI / API clients).
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = grokDefaultResponsesModel
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		testModelID = mapped
	}

	switch {
	case isGrokImageGenerationModel(testModelID):
		return s.testGrokImageGeneration(c, ctx, account, authToken, testModelID, resolveGrokImagePrompt(prompt), opts.ImageDataURL)
	case isGrokVideoGenerationModel(testModelID):
		return s.testGrokVideoGeneration(c, ctx, account, authToken, testModelID, resolveGrokVideoPrompt(prompt), opts)
	default:
		return s.testGrokResponsesConnection(c, ctx, account, authToken, testModelID)
	}
}

func resolveGrokImagePrompt(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return defaultGrokImageTestPrompt
	}
	return strings.TrimSpace(prompt)
}

func resolveGrokVideoPrompt(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return defaultGrokVideoTestPrompt
	}
	return strings.TrimSpace(prompt)
}

func resolveGrokImageTestModel(account *Account, modelID string) string {
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = "grok-imagine-image"
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		return mapped
	}
	return testModelID
}

func resolveGrokVideoTestModel(account *Account, modelID string) string {
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = "grok-imagine-video"
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		return mapped
	}
	return testModelID
}

func (s *AccountTestService) grokTestAccessToken(ctx context.Context, account *Account) (string, error) {
	switch account.Type {
	case AccountTypeOAuth:
		if s.grokTokenProvider == nil {
			return "", fmt.Errorf("grok token provider not configured")
		}
		// Manual tests skip production scheduling eligibility so paused/rate-limited
		// accounts can still be probed by admins (same as Codex/OpenAI tests).
		token, err := s.grokTokenProvider.GetAccessTokenForManualTest(ctx, account)
		if err != nil {
			return "", fmt.Errorf("failed to get grok access token: %s", err.Error())
		}
		return token, nil
	case AccountTypeAPIKey:
		authToken := strings.TrimSpace(account.GetCredential("api_key"))
		if authToken == "" {
			return "", fmt.Errorf("grok api key is missing")
		}
		return authToken, nil
	default:
		return "", fmt.Errorf("unsupported grok account type: %s", account.Type)
	}
}

func (s *AccountTestService) grokTestProxyURL(account *Account) string {
	if account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func (s *AccountTestService) prepareGrokTestSSE(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

func (s *AccountTestService) applyGrokTestRequestHeaders(req *http.Request, account *Account, authToken string, accept string) {
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	// Match gateway media/voice: CLI identity headers only on the CLI chat proxy.
	// api.x.ai media (images/videos) rejects or mistreats OAuth when CLI headers
	// are stamped on the official API host (e.g. ZDR upload_url false positives).
	if account.IsGrokOAuth() && req.URL != nil && isGrokCLIProxyTarget(req.URL.String()) {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)
}

func (s *AccountTestService) observeGrokTestResponse(ctx context.Context, account *Account, resp *http.Response) {
	if resp == nil {
		return
	}
	now := time.Now()
	// Error bodies carry Grok's free-usage, billing, and content-policy
	// classifications when quota headers are absent. Read only non-success
	// responses here, then restore the body because the caller still needs it
	// for the user-facing test result.
	var responseBody []byte
	if resp.StatusCode >= http.StatusBadRequest && resp.Body != nil {
		responseBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
	}
	snapshot := parseGrokQuotaSnapshot(resp.Header, resp.StatusCode, now)
	stampGrokQuotaSnapshotForPlan(account, snapshot, grokRequestedModelFromCtx(ctx))
	if snapshot != nil && s.accountRepo != nil {
		resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, now)
		if limited {
			normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
		}
		_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
		if limited {
			persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
		} else if isSuccessfulGrokRateLimitRecovery(account, snapshot) {
			clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
		}
	} else if s.accountRepo != nil && isSuccessfulGrokRateLimitRecovery(account, &xai.QuotaSnapshot{StatusCode: resp.StatusCode}) {
		clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
	}
	if s.accountRepo == nil {
		return
	}
	// Keep manual-test HTTP 402 handling aligned with the live forwarding path:
	// payment state is recoverable, so pause the account for a bounded window
	// instead of converting the response body into a longer quota reset.
	if resp.StatusCode == http.StatusPaymentRequired {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, now.Add(30*time.Minute), "grok payment required")
		return
	}
	if len(responseBody) == 0 {
		return
	}
	if isGrokContentPolicyRejection(resp.StatusCode, responseBody) {
		return
	}
	decision := classifyGrokUpstreamFailure(resp.StatusCode, responseBody, "")
	if decision.Class == GrokFailureFreeUsage {
		if resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, now); limited && resetAt.After(now) {
			persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
		} else {
			stateCtx, cancel := openAIAccountStateContext(ctx)
			_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, now.Add(grokFreeUsageProbeCooldown), "grok free usage exhausted")
			cancel()
		}
		return
	}
	if decision.Class == GrokFailureBilling && (isGrokSpendingLimitError(responseBody) || strings.Contains(strings.ToLower(decision.Reason), "credit")) {
		persistGrokRateLimit(ctx, s.accountRepo, account, grokSpendingLimitResetAt(account, now))
		return
	}
	cooldown := time.Duration(0)
	reason := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		cooldown, reason = 10*time.Minute, "grok oauth token unauthorized"
	case http.StatusPaymentRequired:
		cooldown, reason = 30*time.Minute, "grok payment required"
	case http.StatusForbidden:
		cooldown, reason = 30*time.Minute, "grok entitlement or subscription tier denied"
	default:
		if resp.StatusCode >= 500 {
			cooldown, reason = 2*time.Minute, "grok upstream temporary error"
		}
	}
	if decision.Class == GrokFailureBilling && cooldown == 0 {
		cooldown, reason = 30*time.Minute, "grok payment required"
	}
	if cooldown > 0 {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		until := now.Add(cooldown)
		if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
			until = *account.TempUnschedulableUntil
		}
		_ = s.accountRepo.SetTempUnschedulable(
			stateCtx,
			account.ID,
			until,
			reason,
		)
	}
}

func (s *AccountTestService) testGrokResponsesConnection(c *gin.Context, ctx context.Context, account *Account, authToken, testModelID string) error {
	apiURL, err := buildGrokResponsesURL(account, s.cfg, s.settingService)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)

	payloadBytes, err := buildGrokQuotaProbeBody(testModelID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok test payload")
	}

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json, text/event-stream")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Responses API request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	s.observeGrokTestResponse(withGrokTeamRateLimitModel(ctx, testModelID), account, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Responses API returned %d: %s", resp.StatusCode, string(body)))
	}

	return s.processOpenAIStream(c, resp.Body)
}

func (s *AccountTestService) testGrokImageGeneration(c *gin.Context, ctx context.Context, account *Account, authToken, modelID, prompt, imageDataURL string) error {
	// With a source image, prefer /images/edits; otherwise /images/generations.
	endpoint := GrokMediaEndpointImagesGenerations
	imageDataURL = strings.TrimSpace(imageDataURL)
	hasSourceImage := imageDataURL != ""
	if hasSourceImage {
		endpoint = GrokMediaEndpointImagesEdits
	}

	// Align model aliases with gateway (e.g. grok-imagine → grok-imagine-image-quality).
	modelID = NormalizeGrokMediaModelForEndpoint(endpoint, modelID, hasSourceImage)
	if modelID == "" {
		modelID = "grok-imagine-image-quality"
	}

	apiURL, err := buildGrokMediaURL(account, s.cfg, endpoint, "")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok media base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})
	if endpoint == GrokMediaEndpointImagesEdits {
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/images/edits with uploaded source image..."})
	} else {
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/images/generations..."})
	}

	// Zero-data-retention teams reject URL format; always request base64 for admin tests.
	payload := map[string]any{
		"model":           modelID,
		"prompt":          prompt,
		"n":               1,
		"response_format": "b64_json",
	}
	if hasSourceImage {
		normalized, err := normalizeAccountTestImageDataURL(imageDataURL)
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		// Match gateway prepareGrokMediaForwardBody shape: {url, type:image_url}.
		payload["image"] = grokMediaImageObject(normalized)
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("source image ready (%d chars data URL)\n", len(normalized))})
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to marshal Grok image request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok image request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")
	req.ContentLength = int64(len(payloadBytes))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payloadBytes)), nil
	}

	// One retry on transport EOF (proxies occasionally drop large edit payloads).
	var resp *http.Response
	var doErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			s.sendEvent(c, TestEvent{Type: "status", Text: "Retrying Grok image request after transport error..."})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
			if err != nil {
				return s.sendErrorAndEnd(c, "Failed to create Grok image retry request")
			}
			s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")
			req.ContentLength = int64(len(payloadBytes))
		}
		resp, doErr = s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if doErr == nil {
			break
		}
		if !isTransientGrokTransportError(doErr) || attempt == 1 {
			return s.sendErrorAndEnd(c, formatGrokImageTransportError(doErr, hasSourceImage, len(payloadBytes)))
		}
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read Grok image response: %s", err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, formatGrokImagesAPIError(resp.StatusCode, body, hasSourceImage))
	}

	var result struct {
		Data []struct {
			URL           string `json:"url"`
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
			MimeType      string `json:"mime_type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to parse Grok image response: %s", err.Error()))
	}
	if len(result.Data) == 0 {
		return s.sendErrorAndEnd(c, "No images returned from Grok API")
	}

	for _, item := range result.Data {
		if item.RevisedPrompt != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: item.RevisedPrompt})
		}
		mimeType := strings.TrimSpace(item.MimeType)
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		switch {
		case strings.TrimSpace(item.B64JSON) != "":
			s.sendEvent(c, TestEvent{
				Type:     "image",
				ImageURL: "data:" + mimeType + ";base64," + item.B64JSON,
				MimeType: mimeType,
			})
		case strings.TrimSpace(item.URL) != "":
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: item.URL, MimeType: mimeType})
		}
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokVideoGeneration(c *gin.Context, ctx context.Context, account *Account, authToken, modelID, prompt string, opts AccountTestOptions) error {
	apiURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideosGenerations, "")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok media base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/videos/generations..."})

	payload := map[string]any{
		"model":        modelID,
		"prompt":       prompt,
		"duration":     6,
		"aspect_ratio": "16:9",
		"resolution":   "480p",
	}
	if img := strings.TrimSpace(opts.ImageDataURL); img != "" {
		normalized, err := normalizeAccountTestImageDataURL(img)
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		// First-frame / image-to-video input (xAI image field).
		payload["image"] = grokMediaImageObject(normalized)
		s.sendEvent(c, TestEvent{Type: "content", Text: "using uploaded first-frame / reference image\n"})
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok video request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read Grok video response: %s", err.Error()))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok videos API returned %d: %s", resp.StatusCode, string(body)))
	}

	requestID := strings.TrimSpace(gjson.GetBytes(body, "request_id").String())
	if requestID == "" {
		requestID = strings.TrimSpace(gjson.GetBytes(body, "id").String())
	}
	if requestID == "" {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video create response missing request_id: %s", string(body)))
	}

	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("video request accepted: %s\n", requestID)})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Polling video status until done (max ~60s)..."})

	statusURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, requestID)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok video status URL: %s", err.Error()))
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return s.sendErrorAndEnd(c, "Grok video poll canceled")
		}
		statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to create Grok video status request")
		}
		s.applyGrokTestRequestHeaders(statusReq, account, authToken, "application/json")
		statusResp, err := s.httpUpstream.Do(statusReq, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video status failed: %s", err.Error()))
		}
		statusBody, _ := io.ReadAll(statusResp.Body)
		_ = statusResp.Body.Close()
		if statusResp.StatusCode != http.StatusOK && statusResp.StatusCode != http.StatusAccepted {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video status returned %d: %s", statusResp.StatusCode, string(statusBody)))
		}
		st := strings.ToLower(strings.TrimSpace(gjson.GetBytes(statusBody, "status").String()))
		progress := gjson.GetBytes(statusBody, "progress")
		if progress.Exists() {
			s.sendEvent(c, TestEvent{Type: "status", Text: fmt.Sprintf("status=%s progress=%v", st, progress.Value())})
		} else {
			s.sendEvent(c, TestEvent{Type: "status", Text: "status=" + st})
		}
		switch st {
		case "done", "completed", "succeeded", "success":
			return s.emitGrokVideoResult(c, ctx, account, authToken, requestID, statusBody)
		case "failed", "error", "canceled", "cancelled":
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video failed: %s", string(statusBody)))
		}
		select {
		case <-ctx.Done():
			return s.sendErrorAndEnd(c, "Grok video poll canceled")
		case <-time.After(3 * time.Second):
		}
	}
	return s.sendErrorAndEnd(c, "Grok video still processing after 60s (request_id="+requestID+")")
}

// emitGrokVideoResult surfaces a playable video URL or downloads /content as data URL.
func (s *AccountTestService) emitGrokVideoResult(c *gin.Context, ctx context.Context, account *Account, authToken, requestID string, statusBody []byte) error {
	videoURL := firstNonEmpty(
		strings.TrimSpace(gjson.GetBytes(statusBody, "video.url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "video_url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "download_url").String()),
	)
	if videoURL != "" && (strings.HasPrefix(videoURL, "http://") || strings.HasPrefix(videoURL, "https://") || strings.HasPrefix(videoURL, "data:")) {
		s.sendEvent(c, TestEvent{Type: "content", Text: "video ready: " + videoURL + "\n"})
		s.sendEvent(c, TestEvent{Type: "video", VideoURL: videoURL, MimeType: "video/mp4"})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}

	// Fetch binary content via official /videos/{id}/content (Bearer-authenticated).
	contentURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoContent, requestID)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok video content URL: %s", err.Error()))
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "Downloading video content for preview..."})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok video content request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "video/*, application/octet-stream, */*")
	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video content download failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MiB cap for admin preview
	if resp.StatusCode != http.StatusOK {
		// Fall back to status URL when binary content is unavailable.
		if videoURL != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: "video completed; content download unavailable, reported url=" + videoURL + "\n"})
			s.sendEvent(c, TestEvent{Type: "video", VideoURL: videoURL, MimeType: "video/mp4"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video content returned %d: %s", resp.StatusCode, truncateString(string(body), 300)))
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
		ct = "video/mp4"
	}
	// Keep only type/subtype for data URL.
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	dataURL := "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("video content downloaded: content-type=%s bytes=%d\n", ct, len(body))})
	s.sendEvent(c, TestEvent{Type: "video", VideoURL: dataURL, MimeType: ct})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokWebSearch(c *gin.Context, ctx context.Context, account *Account, authToken, query string) error {
	query = strings.TrimSpace(query)
	if query == "" {
		query = defaultGrokSearchTestQuery
	}

	// Account-test "web_search" mode mirrors the standalone gateway endpoint
	// POST /v1/web_search (not a free-form chat with tools). Implementation still
	// uses the same DoGrokNativeResponsesJSON helper as the gateway handler so
	// results match production search.
	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-web-search"})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone web_search probe (same as gateway /v1/web_search)..."})

	// Keep parity with handler.buildGrokWebSearchPrompt / include sources.
	const maxResults = 5
	prompt := fmt.Sprintf(
		`Search the web for the user query below. Return ONLY valid JSON with this exact shape: {"results":[{"url":"https://...","title":"page title","snippet":"concise factual summary"}]}. Return at most %d unique results. Every URL must be an actual web_search source. Populate a non-empty title and snippet for every result. Do not wrap the JSON in markdown.

User query:
%s`, maxResults, query)
	payload := map[string]any{
		"model":   grokDefaultResponsesModel,
		"input":   prompt,
		"tools":   []map[string]any{{"type": "web_search"}},
		"include": []string{"web_search_call.action.sources"},
		"store":   false,
		"stream":  false,
	}
	payloadBytes, _ := json.Marshal(payload)

	apiURL, err := buildGrokResponsesURL(account, s.cfg, s.settingService)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok base URL: %s", err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create standalone web_search probe request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("standalone web_search probe failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(withGrokTeamRateLimitModel(ctx, grokDefaultResponsesModel), account, resp)

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("standalone web_search probe returned %d: %s", resp.StatusCode, string(body)))
	}

	// Normalize like gateway extractGrokWebSearchSources (URL-only sources are enough for connectivity).
	sourceCount := 0
	gjson.GetBytes(body, "output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "web_search_call" {
			return true
		}
		sources := item.Get("action.sources")
		if sources.IsArray() {
			sourceCount += len(sources.Array())
		}
		return true
	})
	searchCount := countGrokNativeSearchCallsFromJSONBytes(body)
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("web_search ok: query=%q tool_calls=%d sources=%d\n", query, searchCount, sourceCount)})
	// Optional: first structured result title if model returned JSON text.
	gjson.GetBytes(body, "output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "message" {
			return true
		}
		for _, part := range item.Get("content").Array() {
			text := strings.TrimSpace(part.Get("text").String())
			if text == "" {
				continue
			}
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			s.sendEvent(c, TestEvent{Type: "content", Text: text + "\n"})
			return false
		}
		return true
	})
	if searchCount == 0 && sourceCount == 0 {
		return s.sendErrorAndEnd(c, "standalone web_search probe completed but no search sources/tool calls were observed")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokTTS(c *gin.Context, ctx context.Context, account *Account, authToken, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		text = defaultGrokTTSTestText
	}
	apiURL, err := buildGrokVoiceURL(account, s.cfg, "tts")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok TTS URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-voice-tts"})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/tts..."})

	// xAI requires `language`; optional voice_id. Prefer the shape that matches
	// live gateway probes (text + language [+ voice_id]).
	payloads := []map[string]any{
		{"text": text, "language": "en", "voice_id": "Ara"},
		{"text": text, "language": "en"},
		{"text": text, "language": "English", "voice_id": "Ara"},
	}
	var lastBody string
	var lastCode int
	for _, payload := range payloads {
		payloadBytes, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to create Grok TTS request")
		}
		s.applyGrokTestRequestHeaders(req, account, authToken, "audio/*, application/json, */*")
		resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok TTS failed: %s", err.Error()))
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.observeGrokTestResponse(ctx, account, resp)
		lastCode = resp.StatusCode
		lastBody = string(body)
		if resp.StatusCode == http.StatusOK {
			ct := resp.Header.Get("Content-Type")
			if ct == "" {
				ct = "audio/mpeg"
			}
			if i := strings.Index(ct, ";"); i >= 0 {
				ct = strings.TrimSpace(ct[:i])
			}
			// Cap preview size so SSE stays manageable (~4 MiB audio).
			if len(body) > 4<<20 {
				body = body[:4<<20]
			}
			audioURL := "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
			s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("tts ok: content-type=%s bytes=%d\n", ct, len(body))})
			s.sendEvent(c, TestEvent{Type: "audio", AudioURL: audioURL, MimeType: ct})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			break
		}
	}
	return s.sendErrorAndEnd(c, fmt.Sprintf("Grok TTS returned %d: %s", lastCode, lastBody))
}

// testGrokSTT posts audio to /v1/stt. When audioDataURL is set, uses the
// uploaded file; otherwise a tiny synthetic silent WAV for connectivity only.
func (s *AccountTestService) testGrokSTT(c *gin.Context, ctx context.Context, account *Account, authToken, audioDataURL string) error {
	apiURL, err := buildGrokVoiceURL(account, s.cfg, "stt")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok STT URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-voice-stt"})

	var audioBytes []byte
	filename := "probe.wav"
	if audioDataURL = strings.TrimSpace(audioDataURL); audioDataURL != "" {
		if err := validateAccountTestDataURL(audioDataURL, "audio/"); err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		raw, mime, err := decodeAccountTestDataURL(audioDataURL)
		if err != nil {
			return s.sendErrorAndEnd(c, "Invalid audio data URL: "+err.Error())
		}
		audioBytes = raw
		filename = sttFilenameForMIME(mime)
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/stt with uploaded audio..."})
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("uploaded audio: mime=%s bytes=%d\n", mime, len(audioBytes))})
	} else {
		audioBytes = minimalSilentWAV()
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/stt with a synthetic silent WAV..."})
	}

	var bodyBuf bytes.Buffer
	w := multipart.NewWriter(&bodyBuf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to build STT multipart body")
	}
	if _, err := part.Write(audioBytes); err != nil {
		return s.sendErrorAndEnd(c, "Failed to write STT audio part")
	}
	_ = w.WriteField("model", "grok-stt")
	_ = w.WriteField("language", "en")
	if err := w.Close(); err != nil {
		return s.sendErrorAndEnd(c, "Failed to finalize STT multipart body")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &bodyBuf)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok STT request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	if account.IsGrokOAuth() {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok STT failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// 4xx on synthetic audio still proves the STT endpoint is wired; report clearly.
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok STT returned %d: %s", resp.StatusCode, string(respBody)))
	}
	text := strings.TrimSpace(gjson.GetBytes(respBody, "text").String())
	if text == "" {
		text = strings.TrimSpace(string(respBody))
		if len(text) > 200 {
			text = text[:200] + "..."
		}
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("stt ok: %s\n", text)})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// testGrokRealtime dials the standalone xAI Voice Realtime WebSocket
// (wss://api.x.ai/v1/realtime?model=...) to verify auth + endpoint reachability.
// It does not run a full audio session — success is WS handshake, optionally
// enriched with the first server event type when one arrives quickly.
func (s *AccountTestService) testGrokRealtime(c *gin.Context, ctx context.Context, account *Account, authToken, modelID string) error {
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = defaultGrokRealtimeTestModel
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		model = mapped
	}

	base, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok Realtime URL: %s", err.Error()))
	}
	u, err := url.Parse(base)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok Realtime URL: %s", err.Error()))
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// already websocket
	default:
		return s.sendErrorAndEnd(c, "Invalid Grok Realtime URL scheme")
	}
	q := u.Query()
	if q.Get("model") == "" {
		q.Set("model", model)
	}
	u.RawQuery = q.Encode()
	wsURL := u.String()

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Dialing standalone wss /v1/realtime (connectivity probe)..."})
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("realtime target: %s\n", redactGrokRealtimeURLForLog(wsURL))})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+authToken)
	if account.IsGrokOAuth() {
		applyGrokCLIHeaders(headers)
	}
	account.ApplyHeaderOverrides(headers)

	dialer := s.grokWSDialer
	if dialer == nil {
		dialer = newDefaultOpenAIWSClientDialer()
	}

	dialCtx, cancel := context.WithTimeout(ctx, grokRealtimeProbeTimeout)
	defer cancel()

	conn, status, _, dialErr := dialer.Dial(dialCtx, wsURL, headers, s.grokTestProxyURL(account))
	if dialErr != nil {
		detail := dialErr.Error()
		var hs *openAIWSHandshakeError
		if errors.As(dialErr, &hs) && len(hs.Body) > 0 {
			body := strings.TrimSpace(string(hs.Body))
			if len(body) > 300 {
				body = body[:300] + "..."
			}
			detail = fmt.Sprintf("%s body=%s", detail, body)
		}
		if status > 0 {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Realtime WS handshake failed (HTTP %d): %s", status, detail))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Realtime WS dial failed: %s", detail))
	}
	defer func() { _ = conn.Close() }()

	s.sendEvent(c, TestEvent{Type: "content", Text: "realtime ws handshake ok\n"})

	// Best-effort: read one server event if it arrives quickly (session.created etc.).
	// Handshake alone is enough for connectivity; missing first event is not a failure.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	if msg, readErr := conn.ReadMessage(readCtx); readErr == nil && len(msg) > 0 {
		eventType := strings.TrimSpace(gjson.GetBytes(msg, "type").String())
		if eventType == "" {
			eventType = "unknown"
		}
		preview := strings.TrimSpace(string(msg))
		if len(preview) > 240 {
			preview = preview[:240] + "..."
		}
		s.sendEvent(c, TestEvent{
			Type: "content",
			Text: fmt.Sprintf("realtime first event: type=%s payload=%s\n", eventType, preview),
		})
	} else {
		s.sendEvent(c, TestEvent{
			Type: "content",
			Text: "realtime handshake succeeded (no server event within 3s; still connectivity OK)\n",
		})
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// validateAccountTestDataURL ensures data URLs are well-formed and size-bounded.
func validateAccountTestDataURL(raw, requiredPrefix string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("media data URL is empty")
	}
	if !strings.HasPrefix(raw, "data:") {
		return fmt.Errorf("media must be a data: URL (data:<mime>;base64,...)")
	}
	// Rough size check before decode (base64 expands ~4/3).
	if len(raw) > maxAccountTestMediaBytes*2 {
		return fmt.Errorf("media data URL exceeds size limit")
	}
	_, mime, err := decodeAccountTestDataURL(raw)
	if err != nil {
		return err
	}
	if requiredPrefix != "" && !strings.HasPrefix(strings.ToLower(mime), strings.ToLower(requiredPrefix)) {
		return fmt.Errorf("expected media type prefix %q, got %q", requiredPrefix, mime)
	}
	return nil
}

// normalizeAccountTestImageDataURL validates an image data URL, enforces xAI
// minimum dimensions (8x8), and rewrites to a clean data:image/<type>;base64,... form.
func normalizeAccountTestImageDataURL(raw string) (string, error) {
	if err := validateAccountTestDataURL(raw, "image/"); err != nil {
		return "", err
	}
	data, mime, err := decodeAccountTestDataURL(raw)
	if err != nil {
		return "", err
	}
	// Soft cap decoded bytes (~4 MiB) for edit payloads to avoid upstream/proxy EOF.
	const maxDecodedImage = 4 << 20
	if len(data) > maxDecodedImage {
		return "", fmt.Errorf(
			"source image is too large (%d bytes decoded). Please use a smaller image (under ~4 MB) for admin edit tests",
			len(data),
		)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Keep raw data URL if decoder does not understand the codec (e.g. webp
		// without golang.org/x/image/webp); still send upstream and let xAI validate.
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	if cfg.Width < 8 || cfg.Height < 8 {
		return "", fmt.Errorf(
			"source image is too small (%dx%d). xAI requires both width and height to be at least 8 pixels",
			cfg.Width, cfg.Height,
		)
	}
	// Prefer a stable mime from config when known.
	if mime == "" || mime == "application/octet-stream" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func isTransientGrokTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "timeout awaiting response")
}

func formatGrokImageTransportError(err error, hasSourceImage bool, payloadBytes int) string {
	base := fmt.Sprintf("Grok image request failed: %s", err.Error())
	if !hasSourceImage {
		return base
	}
	return base + fmt.Sprintf(
		" (edit payload ~%d bytes). Tips: use a smaller source image (<4 MB / lower resolution), ensure the account proxy is stable, and retry. xAI /images/edits expects image as {\"url\":\"data:image/...;base64,...\",\"type\":\"image_url\"}.",
		payloadBytes,
	)
}

func formatGrokImagesAPIError(status int, body []byte, hasSourceImage bool) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 800 {
		msg = msg[:800] + "..."
	}
	prefix := fmt.Sprintf("Grok images API returned %d: %s", status, msg)
	lower := strings.ToLower(msg)
	if hasSourceImage && (strings.Contains(lower, "too small") || strings.Contains(lower, "at least 8")) {
		return prefix + " — upload a source image with both width and height ≥ 8 px."
	}
	return prefix
}

func decodeAccountTestDataURL(raw string) (data []byte, mime string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return nil, "", fmt.Errorf("not a data URL")
	}
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil, "", fmt.Errorf("invalid data URL (missing comma)")
	}
	meta := rest[:comma]
	payload := rest[comma+1:]
	mime = "application/octet-stream"
	if semi := strings.Index(meta, ";"); semi >= 0 {
		if t := strings.TrimSpace(meta[:semi]); t != "" {
			mime = t
		}
	} else if t := strings.TrimSpace(meta); t != "" {
		mime = t
	}
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", fmt.Errorf("only base64 data URLs are supported")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Some browsers emit URL-safe base64 without padding.
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode failed: %w", err)
		}
	}
	if len(decoded) == 0 {
		return nil, "", fmt.Errorf("decoded media is empty")
	}
	if len(decoded) > maxAccountTestMediaBytes {
		return nil, "", fmt.Errorf("media exceeds %d byte limit", maxAccountTestMediaBytes)
	}
	return decoded, mime, nil
}

func sttFilenameForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "audio/mpeg", "audio/mp3":
		return "upload.mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "upload.wav"
	case "audio/webm":
		return "upload.webm"
	case "audio/ogg", "audio/opus":
		return "upload.ogg"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "upload.m4a"
	default:
		return "upload.bin"
	}
}

// redactGrokRealtimeURLForLog strips query secrets while keeping model for diagnostics.
func redactGrokRealtimeURLForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return raw
	}
	// Keep model query only.
	model := u.Query().Get("model")
	u.RawQuery = ""
	if model != "" {
		u.RawQuery = "model=" + url.QueryEscape(model)
	}
	// Never log bearer in fragment/userinfo.
	u.User = nil
	u.Fragment = ""
	return u.String()
}

// minimalSilentWAV returns a valid tiny mono 8kHz 16-bit PCM WAV (~0.05s silence).
func minimalSilentWAV() []byte {
	// 400 samples * 2 bytes = 800 data bytes
	const sampleRate = 8000
	const numSamples = 400
	dataSize := numSamples * 2
	buf := make([]byte, 44+dataSize)
	copy(buf[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataSize))
	copy(buf[8:], []byte("WAVE"))
	copy(buf[12:], []byte("fmt "))
	binary.LittleEndian.PutUint32(buf[16:], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(buf[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1)  // mono
	binary.LittleEndian.PutUint32(buf[24:], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:], sampleRate*2) // byte rate
	binary.LittleEndian.PutUint16(buf[32:], 2)            // block align
	binary.LittleEndian.PutUint16(buf[34:], 16)           // bits
	copy(buf[36:], []byte("data"))
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataSize))
	// samples already zero (silence)
	return buf
}

// testOpenAIChatCompletionsConnection tests an OpenAI-compatible APIKey account
// through the raw /v1/chat/completions endpoint.
