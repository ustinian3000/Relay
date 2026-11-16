package pluginhost

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"testing"
)

func TestHostPoolInspectionFailsClosedAndValidatesIDs(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability bool
		ids        []string
		wantErr    bool
		handled    bool
	}{
		{"legacy", false, []string{"a"}, false, false},
		{"authorized", true, []string{"a", "a"}, false, true},
		{"unknown", true, []string{"outside"}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{id: "scope", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				SchedulerPoolInspection: test.capability,
				Scheduler: schedulerFunc(func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
					if !req.InspectPool || len(req.Candidates) > 0 {
						t.Fatal("inspection can select a credential")
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthorizedAuthIDs: test.ids}, nil
				}),
			}}})
			ids, handled, err := host.InspectAuthPool(context.Background(), pluginapi.SchedulerPickRequest{PoolCandidates: []pluginapi.SchedulerAuthCandidate{{ID: "a"}}})
			if (err != nil) != test.wantErr || handled != test.handled {
				t.Fatalf("result: %v %v %v", ids, handled, err)
			}
			if test.name == "authorized" && len(ids) != 1 {
				t.Fatal("duplicate IDs were not removed")
			}
		})
	}
}
