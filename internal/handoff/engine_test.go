package handoff

import (
	"encoding/json"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	e        *Engine
	now      time.Time
	accounts []Account
	path     string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "state.db"),
		accounts: []Account{{ID: "A", Available: true, Known: true, FiveUsed: 10, WeekUsed: 10, Models: []string{"main", "small"}},
			{ID: "B", Available: true, Known: true, FiveUsed: 15, WeekUsed: 15, Models: []string{"main", "small"}}}}
	var err error
	f.e, err = Open(f.path, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	f.e.now = func() time.Time { return f.now }
	t.Cleanup(func() { _ = f.e.Close() })
	return f
}
func (f *fixture) input(id string) Input {
	return Input{Scope: "sk-one", Session: "root", Model: "main", Main: true, RequestID: id, Fingerprint: id, Accounts: f.accounts}
}
func (f *fixture) plan(t *testing.T, in Input) Plan {
	t.Helper()
	p, err := f.e.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *fixture) finish(t *testing.T, p Plan, ok bool) {
	t.Helper()
	if err := f.e.Finish("sk-one", p, ok, []byte("complete mocked SSE response"), f.accounts); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) prime(t *testing.T) {
	p := f.plan(t, f.input("initial"))
	f.finish(t, p, true)
	f.accounts[0].FiveUsed = 92
}

