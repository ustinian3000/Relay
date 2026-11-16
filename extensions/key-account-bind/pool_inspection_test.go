package main

import "testing"

func TestPoolInspectionIncludesBoundUnavailableAccountsWithoutSelecting(t *testing.T) {
	mustConfigure(t, "bindings: [\"sk-one=team-*.json\", \"sk-two=other.json\"]\nunbound: passthrough\n")
	req := schedulerPickRequest{InspectPool: true, Options: schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")}},
		PoolCandidates: []schedulerAuthCandidate{{ID: "team-a.json", Status: "disabled"}, {ID: "team-b.json", Status: "error"}, {ID: "other.json"}},
		Candidates:     []schedulerAuthCandidate{{ID: "other.json"}},
	}
	response, failure := rawPickResult(t, req)
	if failure != nil || !response.Handled || response.AuthID != "" || response.DelegateBuiltin != "" || len(response.AuthorizedAuthIDs) != 2 {
		t.Fatalf("wrong inspection: %+v %v", response, failure)
	}
	req.Options.Metadata["caller_scope"] = callerScopeForKey("unbound")
	if _, failure := rawPickResult(t, req); failure == nil {
		t.Fatal("unbound passthrough was allowed to inspect all accounts")
	}
}

func TestPoolInspectionDoesNotAdvanceRoundRobin(t *testing.T) {
	mustConfigure(t, "bindings: [\"sk-one=team-*\"]\n")
	req := schedulerPickRequest{Options: schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")}}, Candidates: []schedulerAuthCandidate{{ID: "team-a"}, {ID: "team-b"}}}
	first, err := rawPickResult(t, req)
	if err != nil {
		t.Fatal(err)
	}
	inspection := req
	inspection.InspectPool = true
	inspection.PoolCandidates = req.Candidates
	if _, err := rawPickResult(t, inspection); err != nil {
		t.Fatal(err)
	}
	second, err := rawPickResult(t, req)
	if err != nil || first.AuthID == second.AuthID {
		t.Fatalf("inspection changed selection state: %+v %+v %v", first, second, err)
	}
}
