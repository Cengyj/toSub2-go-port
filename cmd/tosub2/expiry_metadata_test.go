package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

func TestRefreshUpdatesSubscriptionExpiryWithoutChangingManualOverride(t *testing.T) {
	for _, test := range []struct {
		name, account, plan, field string
		reported                   any
		want                       string
	}{
		{"new-subscription", "workspace", "plus", "subscription_expires_at", "2031-01-01T08:00:00+08:00", "2031-01-01T00:00:00Z"},
		{"provider-alias", "workspace", "plus", "chatgpt_subscription_active_until", "2031-01-01T08:00:00+08:00", "2031-01-01T00:00:00Z"},
		{"omitted-same-workspace", "workspace", "plus", "", nil, "2030-01-01T00:00:00Z"},
		{"explicit-null", "workspace", "plus", "chatgpt_subscription_active_until", nil, ""},
		{"explicit-empty", "workspace", "plus", "subscription_expires_at", "", ""},
		{"explicit-invalid", "workspace", "plus", "chatgpt_subscription_active_until", "not-a-date", ""},
		{"new-workspace", "new-workspace", "", "", nil, ""},
		{"new-plan", "workspace", "pro", "", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := jobs.NewManager(t.Context(), 1, st, nil)
			job, err := m.Submit(jobs.Spec{ID: "expiry-refresh", Email: "expiry-refresh@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2035-01-01T00:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			document := map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "chatgpt_account_id": "workspace", "plan_type": "plus", "subscription_expires_at": "2030-01-01T00:00:00Z", "refresh_token": "synthetic-refresh"}, "extra": map[string]any{}}}}
			private, err := store.New(filepath.Join(st.Root, job.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", document); err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "synthetic previous grant")
			previous, _ := m.Get(job.ID)
			if previous.SubscriptionExpiresAt != "2030-01-01T00:00:00Z" {
				t.Fatal("previous subscription expiry was not recorded")
			}
			auth := map[string]any{"chatgpt_account_id": test.account, "chatgpt_plan_type": test.plan}
			if test.field != "" {
				auth[test.field] = test.reported
			}
			claims, err := json.Marshal(map[string]any{"exp": 2000000000, "auth": auth})
			if err != nil {
				t.Fatal(err)
			}
			updateRefreshedDocument(document, protocol.TokenSet{AccessToken: metadataFixtureToken(string(claims)), RefreshToken: "synthetic-new-refresh"}, protocol.DefaultBPSClientID)
			if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", document); err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "synthetic refresh completed")
			current, _ := m.Get(job.ID)
			if current.SubscriptionExpiresAt != test.want || current.ManualExpiresAt != "2035-01-01T00:00:00Z" {
				t.Fatal("refresh mixed subscription/token expiry or overwrote manual expiry")
			}
			if expires, source := jobs.EffectiveExpiry(current); expires != current.ManualExpiresAt || source != "manual" {
				t.Fatal("refresh lost manual expiry precedence")
			}
		})
	}
}

func TestRefreshClearsSubscriptionWhenImportedWorkspaceAliasChanges(t *testing.T) {
	for _, location := range []string{"credentials", "extra"} {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			t.Run(location+"/"+key, func(t *testing.T) {
				credentials := map[string]any{"email": "owner@example.test", "refresh_token": "synthetic-refresh", "subscription_expires_at": "2030-01-01T00:00:00Z", "organization_id": "old-org"}
				extra := map[string]any{"subscription_expires_at": "2030-01-01T00:00:00Z", "organization_id": "old-org"}
				account := map[string]any{"credentials": credentials, "extra": extra}
				account[location].(map[string]any)[key] = " old-workspace "
				updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: metadataFixtureToken(`{"auth":{"chatgpt_account_id":"new-workspace"}}`), RefreshToken: "synthetic-rotated-refresh"}, protocol.DefaultBPSClientID)
				for _, fields := range []map[string]any{credentials, extra} {
					for _, field := range []string{"subscription_expires_at", "organization_id"} {
						if _, exists := fields[field]; exists {
							t.Fatal("old imported workspace metadata followed the new grant")
						}
					}
				}
				if credentials["refresh_token"] != "synthetic-rotated-refresh" || credentials["chatgpt_account_id"] != "new-workspace" {
					t.Fatal("workspace metadata cleanup discarded the accepted grant")
				}
				if account[location].(map[string]any)[key] != "new-workspace" {
					t.Fatal("refresh left a contradictory imported workspace alias")
				}
			})
		}
	}
}

