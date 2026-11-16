// Package handoff owns durable caller-scoped account handoff state.
// Records contain identifiers, summary digests and a bounded, short-lived replay
// of the first successful response. No credentials or request bodies are stored.
package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Phase string

const (
	Stable    Phase = "normal"
	Pending   Phase = "pending"
	Triggered Phase = "triggered"
	Awaiting  Phase = "awaiting_context"
	Trial     Phase = "trial"
	Blocked   Phase = "blocked"
)

type Fault struct{ Code string }

func (e *Fault) Error() string { return e.Code }
func fault(code string) error  { return &Fault{Code: code} }
func Code(err error) string {
	var typed *Fault
	if errors.As(err, &typed) {
		return typed.Code
	}
	if err == nil {
		return ""
	}
	return "handoff_storage_unavailable"
}

type Policy struct {
	Soft, Force, Hard, MinRemaining                                        float64
	PendingTimeout, TriggerTimeout, ContextTimeout, TrialTimeout, Cooldown time.Duration
	MaxAttempts, MaxReservations, MaxSessions                              int
}

func DefaultPolicy() Policy {
	return Policy{Soft: 80, Force: 90, Hard: 97, MinRemaining: 20,
		PendingTimeout: 10 * time.Minute, TriggerTimeout: 45 * time.Second,
		ContextTimeout: 2 * time.Minute, TrialTimeout: 90 * time.Second, Cooldown: time.Minute,
		MaxAttempts: 2, MaxReservations: 2, MaxSessions: 10000}
}

func (p Policy) validate() error {
	if !(0 < p.Soft && p.Soft < p.Force && p.Force < p.Hard && p.Hard < 100) ||
		p.MinRemaining <= 0 || p.MinRemaining >= 100 || p.MaxAttempts < 1 ||
		p.MaxReservations < 1 || p.MaxSessions < 1 ||
		p.PendingTimeout <= 0 || p.TriggerTimeout <= 0 || p.ContextTimeout <= 0 ||
		p.TrialTimeout <= 0 || p.Cooldown <= 0 {
		return fault("invalid_policy")
	}
	return nil
}

type Account struct {
	ID                 string
	Available, Known   bool
	FiveUsed, WeekUsed float64
	Models             []string
}

