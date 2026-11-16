package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fake\",\"usage\":{\"input_tokens\":1200}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"MOCK_REPLY\"}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func gateway(f *fixture) *Gateway {
	g := NewGateway(f.e)
	g.Timeout = time.Second
	g.PingInterval = 5 * time.Millisecond
	g.Accounts = func(context.Context, string, []string) ([]Account, error) { return f.accounts, nil }
	g.Identify = func(*http.Request, []byte) (Identity, error) { return Identity{Session: "root", Main: true}, nil }
	return g
}
func request(text string) *http.Request {
	raw, _ := json.Marshal(map[string]any{"model": "main", "stream": true, "messages": []map[string]any{{"role": "user", "content": text}}})
	return httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(string(raw)))
}
func nativeSummary(text string) string {
	return continuation + " that ran out of context.\nSummary:\n" + text + "\n\nContinue the previous task."
}
func TestGatewayNativeEvidence(t *testing.T) {
	f := setup(t)
	f.prime(t)
	if err := f.e.Notify("sk-one", "root", 0, "n", SummaryDigest("<analysis>private</analysis><summary>Keep marker 123.</summary>")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text  string
		match bool
	}{
		{nativeSummary("Keep marker 123."), true},
		{"Old conversation contains Keep marker 123.", false},
		{nativeSummary("Keep marker 124."), false},
	} {
		r := request(tc.text)
		body, _ := io.ReadAll(r.Body)
		d, _, err := f.e.MatchEvidence("sk-one", "root", body)
		if err != nil || (d != "") != tc.match {
			t.Fatalf("%q %q %v", tc.text, d, err)
		}
	}
}
func TestGatewayUsageClientOnlyAndSplitFrames(t *testing.T) {
	f := setup(t)
	f.prime(t)
	g := gateway(f)
	w := httptest.NewRecorder()
	actual := stream
	g.Messages(w, request("normal"), "sk-one", func(ctx context.Context, p Plan, b []byte, out http.ResponseWriter) {
		out.Header().Set("Content-Type", "text/event-stream")
		out.WriteHeader(200)
		for _, v := range []byte(actual) {
			_, _ = out.Write([]byte{v})
		}
	})
	if !strings.Contains(w.Body.String(), "180000") || strings.Contains(actual, "180000") {
		t.Fatalf("%s", w.Body.String())
	}
	w = httptest.NewRecorder()
	g.Messages(w, request("next"), "sk-one", func(_ context.Context, _ Plan, _ []byte, out http.ResponseWriter) { _, _ = out.Write([]byte(stream)) })
	if strings.Contains(w.Body.String(), "180000") {
		t.Fatal("second rewrite")
	}
}