func TestRefreshDoesNotBorrowSubscriptionFromOldIDPlan(t *testing.T) {
	credentials := map[string]any{"chatgpt_account_id": "workspace", "plan_type": "plus", "subscription_expires_at": "2030-01-01T00:00:00Z"}
	extra := map[string]any{"subscription_expires_at": "2030-01-01T00:00:00Z"}
	account := map[string]any{"credentials": credentials, "extra": extra}
	tokens := protocol.TokenSet{
		AccessToken:  metadataFixtureToken(`{"auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"pro"}}`),
		IDToken:      metadataFixtureToken(`{"auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"plus","chatgpt_subscription_active_until":"2031-01-01T00:00:00Z"}}`),
		RefreshToken: "synthetic-refresh",
	}
	updateRefreshedDocument(map[string]any{"accounts": []any{account}}, tokens, protocol.DefaultBPSClientID)
	for _, fields := range []map[string]any{credentials, extra} {
		if _, exists := fields["subscription_expires_at"]; exists {
			t.Fatal("new plan inherited the old ID-token subscription expiry")
		}
	}
}

func TestRefreshSubscriptionMetadataSurvivesCanceledRotationRecovery(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"valid alias", "2031-01-01T08:00:00+08:00", "2031-01-01T00:00:00Z"},
		{"explicit null", nil, ""},
		{"invalid optional metadata", "not-a-date", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sub2api-import-oauth.json")
			before := map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{"credentials": map[string]any{"email": "owner@example.test", "client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "workspace", "plan_type": "plus", "refresh_token": "synthetic-old-refresh", "subscription_expires_at": "2030-01-01T00:00:00Z"}}}}
			if err := protocol.WritePrivateJSON(path, before); err != nil {
				t.Fatal(err)
			}
			claims, err := json.Marshal(map[string]any{"auth": map[string]any{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus", "chatgpt_subscription_active_until": test.value}})
			if err != nil {
				t.Fatal(err)
			}
			response, err := json.Marshal(protocol.TokenSet{AccessToken: metadataFixtureToken(string(claims)), RefreshToken: "synthetic-new-refresh"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				cancel()
				return &transport.Response{StatusCode: 200, Body: response}, nil
			})
			if err := refreshExistingFile(ctx, doer, "https://auth.example.test", protocol.DefaultBPSClientID, path, "owner@example.test"); !errors.Is(err, context.Canceled) || !isLocalRefreshFailure(err) {
				t.Fatal("accepted refresh did not retain recoverable cancellation", err)
			}
			if _, err := os.Stat(refreshRecoveryPath(path)); err != nil {
				t.Fatal("cancellation lost the accepted refresh journal", err)
			}
			if err := refreshExistingFile(t.Context(), doer, "https://auth.example.test", protocol.DefaultBPSClientID, path, "owner@example.test"); err != nil {
				t.Fatal("local refresh recovery rejected optional metadata", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := decodeRefreshDocument(data)
			if err != nil {
				t.Fatal(err)
			}
			credentials := after["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)
			expires, _ := credentials["subscription_expires_at"].(string)
			if expires != test.want || credentials["refresh_token"] != "synthetic-new-refresh" || calls != 1 {
				t.Fatal("local recovery lost the grant or repeated remote rotation")
			}
			if _, err := os.Stat(refreshRecoveryPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed local recovery retained the journal", err)
			}
		})
	}
}
