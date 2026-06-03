package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestResolveOpenAIWSClientReadLimitBytes(t *testing.T) {
	require.Equal(t, int64(64*1024*1024), ResolveOpenAIWSClientReadLimitBytes(nil))

	cfg := &config.Config{}
	require.Equal(t, int64(64*1024*1024), ResolveOpenAIWSClientReadLimitBytes(cfg))

	cfg.Gateway.OpenAIWS.ClientReadLimitBytes = 32 * 1024 * 1024
	require.Equal(t, int64(32*1024*1024), ResolveOpenAIWSClientReadLimitBytes(cfg))
}
