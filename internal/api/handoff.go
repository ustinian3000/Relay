package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func (s *Server) initializeHandoff() {
	cfg := s.cfg.Routing.Handoff
	s.handoffScopes = map[string]bool{}
	if !cfg.Enabled {
		return
	}
	for _, scope := range cfg.CallerScopes {
		if scope != "" {
			s.handoffScopes[scope] = true
		}
	}
	path := cfg.StateFile
	if path == "" {
		path = "handoff-state.db"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(s.configFilePath), path)
	}
	engine, err := handoff.Open(path, handoff.DefaultPolicy())
	if err != nil {
		s.handoffInitError = err
		return
	}
	s.handoffEngine = engine
	gateway := handoff.NewGateway(engine)
	gateway.Accounts = func(ctx context.Context, scope string, models []string) ([]handoff.Account, error) {
		// All supported family routes must be represented in each candidate snapshot.
		models = append(append([]string{}, models...), "claude-sonnet-4-6", "claude-haiku-4-5", "claude-haiku-4-5-20251001")
		return s.handlers.AuthManager.HandoffAccounts(ctx, scope, models)
	}
	gateway.Identify = func(r *http.Request, body []byte) (handoff.Identity, error) {
		if !strings.HasPrefix(r.UserAgent(), "claude-cli/2.1.309 ") {
			return handoff.Identity{}, errors.New("unsupported client")
		}
		info, ok := coresession.ExtractSessionInfo(r.Header, body, nil)
		if !ok || info.ClientType != "claude" {
			return handoff.Identity{}, errors.New("missing native session")
		}
		var payload struct{ Model string }
		if json.Unmarshal(body, &payload) != nil {
			return handoff.Identity{}, errors.New("invalid body")
		}
		switch payload.Model {
		case "claude-sonnet-4-6", "claude-haiku-4-5", "claude-haiku-4-5-20251001":
		default:
			return handoff.Identity{}, errors.New("untested handoff model")
		}
		main := !info.IsSubagent && info.ParentSessionID == "" && payload.Model == "claude-sonnet-4-6"
		return handoff.Identity{Session: info.SessionID, Parent: info.ParentSessionID, Main: main, Model: payload.Model, Compression: info.IsCompaction}, nil
	}
	s.handoffGateway = gateway
}

// Applied after business authentication to every compatible inference alias.
// Opted-in callers cannot escape a strict family through another protocol.
func (s *Server) handoffMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := c.GetString("userApiKey")
		if principal == "" || !s.handoffScopes[coresession.CallerScope(principal)] {
			c.Next()
			return
		}
		scope := coresession.CallerScope(principal)
		if s.handoffGateway == nil {
			c.AbortWithStatusJSON(503, gin.H{"error": gin.H{"code": "handoff_storage_unavailable"}})
			return
		}
		path := c.Request.URL.Path
		if path == "/v1/models" || path == "/v1/account-usage" || strings.HasPrefix(path, "/v1/handoff/") {
			c.Next()
			return
		}
		if path != "/v1/messages" {
			c.AbortWithStatusJSON(409, gin.H{"error": gin.H{"code": "handoff_requires_messages_sse"}})
			return
		}
		handler := claude.NewClaudeCodeAPIHandler(s.handlers)
		template := c.Copy()
		s.handoffGateway.Messages(c.Writer, c.Request, scope, func(ctx context.Context, plan handoff.Plan, body []byte, w http.ResponseWriter) {
			gc := template.Copy()
			gc.Writer = &handoffGinWriter{ResponseWriter: w, status: 200}
			gc.Request = template.Request.Clone(ctx)
			gc.Request.Body = io.NopCloser(bytes.NewReader(body))
			gc.Set("handoff.auth", plan.Auth)
			var selected atomic.Value
			gc.Set("handoff.selected", func(id string) { selected.Store(id) })
			handler.ClaudeMessages(gc)
			if id, _ := selected.Load().(string); id != plan.Auth {
				_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"handoff_error\",\"message\":\"selected_account_mismatch\"}}\n\n"))
			}
		})
		c.Abort()
	}
}

func (s *Server) handoffControl(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope := coresession.CallerScope(c.GetString("userApiKey"))
		if !s.handoffScopes[scope] || s.handoffGateway == nil {
			c.AbortWithStatus(404)
			return
		}
		s.handoffGateway.Control(c.Writer, c.Request, scope, action)
	}
}

// Isolated writer for a copied Gin context. Only Gateway writes to the actual
// network response; handlers and heartbeat ticks never share a ResponseWriter.
type handoffGinWriter struct {
	http.ResponseWriter
	status, size int
	written      bool
}

func (w *handoffGinWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
	}
}
func (w *handoffGinWriter) WriteHeaderNow() {
	if !w.written {
		w.written = true
		w.ResponseWriter.WriteHeader(w.status)
	}
}
func (w *handoffGinWriter) Write(b []byte) (int, error) {
	w.WriteHeaderNow()
	n, err := w.ResponseWriter.Write(b)
	w.size += n
	return n, err
}
func (w *handoffGinWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *handoffGinWriter) Flush() {
	w.WriteHeaderNow()
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
func (w *handoffGinWriter) Status() int { return w.status }
func (w *handoffGinWriter) Size() int {
	if !w.written {
		return -1
	}
	return w.size
}
func (w *handoffGinWriter) Written() bool { return w.written }
func (w *handoffGinWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("handoff does not support websocket")
}
func (w *handoffGinWriter) CloseNotify() <-chan bool { return make(chan bool) }
func (w *handoffGinWriter) Pusher() http.Pusher      { return nil }