func (a Account) used() float64 { return math.Max(a.FiveUsed, a.WeekUsed) }
func (a Account) eligible(models []string, minimum float64) bool {
	if a.ID == "" || !a.Available || !a.Known || math.IsNaN(a.FiveUsed) || math.IsNaN(a.WeekUsed) ||
		a.FiveUsed < 0 || a.WeekUsed < 0 || a.FiveUsed > 100 || a.WeekUsed > 100 ||
		100-a.used() < minimum {
		return false
	}
	for _, model := range models {
		found := false
		for _, supported := range a.Models {
			if supported == model {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type Lease struct {
	Owner                           string
	Token, RequestID, Session, Auth string
	Fingerprint                     string
	Generation                      uint64
	Until                           time.Time
	Trial, Main                     bool
}

type Family struct {
	Requested, TriggerRequested              bool
	Compressing                              map[string]time.Time
	Scope, Root, Auth, Candidate, HandoffID  string
	Generation                               uint64
	Phase                                    Phase
	Deadline, RecentAdmissionUntil           time.Time
	Models                                   []string
	Members                                  map[string]bool
	MemberSummaries                          map[string]string
	Leases                                   map[string]Lease
	Cooldowns                                map[string]time.Time
	Attempts                                 int
	TriggeredGeneration                      *uint64
	ActiveSummary, NoticeSummary, LastNotice string
	NoticeAttempted, Suppressed              bool
	LastReason, ReplayRequest                string
	ReplayFingerprint                        string
	ReplayUntil                              time.Time
	Replay                                   []byte
}

type Input struct {
	Scope, Session, Parent, Model, RequestID, SummaryDigest string
	Fingerprint                                             string
	Main, Compression, Opaque                               bool
	Accounts                                                []Account
}

type Plan struct {
	Auth, Root, Token, RequestID string
	Generation                   uint64
	Trial                        bool
	Replay                       []byte
	Fingerprint                  string
}

type Status struct {
	Session        string   `json:"sessionId"`
	Phase          Phase    `json:"phase"`
	Generation     uint64   `json:"generation"`
	HandoffID      string   `json:"handoffId"`
	BoundAccount   string   `json:"boundAccount"`
	FiveUsed       *float64 `json:"fiveHourUsedPercent"`
	WeekUsed       *float64 `json:"weeklyUsedPercent"`
	CandidateReady bool     `json:"candidateReady"`
	Reason         string   `json:"reason"`
	Soft           float64  `json:"softThreshold"`
	Force          float64  `json:"forceThreshold"`
	Hard           float64  `json:"hardThreshold"`
}

type Engine struct {
	owner   string
	mu      sync.Mutex
	db      *bolt.DB
	policy  Policy
	now     func() time.Time
	closed  bool
	stop    chan struct{}
	stopped chan struct{}
}

var familyBucket = []byte("families-v1")

func Open(path string, policy Policy) (*Engine, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err = db.Update(func(tx *bolt.Tx) error { _, e := tx.CreateBucketIfNotExists(familyBucket); return e }); err != nil {
		_ = db.Close()
		return nil, err
	}
	e := &Engine{db: db, policy: policy, now: time.Now, owner: randomID(), stop: make(chan struct{}), stopped: make(chan struct{})}
	go e.sweeper()
	return e, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	close(e.stop)
	return e.db.Close()
}
func validID(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}
func key(scope, root string) []byte { return []byte(scope + "\x00" + root) }
func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}
func publicID(scope, id string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + id))
	return hex.EncodeToString(sum[:8])
}
func put(bucket *bolt.Bucket, f *Family) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return bucket.Put(key(f.Scope, f.Root), raw)
}
func lookup(bucket *bolt.Bucket, scope, session string) (*Family, error) {
	if raw := bucket.Get(key(scope, session)); raw != nil {
		var f Family
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		return &f, nil
	}
	var found *Family
	err := bucket.ForEach(func(_, raw []byte) error {
		var f Family
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if f.Scope == scope && f.Members[session] {
			if found != nil {
				return fault("ambiguous_family")
			}
			copy := f
			found = &copy
		}
		return nil
	})
	return found, err
}
func account(accounts []Account, id string) (Account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}
func addModel(f *Family, model string) {
	for _, existing := range f.Models {
		if existing == model {
			return
		}
	}
	f.Models = append(f.Models, model)
	sort.Strings(f.Models)
}

func (e *Engine) expire(f *Family, now time.Time) {
	if f.MemberSummaries == nil {
		f.MemberSummaries = map[string]string{}
	}
	if !f.ReplayUntil.After(now) {
		f.Replay = nil
		f.ReplayRequest = ""
		f.ReplayFingerprint = ""
	}
	for token, lease := range f.Leases {
		if !lease.Until.After(now) {
			// An expired lease does not prove the upstream stopped running.
			// Keep it until its owning request explicitly finishes.
			_ = token
			f.Phase = Blocked
			f.LastReason = "request_drain_timeout"
			if lease.Trial {
				f.Cooldowns[lease.Auth] = now.Add(e.policy.Cooldown)
				f.Candidate = ""
				f.Phase = Blocked
				f.LastReason = "trial_timeout"
			}
		}
	}
	if !f.Deadline.IsZero() && !f.Deadline.After(now) {
		switch f.Phase {
		case Triggered:
			f.Phase = Pending
			f.Candidate = ""
			f.LastReason = "trigger_timeout"
		case Awaiting:
			f.Phase = Pending
			f.Candidate = ""
			f.NoticeSummary = ""
			f.LastReason = "context_timeout"
		case Pending:
			f.Candidate = ""
			f.LastReason = "candidate_lease_expired"
		}
		f.Deadline = time.Time{}
	}
}

