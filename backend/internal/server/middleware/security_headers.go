package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

const (
	// CSPNonceKey is the context key for storing the CSP nonce
	CSPNonceKey = "csp_nonce"
	// NonceTemplate is the placeholder in CSP policy for nonce
	NonceTemplate = "__CSP_NONCE__"
	// CloudflareInsightsDomain is the domain for Cloudflare Web Analytics
	CloudflareInsightsDomain = "https://static.cloudflareinsights.com"
	// StripeDomain is the domain for Stripe.js SDK
	StripeDomain = "https://*.stripe.com"
	// AirwallexStaticDomain 是 Airwallex 生产环境 SDK 脚本域名。
	AirwallexStaticDomain = "https://static.airwallex.com"
	// AirwallexCheckoutDomain 是 Airwallex 生产环境收银台元素和 iframe 域名。
	AirwallexCheckoutDomain = "https://checkout.airwallex.com"
	// AirwallexDemoStaticDomain 是 Airwallex 沙箱环境 SDK 脚本域名。
	AirwallexDemoStaticDomain = "https://static-demo.airwallex.com"
	// AirwallexDemoCheckoutDomain 是 Airwallex 沙箱环境收银台元素和 iframe 域名。
	AirwallexDemoCheckoutDomain = "https://checkout-demo.airwallex.com"
)

var requiredCSPDirectiveValues = []struct {
	directive string
	value     string
}{
	// 插件配置 UI 使用同源 iframe；目标响应仍必须显式放开 X-Frame-Options，
	// 因此这里只允许 'self' 不会使其他默认 DENY 的管理/API 页面可被嵌入。
	{"frame-src", "'self'"},
	{"script-src", CloudflareInsightsDomain},
	{"script-src", StripeDomain},
	{"frame-src", StripeDomain},
	{"script-src", AirwallexStaticDomain},
	{"script-src", AirwallexCheckoutDomain},
	{"style-src", AirwallexStaticDomain},
	{"style-src", AirwallexCheckoutDomain},
	{"frame-src", AirwallexCheckoutDomain},
	{"script-src", AirwallexDemoStaticDomain},
	{"script-src", AirwallexDemoCheckoutDomain},
	{"style-src", AirwallexDemoStaticDomain},
	{"style-src", AirwallexDemoCheckoutDomain},
	{"frame-src", AirwallexDemoCheckoutDomain},
}

// GenerateNonce generates a cryptographically secure random nonce.
// 返回 error 以确保调用方在 crypto/rand 失败时能正确降级。
func GenerateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate CSP nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// GetNonceFromContext retrieves the CSP nonce from gin context
func GetNonceFromContext(c *gin.Context) string {
	if nonce, exists := c.Get(CSPNonceKey); exists {
		if s, ok := nonce.(string); ok {
			return s
		}
	}
	return ""
}

// SecurityHeaders sets baseline security headers for all responses.
// getFrameSrcOrigins is an optional function that returns extra origins to inject into frame-src;
// pass nil to disable dynamic frame-src injection.
func SecurityHeaders(cfg config.CSPConfig, getFrameSrcOrigins func() []string) gin.HandlerFunc {
	policy := strings.TrimSpace(cfg.Policy)
	if policy == "" {
		policy = config.DefaultCSPPolicy
	}

	// Enhance policy with required directives (nonce placeholder and Cloudflare Insights)
	policy = enhanceCSPPolicy(policy)

	return func(c *gin.Context) {
		finalPolicy := policy
		if getFrameSrcOrigins != nil {
			for _, origin := range getFrameSrcOrigins() {
				if origin != "" {
					finalPolicy = addToDirective(finalPolicy, "frame-src", origin)
				}
			}
		}

		// Short-circuit BEFORE setting browser-security headers: /v1/*,
		// /antigravity/*, /responses and the v1beta family are machine-to-
		// machine API surfaces. Real Anthropic `/v1/messages` responses
		// do NOT include `X-Content-Type-Options`, `X-Frame-Options`, or
		// `Referrer-Policy` — these are web-admin-panel artefacts and
		// their presence fingerprints us as "proxy behind a Go web app"
		// rather than "Anthropic API". Only the admin UI / OAuth login /
		// setup pages need these headers.
		if isAPIRoutePath(c) {
			c.Next()
			return
		}

		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		if cfg.Enabled {
			// Generate nonce for this request
			nonce, err := GenerateNonce()
			if err != nil {
				// crypto/rand 失败时降级为无 nonce 的 CSP 策略
				log.Printf("[SecurityHeaders] %v — 降级为无 nonce 的 CSP", err)
				c.Header("Content-Security-Policy", strings.ReplaceAll(finalPolicy, NonceTemplate, "'unsafe-inline'"))
			} else {
				c.Set(CSPNonceKey, nonce)
				c.Header("Content-Security-Policy", strings.ReplaceAll(finalPolicy, NonceTemplate, "'nonce-"+nonce+"'"))
			}
		}
		c.Next()
	}
}

func isAPIRoutePath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := c.Request.URL.Path
	return strings.HasPrefix(path, "/v1/") ||
		strings.HasPrefix(path, "/v1beta/") ||
		strings.HasPrefix(path, "/antigravity/") ||
		strings.HasPrefix(path, "/responses") ||
		strings.HasPrefix(path, "/images")
}

// enhanceCSPPolicy 确保 CSP 策略包含 nonce 支持和支付 SDK 必需域名。
// 这样旧配置文件没有及时补域名时，前端支付组件仍能正常加载。
func enhanceCSPPolicy(policy string) string {
	// Add nonce placeholder to script-src if not present
	if !strings.Contains(policy, NonceTemplate) && !strings.Contains(policy, "'nonce-") {
		policy = addToDirective(policy, "script-src", NonceTemplate)
	}

	for _, required := range requiredCSPDirectiveValues {
		if !directiveHasValue(policy, required.directive, required.value) {
			policy = addToDirective(policy, required.directive, required.value)
		}
	}

	return policy
}

func directiveHasValue(policy, directive, value string) bool {
	for _, rawDirective := range strings.Split(policy, ";") {
		fields := strings.Fields(strings.TrimSpace(rawDirective))
		if len(fields) == 0 || fields[0] != directive {
			continue
		}
		for _, field := range fields[1:] {
			if field == value {
				return true
			}
		}
		return false
	}
	return false
}

// addToDirective adds a value to a specific CSP directive.
// If the directive doesn't exist, it will be added after default-src.
func addToDirective(policy, directive, value string) string {
	if end, ok := cspDirectiveEnd(policy, directive); ok {
		return policy[:end] + " " + value + policy[end:]
	}
	trimmed := strings.TrimSpace(policy)
	if trimmed == "" {
		return newCSPDirective(directive, value)
	}
	if !strings.HasSuffix(trimmed, ";") {
		trimmed += ";"
	}
	return trimmed + " " + newCSPDirective(directive, value)
}

func cspDirectiveEnd(policy, directive string) (int, bool) {
	start := 0
	for start <= len(policy) {
		end := len(policy)
		if relativeEnd := strings.IndexByte(policy[start:], ';'); relativeEnd >= 0 {
			end = start + relativeEnd
		}
		fields := strings.Fields(policy[start:end])
		if len(fields) > 0 && fields[0] == directive {
			return end, true
		}
		if end == len(policy) {
			break
		}
		start = end + 1
	}
	return 0, false
}

func newCSPDirective(directive, value string) string {
	if value == "'self'" {
		return directive + " 'self';"
	}
	return directive + " 'self' " + value + ";"
}
