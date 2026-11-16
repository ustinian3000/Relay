package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type Identity struct {
	Session, Parent, Model string
	Main, Compression      bool
}
type Execute func(context.Context, Plan, []byte, http.ResponseWriter)
type Gateway struct {
	Engine                *Engine
	Accounts              func(context.Context, string, []string) ([]Account, error)
	Identify              func(*http.Request, []byte) (Identity, error)
	MaxBody, MaxBuffer    int64
	Timeout, PingInterval time.Duration
}

func NewGateway(engine *Engine) *Gateway {
	return &Gateway{Engine: engine, MaxBody: 16 << 20, MaxBuffer: 1 << 20, Timeout: 90 * time.Second, PingInterval: 10 * time.Second}
}
func writeFault(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "handoff_error", "code": code, "message": code}})
}
func canonical(s string) string {
	if !strings.HasPrefix(s, "claude:") {
		return "claude:" + s
	}
	return s
}

// Control is mounted only behind the same SK authentication as inference.
func (g *Gateway) Control(w http.ResponseWriter, r *http.Request, scope, action string) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Session    string `json:"session_id"`
		Generation uint64 `json:"generation"`
		Notice     string `json:"notice_id"`
		Summary    string `json:"compact_summary"`
		Action     string `json:"action"`
	}
	if action == "status" {
		req.Session = r.URL.Query().Get("session_id")
	} else {
		decoder := json.NewDecoder(io.LimitReader(r.Body, 256<<10))
		if err := decoder.Decode(&req); err != nil {
			writeFault(w, 400, "invalid_control_body")
			return
		}
	}
	if !validID(req.Session) {
		writeFault(w, 400, "invalid_session")
		return
	}
	req.Session = canonical(req.Session)
	var err error
	switch action {
	case "pre-compact":
		err = g.Engine.BeginCompression(scope, req.Session, req.Generation)
	case "compact":
		if len(strings.Fields(req.Summary)) == 0 {
			writeFault(w, 400, "missing_summary")
			return
		}
		err = g.Engine.Notify(scope, req.Session, req.Generation, req.Notice, SummaryDigest(req.Summary))
	case "manual":
		err = g.Engine.Manual(scope, req.Session, req.Action)
	case "status":
		accounts, loadErr := g.Accounts(r.Context(), scope, nil)
		if loadErr != nil {
			writeFault(w, 503, "account_pool_unavailable")
			return
		}
		status, lookupErr := g.Engine.Observe(scope, req.Session, accounts)
		err = lookupErr
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(status)
			return
		}
	default:
		writeFault(w, 404, "unknown_handoff_action")
		return
	}
	if err != nil {
		writeFault(w, 409, Code(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"ok\":true}"))
}

type streamPacket struct {
	data   []byte
	status int
	header http.Header
	done   bool
	err    error
}
type streamSink struct {
	ctx    context.Context
	ch     chan<- streamPacket
	header http.Header
	status int
}