const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (f *fixture) notice(t *testing.T) {
	t.Helper()
	if _, err := f.e.Observe("sk-one", "root", f.accounts); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Notify("sk-one", "root", 0, "receipt-1", digest); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) trial(t *testing.T) Plan {
	f.notice(t)
	in := f.input("trial")
	in.SummaryDigest = digest
	return f.plan(t, in)
}
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if Code(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestNoCandidateNoTrigger(t *testing.T) {
	f := setup(t)
	f.accounts = f.accounts[:1]
	f.prime(t)
	p := f.plan(t, f.input("second"))
	changed, err := f.e.Rewrite("sk-one", p, f.accounts, true)
	if err != nil || changed {
		t.Fatalf("rewrite %v %v", changed, err)
	}
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Phase != Stable || s.CandidateReady {
		t.Fatal(s)
	}
	f.finish(t, p, true)
}
func TestUnknownOrInsufficientCandidate(t *testing.T) {
	for _, kind := range []string{"unknown", "low", "disabled", "model"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.prime(t)
			switch kind {
			case "unknown":
				f.accounts[1].Known = false
			case "low":
				f.accounts[1].WeekUsed = 90
			case "disabled":
				f.accounts[1].Available = false
			case "model":
				f.accounts[1].Models = []string{"small"}
			}
			s, err := f.e.Observe("sk-one", "root", f.accounts)
			if err != nil || s.CandidateReady || s.Phase != Stable {
				t.Fatalf("%+v %v", s, err)
			}
		})
	}
}
func TestTriggerOnceAndTimeout(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.plan(t, f.input("second"))
	for i := 0; i < 2; i++ {
		yes, err := f.e.Rewrite("sk-one", p, f.accounts, true)
		if err != nil || yes != (i == 0) {
			t.Fatalf("%v %v", yes, err)
		}
	}
	f.finish(t, p, true)
	f.now = f.now.Add(time.Minute)
	s, err := f.e.Observe("sk-one", "root", f.accounts)
	if err != nil || s.Phase != Pending {
		t.Fatalf("%+v %v", s, err)
	}
	p = f.plan(t, f.input("third"))
	yes, _ := f.e.Rewrite("sk-one", p, f.accounts, true)
	if yes {
		t.Fatal("repeated trigger")
	}
	f.finish(t, p, true)
}
func TestMainOnlyTrigger(t *testing.T) {
	f := setup(t)
	f.prime(t)
	in := f.input("child")
	in.Session = "child"
	in.Parent = "root"
	in.Main = false
	p := f.plan(t, in)
	yes, err := f.e.Rewrite("sk-one", p, f.accounts, true)
	if err != nil || yes {
		t.Fatalf("%v %v", yes, err)
	}
	f.finish(t, p, true)
}
func TestHandoffPersistenceAndReplay(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.trial(t)
	if !p.Trial || p.Auth != "B" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
	_ = f.e.Close()
	var err error
	f.e, err = Open(f.path, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	f.e.now = func() time.Time { return f.now }
	in := f.input("trial")
	in.SummaryDigest = digest
	p = f.plan(t, in)
	if p.Auth != "B" || len(p.Replay) == 0 || p.Generation != 1 {
		t.Fatal(p)
	}
	in.Fingerprint = "different"
	_, err = f.e.Plan(in)
	wantCode(t, err, "request_id_conflict")
	in = f.input("next")
	in.SummaryDigest = digest
	p = f.plan(t, in)
	if p.Auth != "B" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
}
func TestFailedTrialKeepsOriginAndRequiresManualRetry(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.trial(t)
	f.finish(t, p, false)
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Phase != Blocked || s.Generation != 0 || s.BoundAccount != publicID("sk-one", "A") {
		t.Fatal(s)
	}
	in := f.input("retry")
	in.SummaryDigest = digest
	_, err := f.e.Plan(in)
	wantCode(t, err, "handoff_retry_required")
	f.now = f.now.Add(2 * time.Minute)
	if err = f.e.Manual("sk-one", "root", "retry"); err != nil {
		t.Fatal(err)
	}
	p = f.plan(t, in)
	if p.Auth != "B" || !p.Trial {
		t.Fatal(p)
	}
	f.finish(t, p, false)
	wantCode(t, f.e.Manual("sk-one", "root", "retry"), "retry_limit")
	if err = f.e.Manual("sk-one", "root", "cancel"); err != nil {
		t.Fatal(err)
	}
	p = f.plan(t, in)
	if p.Auth != "A" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
}
func TestStaleAndOpaqueContextNeverReachesB(t *testing.T) {
	f := setup(t)
	f.prime(t)
	f.notice(t)
	p := f.plan(t, f.input("old"))
	if p.Trial || p.Auth != "A" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
	in := f.input("opaque")
	in.SummaryDigest = digest
	in.Opaque = true
	_, err := f.e.Plan(in)
	wantCode(t, err, "opaque_context")
}
func TestNoticeIdempotencyAndScope(t *testing.T) {
	f := setup(t)
	f.prime(t)
	f.notice(t)
	if err := f.e.Notify("sk-one", "root", 0, "receipt-1", digest); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.e.Notify("sk-one", "root", 1, "other", digest), "stale_notice")
	wantCode(t, f.e.Notify("other-sk", "root", 0, "other", digest), "unknown_session")
	s, err := f.e.Observe("other-sk", "root", f.accounts)
	wantCode(t, err, "unknown_session")
	if s.BoundAccount != "" {
		t.Fatal("scope leaked")
	}
}
func TestDrainAndExpiredLeasePersist(t *testing.T) {
	f := setup(t)
	f.prime(t)
	in := f.input("child")
	in.Session = "child"
	in.Parent = "root"
	in.Main = false
	p := f.plan(t, in)
	f.notice(t)
	in = f.input("trial")
	in.SummaryDigest = digest
	_, err := f.e.Plan(in)
	wantCode(t, err, "family_draining")
	f.now = f.now.Add(100 * time.Second)
	_, err = f.e.Plan(in)
	wantCode(t, err, "family_draining")
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Phase != Blocked {
		t.Fatalf("timeout transition rolled back: %+v", s)
	}
	f.finish(t, p, false)
}
func TestActualExhaustionNeverSwitches(t *testing.T) {
	f := setup(t)
	f.prime(t)
	f.notice(t)
	f.accounts[0].FiveUsed = 100
	in := f.input("trial")
	in.SummaryDigest = digest
	_, err := f.e.Plan(in)
	wantCode(t, err, "bound_account_unavailable")
}
func TestRevokedCandidateAndOrigin(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.trial(t)
	f.accounts = f.accounts[:1]
	wantCode(t, f.e.Finish("sk-one", p, true, []byte("mock"), f.accounts), "trial_commit_rejected")
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Generation != 0 {
		t.Fatal(s)
	}
	f.accounts = nil
	_, err := f.e.Plan(f.input("later"))
	wantCode(t, err, "bound_account_not_authorized")
}
func TestFamilyRoutesAndChildGate(t *testing.T) {
	f := setup(t)
	f.prime(t)
	in := f.input("child-init")
	in.Session = "child"
	in.Parent = "root"
	in.Main = false
	in.Model = "small"
	p := f.plan(t, in)
	f.finish(t, p, true)
	p = f.trial(t)
	f.finish(t, p, true)
	in = f.input("child-old")
	in.Session = "child"
	in.Parent = "root"
	in.Main = false
	in.Model = "small"
	_, err := f.e.Plan(in)
	wantCode(t, err, "child_context_requires_compaction")
	if err = f.e.Notify("sk-one", "child", 1, "child-notice", digest); err != nil {
		t.Fatal(err)
	}
	in.SummaryDigest = digest
	p = f.plan(t, in)
	if p.Auth != "B" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
	in = f.input("root-old")
	_, err = f.e.Plan(in)
	wantCode(t, err, "stale_context")
}
func TestConcurrentSingleTrial(t *testing.T) {
	f := setup(t)
	f.prime(t)
	f.notice(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	trials := 0
	var winner Plan
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := f.input(fmt.Sprintf("trial-%d", i))
			in.SummaryDigest = digest
			p, err := f.e.Plan(in)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				trials++
				winner = p
			} else if Code(err) != "handoff_in_progress" {
				t.Errorf("unexpected %v", err)
			}
		}(i)
	}
	wg.Wait()
	if trials != 1 {
		t.Fatal(trials)
	}
	f.finish(t, winner, true)
}
func TestBoundedReplayAndFailureTimeout(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.trial(t)
	f.now = f.now.Add(2 * time.Minute)
	wantCode(t, f.e.Finish("sk-one", p, true, []byte("mock"), f.accounts), "trial_commit_rejected")
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if s.Generation != 0 || s.Phase != Blocked {
		t.Fatal(s)
	}
}
func TestClosedStore(t *testing.T) {
	f := setup(t)
	_ = f.e.Close()
	_, err := f.e.Plan(f.input("new"))
	wantCode(t, err, "store_closed")
}

