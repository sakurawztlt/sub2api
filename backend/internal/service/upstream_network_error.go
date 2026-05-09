package service

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// IsUpstreamNetworkError reports whether err originates from a transport-level
// failure (TCP connect refused, DNS lookup failure, TLS handshake timeout,
// SOCKS proxy error, network unreachable, …) rather than an HTTP-level
// upstream response. Network-level failures must NOT bind a request to the
// account that produced them — the same sessionHash will keep routing back
// to that broken-network account and burn forever (e.g. proxy IP becomes
// unreachable but DB still says status=active).
//
// 5/9 codex audit: handler 路径 2 之前对所有非 UpstreamFailoverError 都直接
// return 502, 不 failover, 不删 sticky → connection refused 死循环.
//
// Detection layers (most specific first):
//
//  1. Errors that signal transport problems via Go's typed error tree
//     (net.OpError / net.DNSError / syscall ECONNREFUSED etc.).
//  2. context.DeadlineExceeded (request timeout) — counts as network for
//     failover purposes; client cancel (context.Canceled) does NOT.
//  3. String fallback for opaque proxy / SOCKS / TLS handshake errors
//     where the typed chain is wrapped to plain fmt.Errorf strings.
//
// Deliberately AVOID matching the bare substring "timeout" — many business
// errors mention timeout in their message and we don't want to treat them
// as network errors and break sticky for the wrong reason.
func IsUpstreamNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Client cancelled is NOT a network error — we should not break sticky
	// or count an account as failed when the client just disconnected.
	if errors.Is(err, context.Canceled) {
		return false
	}

	// Layer 1: typed errors.
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ENETDOWN) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Layer 3: string fallback for wrapped/opaque cases. Keep the patterns
	// SPECIFIC — never just "timeout" or "error".
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"proxyconnect",
		"socks connect",
		"network is unreachable",
		"host is unreachable",
		"tls handshake timeout",
		"tls: handshake failure",
		"broken pipe",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
