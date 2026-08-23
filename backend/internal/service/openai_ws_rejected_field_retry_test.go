package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIGatewayService_ProxyResponsesWebSocketFromClient_RetriesRejectedFieldBeforeClientOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","status":400,"error":{"code":"unsupported_parameter","message":"Unsupported parameter: max_output_tokens","param":"max_output_tokens"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_rejected_field_retry","model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := &Account{
		ID: 6510, Name: "openai-ingress-rejected-field", Platform: PlatformOpenAI,
		Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}

	serverErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = r.Clone(r.Context())
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			serverErrCh <- errors.New("unsupported websocket client message type")
			return
		}
		serverErrCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "sk-test", firstMessage, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	payload := []byte(`{"type":"response.create","model":"gpt-5.5","stream":false,"max_output_tokens":4096,"input":"hello"}`)
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, payload))
	cancelWrite()

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, downstreamMessage, readErr := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, readErr)
	require.Equal(t, "response.completed", gjson.GetBytes(downstreamMessage, "type").String())
	require.Equal(t, "resp_rejected_field_retry", gjson.GetBytes(downstreamMessage, "response.id").String())

	_ = clientConn.Close(coderws.StatusNormalClosure, "done")
	select {
	case serverErr := <-serverErrCh:
		require.NoError(t, serverErr)
	case <-time.After(5 * time.Second):
		t.Fatal("waiting for ingress websocket shutdown timed out")
	}

	require.Len(t, captureConn.writes, 2)
	require.Equal(t, json.Number("4096"), captureConn.writes[0]["max_output_tokens"])
	require.NotContains(t, captureConn.writes[1], "max_output_tokens")
}
