package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type poolDiagnosticScheduler struct{ fakePluginScheduler }

func (*poolDiagnosticScheduler) SchedulerWantsPoolDiagnostics() bool { return true }

func TestPluginPoolCooldownIsScopedAndRetains429(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, otherCooling := range []bool{false, true} {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			now := time.Now()
			for _, account := range []*Auth{
				{ID: "owned", Provider: "gemini", Status: StatusError, Unavailable: true,
					Quota:     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)},
					LastError: &Error{Code: "rate_limit_error", Message: "assigned account quota exhausted", HTTPStatus: http.StatusTooManyRequests}},
				{ID: "other", Provider: "gemini", Status: StatusActive},
			} {
				if account.ID == "other" && otherCooling {
					account.Unavailable = true
					account.Quota = QuotaState{Exceeded: true, NextRecoverAt: now.Add(5 * time.Minute)}
				}
				if _, err := manager.Register(context.Background(), account); err != nil {
					t.Fatal(err)
				}
			}
			scheduler := &poolDiagnosticScheduler{}
			scheduler.pick = func(_ context.Context, request pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if len(request.PoolCandidates) != 2 {
					t.Fatalf("diagnostic pool lost: %+v", request)
				}
				for _, candidate := range request.Candidates {
					if candidate.ID == "owned" {
						t.Fatal("cooling auth was made selectable")
					}
				}
				return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, UnavailableAuthIDs: []string{"owned"}}, true, nil
			}
			manager.SetPluginScheduler(scheduler)
			var err error
			if mixed {
				_, _, _, err = manager.pickNextMixed(context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				_, _, err = manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			}
			if err == nil || statusCodeFromError(err) != http.StatusTooManyRequests || !strings.Contains(err.Error(), "assigned account quota exhausted") {
				t.Fatalf("lost scoped quota: mixed=%v otherCooling=%v err=%v", mixed, otherCooling, err)
			}
			if retry, _ := strconv.Atoi(SafeResponseHeaders(err).Get("Retry-After")); retry < 3540 {
				t.Fatalf("wrong pool reset time: %v", retry)
			}
		}
	}
}

func TestPluginPoolDiagnosticRejectsUnknownCredential(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	err := manager.schedulerPoolAvailabilityError(context.Background(), "gemini", "", cliproxyexecutor.Options{}, []*Auth{{ID: "owned"}}, []string{"outside"})
	if err == nil || statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("invalid diagnostic accepted: %v", err)
	}
}
