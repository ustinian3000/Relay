package api

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"net/http"
)

func (s *Server) accountUsage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Writer.Header().Add("Vary", "Authorization, X-Api-Key, X-Goog-Api-Key")
	principal := c.GetString("userApiKey")
	if principal == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if s.handlers == nil || s.handlers.AuthManager == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	result, err := s.handlers.AuthManager.AccountUsage(c.Request.Context(), session.CallerScope(principal))
	if err != nil {
		doc := ojson.NewDocString(`{}`)
		_ = doc.Set("error.code", "account_pool_unavailable")
		_ = doc.Set("error.message", "Account usage could not be authorized or completed; no account data is returned")
		c.Data(http.StatusServiceUnavailable, "application/json; charset=utf-8", doc.Bytes())
		return
	}
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("accounts", result.Accounts)
	_ = doc.Set("queriedAt", result.QueriedAt)
	_ = doc.Set("cacheSeconds", 60)
	c.Data(http.StatusOK, "application/json; charset=utf-8", doc.Bytes())
}
