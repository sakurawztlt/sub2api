package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

func TestIsUpstreamNetworkError_Nil(t *testing.T) {
	if IsUpstreamNetworkError(nil) {
		t.Errorf("nil should not be network error")
	}
}

func TestIsUpstreamNetworkError_ClientCanceled(t *testing.T) {
	if IsUpstreamNetworkError(context.Canceled) {
		t.Errorf("client cancel must NOT be classified as network error (would break sticky for nothing)")
	}
}

func TestIsUpstreamNetworkError_DeadlineExceeded(t *testing.T) {
	if !IsUpstreamNetworkError(context.DeadlineExceeded) {
		t.Errorf("DeadlineExceeded should be network error")
	}
}

func TestIsUpstreamNetworkError_Syscall(t *testing.T) {
	cases := []syscall.Errno{
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ETIMEDOUT,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.ENETDOWN,
		syscall.EPIPE,
	}
	for _, e := range cases {
		t.Run(e.Error(), func(t *testing.T) {
			if !IsUpstreamNetworkError(e) {
				t.Errorf("%v should be network error", e)
			}
			// also wrapped
			wrapped := fmt.Errorf("wrap: %w", e)
			if !IsUpstreamNetworkError(wrapped) {
				t.Errorf("wrapped %v should also be network error", e)
			}
		})
	}
}

func TestIsUpstreamNetworkError_IOErrors(t *testing.T) {
	if !IsUpstreamNetworkError(io.EOF) {
		t.Errorf("io.EOF should be network error")
	}
	if !IsUpstreamNetworkError(io.ErrUnexpectedEOF) {
		t.Errorf("ErrUnexpectedEOF should be network error")
	}
}

func TestIsUpstreamNetworkError_DNSError(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "chatgpt.com"}
	if !IsUpstreamNetworkError(dnsErr) {
		t.Errorf("DNSError should be network error")
	}
}

func TestIsUpstreamNetworkError_OpError(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	if !IsUpstreamNetworkError(opErr) {
		t.Errorf("OpError(dial connection refused) should be network error")
	}
}

// 实测过的错误字符串 (production prod 51 5/9 case)
func TestIsUpstreamNetworkError_RealProdString(t *testing.T) {
	// 真实在 prod 51 看到的 sub2api forward_failed 错误格式
	cases := []string{
		`upstream request failed: Post "https://chatgpt.com/backend-api/codex/responses": socks connect tcp 134.195.211.73:14262->chatgpt.com:443: dial tcp 134.195.211.73:14262: connect: connection refused`,
		"Post \"https://api.openai.com/v1/responses\": dial tcp: lookup api.openai.com: no such host",
		"net/http: TLS handshake timeout",
		"read tcp 10.0.0.1:443: read: connection reset by peer",
		"write tcp 10.0.0.1:443: write: broken pipe",
		"proxyconnect tcp: dial tcp: i/o timeout",
		"network is unreachable",
		"host is unreachable",
	}
	for _, msg := range cases {
		t.Run(msg[:min(40, len(msg))], func(t *testing.T) {
			err := fmt.Errorf("%s", msg)
			if !IsUpstreamNetworkError(err) {
				t.Errorf("real prod error string not classified: %s", msg)
			}
		})
	}
}

// 业务错误绝对 NOT 被误归类
func TestIsUpstreamNetworkError_NoFalsePositives(t *testing.T) {
	notNetwork := []string{
		"openai: invalid_request_error: max_tokens too high",
		"upstream returned 429 rate_limit",
		"timeout while validating prompt cache key",                   // 单词 timeout 不该匹配
		"context deadline exceeded by 5s waiting for a response",      // 描述性, 不是真 ctx.DeadlineExceeded
		"this account is rate limited until ...",
		"failed to parse openai response",
	}
	for _, msg := range notNetwork {
		t.Run(msg[:min(40, len(msg))], func(t *testing.T) {
			err := errors.New(msg)
			if IsUpstreamNetworkError(err) {
				t.Errorf("FALSE POSITIVE: %q should NOT be network error", msg)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
