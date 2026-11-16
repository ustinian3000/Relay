package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type handoffTestScheduler struct{ calls atomic.Int32 }
type handoffTestAuthentication struct{ usageHTTPAuthentication }

func (handoffTestAuthentication) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	clone := r.Clone(ctx)
	clone.Header = r.Header.Clone()
	if clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Bearer "+r.Header.Get("X-Api-Key"))
	}
	return (usageHTTPAuthentication{}).Authenticate(ctx, clone)
}

func (s *handoffTestScheduler) InspectAuthPool(_ context.Context, req pluginapi.SchedulerPickRequest) ([]string, bool, error) {
	scope, _ := req.Options.Metadata[pluginapi.CallerScopeMetadataKey].(string)
	if scope != session.CallerScope("key-a") {
		return []string{"mock-B"}, true, nil
	}
	return []string{"mock-A", "mock-B"}, true, nil
}
func (s *handoffTestScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.calls.Add(1)
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, true, errors.New("no candidate")
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
}

type handoffCall struct {
	Auth                                               string
	Compacting, HasSummary, HasOld, ToolResult, Strict bool
}
type handoffTestExecutor struct {
	auth.ProviderExecutor
	engine  *handoff.Engine
	mu      sync.Mutex
	calls   []handoffCall
	failB   bool
	tool    bool
	fixture string
}

func (e *handoffTestExecutor) Identifier() string { return "claude" }
func (e *handoffTestExecutor) ExecuteStream(ctx context.Context, a *auth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	info, _ := session.ExtractSessionInfo(opts.Headers, opts.OriginalRequest, opts.Metadata)
	scope, _ := opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	compacting, _ := e.engine.IsCompressing(scope, info.SessionID)
	raw := string(opts.OriginalRequest)
	if raw == "" {
		raw = string(req.Payload)
	}
	strict, _ := opts.Metadata[coreexecutor.StrictHandoffMetadataKey].(bool)
	e.mu.Lock()
	row := handoffCall{Auth: a.ID, Compacting: compacting, HasSummary: strings.Contains(raw, "HANDOFF_SIM_MARKER"), HasOld: strings.Contains(raw, "ORIGINAL_PRIVATE_CONTEXT_882"), ToolResult: strings.Contains(raw, "tool_result"), Strict: strict}
	first := len(e.calls) == 0
	e.calls = append(e.calls, row)
	if dir := os.Getenv("HANDOFF_REPORT_DIR"); dir != "" && strings.Contains(raw, "This session is being continued") {
		var doc struct{ Messages any }
		_ = json.Unmarshal([]byte(raw), &doc)
		data, _ := json.MarshalIndent(doc, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "synthetic-continuation-"+fmt.Sprint(len(e.calls))+".json"), data, 0600)
	}
	fail := e.failB && a.ID == "mock-B"
	e.mu.Unlock()
	if fail {
		return nil, &auth.Error{Code: "simulated_rejection", Message: "mock B rejected", HTTPStatus: 500}
	}
	text := "MOCK_REPLY HANDOFF_SIM_MARKER"
	stop := "end_turn"
	content := map[string]any{"type": "text", "text": text}
	if compacting {
		content["text"] = "<analysis>mock reasoning</analysis><summary>HANDOFF_SIM_MARKER. Continue the synthetic task; the fixture Read already completed.</summary>"
	}
	if e.tool && first {
		content = map[string]any{"type": "tool_use", "id": "toolu_mock_read_882", "name": "Read", "input": map[string]any{"file_path": e.fixture}}
		stop = "tool_use"
	}
	start := map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_mock", "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 1200, "output_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}}
	block := map[string]any{}
	for k, v := range content {
		block[k] = v
	}
	delta := map[string]any{"type": "text_delta", "text": content["text"]}
	if content["type"] == "text" {
		block["text"] = ""
	} else {
		block["input"] = map[string]any{}
		encoded, _ := json.Marshal(content["input"])
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}
	}
	events := []map[string]any{start, {"type": "content_block_start", "index": 0, "content_block": block}, {"type": "content_block_delta", "index": 0, "delta": delta}, {"type": "content_block_stop", "index": 0}, {"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"input_tokens": 1200, "output_tokens": 20}}, {"type": "message_stop"}}
	chunks := make(chan coreexecutor.StreamChunk, len(events))
	for _, event := range events {
		data, _ := json.Marshal(event)
		chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event["type"], data))}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks, Headers: http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

