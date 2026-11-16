package main

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAuthenticatedIdentityOverridesClientHeaders(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\", \"sk-two=account-b\"]\nunbound: deny\n")
	for _, headers := range []map[string][]string{
		nil,
		{"Authorization": {"Bearer sk-two"}},
		{"Authorization": {"invalid"}, "X-Api-Key": {"sk-two"}},
		{"X-Goog-Api-Key": {"sk-two"}, "X-Api-Key": {"sk-one"}},
	} {
		response, failure := rawPickResult(test, schedulerPickRequest{
			Options: schedulerPickOptions{
				Headers:  headers,
				Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")},
			},
			Candidates: []schedulerAuthCandidate{{ID: "account-a"}, {ID: "account-b"}},
		})
		if failure != nil || response.AuthID != "account-a" || !response.Handled {
			test.Fatalf("authenticated scope was not authoritative: response=%+v failure=%+v", response, failure)
		}
	}
}

func TestMissingOrMalformedIdentityFailsClosed(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\nunbound: passthrough\n")
	for _, metadata := range []map[string]any{
		nil,
		{},
		{"caller_scope": ""},
		{"caller_scope": "sk-one"},
		{"caller_scope": 42},
		{"caller_scope": "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
	} {
		response, failure := rawPickResult(test, schedulerPickRequest{
			Options: schedulerPickOptions{
				Headers:  map[string][]string{"Authorization": {"Bearer sk-one"}},
				Metadata: metadata,
			},
			Candidates: []schedulerAuthCandidate{{ID: "account-a"}, {ID: "account-b"}},
		})
		if failure == nil || failure.Code != "caller_identity_missing" || response.Handled {
			test.Fatalf("missing identity did not fail closed: response=%+v failure=%+v", response, failure)
		}
	}
}

func TestCaseDistinctKeysHaveSeparatePools(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-One=account-a\", \"sk-one=account-b\"]\n")
	for key, expected := range map[string]string{"sk-One": "account-a", "sk-one": "account-b"} {
		response, failure := rawPickResult(test, schedulerPickRequest{
			Options:    schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey(key)}},
			Candidates: []schedulerAuthCandidate{{ID: "account-a"}, {ID: "account-b"}},
		})
		if failure != nil || response.AuthID != expected {
			test.Fatalf("case-distinct identity selected wrong pool: response=%+v failure=%+v", response, failure)
		}
	}
}

func TestUnboundDefaultsToDeny(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\n")
	response, failure := rawPickResult(test, schedulerPickRequest{
		Options:    schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-unbound")}},
		Candidates: []schedulerAuthCandidate{{ID: "account-a"}},
	})
	if failure == nil || failure.Code != "key_not_bound" || response.Handled {
		test.Fatalf("unbound caller did not default to deny: response=%+v failure=%+v", response, failure)
	}
}

func TestInvalidReconfigurationInvalidatesPolicy(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\n")
	invalid := []byte("bindings: [\"invalid-rule\"]")
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: invalid})
	if err != nil {
		test.Fatal(err)
	}
	result := handleConfigure(payload)
	var configuration envelope
	if err := json.Unmarshal(result, &configuration); err != nil || configuration.Error == nil {
		test.Fatalf("expected invalid configuration: %s", result)
	}
	_, failure := rawPickResult(test, schedulerPickRequest{
		Options:    schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey("sk-one")}},
		Candidates: []schedulerAuthCandidate{{ID: "account-a"}},
	})
	if failure == nil || failure.Code != "policy_unavailable" {
		test.Fatalf("invalid reload retained an active policy: failure=%+v", failure)
	}
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\n")
}

func TestMalformedLifecycleInvalidatesPolicy(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\n")
	result := handleConfigure([]byte("{"))
	var configuration envelope
	if err := json.Unmarshal(result, &configuration); err != nil || configuration.Error == nil {
		test.Fatalf("expected malformed lifecycle failure: %s", result)
	}
	if state.Load() != nil {
		test.Fatal("malformed lifecycle retained an active policy")
	}
}

func TestScopeEncodingMatchesHostContract(test *testing.T) {
	if callerScopeForKey("sk-one") != "89aef66603513a30dfaec9fbdbdb36dc0396735597a7ff0673cacf05751d282f" {
		test.Fatal("scope does not match the host contract vector")
	}
	if callerScopeForKey(" sk-one ") != callerScopeForKey("sk-one") || callerScopeForKey("") != "" {
		test.Fatal("scope normalization differs from host identity normalization")
	}
}

func TestConcurrentReconfigurationPreservesPoolBounds(test *testing.T) {
	configuration := "bindings: [\"sk-one=account-a\", \"sk-two=account-b\"]\n"
	mustConfigure(test, configuration)
	var workers sync.WaitGroup
	var violations atomic.Int64
	for worker := 0; worker < 4; worker++ {
		key := "sk-one"
		expected := "account-a"
		if worker%2 != 0 {
			key = "sk-two"
			expected = "account-b"
		}
		workers.Add(1)
		go func(key, expected string) {
			defer workers.Done()
			for iteration := 0; iteration < 200; iteration++ {
				response, failure := rawPickResult(test, schedulerPickRequest{
					Options:    schedulerPickOptions{Metadata: map[string]any{"caller_scope": callerScopeForKey(key)}},
					Candidates: []schedulerAuthCandidate{{ID: "account-a"}, {ID: "account-b"}},
				})
				if failure != nil {
					if failure.Code != "policy_unavailable" {
						violations.Add(1)
					}
				} else if !response.Handled || response.AuthID != expected {
					violations.Add(1)
				}
			}
		}(key, expected)
	}
	for iteration := 0; iteration < 100; iteration++ {
		mustConfigure(test, configuration)
	}
	workers.Wait()
	if violations.Load() != 0 {
		test.Fatalf("concurrent policy reload produced %d isolation violations", violations.Load())
	}
}