func TestCandidateReservationsAndBestRemaining(t *testing.T) {
	f := setup(t)
	f.accounts = append(f.accounts, Account{ID: "C", Available: true, Known: true, FiveUsed: 5, WeekUsed: 5, Models: []string{"main", "small"}})
	f.prime(t)
	s, _ := f.e.Observe("sk-one", "root", f.accounts)
	if !s.CandidateReady {
		t.Fatal(s)
	}
	f.notice(t)
	in := f.input("trial")
	in.SummaryDigest = digest
	p := f.plan(t, in)
	if p.Auth != "C" {
		t.Fatalf("best remaining not selected: %s", p.Auth)
	}
	f.finish(t, p, true)
	// Recently admitted C consumes one of its two admission slots.
	for n := 0; n < 3; n++ {
		in = f.input(fmt.Sprintf("new-%d", n))
		in.Session = fmt.Sprintf("session-%d", n)
		p = f.plan(t, in)
		f.finish(t, p, true)
	}
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	err := f.e.db.View(func(tx *bolt.Tx) error {
		cCount := 0
		return tx.Bucket(familyBucket).ForEach(func(_, raw []byte) error {
			var family Family
			if err := json.Unmarshal(raw, &family); err != nil {
				return err
			}
			if family.Candidate == "C" {
				cCount++
			}
			if cCount > 1 {
				t.Error("reservation capacity exceeded")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLostNotificationKeepsA(t *testing.T) {
	f := setup(t)
	f.prime(t)
	in := f.input("summary-without-receipt")
	in.SummaryDigest = digest
	p := f.plan(t, in)
	if p.Auth != "A" || p.Trial {
		t.Fatal(p)
	}
	f.finish(t, p, true)
	f.notice(t)
	in.RequestID = "after-receipt"
	p = f.plan(t, in)
	if p.Auth != "B" || !p.Trial {
		t.Fatal(p)
	}
	f.finish(t, p, true)
}

func TestRestartInterruptedLeaseNeedsExplicitRecovery(t *testing.T) {
	f := setup(t)
	f.prime(t)
	_ = f.trial(t)
	_ = f.e.Close()
	var err error
	f.e, err = Open(f.path, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	f.e.now = func() time.Time { return f.now }
	in := f.input("late")
	in.SummaryDigest = digest
	_, err = f.e.Plan(in)
	wantCode(t, err, "handoff_in_progress")
	f.now = f.now.Add(2 * time.Minute)
	_, err = f.e.Plan(in)
	wantCode(t, err, "family_draining")
	if err = f.e.Manual("sk-one", "root", "acknowledge-drain"); err != nil {
		t.Fatal(err)
	}
	if err = f.e.Manual("sk-one", "root", "cancel"); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, in)
	if p.Auth != "A" {
		t.Fatal(p)
	}
	f.finish(t, p, true)
}

func TestActiveLeaseCannotBeManuallyReleased(t *testing.T) {
	f := setup(t)
	f.prime(t)
	p := f.plan(t, f.input("slow"))
	f.now = f.now.Add(2 * time.Minute)
	_, _ = f.e.Observe("sk-one", "root", f.accounts)
	wantCode(t, f.e.Manual("sk-one", "root", "acknowledge-drain"), "family_busy")
	f.finish(t, p, false)
}

func TestExplicitHandoffPreservesQuotaAndRequiresCandidate(t *testing.T) {
	for _, mode := range []string{"prepare", "trigger", "no-candidate"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := f.plan(t, f.input("initial"))
			f.finish(t, p, true)
			if mode == "no-candidate" {
				f.accounts = f.accounts[:1]
			}
			action := mode
			if mode == "no-candidate" {
				action = "trigger"
			}
			if err := f.e.Manual("sk-one", "root", action); err != nil {
				t.Fatal(err)
			}
			p = f.plan(t, f.input("next"))
			changed, err := f.e.Rewrite("sk-one", p, f.accounts, true)
			if err != nil || changed != (mode == "trigger") {
				t.Fatalf("rewrite=%v err=%v", changed, err)
			}
			f.finish(t, p, true)
			s, err := f.e.Observe("sk-one", "root", f.accounts)
			if err != nil || s.FiveUsed == nil || *s.FiveUsed != 10 {
				t.Fatalf("quota modified %+v %v", s, err)
			}
			if mode == "no-candidate" && s.CandidateReady {
				t.Fatal("invalid candidate")
			}
		})
	}
}
