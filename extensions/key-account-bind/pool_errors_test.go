package main

import "testing"

func TestUnavailablePoolReturnsOnlyAuthorizedDiagnosticIDs(t *testing.T) {
	mustConfigure(t, "bindings: [\"sk-one=account-a\", \"sk-two=account-b\"]\n")
	for _, available := range [][]schedulerAuthCandidate{nil, {{ID: "account-b"}}} {
		response, failure := rawPickResult(t, schedulerPickRequest{
			Options:        schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")}},
			Candidates:     available,
			PoolCandidates: []schedulerAuthCandidate{{ID: "account-a", Status: "error"}, {ID: "account-b"}},
		})
		if failure != nil || !response.Handled || !response.Reject || response.AuthID != "" ||
			len(response.UnavailableAuthIDs) != 1 || response.UnavailableAuthIDs[0] != "account-a" {
			t.Fatalf("wrong unavailable pool: response=%+v failure=%+v", response, failure)
		}
	}
}

func TestHostEligibleErrorStatusCanRecover(t *testing.T) {
	mustConfigure(t, "bindings: [\"sk-one=account-a\"]\n")
	response, failure := rawPickResult(t, schedulerPickRequest{
		Options:    schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")}},
		Candidates: []schedulerAuthCandidate{{ID: "account-a", Status: "error"}, {ID: "account-b"}},
	})
	if failure != nil || response.AuthID != "account-a" || response.Reject {
		t.Fatalf("host-eligible historical error cannot recover: %+v %v", response, failure)
	}
}
