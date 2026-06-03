package service

import "github.com/Wei-Shaw/sub2api/internal/config"

const defaultOpenAIWSClientReadLimitBytes int64 = 64 * 1024 * 1024

func ResolveOpenAIWSClientReadLimitBytes(cfg *config.Config) int64 {
	if cfg != nil && cfg.Gateway.OpenAIWS.ClientReadLimitBytes > 0 {
		return cfg.Gateway.OpenAIWS.ClientReadLimitBytes
	}
	return defaultOpenAIWSClientReadLimitBytes
}
