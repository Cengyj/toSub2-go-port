package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCleanupSubprocess(t *testing.T) {
	if os.Getenv("TOSUB2_SESSION_CLEANUP_TEST") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("cleanup", flag.ExitOnError)
	os.Args = append([]string{"cleanup"}, os.Args[3:]...)
	main()
	os.Exit(0)
}

func TestOfflineRemovalPreservesMixedBusinessResults(t *testing.T) {
	root := t.TempDir()
	write := func(name string, v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		p := filepath.Join(root, name)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
		return b
	}
	checkpoint := map[string]any{"version": 1, "email": "fixture@example.test", "stage": "callback_ready", "cookies": []any{map[string]any{"name": "session", "value": "synthetic-session"}}, "oauth": map[string]any{"code": "synthetic-code", "codeVerifier": "synthetic-verifier", "state": "synthetic-state"}}
	write("data/job-fixture/login-checkpoint.json", checkpoint)
	write("backup/job-fixture/login-checkpoint.json", checkpoint)
	journal := map[string]any{"reset_totp_requested": true, "email": "fixture@example.test", "operation_id": "operation", "account_id": "account", "new_totp_factor_id": "factor", "session_id": "enrollment-binding", "secret": "SYNTHETIC-SEED", "activation_succeeded": true, "activation_verified": true, "credential_persisted": false, "activation_confirmation_source": "business_receipt", "activation_evidence": map[string]any{"account_id": "account", "factor_id": "factor", "operation_id": "operation"}, "password": "synthetic-password", "tokenResponse": map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh"}, "recovery_checkpoint": checkpoint, "previous_session_ref": map[string]any{"session_id": "old-session"}}
	journal["business_counter"] = json.Number("9007199254740993")
	write("data/job-fixture/totp-setup-result.json", journal)
	token := write("data/job-fixture/sub2api-import-oauth.json", map[string]any{"accounts": []any{map[string]any{"credentials": map[string]any{"refresh_token": "synthetic-refresh"}}}})
	cred := write("data/credentials/protected.cred", map[string]any{"ciphertext": "synthetic-ciphertext"})
	other := write("data/status.json", map[string]any{"pending": true, "session_id": "unrelated-business-id"})
	cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupSubprocess$", "--", "--apply", "--service=com.local.chatgpt-onboarding.credentials.local.fixture", "--credentials="+filepath.Join(root, "data", "credentials"), root)
	cmd.Env = append(os.Environ(), "TOSUB2_SESSION_CLEANUP_TEST=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleanup failed: %v %s", err, output)
	}
	for _, p := range []string{"data/job-fixture/login-checkpoint.json", "backup/job-fixture/login-checkpoint.json"} {
		if _, e := os.Stat(filepath.Join(root, p)); !os.IsNotExist(e) {
			t.Fatalf("session copy remains: %s", p)
		}
	}
	b, e := os.ReadFile(filepath.Join(root, "data/job-fixture/totp-setup-result.json"))
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if e = decodeObject(b, &got); e != nil {
		t.Fatal(e)
	}
	delete(journal, "recovery_checkpoint")
	delete(journal, "previous_session_ref")
	if !reflect.DeepEqual(got, journal) {
		t.Fatal("business credentials, bindings or success evidence changed")
	}
	for p, want := range map[string][]byte{"data/job-fixture/sub2api-import-oauth.json": token, "data/credentials/protected.cred": cred, "data/status.json": other} {
		got, e := os.ReadFile(filepath.Join(root, p))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatalf("unrelated/business file changed: %s", p)
		}
	}
	if bytes.Contains(b, []byte("synthetic-session")) || bytes.Contains(b, []byte("synthetic-code")) {
		t.Fatal("recoverable session retained")
	}
}

func TestUnclassifiedBusinessCredentialsBlockContainerDeletion(t *testing.T) {
	cp := map[string]any{"email": "fixture@example.test", "stage": "tokens_received", "cookies": []any{}, "oauth": map[string]any{"tokenResponse": map[string]any{"access_token": "synthetic-token"}}}
	changed, remove, err := sanitize(cp, "")
	if err == nil || changed || remove {
		t.Fatal("must transfer issued token into normal storage before removing its mixed container")
	}
}