func (e *Engine) choose(bucket *bolt.Bucket, f *Family, accounts []Account, now time.Time) (string, error) {
	held := map[string]int{}
	err := bucket.ForEach(func(_, raw []byte) error {
		var other Family
		if err := json.Unmarshal(raw, &other); err != nil {
			return err
		}
		if other.Root == f.Root && other.Scope == f.Scope {
			return nil
		}
		if other.Candidate != "" && other.Deadline.After(now) {
			held[other.Candidate]++
		}
		if other.RecentAdmissionUntil.After(now) {
			held[other.Auth]++
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	candidates := append([]Account(nil), accounts...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].used() == candidates[j].used() {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].used() < candidates[j].used()
	})
	for _, a := range candidates {
		if a.ID != f.Auth && a.eligible(f.Models, e.policy.MinRemaining) &&
			!f.Cooldowns[a.ID].After(now) && held[a.ID] < e.policy.MaxReservations {
			return a.ID, nil
		}
	}
	return "", nil
}

func (e *Engine) evaluate(bucket *bolt.Bucket, f *Family, accounts []Account, now time.Time) error {
	e.expire(f, now)
	origin, allowed := account(accounts, f.Auth)
	if !allowed {
		return fault("bound_account_not_authorized")
	}
	if f.Phase == Trial {
		return nil
	}
	if f.Suppressed && origin.Known && origin.used() < e.policy.Soft {
		f.Suppressed = false
	}
	if f.Phase == Blocked || f.Suppressed {
		return nil
	}
	if !origin.Known || (!f.Requested && origin.used() < e.policy.Soft) {
		if f.Phase == Pending {
			f.Phase = Stable
			f.Candidate = ""
			f.HandoffID = ""
		}
		return nil
	}
	if !origin.Available {
		f.Candidate = ""
		return nil
	}
	if f.Candidate != "" && f.Deadline.After(now) {
		if candidate, ok := account(accounts, f.Candidate); ok && candidate.eligible(f.Models, e.policy.MinRemaining) {
			return nil
		}
		f.Candidate = ""
		f.NoticeSummary = ""
		f.NoticeAttempted = false
		f.Phase = Pending
		f.LastReason = "candidate_revoked"
	}
	candidate, err := e.choose(bucket, f, accounts, now)
	if err != nil {
		return err
	}
	if candidate == "" {
		f.Candidate = ""
		f.LastReason = "no_eligible_candidate"
		if f.Phase == Pending {
			f.Phase = Stable
		}
		return nil
	}
	f.Candidate = candidate
	if f.Phase == Stable {
		f.Phase = Pending
		f.HandoffID = randomID()
		f.Attempts = 0
		f.LastReason = ""
	}
	if f.Phase == Pending && f.Deadline.IsZero() {
		f.Deadline = now.Add(e.policy.PendingTimeout)
	}
	return nil
}

func (e *Engine) Plan(in Input) (Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var plan Plan
	if e.closed {
		return plan, fault("store_closed")
	}
	if !validID(in.Scope) || !validID(in.Session) || !validID(in.Model) || !validID(in.RequestID) {
		return plan, fault("invalid_request_identity")
	}
	var businessErr error
	err := e.db.Update(func(tx *bolt.Tx) (txErr error) {
		bucket := tx.Bucket(familyBucket)
		now := e.now()
		f, err := lookup(bucket, in.Scope, in.Session)
		if err != nil {
			return err
		}
		defer func() {
			var typed *Fault
			if f != nil && f.Auth != "" && errors.As(txErr, &typed) {
				businessErr = txErr
				txErr = put(bucket, f)
			}
		}()
		if in.Parent != "" {
			parent, err := lookup(bucket, in.Scope, in.Parent)
			if err != nil {
				return err
			}
			if parent == nil {
				return fault("parent_not_registered")
			}
			if f != nil && f.Root != parent.Root {
				return fault("family_conflict")
			}
			f = parent
		}
		if f == nil {
			if bucket.Stats().KeyN >= e.policy.MaxSessions {
				return fault("session_limit")
			}
			f = &Family{Scope: in.Scope, Root: in.Session, Phase: Stable,
				Members: map[string]bool{}, Leases: map[string]Lease{}, Cooldowns: map[string]time.Time{}}
			addModel(f, in.Model)
			for _, a := range in.Accounts {
				if a.eligible(f.Models, 0) {
					f.Auth = a.ID
					break
				}
			}
			if f.Auth == "" {
				return fault("no_initial_account")
			}
		}
		knownMember := f.Members[in.Session]
		if !knownMember && len(f.Members) >= 128 {
			return fault("family_member_limit")
		}
		if len(f.Leases) >= 128 {
			return fault("family_request_limit")
		}
		if origin, ok := account(in.Accounts, f.Auth); ok {
			modelOK := false
			for _, m := range origin.Models {
				if m == in.Model {
					modelOK = true
				}
			}
			if !modelOK {
				return fault("bound_model_unavailable")
			}
		}
		f.Members[in.Session] = true
		addModel(f, in.Model)
		if err := e.evaluate(bucket, f, in.Accounts, now); err != nil {
			return err
		}
		origin, allowed := account(in.Accounts, f.Auth)
		if !allowed {
			return fault("bound_account_not_authorized")
		}
		if f.ReplayRequest == in.RequestID && len(f.Replay) > 0 {
			if f.ReplayFingerprint != in.Fingerprint {
				return fault("request_id_conflict")
			}
			plan = Plan{Auth: f.Auth, Root: f.Root, Generation: f.Generation, Replay: append([]byte(nil), f.Replay...)}
			return put(bucket, f)
		}
		for _, lease := range f.Leases {
			if lease.RequestID == in.RequestID {
				if lease.Fingerprint != in.Fingerprint {
					return fault("request_id_conflict")
				}
				return fault("request_in_progress")
			}
		}
		if f.Phase == Trial {
			return fault("handoff_in_progress")
		}
		if f.Phase == Blocked && f.LastReason == "request_drain_timeout" {
			return fault("family_draining")
		}
		if !origin.Available || (origin.Known && origin.used() >= 100) {
			return fault("bound_account_unavailable")
		}
		if in.Session != f.Root && f.Generation > 0 && !in.Compression {
			expected := f.MemberSummaries[in.Session]
			if expected == "" && !knownMember && in.SummaryDigest == f.ActiveSummary {
				expected = f.ActiveSummary
				f.MemberSummaries[in.Session] = expected
			}
			if expected == "" || in.SummaryDigest != expected {
				return fault("child_context_requires_compaction")
			}
		}
		if in.Session != f.Root && f.Generation > 0 && in.Compression && f.MemberSummaries[in.Session] == "" {
			return fault("child_context_requires_new_session")
		}
		if in.Session == f.Root && f.ActiveSummary != "" && !in.Compression && in.SummaryDigest != f.ActiveSummary && (f.NoticeSummary == "" || in.SummaryDigest != f.NoticeSummary) {
			return fault("stale_context")
		}
		isTrial := false
		target := f.Auth
		if in.Main && !in.Compression && f.NoticeSummary != "" && in.SummaryDigest == f.NoticeSummary {
			if in.Opaque {
				return fault("opaque_context")
			}
			if len(f.Leases) > 0 {
				return fault("family_draining")
			}
			if f.NoticeAttempted && f.Phase == Blocked {
				return fault("handoff_retry_required")
			}
			if f.NoticeAttempted && f.Candidate != "" && f.Attempts < e.policy.MaxAttempts {
				candidate, ok := account(in.Accounts, f.Candidate)
				if !ok || !candidate.eligible(f.Models, e.policy.MinRemaining) || f.Cooldowns[candidate.ID].After(now) {
					return fault("candidate_unavailable")
				}
				isTrial = true
				target = candidate.ID
				f.Phase = Trial
				f.Attempts++
				f.Deadline = now.Add(e.policy.TrialTimeout)
			} else if f.NoticeAttempted {
				return fault("handoff_retry_required")
			} else {
				f.ActiveSummary = f.NoticeSummary
				f.NoticeSummary = ""
			}
		}
		if !isTrial {
			if !origin.eligible(f.Models, 0) {
				return fault("bound_model_unavailable")
			}
			if in.Opaque && f.Generation > 0 {
				return fault("opaque_context")
			}
		}
		token := randomID()
		f.Leases[token] = Lease{Owner: e.owner, Token: token, RequestID: in.RequestID, Session: in.Session, Auth: target, Fingerprint: in.Fingerprint,
			Generation: f.Generation, Until: now.Add(e.policy.TrialTimeout), Trial: isTrial, Main: in.Main}
		plan = Plan{Auth: target, Root: f.Root, Token: token, RequestID: in.RequestID, Fingerprint: in.Fingerprint, Generation: f.Generation, Trial: isTrial}
		return put(bucket, f)
	})
	if err == nil {
		err = businessErr
	}
	return plan, err
}

// Rewrite consumes the generation's single trigger before releasing changed bytes.
func (e *Engine) Rewrite(scope string, plan Plan, accounts []Account, eligible bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rewrite := false
	err := e.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(familyBucket)
		f, err := lookup(bucket, scope, plan.Root)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		lease, ok := f.Leases[plan.Token]
		if !ok || lease.Generation != f.Generation {
			return fault("stale_lease")
		}
		if err := e.evaluate(bucket, f, accounts, e.now()); err != nil {
			return err
		}
		origin, _ := account(accounts, f.Auth)
		if eligible && lease.Main && !lease.Trial && f.Phase == Pending && f.Candidate != "" &&
			origin.Known && (origin.used() >= e.policy.Force || f.TriggerRequested) &&
			(f.TriggeredGeneration == nil || *f.TriggeredGeneration != f.Generation) {
			generation := f.Generation
			f.TriggeredGeneration = &generation
			f.Phase = Triggered
			f.Deadline = e.now().Add(e.policy.TriggerTimeout)
			rewrite = true
		}
		return put(bucket, f)
	})
	return rewrite, err
}