func (s *streamSink) Header() http.Header { return s.header }
func (s *streamSink) send(p streamPacket) error {
	select {
	case s.ch <- p:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (s *streamSink) WriteHeader(code int) {
	if s.status != 0 {
		return
	}
	s.status = code
	_ = s.send(streamPacket{status: code, header: s.header.Clone()})
}
func (s *streamSink) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.WriteHeader(200)
	}
	if err := s.send(streamPacket{data: bytes.Clone(b)}); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (s *streamSink) Flush() {
	if s.status == 0 {
		s.WriteHeader(200)
	}
}

// Messages is the sole inference path allowed for opted-in callers.
func (g *Gateway) Messages(w http.ResponseWriter, r *http.Request, scope string, execute Execute) {
	body, err := io.ReadAll(io.LimitReader(r.Body, g.MaxBody+1))
	if err != nil || int64(len(body)) > g.MaxBody {
		writeFault(w, 413, "request_body_limit")
		return
	}
	var doc struct {
		Stream bool
		Model  string
	}
	if json.Unmarshal(body, &doc) != nil || !doc.Stream {
		writeFault(w, 400, "handoff_requires_messages_sse")
		return
	}
	identity, err := g.Identify(r, body)
	if err != nil || identity.Session == "" {
		writeFault(w, 400, "unsupported_client_or_session")
		return
	}
	digest, opaque, err := g.Engine.MatchEvidence(scope, identity.Session, body)
	if err != nil {
		writeFault(w, 409, Code(err))
		return
	}
	compression, err := g.Engine.IsCompressing(scope, identity.Session)
	if err != nil {
		writeFault(w, 503, Code(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.Timeout)
	defer cancel()
	accounts, err := g.Accounts(ctx, scope, []string{doc.Model})
	if err != nil {
		writeFault(w, 503, "account_pool_unavailable")
		return
	}
	sum := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(sum[:])
	requestID := r.Header.Get("Idempotency-Key")
	if requestID == "" {
		requestID = fingerprint
	}
	plan, err := g.Engine.Plan(Input{Scope: scope, Session: identity.Session, Parent: identity.Parent, Model: doc.Model, RequestID: requestID, Fingerprint: fingerprint, SummaryDigest: digest, Opaque: opaque, Main: identity.Main, Compression: compression || identity.Compression, Accounts: accounts})
	if err != nil {
		writeFault(w, 409, Code(err))
		return
	}
	if len(plan.Replay) > 0 {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(plan.Replay)
		return
	}
	packets := make(chan streamPacket, 4)
	finished := make(chan struct{})
	sink := &streamSink{ctx: ctx, ch: packets, header: make(http.Header)}
	go func() {
		defer close(finished)
		defer func() {
			if recover() != nil {
				_ = sink.send(streamPacket{done: true, err: errors.New("handler_panic")})
			}
		}()
		execute(ctx, plan, body, sink)
		_ = sink.send(streamPacket{done: true})
	}()
	timer := time.NewTicker(g.PingInterval)
	defer timer.Stop()
	started := false
	status := 0
	validStart := false
	stopped := false
	failed := false
	rewriteAttempted := false
	rewriteActive := false
	var pending, buffer bytes.Buffer
	emit := func(b []byte) bool {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)
			started = true
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return true
	}
	fail := func(code string) {
		if started {
			raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "handoff_error", "message": code}})
			_ = emit(append(append([]byte("event: error\ndata: "), raw...), []byte("\n\n")...))
		} else {
			writeFault(w, 502, code)
		}
	}
	finish := func(success bool) error {
		// Authorize again before committing B; revoked credentials cannot be adopted.
		final := accounts
		if plan.Trial && success {
			var loadErr error
			final, loadErr = g.Accounts(ctx, scope, []string{doc.Model})
			if loadErr != nil {
				success = false
			}
		}
		return g.Engine.Finish(scope, plan, success, buffer.Bytes(), final)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		cancel()
		select {
		case <-finished:
			_ = finish(false)
		default:
			go func() { <-finished; _ = g.Engine.Finish(scope, plan, false, nil, accounts) }()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			fail("handoff_response_timeout_or_cancel")
			return
		case <-timer.C:
			if !emit([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")) {
				return
			}
		case packet := <-packets:
			if packet.status != 0 {
				status = packet.status
				if status != 200 {
					failed = true
				}
				continue
			}
			if packet.done {
				success := !failed && packet.err == nil && status == 200 && validStart && stopped && pending.Len() == 0
				err := finish(success)
				completed = true
				if !success || err != nil {
					fail("handoff_incomplete_or_rejected")
					return
				}
				if plan.Trial {
					if !emit(buffer.Bytes()) {
						return
					}
				}
				return
			}
			if int64(pending.Len()+len(packet.data)) > g.MaxBuffer {
				fail("handoff_buffer_limit")
				return
			}
			_, _ = pending.Write(packet.data)
			for {
				raw := pending.Bytes()
				end := bytes.Index(raw, []byte("\n\n"))
				separator := 2
				cr := bytes.Index(raw, []byte("\r\n\r\n"))
				if cr >= 0 && (end < 0 || cr < end) {
					end = cr
					separator = 4
				}
				if end < 0 {
					break
				}
				frame := bytes.Clone(pending.Next(end + separator))
				event, parsed, parseErr := parseFrame(frame)
				if parseErr != nil {
					failed = true
				}
				if stopped && event != "ping" {
					failed = true
				}
				switch event {
				case "message_start":
					if validStart {
						failed = true
					}
					validStart = true
					if !plan.Trial && !rewriteAttempted && identity.Main && !compression && !identity.Compression {
						rewriteAttempted = true
						change, rewriteErr := g.Engine.Rewrite(scope, plan, accounts, true)
						if rewriteErr != nil {
							fail("handoff_storage_unavailable")
							return
						}
						if change {
							rewriteActive = true
						}
					}
				case "message_stop":
					if !validStart {
						failed = true
					}
					stopped = true
				case "error":
					failed = true
				}
				if rewriteActive {
					var rewriteErr error
					frame, rewriteErr = rewriteClientUsage(event, parsed, frame)
					if rewriteErr != nil {
						fail("handoff_invalid_usage")
						return
					}
				}
				if plan.Trial {
					if int64(buffer.Len()+len(frame)) > g.MaxBuffer {
						fail("handoff_buffer_limit")
						return
					}
					_, _ = buffer.Write(frame)
				} else if !emit(frame) {
					return
				}
			}
		}
	}
}

// A response is one trigger, even if the provider repeats usage in later SSE
// events. Keep the reported input floor consistent until this response ends.
// These maps are decoded from copies, after upstream accounting has completed.
func rewriteClientUsage(event string, payload map[string]any, original []byte) ([]byte, error) {
	var usage map[string]any
	switch event {
	case "message_start":
		message, _ := payload["message"].(map[string]any)
		usage, _ = message["usage"].(map[string]any)
		if usage == nil {
			return nil, errors.New("missing initial usage")
		}
	case "message_delta":
		usage, _ = payload["usage"].(map[string]any)
		if usage == nil {
			return original, nil
		}
		if _, present := usage["input_tokens"]; !present {
			return original, nil
		}
	default:
		return original, nil
	}
	input, ok := usage["input_tokens"].(float64)
	if !ok || input < 0 {
		return nil, errors.New("invalid input tokens")
	}
	if input < 180000 {
		usage["input_tokens"] = 180000
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("event: "+event+"\ndata: "), raw...), []byte("\n\n")...), nil
}

func parseFrame(frame []byte) (string, map[string]any, error) {
	event := ""
	var data []string
	for _, line := range strings.Split(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n") {
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(v))
		}
	}
	if len(data) == 0 {
		return event, nil, nil
	}
	var payload map[string]any
	err := json.Unmarshal([]byte(strings.Join(data, "\n")), &payload)
	kind, _ := payload["type"].(string)
	if event == "" {
		event = kind
	} else if kind != "" && kind != event {
		err = errors.New("event_type_mismatch")
	}
	return event, payload, err
}
