package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokConversationIDHeader        = "X-Grok-Conv-Id"
	grokFreeCacheNativeToolsJSON    = `[{"type":"web_search"},{"type":"x_search"}]`
	grokFreeCacheDisabledToolChoice = "none"
)

// resolveGrokCacheIdentity derives one stable, tenant-isolated routing
// identity for xAI's server-side prompt cache. It never exposes the client's
// raw session identifier and fails closed without a downstream API key.
func resolveGrokCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 || isOpenAIResponsesCompactPath(c) {
		return ""
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}
	seed := explicitGrokCacheSeed(c, body, explicitKey)
	if seed == "" {
		seed = deriveOpenAIContentSessionSeed(body)
	}
	if seed == "" {
		return ""
	}
	return generateSessionUUID(fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed))
}

func explicitGrokCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	seed := ""
	if c != nil {
		seed = strings.TrimSpace(c.GetHeader("session_id"))
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader("conversation_id"))
		}
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader(grokConversationIDHeader))
		}
	}
	if seed == "" && len(body) > 0 {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}

func isGrokRequestContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, exists := c.Get("api_key")
	if !exists {
		return false
	}
	apiKey, ok := v.(*APIKey)
	return ok && apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == PlatformGrok
}

// applyGrokResponsesCacheIdentity writes the tenant-isolated cache identity
// into a Responses request. Free OAuth requests without client tools also get
// the native-tool routing shape with tool_choice=none; explicit client intent
// is never overwritten.
func applyGrokResponsesCacheIdentity(body, intentSourceBody []byte, identity string, injectFreeTierTools bool) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		if gjson.GetBytes(body, "prompt_cache_key").Exists() {
			return sjson.DeleteBytes(body, "prompt_cache_key")
		}
		return body, nil
	}
	out, err := sjson.SetBytes(body, "prompt_cache_key", identity)
	if err != nil {
		return nil, err
	}
	if !injectFreeTierTools {
		return out, nil
	}
	if gjson.GetBytes(intentSourceBody, "tools").Exists() || gjson.GetBytes(intentSourceBody, "tool_choice").Exists() {
		return out, nil
	}
	out, err = sjson.SetRawBytes(out, "tools", []byte(grokFreeCacheNativeToolsJSON))
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "tool_choice", grokFreeCacheDisabledToolChoice)
}

// applyGrokCacheHeaders applies the Chat Completions conversation-routing
// header from the server-derived identity, replacing any client value.
func applyGrokCacheHeaders(headers http.Header, identity string) {
	if headers == nil {
		return
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		headers.Del(grokConversationIDHeader)
		return
	}
	headers.Set(grokConversationIDHeader, identity)
}

func stripGrokChatPromptCacheKey(body []byte) ([]byte, error) {
	if !gjson.GetBytes(body, "prompt_cache_key").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "prompt_cache_key")
}
