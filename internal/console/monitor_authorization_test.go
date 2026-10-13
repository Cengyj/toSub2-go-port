package console

import (
	"context"
	"reflect"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorPreservesOAuthClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, local string
		clients     []sub2api.RemoteAccount
		allowed     bool
	}{
		{"bps-matching", "bps", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}}}, true},
		{"bps-extra-fallback", "bps", []sub2api.RemoteAccount{{Extra: map[string]any{"client_id": protocol.DefaultBPSClientID}}}, true},
		{"legacy-missing-identity", "bps", []sub2api.RemoteAccount{{}}, true},
		{"chatgpt-matching", "chatgpt", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultCodexClientID}}}, true},
		{"chatgpt-cannot-repair-bps", "chatgpt", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}}}, false},
		{"bps-cannot-repair-chatgpt", "bps", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultCodexClientID}}}, false},
		{"conflicting-metadata", "bps", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}, Extra: map[string]any{"client_id": protocol.DefaultCodexClientID}}}, false},
		{"conflicting-duplicates", "bps", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}}, {Credentials: map[string]any{"client_id": protocol.DefaultCodexClientID}}}, false},
		{"unknown-client", "chatgpt", []sub2api.RemoteAccount{{Credentials: map[string]any{"client_id": "unknown-client"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			job, err := manager.Submit(jobs.Spec{ID: "identity", Email: "identity@example.test", Input: map[string]any{"password": "fixture-password", "authorizationMethod": tc.local}})
			if err != nil {
				t.Fatal(err)
			}
			manager.Cancel(job.ID)
			manager.RecordAuthorization(job.ID, true, "", map[string]bool{"password": true})
			for i := range tc.clients {
				tc.clients[i].ID, tc.clients[i].Email = int64(i+1), job.Email
			}
			before, _ := manager.Get(job.ID)
			hooks := srv.monitorHooks(sub2api.Config{BaseURL: "https://example.test"})
			decision, err := hooks.Evaluate(context.Background(), job.Email, tc.clients)
			if err != nil || (decision.Action == sub2api.ActionStart) != tc.allowed {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			if !tc.allowed && decision.Reason == "" {
				t.Fatal("conflict was silently treated as generic ineligibility")
			}
			err = hooks.StartRepair(context.Background(), job.Email, tc.clients)
			if (err == nil) != tc.allowed {
				t.Fatalf("StartRepair=%v", err)
			}
			after, _ := manager.Get(job.ID)
			if !tc.allowed && !reflect.DeepEqual(before, after) {
				t.Fatal("rejected OAuth client conflict mutated the job")
			}
			if tc.allowed && after.AuthorizationMethod != tc.local {
				t.Fatal("automatic repair changed authorization provider")
			}
		})
	}
}