// Notify is a digest receipt for this exact caller, family and generation.
func (e *Engine) Notify(scope, session string, generation uint64, noticeID, digest string) error {
	if !validID(noticeID) || len(digest) != 64 {
		return fault("invalid_notice")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fault("invalid_notice")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(familyBucket)
		f, err := lookup(bucket, scope, session)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		if f.Generation != generation {
			return fault("stale_notice")
		}
		delete(f.Compressing, session)
		e.expire(f, e.now())
		if session != f.Root {
			if len(f.Leases) > 0 {
				return fault("family_busy")
			}
			f.MemberSummaries[session] = digest
			return put(bucket, f)
		}
		if f.LastNotice == noticeID {
			if f.NoticeSummary == digest || f.ActiveSummary == digest {
				return nil
			}
			return fault("notice_conflict")
		}
		if f.Phase == Trial {
			return fault("handoff_in_progress")
		}
		if f.Phase == Blocked {
			return fault("handoff_retry_required")
		}
		f.LastNotice = noticeID
		f.NoticeSummary = digest
		f.NoticeAttempted = f.Phase == Pending || f.Phase == Triggered || f.Phase == Awaiting
		if f.NoticeAttempted {
			f.Phase = Awaiting
			f.Deadline = e.now().Add(e.policy.ContextTimeout)
		}
		return put(bucket, f)
	})
}