func TestPendingCredentialsAndSnapshotBusinessFieldsAreNeverDiscarded(t *testing.T) {
	for _, key := range []string{"pendingPassword", "pendingTotpSecret", "pendingTotpUri", "apiKey", "mailHeaders", "proxyUrl"} {
		for _, nested := range []string{"web", "snapshot", "cookie_state", "recovery_checkpoint"} {
			cp := map[string]any{"email": "fixture@example.test", "stage": "web_password", "cookies": []any{}, nested: map[string]any{key: "synthetic-business-credential"}}
			if changed, remove, err := sanitize(cp, ""); err == nil || changed || remove {
				t.Fatalf("would delete %s inside %s", key, nested)
			}
		}
	}
}

func TestUnknownAccountDataRequiresReviewBeforeSessionDeletion(t *testing.T) {
	cp := map[string]any{"email": "fixture@example.test", "stage": "web_password", "cookies": []any{}, "accountBusinessData": map[string]any{"plan": "paid"}}
	if changed, remove, err := sanitize(cp, ""); err == nil || changed || remove {
		t.Fatal("unclassified account data was discarded")
	}
}

func TestAmbiguousDocumentsAreNotRewritten(t *testing.T) {
	for _, data := range []string{
		`{"reset_totp_requested":true,"tokenResponse":{"access_token":"synthetic-token"},"tokenResponse":{},"cookies":[]}`,
		`{"cookies":[],"business_counter":9007199254740993,"business_counter":1}`,
	} {
		var object map[string]any
		if decodeObject([]byte(data), &object) == nil {
			t.Fatal("duplicate fields would silently discard business results")
		}
	}
}

func TestUnclassifiedSessionContainersAreReportedWithoutModification(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"duplicate.json":   `{"cookies":[],"business":1,"business":2}`,
		"invalid.json":     `{"cookies":[`,
		"unknown.json":     `{"export":{"cookie_snapshot":{"value":"synthetic-session"}},"account_plan":"paid"}`,
		"array.json":       `[{"cookie_ref":"synthetic-session","access_token":"synthetic-token"}]`,
		"valid-array.json": `[{"state":"paused","session_id":"business-id"}]`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "unknown.json"), filepath.Join(root, "linked.json")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupSubprocess$", "--", "--apply", "--service=com.local.chatgpt-onboarding.credentials.local.fixture", "--credentials="+filepath.Join(root, "credentials"), root)
	cmd.Env = append(os.Environ(), "TOSUB2_SESSION_CLEANUP_TEST=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("unclassified files incorrectly reported fully clean")
	}
	var report struct {
		Unresolved []string       `json:"unresolved"`
		Counts     map[string]int `json:"counts"`
	}
	if json.Unmarshal(output, &report) != nil || len(report.Unresolved) != 5 || len(report.Counts) != 0 {
		t.Fatalf("unexpected cleanup report: %s", output)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("unclassified data was modified: %s", name)
		}
	}
	if bytes.Contains(output, []byte("synthetic-session")) || bytes.Contains(output, []byte("synthetic-token")) {
		t.Fatal("private values leaked in report")
	}
}

func TestReceiptBodyCannotHideSessionBehindDuplicateJSONFields(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(`{"extra":{"cookies":"synthetic-session"},"extra":{}}`))
	journal := map[string]any{"activation_succeeded": false, "recovery_checkpoint": map[string]any{"cookies": []any{}}, "activation_receipt": map[string]any{"body_base64": body}}
	if changed, remove, err := sanitize(journal, "totp-reset"); err == nil || changed || remove {
		t.Fatal("encoded receipt with hidden session was incorrectly classified clean")
	}
}

func TestEncodedBusinessCredentialsAndUnknownContainerTypesAreNotDeleted(t *testing.T) {
	for _, material := range []any{`{"tokenResponse":{"refresh_token":"synthetic-issued"}}`, "unknown-non-object-payload", []any{"unknown-payload"}} {
		checkpoint := map[string]any{"stage": "tokens_received", "email": "fixture@example.test", "cookies": []any{}, "oauth": material}
		if changed, remove, err := sanitize(checkpoint, ""); err == nil || changed || remove {
			t.Fatal("unclassified checkpoint contents would be deleted")
		}
		journal := map[string]any{"activation_succeeded": false, "recovery_checkpoint": material}
		if changed, remove, err := sanitize(journal, "totp-reset"); err == nil || changed || remove {
			t.Fatal("unclassified journal contents would be removed")
		}
	}
	if !hasBusinessMaterial(map[string]any{"extension": `{"tokenResponse":{"access_token":"synthetic-issued"}}`}) {
		t.Fatal("encoded business credentials were overlooked")
	}
}