func makeHandoffServer(t *testing.T) (*Server, *handoffTestExecutor) {
	cfg := &config.Config{Routing: config.RoutingConfig{Handoff: config.HandoffConfig{Enabled: true, CallerScopes: []string{session.CallerScope("key-a")}}}}
	s := newTestServerWithConfig(t, cfg)
	s.accessManager.SetProviders([]sdkaccess.Provider{handoffTestAuthentication{}})
	affinity := auth.NewSessionAffinitySelectorWithConfig(auth.SessionAffinityConfig{RebindOn: "quota", TTL: time.Hour})
	s.handlers.AuthManager.SetSelector(affinity)
	t.Cleanup(affinity.Stop)
	s.handlers.AuthManager.SetPluginScheduler(&handoffTestScheduler{})
	executor := &handoffTestExecutor{engine: s.handoffEngine}
	s.handlers.AuthManager.RegisterExecutor(executor)
	for _, id := range []string{"mock-A", "mock-B"} {
		_, err := s.handlers.AuthManager.Register(context.Background(), &auth.Auth{ID: id, Provider: "claude", Status: auth.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	s.handoffGateway.Accounts = func(context.Context, string, []string) ([]handoff.Account, error) {
		return []handoff.Account{{ID: "mock-A", Available: true, Known: true, FiveUsed: 92, WeekUsed: 10, Models: []string{"claude-sonnet-4-6"}},
			{ID: "mock-B", Available: true, Known: true, FiveUsed: 10, WeekUsed: 10, Models: []string{"claude-sonnet-4-6"}}}, nil
	}
	t.Cleanup(func() { _ = s.handoffEngine.Close() })
	return s, executor
}
func handoffHTTP(s *Server, path, body, key string) *httptest.ResponseRecorder {
	method := "POST"
	if strings.Contains(path, "status") {
		method = "GET"
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("User-Agent", "claude-cli/2.1.309 (external, cli)")
	r.Header.Set("X-Claude-Code-Session-Id", "mock-session")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}
func TestHandoffCoreIntegration(t *testing.T) {
	s, e := makeHandoffServer(t)
	body := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "stream": true, "max_tokens": 1024, "messages": []any{map[string]any{"role": "user", "content": text}}})
		return string(raw)
	}
	w := handoffHTTP(s, "/v1/messages", body("ORIGINAL_PRIVATE_CONTEXT_882"), "key-a")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "180000") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, action := range []string{"pre-compact", "compact"} {
		w = handoffHTTP(s, "/v1/handoff/"+action, "{\"session_id\":\"mock-session\",\"generation\":0,\"notice_id\":\"n\",\"compact_summary\":\"HANDOFF_SIM_MARKER\"}", "key-a")
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	w = handoffHTTP(s, "/v1/messages", body("This session is being continued from a previous conversation that ran out of context.\nSummary:\nHANDOFF_SIM_MARKER"), "key-a")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "MOCK_REPLY") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	state, err := s.handoffEngine.Observe(session.CallerScope("key-a"), "claude:mock-session", []handoff.Account{{ID: "mock-A", Available: true, Known: true, Models: []string{"claude-sonnet-4-6"}}, {ID: "mock-B", Available: true, Known: true, Models: []string{"claude-sonnet-4-6"}}})
	if err != nil || state.Generation != 1 {
		t.Fatalf("%+v %v", state, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) != 2 || e.calls[0].Auth != "mock-A" || e.calls[1].Auth != "mock-B" || !e.calls[1].Strict {
		t.Fatalf("%+v", e.calls)
	}
}
func TestHandoffRoutesAndAuthentication(t *testing.T) {
	s, _ := makeHandoffServer(t)
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/backend-api/codex/responses", "/openai/v1/videos", "/v1beta/interactions", "/v1/realtime"} {
		w := handoffHTTP(s, path, "{}", "key-a")
		if w.Code != 409 {
			t.Fatalf("%s -> %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, key := range []string{"", "invalid"} {
		w := handoffHTTP(s, "/v1/handoff/status?session_id=mock-session", "", key)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	w := handoffHTTP(s, "/v1/handoff/status?session_id=mock-session", "", "key-b")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestHandoffCoreRejectedBDoesNotRetryOrChangeBinding(t *testing.T) {
	s, e := makeHandoffServer(t)
	body := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "stream": true, "max_tokens": 128, "messages": []any{map[string]any{"role": "user", "content": text}}})
		return string(raw)
	}
	w := handoffHTTP(s, "/v1/messages", body("initial"), "key-a")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = handoffHTTP(s, "/v1/handoff/compact", "{\"session_id\":\"mock-session\",\"generation\":0,\"notice_id\":\"notice\",\"compact_summary\":\"marker\"}", "key-a")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	e.mu.Lock()
	e.failB = true
	e.mu.Unlock()
	compacted := body("This session is being continued from a previous conversation that ran out of context.\nSummary:\nmarker")
	_ = handoffHTTP(s, "/v1/messages", compacted, "key-a")
	_ = handoffHTTP(s, "/v1/messages", compacted, "key-a")
	e.mu.Lock()
	calls := append([]handoffCall(nil), e.calls...)
	e.mu.Unlock()
	bCalls := 0
	for _, c := range calls {
		if c.Auth == "mock-B" {
			bCalls++
		}
	}
	if bCalls != 1 {
		t.Fatalf("B retried %d times: %+v", bCalls, calls)
	}
	status := handoffHTTP(s, "/v1/handoff/status?session_id=mock-session", "", "key-a")
	var state handoff.Status
	_ = json.Unmarshal(status.Body.Bytes(), &state)
	if state.Generation != 0 || state.Phase != handoff.Blocked {
		t.Fatalf("%+v", state)
	}
}

// Opt-in local executable test: synthetic accounts, mocked executor, loopback only.
func TestHandoffNativeClient(t *testing.T) {
	script := os.Getenv("HANDOFF_NATIVE_TEST_SCRIPT")
	if script == "" {
		t.Skip("native simulated test script not selected")
	}
	for _, tool := range []bool{false, true} {
		name := "text"
		if tool {
			name = "tool"
		}
		t.Run(name, func(t *testing.T) {
			s, e := makeHandoffServer(t)
			e.tool = tool
			fixture := filepath.Join(t.TempDir(), "fixture.txt")
			if err := os.WriteFile(fixture, []byte("SYNTHETIC_FIXTURE_882"), 0600); err != nil {
				t.Fatal(err)
			}
			e.fixture = fixture
			httpServer := httptest.NewServer(s.engine)
			defer httpServer.Close()
			cmd := exec.Command("python", script, httpServer.URL, name, e.fixture)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("native test: %v %s", err, out)
			}
			t.Log(string(out))
			e.mu.Lock()
			calls := append([]handoffCall(nil), e.calls...)
			e.mu.Unlock()
			seenCompact := false
			seenB := false
			toolResult := false
			for _, row := range calls {
				if row.Compacting {
					seenCompact = true
					if row.Auth != "mock-A" {
						t.Fatal("compression escaped A")
					}
				}
				if row.Auth == "mock-B" {
					seenB = true
					if !row.HasSummary || row.HasOld {
						t.Fatalf("unsafe B request: %+v", row)
					}
				}
				toolResult = toolResult || row.ToolResult
			}
			if !seenCompact || !seenB || tool && !toolResult {
				t.Fatalf("missing path %+v", calls)
			}
			reportDir := os.Getenv("HANDOFF_REPORT_DIR")
			if reportDir != "" {
				raw, _ := json.MarshalIndent(calls, "", "  ")
				_ = os.WriteFile(filepath.Join(reportDir, "native-"+name+"-core-requests.json"), raw, 0600)
			}
		})
	}
}