func (e *Engine) Finish(scope string, plan Plan, success bool, response []byte, accounts []Account) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rejected := false
	err := e.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(familyBucket)
		if err := trimReplays(bucket, e.now()); err != nil {
			return err
		}
		f, err := lookup(bucket, scope, plan.Root)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		lease, ok := f.Leases[plan.Token]
		if !ok {
			return fault("stale_lease")
		}
		if lease.Auth != plan.Auth || lease.RequestID != plan.RequestID {
			return fault("lease_mismatch")
		}
		delete(f.Leases, plan.Token)
		if lease.Generation != f.Generation {
			return fault("stale_lease")
		}
		if lease.Trial {
			candidate, allowed := account(accounts, lease.Auth)
			requestedSuccess := success
			success = success && allowed && candidate.eligible(f.Models, 0) && lease.Until.After(e.now()) && len(response) > 0 && len(response) <= 1<<20
			rejected = requestedSuccess && !success
			if success {
				f.Auth = lease.Auth
				f.Generation++
				f.Phase = Stable
				f.ActiveSummary = f.NoticeSummary
				f.NoticeSummary = ""
				f.NoticeAttempted = false
				f.Candidate = ""
				f.HandoffID = ""
				f.Deadline = time.Time{}
				f.LastReason = ""
				f.TriggeredGeneration = nil
				f.Requested = false
				f.TriggerRequested = false
				f.RecentAdmissionUntil = e.now().Add(e.policy.Cooldown)
				f.ReplayRequest = lease.RequestID
				f.Replay = append([]byte(nil), response...)
				f.ReplayFingerprint = lease.Fingerprint
				f.ReplayUntil = e.now().Add(5 * time.Minute)
				f.MemberSummaries = map[string]string{}
			} else {
				f.Cooldowns[lease.Auth] = e.now().Add(e.policy.Cooldown)
				f.Candidate = ""
				f.Phase = Blocked
				f.LastReason = "trial_failed"
				f.Deadline = time.Time{}
			}
		}
		return put(bucket, f)
	})
	if err == nil && rejected {
		return fault("trial_commit_rejected")
	}
	return err
}

