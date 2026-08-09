package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type GrokOAuthHandler struct {
	grokOAuthService *service.GrokOAuthService
	adminService     service.AdminService
	quotaService     *service.GrokQuotaService
	importProber     grokUsageProber
}

func NewGrokOAuthHandler(
	grokOAuthService *service.GrokOAuthService,
	adminService service.AdminService,
	quotaService *service.GrokQuotaService,
) *GrokOAuthHandler {
	return &GrokOAuthHandler{
		grokOAuthService: grokOAuthService,
		adminService:     adminService,
		quotaService:     quotaService,
		importProber:     quotaService,
	}
}

type GrokGenerateAuthURLRequest struct {
	ProxyID     *int64 `json:"proxy_id"`
	RedirectURI string `json:"redirect_uri"`
}

func (h *GrokOAuthHandler) GetCapabilities(c *gin.Context) {
	response.Success(c, h.grokOAuthService.GetCapabilities())
}

func (h *GrokOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req GrokGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		req = GrokGenerateAuthURLRequest{}
	}
	result, err := h.grokOAuthService.GenerateAuthURL(c.Request.Context(), req.ProxyID, req.RedirectURI)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type GrokExchangeCodeRequest struct {
	SessionID   string `json:"session_id" binding:"required"`
	Code        string `json:"code" binding:"required"`
	State       string `json:"state"`
	RedirectURI string `json:"redirect_uri"`
	ProxyID     *int64 `json:"proxy_id"`
}

func (h *GrokOAuthHandler) ExchangeCode(c *gin.Context) {
	var req GrokExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.grokOAuthService.ExchangeCode(c.Request.Context(), &service.GrokExchangeCodeInput{
		SessionID:   req.SessionID,
		Code:        req.Code,
		State:       req.State,
		RedirectURI: req.RedirectURI,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

type GrokRefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token"`
	RT           string `json:"rt"`
	ClientID     string `json:"client_id"`
	ProxyID      *int64 `json:"proxy_id"`
}

type GrokSSOTokenRequest struct {
	SSOToken string `json:"sso_token"`
	ProxyID  *int64 `json:"proxy_id"`
}

type GrokPasswordAuthorizeRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	ProxyID  *int64 `json:"proxy_id"`
}

func (h *GrokOAuthHandler) RefreshToken(c *gin.Context) {
	var req GrokRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(req.RT)
	}
	if refreshToken == "" {
		response.BadRequest(c, "refresh_token is required")
		return
	}

	var proxyURL string
	if req.ProxyID != nil {
		proxy, err := h.adminService.GetProxy(c.Request.Context(), *req.ProxyID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if proxy == nil {
			response.BadRequest(c, "GROK_OAUTH_PROXY_NOT_FOUND: proxy not found")
			return
		}
		proxyURL = proxy.URL()
	}
	tokenInfo, err := h.grokOAuthService.RefreshToken(c.Request.Context(), refreshToken, proxyURL, req.ClientID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

// ValidateSSOToken converts a Web SSO cookie into Build OAuth tokens.
// The response contains OAuth token info only and never echoes sso_token.
func (h *GrokOAuthHandler) ValidateSSOToken(c *gin.Context) {
	var req GrokSSOTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.grokOAuthService.ValidateSSOToken(c.Request.Context(), req.SSOToken, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

// AuthorizePassword exchanges email/password for Build OAuth tokens via SSO
// conversion. The response never includes the password or raw sso_token.
func (h *GrokOAuthHandler) AuthorizePassword(c *gin.Context) {
	var req GrokPasswordAuthorizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.grokOAuthService.AuthorizePassword(c.Request.Context(), req.Email, req.Password, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

func (h *GrokOAuthHandler) RefreshAccountToken(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account.Platform != service.PlatformGrok {
		response.BadRequest(c, "Account platform does not match Grok OAuth endpoint")
		return
	}
	if !account.IsOAuth() {
		response.BadRequest(c, "Cannot refresh non-OAuth account credentials")
		return
	}
	tokenInfo, err := h.grokOAuthService.RefreshAccountToken(c.Request.Context(), account)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	newCredentials := h.grokOAuthService.BuildAccountCredentials(tokenInfo)
	newCredentials = service.MergeCredentials(account.Credentials, newCredentials)
	if baseURL := strings.TrimSpace(account.GetCredential("base_url")); baseURL != "" {
		newCredentials["base_url"] = baseURL
	}
	updatedAccount, err := h.adminService.UpdateAccount(c.Request.Context(), accountID, &service.UpdateAccountInput{
		Credentials: newCredentials,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(updatedAccount))
}

func (h *GrokOAuthHandler) CreateAccountFromOAuth(c *gin.Context) {
	var req struct {
		SessionID   string  `json:"session_id" binding:"required"`
		Code        string  `json:"code" binding:"required"`
		State       string  `json:"state"`
		RedirectURI string  `json:"redirect_uri"`
		ProxyID     *int64  `json:"proxy_id"`
		Name        string  `json:"name"`
		Concurrency int     `json:"concurrency"`
		Priority    int     `json:"priority"`
		GroupIDs    []int64 `json:"group_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.grokOAuthService.ExchangeCode(c.Request.Context(), &service.GrokExchangeCodeInput{
		SessionID:   req.SessionID,
		Code:        req.Code,
		State:       req.State,
		RedirectURI: req.RedirectURI,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	credentials := h.grokOAuthService.BuildAccountCredentials(tokenInfo)

	name := strings.TrimSpace(req.Name)
	if name == "" && tokenInfo.Email != "" {
		name = tokenInfo.Email
	}
	if name == "" {
		name = "Grok OAuth Account"
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Platform:    service.PlatformGrok,
		Type:        service.AccountTypeOAuth,
		Credentials: credentials,
		ProxyID:     req.ProxyID,
		Concurrency: req.Concurrency,
		Priority:    req.Priority,
		GroupIDs:    req.GroupIDs,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.scheduleGrokImportProbe(account)
	response.Success(c, dto.AccountFromService(account))
}

func (h *GrokOAuthHandler) QueryQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h.quotaService == nil {
		response.BadRequest(c, "grok quota service is not enabled")
		return
	}
	result, err := h.quotaService.QueryQuota(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *GrokOAuthHandler) ResetQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h.quotaService == nil {
		response.BadRequest(c, "grok quota service is not enabled")
		return
	}
	result, err := h.quotaService.ResetQuota(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *GrokOAuthHandler) RuntimeSanity(c *gin.Context) {
	response.Success(c, xai.RuntimeSanity())
}
