package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

// This is the output shape written before explicit BPS lists were preserved.
// It deliberately does not use the current updater to construct expected data.
func oldBPSRefreshFixture(t *testing.T, samePath, emptyModels bool) (string, string, string, map[string]any) {
	t.Helper()
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	if samePath {
		target = source
	}
	models := []string{"user-selected-model"}
	if emptyModels {
		models = []string{}
	}
	original := map[string]any{
		"type": "sub2api-data", "version": 1, "exported_at": "2026-10-01T00:00:00Z", "large_custom_id": json.Number("9007199254740993"), "proxies": []any{},
		"accounts": []any{
			map[string]any{"name": "Preserve this name", "credentials": map[string]any{"email": "first@example.test", "client_id": protocol.DefaultBPSClientID, "access_token": "old-access", "refresh_token": "old-refresh", "custom_grant_field": json.Number("9007199254740995")}, "extra": map[string]any{"client_id": protocol.DefaultBPSClientID, "openai_excel_bps_models": models, "openai_excel_bps_config_mode": "custom", "openai_excel_bps_auto_disable_on_403": false, "private_extension": "preserve"}},
			map[string]any{"name": "Untouched second account", "credentials": map[string]any{"email": "second@example.test", "refresh_token": "second-refresh"}, "unknown_counter": json.Number("9007199254740997")},
		},
	}
	if err := protocol.WritePrivateJSON(source, original); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	originalHash, err := refreshDocumentHash(original)
	if err != nil {
		t.Fatal(err)
	}
	accepted := "2026-10-31T12:34:56.123456789Z"
	receipt := refreshGrantRecovery{Version: 1, Type: "tosub2-refresh-grant", AcceptedAt: accepted, AuthBase: "https://auth.example.test", ClientID: protocol.DefaultBPSClientID, SourcePath: source, TargetPath: target, SourceSHA256: originalHash, Source: original, Tokens: protocol.TokenSet{AccessToken: "accepted-access", RefreshToken: "accepted-refresh"}, TargetAbsent: !samePath}
	if samePath {
		receipt.TargetSHA256 = refreshBytesHash(originalBytes)
	}
	if err := protocol.WritePrivateJSON(refreshRecoveryPath(target), receipt); err != nil {
		t.Fatal(err)
	}
	// Make an independent JSON copy so fixture output cannot change the receipt.
	expected, err := decodeRefreshDocument(originalBytes)
	if err != nil {
		t.Fatal(err)
	}
	expected["exported_at"] = accepted
	first := expected["accounts"].([]any)[0].(map[string]any)
	creds := first["credentials"].(map[string]any)
	creds["access_token"], creds["refresh_token"], creds["model_mapping_mode"] = "accepted-access", "accepted-refresh", "whitelist"
	extra := first["extra"].(map[string]any)
	extra["openai_excel_bps"] = true
	extra["openai_excel_bps_models"] = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "codex-auto-review"}
	extra["openai_excel_bps_ignore_encrypted_content"] = true
	extra["openai_excel_bps_omit_unsupported_tools"] = true
	extra["openai_excel_bps_auto_move_on_403"] = true
	extra["openai_excel_bps_403_target_group_id"] = 0
	extra["openai_excel_bps_403_recovery_interval_minutes"] = 60
	return source, target, receipt.AuthBase, expected
}

func TestBPSRefreshV1AlreadyWrittenResultCompletesLocallyAcrossConfigurationChange(t *testing.T) {
	for _, samePath := range []bool{true, false} {
		for _, empty := range []bool{true, false} {
			name := map[bool]string{true: "same", false: "separate"}[samePath] + map[bool]string{true: "-empty", false: "-custom"}[empty]
			t.Run(name, func(t *testing.T) {
				source, target, base, oldResult := oldBPSRefreshFixture(t, samePath, empty)
				if err := protocol.WritePrivateJSON(target, oldResult); err != nil {
					t.Fatal(err)
				}
				sourceBefore, _ := os.ReadFile(source)
				targetBefore, _ := os.ReadFile(target)
				calls := 0
				doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
					calls++
					return nil, errors.New("network forbidden in local completion")
				})
				if err := refreshExistingExportAt(t.Context(), doer, base, protocol.DefaultBPSClientID, source, target, "first@example.test"); err != nil {
					t.Fatal("old confirmed result did not complete locally", err)
				}
				sourceAfter, _ := os.ReadFile(source)
				targetAfter, _ := os.ReadFile(target)
				if calls != 0 || !bytes.Equal(sourceBefore, sourceAfter) || !bytes.Equal(targetBefore, targetAfter) {
					t.Fatal("compatibility rewrote confirmed output/source or repeated refresh")
				}
				if _, err := os.Stat(refreshRecoveryPath(target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed result retained pending receipt", err)
				}
			})
		}
	}
}

func TestBPSRefreshV1CompatibilityRejectsEveryOtherChangedField(t *testing.T) {
	for _, samePath := range []bool{true, false} {
		for _, field := range []string{"access", "refresh", "email", "client", "accepted-time", "models", "custom", "second-account", "large-number"} {
			t.Run(map[bool]string{true: "same/", false: "separate/"}[samePath]+field, func(t *testing.T) {
				source, target, base, result := oldBPSRefreshFixture(t, samePath, false)
				first := result["accounts"].([]any)[0].(map[string]any)
				creds := first["credentials"].(map[string]any)
				switch field {
				case "access":
					creds["access_token"] = "different-access"
				case "refresh":
					creds["refresh_token"] = "different-refresh"
				case "email":
					creds["email"] = "other@example.test"
				case "client":
					creds["client_id"] = "other-client"
				case "accepted-time":
					result["exported_at"] = "2026-11-01T00:00:00Z"
				case "models":
					first["extra"].(map[string]any)["openai_excel_bps_models"] = []string{"gpt-6.1-sol"}
				case "custom":
					first["extra"].(map[string]any)["private_extension"] = "changed"
				case "second-account":
					result["accounts"].([]any)[1].(map[string]any)["name"] = "changed"
				case "large-number":
					result["large_custom_id"] = json.Number("9007199254740994")
				}
				if err := protocol.WritePrivateJSON(target, result); err != nil {
					t.Fatal(err)
				}
				sourceBefore, _ := os.ReadFile(source)
				targetBefore, _ := os.ReadFile(target)
				receiptBefore, _ := os.ReadFile(refreshRecoveryPath(target))
				calls := 0
				doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
					calls++
					return nil, errors.New("network forbidden")
				})
				err := refreshExistingExportAt(t.Context(), doer, base, protocol.DefaultBPSClientID, source, target, "first@example.test")
				if !isLocalRefreshFailure(err) {
					t.Fatal("unrelated target change was accepted", err)
				}
				sourceAfter, _ := os.ReadFile(source)
				targetAfter, _ := os.ReadFile(target)
				receiptAfter, _ := os.ReadFile(refreshRecoveryPath(target))
				if calls != 0 || !bytes.Equal(sourceBefore, sourceAfter) || !bytes.Equal(targetBefore, targetAfter) || !bytes.Equal(receiptBefore, receiptAfter) {
					t.Fatal("rejected compatibility changed files or retried remote grant")
				}
			})
		}
	}
}