func TestGatewayUsageFloorPersistsAcrossFinalDelta(t *testing.T) {
	f := setup(t)
	f.prime(t)
	g := gateway(f)
	delta := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1234,\"cache_read_input_tokens\":500,\"output_tokens\":25}}\n\n"
	raw := strings.Replace(stream, "event: message_stop", delta+"event: message_stop", 1)
	w := httptest.NewRecorder()
	g.Messages(w, request("floor"), "sk-one", func(_ context.Context, _ Plan, _ []byte, out http.ResponseWriter) { _, _ = out.Write([]byte(raw)) })
	if strings.Count(w.Body.String(), "180000") != 2 || !strings.Contains(raw, "1234") || strings.Contains(raw, "180000") {
		t.Fatal(w.Body.String())
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.Contains(line, "message_delta") && strings.HasPrefix(line, "data:") {
			var payload map[string]any
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &payload)
			usage := payload["usage"].(map[string]any)
			if usage["output_tokens"] != float64(25) || usage["cache_read_input_tokens"] != float64(500) {
				t.Fatal(usage)
			}
		}
	}
}
func TestGatewayTrialBufferCommitAndReplay(t *testing.T) {
	f := setup(t)
	f.prime(t)
	g := gateway(f)
	if _, err := f.e.Observe("sk-one", "root", f.accounts); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Notify("sk-one", "root", 0, "n", SummaryDigest("keep marker")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := httptest.NewRecorder()
	run := func(_ context.Context, p Plan, _ []byte, out http.ResponseWriter) {
		calls++
		if p.Auth != "B" {
			t.Errorf("target %s", p.Auth)
		}
		_, _ = out.Write([]byte(stream))
	}
	g.Messages(w, request(nativeSummary("keep marker")), "sk-one", run)
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Generation != 1 || s.BoundAccount != publicID("sk-one", "B") || !strings.Contains(w.Body.String(), "MOCK_REPLY") {
		t.Fatalf("%+v %s", s, w.Body.String())
	}
	again := httptest.NewRecorder()
	g.Messages(again, request(nativeSummary("keep marker")), "sk-one", run)
	if calls != 1 || again.Body.String() != w.Body.String() {
		t.Fatalf("calls=%d", calls)
	}
}
func TestGatewayTrialErrorsNeverLeakBufferedText(t *testing.T) {
	for _, mode := range []string{"http", "incomplete", "error", "limit", "revoke", "timeout", "panic"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.prime(t)
			g := gateway(f)
			g.Timeout = 40 * time.Millisecond
			_, _ = f.e.Observe("sk-one", "root", f.accounts)
			_ = f.e.Notify("sk-one", "root", 0, "n", SummaryDigest("marker"))
			w := httptest.NewRecorder()
			g.Messages(w, request(nativeSummary("marker")), "sk-one", func(ctx context.Context, _ Plan, _ []byte, out http.ResponseWriter) {
				switch mode {
				case "http":
					out.WriteHeader(429)
					_, _ = out.Write([]byte("quota exhausted"))
				case "incomplete":
					_, _ = out.Write([]byte(strings.ReplaceAll(stream, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "")))
				case "error":
					_, _ = out.Write([]byte(strings.ReplaceAll(stream, "event: message_stop", "event: error")))
				case "limit":
					_, _ = out.Write([]byte(strings.Repeat("x", int(g.MaxBuffer)+1)))
				case "revoke":
					_, _ = out.Write([]byte(stream))
					f.accounts = f.accounts[:1]
				case "timeout":
					<-ctx.Done()
				case "panic":
					panic("mock")
				}
			})
			if strings.Contains(w.Body.String(), "MOCK_REPLY") {
				t.Fatal("trial output leaked")
			}
			deadline := time.Now().Add(time.Second)
			for {
				s, _ := f.e.Observe("sk-one", "root", f.accounts)
				if s.Generation != 0 {
					t.Fatal("committed failed trial")
				}
				if s.Phase != Trial {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("trial not released")
				}
				time.Sleep(time.Millisecond)
			}
			if mode == "timeout" && !strings.Contains(w.Body.String(), "event: ping") {
				t.Fatal("no keepalive")
			}
		})
	}
}
func TestGatewayCanceledRequestNoCommit(t *testing.T) {
	f := setup(t)
	f.prime(t)
	g := gateway(f)
	_, _ = f.e.Observe("sk-one", "root", f.accounts)
	_ = f.e.Notify("sk-one", "root", 0, "n", SummaryDigest("marker"))
	ctx, cancel := context.WithCancel(context.Background())
	r := request(nativeSummary("marker")).WithContext(ctx)
	w := httptest.NewRecorder()
	g.Messages(w, r, "sk-one", func(ctx context.Context, _ Plan, _ []byte, out http.ResponseWriter) { cancel(); <-ctx.Done() })
	time.Sleep(10 * time.Millisecond)
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Generation != 0 {
		t.Fatal(s)
	}
}
func TestControlNotificationsAndStatus(t *testing.T) {
	f := setup(t)
	p := f.input("init")
	p.Session = "claude:root"
	plan := f.plan(t, p)
	f.finish(t, plan, true)
	g := gateway(f)
	for _, action := range []string{"pre-compact", "compact"} {
		body := fmt.Sprintf("{\"session_id\":\"root\",\"generation\":0,\"notice_id\":\"n\",\"compact_summary\":\"marker\"}")
		w := httptest.NewRecorder()
		g.Control(w, httptest.NewRequest("POST", "http://localhost", strings.NewReader(body)), "sk-one", action)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	g.Control(w, httptest.NewRequest("GET", "http://localhost?session_id=root", nil), "other-sk", "status")
	if w.Code == 200 || strings.Contains(w.Body.String(), publicID("sk-one", "A")) {
		t.Fatal("scope leak")
	}
}