func (e *Engine) Observe(scope, session string, accounts []Account) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var result Status
	err := e.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(familyBucket)
		f, err := lookup(bucket, scope, session)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		if err := e.evaluate(bucket, f, accounts, e.now()); err != nil {
			return err
		}
		result = Status{Session: session, Phase: f.Phase, Generation: f.Generation, HandoffID: f.HandoffID,
			BoundAccount: publicID(scope, f.Auth), CandidateReady: f.Candidate != "", Reason: f.LastReason,
			Soft: e.policy.Soft, Force: e.policy.Force, Hard: e.policy.Hard}
		if a, ok := account(accounts, f.Auth); ok && a.Known {
			five, week := a.FiveUsed, a.WeekUsed
			result.FiveUsed = &five
			result.WeekUsed = &week
		}
		return put(bucket, f)
	})
	return result, err
}

func (e *Engine) Manual(scope, session, action string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(familyBucket)
		f, err := lookup(bucket, scope, session)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		e.expire(f, e.now())
		if action == "acknowledge-drain" {
			if f.LastReason != "request_drain_timeout" && f.LastReason != "trial_timeout" {
				return fault("no_expired_drain")
			}
			for _, lease := range f.Leases {
				if lease.Owner == e.owner || lease.Until.After(e.now()) {
					return fault("family_busy")
				}
			}
			f.Leases = map[string]Lease{}
			f.Phase = Blocked
			f.LastReason = "operator_acknowledged_drain"
			return put(bucket, f)
		}
		if len(f.Leases) > 0 {
			return fault("family_busy")
		}
		switch action {
		case "prepare", "trigger":
			if f.Phase == Blocked {
				return fault("handoff_retry_required")
			}
			f.Requested = true
			f.TriggerRequested = action == "trigger"
			f.LastReason = "explicit_handoff_requested"
		case "cancel":
			f.Requested = false
			f.TriggerRequested = false
			f.Phase = Stable
			f.Candidate = ""
			f.Suppressed = true
			f.NoticeAttempted = false
			f.HandoffID = ""
			f.Deadline = time.Time{}
			f.LastReason = "operator_cancelled"
		case "retry":
			if f.Attempts >= e.policy.MaxAttempts {
				return fault("retry_limit")
			}
			f.Phase = Pending
			f.Candidate = ""
			f.Suppressed = false
			f.LastReason = "manual_retry"
		default:
			return fault("invalid_action")
		}
		return put(bucket, f)
	})
}
