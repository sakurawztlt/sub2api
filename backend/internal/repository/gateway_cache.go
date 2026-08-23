package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const stickySessionPrefix = "sticky_session:"
const openAIResponsesSessionWindowPrefix = "openai_responses_session_window:"

type gatewayCache struct {
	rdb *redis.Client
}

func NewGatewayCache(rdb *redis.Client) service.GatewayCache {
	return &gatewayCache{rdb: rdb}
}

// buildSessionKey 构建 session key，包含 groupID 实现分组隔离
// 格式: sticky_session:{groupID}:{sessionHash}
func buildSessionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", stickySessionPrefix, groupID, sessionHash)
}

func buildOpenAIResponsesSessionWindowKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", openAIResponsesSessionWindowPrefix, groupID, sessionHash)
}

func (c *gatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	key := buildSessionKey(groupID, sessionHash)
	accountID, err := c.rdb.Get(ctx, key).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, service.ErrStickySessionNotFound
		}
		return 0, err
	}
	return accountID, nil
}

func (c *gatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Set(ctx, key, accountID, ttl).Err()
}

func (c *gatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// DeleteSessionAccountID 删除粘性会话与账号的绑定关系。
// 当检测到绑定的账号不可用（如状态错误、禁用、不可调度等）时调用，
// 以便下次请求能够重新选择可用账号。
//
// DeleteSessionAccountID removes the sticky session binding for the given session.
// Called when the bound account becomes unavailable (e.g., error status, disabled,
// or unschedulable), allowing subsequent requests to select a new available account.
func (c *gatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Del(ctx, key).Err()
}

var claimOpenAIResponsesSessionWindowScript = redis.NewScript(`
local previous = redis.call('GET', KEYS[1])
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return previous
`)

var compareAndRefreshOpenAIResponsesSessionWindowScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == false or current ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

var compareAndDeleteOpenAIResponsesSessionWindowScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == false or current ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
`)

func (c *gatewayCache) ClaimOpenAIResponsesSessionWindow(ctx context.Context, groupID int64, sessionHash string, owner []byte, ttl time.Duration) ([]byte, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("gateway cache unavailable")
	}
	if len(owner) == 0 || strings.TrimSpace(sessionHash) == "" || ttl <= 0 {
		return nil, errors.New("invalid OpenAI Responses session-window claim")
	}
	result, err := claimOpenAIResponsesSessionWindowScript.Run(
		ctx,
		c.rdb,
		[]string{buildOpenAIResponsesSessionWindowKey(groupID, sessionHash)},
		owner,
		ttl.Milliseconds(),
	).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch value := result.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(value), nil
	case []byte:
		return append([]byte(nil), value...), nil
	default:
		return nil, fmt.Errorf("unexpected OpenAI Responses session-window claim result %T", result)
	}
}

func (c *gatewayCache) CompareAndRefreshOpenAIResponsesSessionWindow(ctx context.Context, groupID int64, sessionHash string, expected []byte, ttl time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, errors.New("gateway cache unavailable")
	}
	if len(expected) == 0 || strings.TrimSpace(sessionHash) == "" || ttl <= 0 {
		return false, errors.New("invalid OpenAI Responses session-window refresh")
	}
	n, err := compareAndRefreshOpenAIResponsesSessionWindowScript.Run(
		ctx,
		c.rdb,
		[]string{buildOpenAIResponsesSessionWindowKey(groupID, sessionHash)},
		expected,
		ttl.Milliseconds(),
	).Int()
	return n == 1, err
}

func (c *gatewayCache) CompareAndDeleteOpenAIResponsesSessionWindow(ctx context.Context, groupID int64, sessionHash string, expected []byte) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, errors.New("gateway cache unavailable")
	}
	if len(expected) == 0 || strings.TrimSpace(sessionHash) == "" {
		return false, errors.New("invalid OpenAI Responses session-window delete")
	}
	n, err := compareAndDeleteOpenAIResponsesSessionWindowScript.Run(
		ctx,
		c.rdb,
		[]string{buildOpenAIResponsesSessionWindowKey(groupID, sessionHash)},
		expected,
	).Int()
	return n == 1, err
}

var _ service.OpenAIWSSessionPreemptionCache = (*gatewayCache)(nil)

const (
	grokVideoPendingBillingPrefix = "grok_video_pending:"
	grokVideoBilledPrefix         = "grok_video_billed:"
)

func (c *gatewayCache) SetGrokVideoPendingBilling(ctx context.Context, key string, payload []byte, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return errors.New("gateway cache unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" || len(payload) == 0 {
		return errors.New("invalid grok video pending billing payload")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return c.rdb.Set(ctx, grokVideoPendingBillingPrefix+key, payload, ttl).Err()
}

func (c *gatewayCache) GetGrokVideoPendingBilling(ctx context.Context, key string) ([]byte, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("gateway cache unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("invalid grok video pending billing key")
	}
	value, err := c.rdb.Get(ctx, grokVideoPendingBillingPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return value, err
}

func (c *gatewayCache) ClaimGrokVideoBilled(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, errors.New("gateway cache unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return false, errors.New("invalid grok video billed key")
	}
	if ttl <= 0 {
		ttl = 48 * time.Hour
	}
	return c.rdb.SetNX(ctx, grokVideoBilledPrefix+key, "1", ttl).Result()
}

func (c *gatewayCache) ReleaseGrokVideoBilled(ctx context.Context, key string) error {
	if c == nil || c.rdb == nil {
		return errors.New("gateway cache unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("invalid grok video billed key")
	}
	return c.rdb.Del(ctx, grokVideoBilledPrefix+key).Err()
}

// Compile-time assertion: gatewayCache must implement CyberSessionBlockStore.
var _ service.CyberSessionBlockStore = (*gatewayCache)(nil)

const reasoningContentPrefix = "reasoning_content:"

// reasoningContentDefaultTTL 是 reasoning 缓存的默认过期时间。Codex 会话可能
// 跨多天恢复，取 7 天；调用方传入非正 TTL 时兜底。
const reasoningContentDefaultTTL = 7 * 24 * time.Hour

// SetReasoningContent 按 reasoning item id 缓存 reasoning 全文。
// itemID 或 content 为空时直接返回 nil（无可缓存内容，属正常情况而非错误）。
func (c *gatewayCache) SetReasoningContent(ctx context.Context, itemID string, content string, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return errors.New("gateway cache unavailable")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" || content == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = reasoningContentDefaultTTL
	}
	return c.rdb.Set(ctx, reasoningContentPrefix+itemID, content, ttl).Err()
}

// GetReasoningContent 返回缓存的 reasoning 全文；未命中返回
// service.ErrReasoningContentNotFound。
func (c *gatewayCache) GetReasoningContent(ctx context.Context, itemID string) (string, error) {
	if c == nil || c.rdb == nil {
		return "", errors.New("gateway cache unavailable")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return "", service.ErrReasoningContentNotFound
	}
	val, err := c.rdb.Get(ctx, reasoningContentPrefix+itemID).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", service.ErrReasoningContentNotFound
		}
		return "", err
	}
	return val, nil
}

const (
	cyberSessionBlockPrefix         = "cyber_session_block:"
	cyberSessionScopePrefix         = "cyber_session_scope:"
	cyberSessionRedisCommandMaxKeys = 128
)

// SetCyberSessionBlocked writes exact blocks in bounded transactions. The
// coarse scope is activated only after all exact blocks have been stored.
func (c *gatewayCache) SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error {
	if len(keys) == 0 {
		return nil
	}
	exactKeys := make([]string, 0, cyberSessionRedisCommandMaxKeys)
	flush := func() error {
		if len(exactKeys) == 0 {
			return nil
		}
		pipe := c.rdb.TxPipeline()
		for _, key := range exactKeys {
			pipe.Set(ctx, cyberSessionBlockPrefix+key, "1", ttl)
		}
		_, err := pipe.Exec(ctx)
		exactKeys = exactKeys[:0]
		return err
	}
	for _, key := range keys {
		if key != "" {
			exactKeys = append(exactKeys, key)
			if len(exactKeys) == cyberSessionRedisCommandMaxKeys {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if scopeKey != "" {
		return c.rdb.Set(ctx, cyberSessionScopePrefix+scopeKey, "1", ttl).Err()
	}
	return nil
}

func (c *gatewayCache) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	n, err := c.rdb.Exists(ctx, cyberSessionScopePrefix+scopeKey).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// FindCyberSessionBlocked checks bounded batches in caller order and stops at
// the first blocked key, preserving the original earliest-match behavior.
func (c *gatewayCache) FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error) {
	if len(keys) == 0 {
		return "", nil
	}
	for start := 0; start < len(keys); start += cyberSessionRedisCommandMaxKeys {
		end := start + cyberSessionRedisCommandMaxKeys
		if end > len(keys) {
			end = len(keys)
		}
		redisKeys := make([]string, end-start)
		for i, key := range keys[start:end] {
			redisKeys[i] = cyberSessionBlockPrefix + key
		}
		values, err := c.rdb.MGet(ctx, redisKeys...).Result()
		if err != nil {
			return "", err
		}
		for i, value := range values {
			if value != nil {
				return keys[start+i], nil
			}
		}
	}
	return "", nil
}
