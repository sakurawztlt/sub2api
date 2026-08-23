package service

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// readOpenAIUpstreamError reads the bounded upstream error body and rewinds it
// because endpoint-specific handlers may consume it again.
func (s *OpenAIGatewayService) readOpenAIUpstreamError(resp *http.Response) ([]byte, string) {
	respBody := s.readUpstreamErrorBody(resp)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
	return respBody, sanitizeUpstreamErrorMessage(upstreamMsg)
}
