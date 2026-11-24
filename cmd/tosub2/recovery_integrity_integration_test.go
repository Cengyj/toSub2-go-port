package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/maintenance"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/settings"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

// The backup exercise uses real private-state envelopes and an independently
// backed-up synthetic key. This portable adapter deliberately never opens an
// OS keychain and is not a substitute for platform-native vault acceptance.
type integrityVault struct {
	*runnerSyntheticStateProtector
	root     string
	registry *privatestate.Registry
}

func newIntegrityVault(root, keyPath string) (*integrityVault, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid synthetic backup key")
	}
	v := &integrityVault{runnerSyntheticStateProtector: &runnerSyntheticStateProtector{}, root: root, registry: privatestate.NewRegistry()}
	copy(v.key[:], key)
	if err := v.registry.Require(root); err != nil {
		return nil, err
	}
	return v, v.registry.Register(root, v)
}

func (v *integrityVault) path(identity string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(identity))))
	return filepath.Join(v.root, "synthetic-vault", hex.EncodeToString(sum[:])+".json")
}

func (v *integrityVault) Save(identity string, value credentials.Credentials) error {
	return v.SaveContext(context.Background(), identity, value)
}
func (v *integrityVault) Load(identity string) (credentials.Credentials, error) {
	return v.LoadContext(context.Background(), identity)
}
func (v *integrityVault) Delete(identity string) error {
	return v.DeleteContext(context.Background(), identity)
}
func (v *integrityVault) SaveContext(ctx context.Context, identity string, value credentials.Credentials) error {
	return v.registry.WriteJSON(ctx, v.path(identity), "synthetic-vault", identity, "credential-record", value)
}
func (v *integrityVault) LoadContext(ctx context.Context, identity string) (credentials.Credentials, error) {
	var value credentials.Credentials
	raw, binding, err := v.registry.ReadJSONWithBinding(ctx, v.path(identity), "synthetic-vault", 64<<10)
	if err != nil {
		return value, err
	}
	if binding.Identity != privatestate.Identity(identity) || binding.Operation != "credential-record" {
		return value, errors.New("synthetic credential binding changed")
	}
	return value, json.Unmarshal(raw, &value)
}
func (v *integrityVault) DeleteContext(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return privatefile.Remove(ctx, v.path(identity))
}

const integrityTime = "2026-11-01T01:02:03Z"
const integritySeed = "JBSWY3DPEHPK3PXP"
const integrityPassword = "Synthetic-new-password9!"
const integrityCPARef = "cpa-management-00112233445566778899aabbccddeeff"

var integritySettings = json.RawMessage(`{"adminApiKey":"synthetic-private-value","empty":[],"enabled":false,"zero":0,"vendor":{"huge":9007199254740993}}`)

func integrityJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := protocol.WritePrivateJSON(path, value); err != nil {
		t.Fatal(err)
	}
}

func integrityJob(id, email, status, operation string) map[string]any {
	return map[string]any{"id": id, "email": email, "status": status, "operation": operation,
		"authorizationMethod": "chatgpt", "createdAt": integrityTime, "updatedAt": integrityTime,
		"lastOperationAt": integrityTime, "operationStatus": "running", "attempts": 1}
}

func integrityGrant(email, base string) protocol.SavedOAuthGrant {
	claims, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synthetic-" + email, "chatgpt_plan_type": "plus"}})
	return protocol.SavedOAuthGrant{Version: 1, Email: email, AuthorizationMethod: "chatgpt",
		AuthBase: base, ChatGPTBase: base, ClientID: protocol.DefaultCodexClientID, RedirectURI: protocol.DefaultCodexRedirectURI,
		Tokens: protocol.TokenSet{AccessToken: "synthetic-access-" + email, RefreshToken: "synthetic-refresh-" + email,
			IDToken: "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"}}
}

func integrityExport(t *testing.T, root, id, email, base string) {
	t.Helper()
	g := integrityGrant(email, base)
	export, err := protocol.BuildSub2APIOAuthExport(g.Tokens, g.ClientID, email, "Retained custom name", 3, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(export)
	doc, err := decodeRefreshDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc["retained_extension"] = map[string]any{"disabled": false, "zero": 0, "empty": []any{}, "large": json.Number("9007199254740993")}
	integrityJSON(t, filepath.Join(root, id, "sub2api-import-oauth.json"), doc)
}

func seedIntegrityBackup(t *testing.T, root, keyPath, base string) *integrityVault {
	t.Helper()
	if _, err := store.New(root); err != nil {
		t.Fatal(err)
	}
	vault, err := newIntegrityVault(root, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.Register(root, vault); err != nil {
		t.Fatal(err)
	}
	active := integrityJob("active", "active@example.test", jobs.StatusCompleted, "login")
	active["operationStatus"], active["manualExpiresAt"] = "succeeded", "2026-11-02T00:00:00Z"
	active["logs"] = []string{"retained account log"}
	active["logEntries"] = []map[string]any{{"at": integrityTime, "stage": "completed", "message": "retained structured log"}}
	history := integrityJob("active-history", "active@example.test", jobs.StatusFailed, "login")
	history["operationStatus"], history["createdAt"], history["logs"] = "failed", "2026-10-01T00:00:00Z", []string{"older failed operation"}
	integrityExport(t, root, "active", "active@example.test", base)
	// Build the historical cache revision using the application's actual
	// projection, before adding interrupted operations and their evidence.
	integrityJSON(t, filepath.Join(root, "job-meta.json"), map[string]any{"version": 2, "jobs": []any{active, history}, "trash": []any{}})
	m := jobs.NewManager(t.Context(), 2, &store.Store{Root: root}, nil)
	m.SetCredentialStore(vault)
	if err := m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	revision := ""
	for _, entry := range application.ReadOnlyAccountInventory(root, m) {
		if entry.ID == "active" {
			revision = entry.Revision
		}
	}
	var aggregate struct {
		Version int              `json:"version"`
		Jobs    []map[string]any `json:"jobs"`
		Trash   []any            `json:"trash"`
	}
	if err := (&store.Store{Root: root}).ReadJSON(t.Context(), "job-meta.json", &aggregate); err != nil || revision == "" {
		t.Fatal("cannot prepare stable account history", err)
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, email, status, operation string }{
		{"queued", "queued@example.test", jobs.StatusQueued, "login"},
		{"waiting", "waiting@example.test", jobs.StatusWaitingInput, "login"},
		{"issued", "issued@example.test", jobs.StatusRunning, "login"},
		{"refresh", "first@example.test", jobs.StatusRunning, "refresh"},
		{"password-confirmed", "password-confirmed@example.test", jobs.StatusRunning, "add-password"},
		{"password-unknown", "password-unknown@example.test", jobs.StatusRunning, "add-password"},
		{"totp-confirmed", "totp-confirmed@example.test", jobs.StatusRunning, "setup-2fa"},
		{"totp-unknown", "totp-unknown@example.test", jobs.StatusRunning, "reset-2fa"},
		{"upload-unknown", "upload-unknown@example.test", jobs.StatusCompleted, "login"},
		{"restored-paused", "restored-paused@example.test", jobs.StatusFailed, "login"},
	} {
		j := integrityJob(item.id, item.email, item.status, item.operation)
		switch item.id {
		case "waiting":
			j["stage"], j["input"] = "email_otp", map[string]any{"emailCode": "123456", "password": "stale-input-password"}
		case "issued":
			j["oauthResultId"] = "issued-result"
		case "totp-unknown":
			j["resetTotp"], j["totpResetOperationId"] = true, "unknown-reset-operation"
		case "upload-unknown":
			j["operationStatus"], j["resultSaved"] = "succeeded", true
			j["autoRepairSyncPaused"], j["autoRepairPendingAccountIds"] = true, []int{41, 42}
			j["autoRepairBackend"], j["uploadTarget"] = sub2api.BackendIdentity(base), "sub2api"
			j["autoRepairLastError"] = "同步结果未知，需要人工核对"
		case "restored-paused":
			j["operationStatus"], j["recoveryPaused"] = "failed", true
		}
		aggregate.Jobs = append(aggregate.Jobs, j)
	}
	trashJobs := []any{
		integrityJob("trash-current", "trash@example.test", jobs.StatusCompleted, "login"),
		integrityJob("trash-history", "trash@example.test", jobs.StatusFailed, "login"),
	}
	aggregate.Trash = []any{
		map[string]any{"id": "trash-entry", "email": "trash@example.test", "deletedAt": integrityTime, "state": jobs.TrashStateTrashed, "jobs": trashJobs},
		map[string]any{"id": "purging-entry", "email": "purging@example.test", "deletedAt": integrityTime, "purgeStartedAt": integrityTime, "state": jobs.TrashStatePurging, "lastError": "synthetic cleanup interrupted", "jobs": []any{integrityJob("purging-history", "purging@example.test", jobs.StatusFailed, "login")}},
	}
	integrityJSON(t, filepath.Join(root, "job-meta.json"), aggregate)
	for _, pair := range []struct{ id, email string }{{"upload-unknown", "upload-unknown@example.test"}, {"trash-current", "trash@example.test"}, {"purging-history", "purging@example.test"}} {
		integrityExport(t, root, pair.id, pair.email, base)
	}
	// A directory discovery must not resurrect any archived historical job.
	integrityJSON(t, filepath.Join(root, "trash-history", "job-meta.json"), trashJobs[1])
	integrityJSON(t, filepath.Join(root, "waiting", "login-checkpoint.json"), map[string]any{"stage": "email_otp", "cookies": []string{"synthetic-stale-cookie"}, "emailCode": "123456"})
	for _, email := range []string{"active@example.test", "trash@example.test", "purging@example.test"} {
		if err := vault.Save(email, credentials.Credentials{Password: "Retained-password9!", MailAPIURL: base + "/mail", MailMethod: "POST", MailBody: "encoded=preserved", MailHeaders: map[string]string{"X-Synthetic": "retained"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"confirmed", "unknown"} {
		id, email := "password-"+state, "password-"+state+"@example.test"
		if err := vault.Save(email, credentials.Credentials{Password: "Previous-password9!", PendingPassword: integrityPassword, MailAPIURL: base + "/mail", MailMethod: "POST", MailHeaders: map[string]string{"X-Synthetic": "retained"}}); err != nil {
			t.Fatal(err)
		}
		r := protocol.PasswordAddRecovery{Version: 1, OperationID: "password-operation-" + state, Email: email, Password: integrityPassword, State: "may_have_been_sent"}
		if state == "confirmed" {
			r.State, r.AddedAt = "succeeded", integrityTime
		}
		path := protocol.PasswordAddIntentPath(filepath.Join(root, id, "password-add-result.json"))
		if err := privatestate.WriteJSON(t.Context(), path, "password-add", email, r.OperationID, r); err != nil {
			t.Fatal(err)
		}
	}
	uri, err := protocol.BuildTOTPURI("totp-confirmed@example.test", integritySeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Save("totp-confirmed@example.test", credentials.Credentials{Password: "Retained-password9!", TOTPSecret: "OLDSEED", TOTPFactorID: "old-factor", PendingTOTPSecret: integritySeed, PendingTOTPURI: uri, PendingTOTPFactorID: "new-factor"}); err != nil {
		t.Fatal(err)
	}
	if err := privatestate.WriteJSON(t.Context(), filepath.Join(root, "totp-confirmed", "totp-setup-result.json"), "totp-reset", "totp-confirmed@example.test", "setup-operation", map[string]any{
		"version": 1, "email": "totp-confirmed@example.test", "secret": integritySeed, "otpauth_uri": uri,
		"operation_id": "setup-operation", "session_id": "setup-session", "activation_succeeded": true,
		"setup_evidence_schema": 1, "activation_mode": "automatic", "seed_verified": true,
		"new_totp_factor_id": "new-factor", "enrollment_factor": map[string]any{"id": "new-factor", "factor_type": "totp", "is_recovery": false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Save("totp-unknown@example.test", credentials.Credentials{Password: "Retained-password9!", TOTPSecret: "OLDQUARANTINEDSEED", TOTPFactorID: "old-factor", TOTPResetPending: true, TOTPResetOperationID: "unknown-reset-operation"}); err != nil {
		t.Fatal(err)
	}
	if err := privatestate.WriteJSON(t.Context(), filepath.Join(root, "totp-unknown", "totp-setup-result.json"), "totp-reset", "totp-unknown@example.test", "unknown-reset-operation", map[string]any{
		"version": 1, "email": "totp-unknown@example.test", "operation_scope": "current_session", "operation_id": "unknown-reset-operation", "account_id": "fixture-account",
		"reset_totp_requested": true, "old_totp_factor_id": "old-factor", "reset_totp_phase": "disable_pending", "reset_totp_status": "unknown",
		"mutation_schema": 1, "disable_dispatch": "may_have_been_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
	}); err != nil {
		t.Fatal(err)
	}
	grantPath := jobs.OAuthGrantPath(root, &jobs.Job{ID: "issued", OAuthResultID: "issued-result"})
	if err := protocol.WriteSavedOAuthGrant(grantPath, integrityGrant("issued@example.test", base)); err != nil {
		t.Fatal(err)
	}
	source, target := filepath.Join(root, "refresh", "source.json"), filepath.Join(root, "refresh", "sub2api-import-oauth.json")
	original := refreshRotationFixture(t, source)
	doc, err := decodeRefreshDocument(original)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := refreshDocumentHash(doc)
	if err != nil {
		t.Fatal(err)
	}
	integrityJSON(t, refreshRecoveryPath(target), refreshGrantRecovery{Version: 1, Type: "tosub2-refresh-grant", AcceptedAt: integrityTime,
		AuthBase: base, ClientID: "fixture-client", SourcePath: source, TargetPath: target, TargetAbsent: true, SourceSHA256: digest, Source: doc,
		Tokens: protocol.TokenSet{AccessToken: "fixture-new-access", RefreshToken: "fixture-rotated-refresh"}})

	for _, scope := range []settings.Scope{settings.SMS, settings.Mail, settings.Sub2API, settings.Proxy} {
		if _, err := settings.New(root, vault).Save(t.Context(), scope, 0, integritySettings); err != nil {
			t.Fatal(err)
		}
	}
	subRef := "sub2api-monitor-" + sub2api.BackendIdentity(base) + "-00112233445566778899aabbccddeeff"
	for _, reference := range []string{integrityCPARef, subRef} {
		if err := vault.Save(reference, credentials.Credentials{Password: "synthetic-integration-key"}); err != nil {
			t.Fatal(err)
		}
	}
	stamp, _ := time.Parse(time.RFC3339, integrityTime)
	integrityJSON(t, filepath.Join(root, "sub2api-monitor.json"), sub2api.MonitorState{Version: 2, CredentialRef: subRef, BaseURL: base, Enabled: true,
		OpenAIRequestTimezone: "Asia/Shanghai", ExcludedPlanTypes: []string{}, LastCheckAt: &stamp, LastResult: &sub2api.MonitorSummary{Checked: 3, AutoUploadPaused: 1},
		Cooldowns: map[string]sub2api.CooldownEntry{"active@example.test": {Until: stamp, Reason: "retained cooldown"}}, Permanent: map[string]string{"blocked@example.test": "retained block"}, UpdatedAt: stamp})
	integrityJSON(t, filepath.Join(root, "cpa-settings.json"), map[string]any{"version": 1, "revision": 7, "baseUrl": base, "credentialRef": integrityCPARef,
		"priority": 0, "weight": 1, "monitorEnabled": false, "excludedPlanTypes": []string{}, "intervalMinutes": 5, "cooldownMinutes": 0})
	integrityJSON(t, filepath.Join(root, "pool-monitor.json"), map[string]any{"version": 1, "revision": 9, "target": "sub2api", "enabled": true, "excludedPlanTypes": []string{}, "intervalMinutes": 1, "cooldownMinutes": 0})
	integrityJSON(t, filepath.Join(root, "recycle-bin-settings.json"), map[string]any{"schemaVersion": 1, "revision": 3, "retentionDays": 1})
	integrityJSON(t, filepath.Join(root, "maintenance.json"), maintenance.State{Schema: 1, Revision: 4, Enabled: true})
	insights := accountinsight.DefaultSettings()
	insights.Enabled, insights.Revision = true, 4
	integrityJSON(t, filepath.Join(root, "account-insights", "settings.json"), insights)
	quality := accountquality.DefaultSettings()
	quality.Enabled, quality.BeforeUpload, quality.CooldownSeconds = true, true, 0
	integrityJSON(t, filepath.Join(root, "account-quality", "settings.json"), quality)
	for _, id := range []string{"active", "trash-current", "trash-history", "purging-history"} {
		sum := sha256.Sum256([]byte(id))
		name := hex.EncodeToString(sum[:]) + ".json"
		insight := accountinsight.Snapshot{ID: id, AuthorizationMethod: "chatgpt", LastAttempt: accountinsight.Attempt{QueryID: "prior-query-" + id, Status: "running"}, LastSuccessAt: &stamp}
		probe := accountquality.Snapshot{ID: id, LastAttempt: accountquality.Attempt{CheckID: "prior-probe-" + id, Status: "running"}, LastResult: &modelprobe.Result{Model: "synthetic-model", RuleVersion: "candy-v1", Verdict: "pass", Score: 1, Total: 1, CheckedAt: stamp}}
		integrityJSON(t, filepath.Join(root, "account-insights", name), map[string]any{"schemaVersion": 1, "accountRevision": revision, "snapshot": insight})
		integrityJSON(t, filepath.Join(root, "account-quality", name), map[string]any{"schemaVersion": 1, "accountRevision": revision, "snapshot": probe})
	}
	return vault
}

// Manifest compares every retained file, including opaque credentials and
// unknown extensions. A copy preserves mtimes because projection revisions
// include authorization file size and mtime; no record is rebound or edited.
func integrityManifest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			return errors.New("nonregular file in synthetic backup")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func integrityCopy(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return privatefile.EnsureDir(destination)
		}
		if !entry.Type().IsRegular() {
			return errors.New("refuse to copy nonregular backup file")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := privatefile.Write(t.Context(), destination, data); err != nil {
			return err
		}
		return os.Chtimes(destination, info.ModTime(), info.ModTime())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertIntegrityUnchanged(t *testing.T, before, after map[string]string, allowed ...string) {
	t.Helper()
	a, b := make(map[string]string), make(map[string]string)
	for path, hash := range before {
		a[path] = hash
	}
	for path, hash := range after {
		b[path] = hash
	}
	for _, path := range allowed {
		delete(a, path)
		delete(b, path)
	}
	if !reflect.DeepEqual(a, b) {
		changed := []string{}
		for path, hash := range a {
			if b[path] != hash {
				changed = append(changed, path)
			}
		}
		for path := range b {
			if _, ok := a[path]; !ok {
				changed = append(changed, path)
			}
		}
		t.Fatalf("recovery changed unrelated backup files: %v", changed)
	}
}

func TestRecoveryIntegrityBackupAcrossProcesses(t *testing.T) {
	// Ambient operator configuration must never supply secrets or targets to
	// this entirely synthetic restored console or either child process.
	for _, name := range []string{"SUB2API_BASE_URL", "SUB2API_ADMIN_API_KEY", "SUB2API_MONITOR_INTERVAL_MS", "SUB2API_AUTO_REPAIR_COOLDOWN_MS"} {
		t.Setenv(name, "")
	}
	taskRoot := t.TempDir()
	root, keyPath := filepath.Join(taskRoot, "service", "data"), filepath.Join(taskRoot, "independent-synthetic-key")
	key := sha256.Sum256([]byte("synthetic-independent-backup-key:" + taskRoot))
	if err := privatefile.Write(t.Context(), keyPath, key[:]); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"synthetic recovery must not issue remote requests"}`)
	}))
	defer sentinel.Close()
	vault := seedIntegrityBackup(t, root, keyPath, sentinel.URL)
	before := integrityManifest(t, root)
	backup := filepath.Join(taskRoot, "untouched-backup")
	if err := os.Rename(root, backup); err != nil {
		t.Fatal(err)
	}
	// Restore to the same path: refresh recovery intentionally binds absolute
	// source and destination paths and must reject an implicit relocation.
	integrityCopy(t, backup, root)
	assertIntegrityUnchanged(t, before, integrityManifest(t, root))
	t.Run("missing-and-wrong-key-preserve-protected-data", func(t *testing.T) {
		if _, err := newIntegrityVault(root, keyPath+".missing"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing independent key was silently regenerated")
		}
		wrongPath := filepath.Join(taskRoot, "wrong-synthetic-key")
		wrong := sha256.Sum256([]byte("different independent key"))
		if err := privatefile.Write(t.Context(), wrongPath, wrong[:]); err != nil {
			t.Fatal(err)
		}
		wrongVault, err := newIntegrityVault(root, wrongPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wrongVault.Load("password-unknown@example.test"); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatal("wrong key was treated as absent credentials")
		}
		if err := wrongVault.Save("password-unknown@example.test", credentials.Credentials{Password: "must-not-replace"}); err == nil {
			t.Fatal("wrong key replaced protected credentials")
		}
		if _, err := settings.New(root, wrongVault).Load(t.Context(), settings.Sub2API); err == nil {
			t.Fatal("wrong key opened protected settings")
		}
		registry := privatestate.NewRegistry()
		if err := registry.Require(root); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.ReadJSON(t.Context(), filepath.Join(root, "password-unknown", "password-add-result.json.intent"), "password-add", 64<<10); !errors.Is(err, privatestate.ErrProtectionRequired) {
			t.Fatal("missing protector treated unknown intent as absent", err)
		}
		assertIntegrityUnchanged(t, before, integrityManifest(t, root))
	})
	t.Run("relocation-and-truncation-fail-without-replay", func(t *testing.T) {
		relocated := filepath.Join(taskRoot, "rejected-relocation")
		integrityCopy(t, backup, relocated)
		copyBefore := integrityManifest(t, relocated)
		handled, err := finishPendingRefreshAt(t.Context(), sentinel.URL, "fallback-client", filepath.Join(relocated, "refresh", "source.json"), filepath.Join(relocated, "refresh", "sub2api-import-oauth.json"), "first@example.test")
		if !handled || !isLocalRefreshFailure(err) {
			t.Fatal("relocated refresh silently rebound the accepted grant", err)
		}
		assertIntegrityUnchanged(t, copyBefore, integrityManifest(t, relocated))
		meta := filepath.Join(relocated, "job-meta.json")
		if err := privatefile.Write(t.Context(), meta, []byte(`{"version":2,"jobs":[`)); err != nil {
			t.Fatal(err)
		}
		corrupt := integrityManifest(t, relocated)
		m := jobs.NewManager(t.Context(), 2, &store.Store{Root: relocated}, nil)
		defer m.Shutdown(context.Background())
		if err := m.Restore(t.Context()); err == nil || len(m.List()) != 0 || !m.ActivitySnapshot().Drained {
			t.Fatal("truncated aggregate exposed a partial restored manager")
		}
		assertIntegrityUnchanged(t, corrupt, integrityManifest(t, relocated))
	})
	run := func(pass string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryIntegrityProcessHelper$", "-test.v")
		command.Env = append(os.Environ(), "TOSUB2_INTEGRITY_HELPER="+pass, "TOSUB2_INTEGRITY_ROOT="+root, "TOSUB2_INTEGRITY_KEY="+keyPath, "TOSUB2_INTEGRITY_BASE="+sentinel.URL)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("recovery subprocess %s failed: %v\n%s", pass, err, out)
		}
		t.Logf("recovery subprocess %s: %s", pass, bytes.TrimSpace(out))
	}
	run("first")
	allowed := []string{"job-meta.json", filepath.Join("password-confirmed", "password-add-result.json.intent"), filepath.Join("totp-confirmed", "totp-setup-result.json"),
		filepath.Join("issued", "oauth-grant-issued-result.json"), filepath.Join("issued", "sub2api-import-oauth.json"),
		filepath.Join("refresh", ".sub2api-import-oauth.json.refresh-grant-recovery.json"), filepath.Join("refresh", "sub2api-import-oauth.json")}
	for _, email := range []string{"password-confirmed@example.test", "totp-confirmed@example.test"} {
		relative, _ := filepath.Rel(root, vault.path(email))
		allowed = append(allowed, relative)
	}
	afterFirst := integrityManifest(t, root)
	assertIntegrityUnchanged(t, before, afterFirst, allowed...)
	run("second")
	assertIntegrityUnchanged(t, afterFirst, integrityManifest(t, root), "job-meta.json")
	assertIntegrityUnchanged(t, before, integrityManifest(t, backup))
	if requests.Load() != 0 {
		t.Fatalf("backup recovery replayed %d remote requests", requests.Load())
	}
	t.Logf("integrity manifest: %d source files; 12 active/history records, 2 trash entries/3 histories; two fresh processes; zero remote requests", len(before))
}

func integrityRequest(t *testing.T, handler http.Handler, method, path string, body any, want int) []byte {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Host = "127.0.0.1:4399"
	r.Header.Set("X-Console-Token", "synthetic-integrity-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("recovery API %s returned %d, want %d: %s", path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}

func TestRecoveryIntegrityProcessHelper(t *testing.T) {
	pass := os.Getenv("TOSUB2_INTEGRITY_HELPER")
	if pass == "" {
		return
	}
	root, base := os.Getenv("TOSUB2_INTEGRITY_ROOT"), os.Getenv("TOSUB2_INTEGRITY_BASE")
	vault, err := newIntegrityVault(root, os.Getenv("TOSUB2_INTEGRITY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := config.Config{Host: "127.0.0.1", Port: 4399, OutputRoot: root, MaxActiveJobs: 2, StartupToken: "synthetic-integrity-token", TLSProfile: "native",
		ChatGPTBase: base, AuthBase: base, CodexClientID: protocol.DefaultCodexClientID, CodexRedirectURI: protocol.DefaultCodexRedirectURI, RequestTimeoutMS: 1000, ShutdownTimeoutMS: 2000}
	m := jobs.NewManager(ctx, 2, &store.Store{Root: root}, nil)
	configureAccountOperationLease(cfg, m)
	realRunner := makeJobRunnerWithCredentials(cfg, m, nil, vault, true)
	var runs atomic.Int32
	m.SetRunner(func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
		runs.Add(1)
		return realRunner(ctx, job, emit)
	})
	if err := m.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	assertIntegrityJobs(t, m, vault, pass)
	s := console.NewServer(cfg, m, embed.FS{}, nil, vault)
	defer s.Close()
	defer m.Shutdown(context.Background())
	if err := s.InitializationError(); err != nil {
		t.Fatal(err)
	}
	m.Start()
	s.StartMonitor(ctx)
	s.StartAccountInsights(ctx)
	s.StartAccountQuality(ctx)
	s.StartRecycleBin(ctx)
	assertIntegrityAPIs(t, s.Handler())
	// Close waits for every service's startup pass and cancellation. Combined
	// with the sentinel's zero count this checks real automatic startup paths,
	// rather than relying on a sleep or on never starting those components.
	closeCtx, stopClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopClose()
	if err := s.CloseContext(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(closeCtx); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 0 || !m.ActivitySnapshot().Drained {
		t.Fatal("restart resumed an old operation or retained scheduler ownership")
	}
	release, err := maintenance.AcquireOffline(root)
	if err != nil {
		t.Fatal("closed maintenance did not release local ownership", err)
	}
	defer release()
	if pass == "first" {
		grantPath := jobs.OAuthGrantPath(root, &jobs.Job{ID: "issued", OAuthResultID: "issued-result"})
		grant, err := protocol.ReadSavedOAuthGrant(grantPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := protocol.FinalizeSavedOAuthGrant(ctx, grant, protocol.FlowOptions{Email: grant.Email, AuthBase: base, ChatGPTBase: base,
			ClientID: grant.ClientID, RedirectURI: grant.RedirectURI, AuthorizationMethod: grant.AuthorizationMethod, GrantPath: grantPath, OutputPath: filepath.Join(root, "issued", "sub2api-import-oauth.json")})
		if err != nil || len(result.Export.Accounts) != 1 || result.Export.Accounts[0].Credentials.RefreshToken != grant.Tokens.RefreshToken {
			t.Fatal("issued OAuth result was not finalized locally", err)
		}
		doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			t.Error("local accepted-grant recovery attempted another refresh")
			return nil, errors.New("unexpected network call")
		})
		if err := refreshExistingExportAt(ctx, doer, base, "fallback-client", filepath.Join(root, "refresh", "source.json"), filepath.Join(root, "refresh", "sub2api-import-oauth.json"), "first@example.test"); err != nil {
			t.Fatal(err)
		}
	} else if pass != "second" {
		t.Fatal("invalid recovery exercise pass")
	}
	original, err := os.ReadFile(filepath.Join(root, "refresh", "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertRotatedFirstAccount(t, filepath.Join(root, "refresh", "sub2api-import-oauth.json"), original)
	if handled, err := finishPendingRefreshAt(ctx, base, "fallback-client", filepath.Join(root, "refresh", "source.json"), filepath.Join(root, "refresh", "sub2api-import-oauth.json"), "first@example.test"); handled || err != nil {
		t.Fatal("completed refresh retained a replayable recovery record", err)
	}
	if _, err := os.Stat(jobs.OAuthGrantPath(root, &jobs.Job{ID: "issued", OAuthResultID: "issued-result"})); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified OAuth output retained the consumed grant receipt", err)
	}
	t.Log("accounts, histories, trash, protected settings/references, local receipts and unknown intents verified; no runner invocation")
}

func assertIntegrityJobs(t *testing.T, m *jobs.Manager, vault *integrityVault, pass string) {
	t.Helper()
	if len(m.List()) != 12 || len(m.ListTrash()) != 2 {
		t.Fatal("active/history/trash cardinality changed", len(m.List()), len(m.ListTrash()))
	}
	job := func(id string) *jobs.Job {
		t.Helper()
		j, ok := m.Get(id)
		if !ok {
			t.Fatal("missing restored record", id)
		}
		return j
	}
	a := job("active")
	if !a.ResultSaved || a.ManualExpiresAt != "2026-11-02T00:00:00Z" || !strings.Contains(strings.Join(a.Logs, "\n"), "retained account log") || len(a.LogEntries) != 1 || job("active-history").Email != a.Email {
		t.Fatal("account deadline, authorization or historical logs changed")
	}
	for _, id := range []string{"queued", "waiting"} {
		j := job(id)
		if j.Status != jobs.StatusFailed || j.OperationStatus != "interrupted" {
			t.Fatal("old login was not interrupted", id, j.Status, j.OperationStatus)
		}
		input, _ := m.InputCopy(id)
		for _, key := range []string{"emailCode", "password", "cookies", "callback", "codeVerifier"} {
			if input[key] != nil {
				t.Fatal("restart reused transient authentication input", id, key)
			}
		}
	}
	if pass == "first" {
		if job("issued").Status != "resume_available" || job("issued").OAuthResultID != "issued-result" || job("refresh").Stage != "refresh_output_pending" {
			t.Fatal("pending grants lost their independently recoverable identity")
		}
	} else if !job("issued").ResultSaved || !job("refresh").ResultSaved {
		t.Fatal("finalized local authorizations were not rediscovered")
	}
	for _, state := range []string{"confirmed", "unknown"} {
		c, err := vault.Load("password-" + state + "@example.test")
		j := job("password-" + state)
		if err != nil || c.MailMethod != "POST" || c.MailHeaders["X-Synthetic"] != "retained" {
			t.Fatal("password recovery lost unrelated credential fields", err)
		}
		if state == "confirmed" {
			if c.Password != integrityPassword || c.PendingPassword != "" || j.OperationStatus != "succeeded" || j.PasswordAddOutcome != jobs.PasswordOutcomeAdded {
				t.Fatal("confirmed password receipt was not committed locally")
			}
		} else if c.Password != "Previous-password9!" || c.PendingPassword != integrityPassword || j.OperationStatus != "failed" || !strings.Contains(j.PasswordAddError, "PASSWORD_ADD_RESULT_UNKNOWN") {
			t.Fatal("unknown password result overwrote credentials or became success")
		}
	}
	c, err := vault.Load("totp-confirmed@example.test")
	if err != nil || c.TOTPSecret != integritySeed || c.TOTPFactorID != "new-factor" || c.PendingTOTPSecret != "" || c.PendingTOTPFactorID != "" || job("totp-confirmed").OperationStatus != "succeeded" {
		t.Fatal("confirmed TOTP did not retain seed/factor binding", err)
	}
	c, err = vault.Load("totp-unknown@example.test")
	u := job("totp-unknown")
	if err != nil || c.TOTPSecret != "OLDQUARANTINEDSEED" || !c.TOTPResetPending || c.TOTPResetOperationID != "unknown-reset-operation" || u.Status != jobs.StatusFailed || !u.TOTPRecoveryPending || u.TOTPResetCommitted {
		t.Fatal("unknown reset lost its quarantine or binding", err)
	}
	u = job("upload-unknown")
	if !u.AutoRepairSyncPaused || !reflect.DeepEqual(u.AutoRepairPendingAccountIDs, []int{41, 42}) || !u.ResultSaved || !job("restored-paused").RecoveryPaused {
		t.Fatal("unknown upload or explicitly restored history resumed")
	}
	retained := map[string]bool{}
	for _, entry := range m.ListTrash() {
		for _, j := range entry.Jobs {
			retained[j.ID] = true
			if _, active := m.Get(j.ID); active {
				t.Fatal("archived historical record was rediscovered as active")
			}
		}
		if entry.ID == "purging-entry" && (entry.State != jobs.TrashStatePurging || entry.PurgeStartedAt == nil || entry.LastError != "synthetic cleanup interrupted") {
			t.Fatal("irreversible purge tombstone lost its cleanup evidence")
		}
	}
	if len(retained) != 3 || !retained["trash-current"] || !retained["trash-history"] || !retained["purging-history"] || !m.ActivitySnapshot().Drained {
		t.Fatal("trash history or restart queue integrity failed")
	}
	for _, email := range []string{"active@example.test", "trash@example.test", "purging@example.test"} {
		value, err := vault.Load(email)
		if err != nil || value.Password != "Retained-password9!" || value.MailBody != "encoded=preserved" || value.MailHeaders["X-Synthetic"] != "retained" {
			t.Fatal("active or archived credentials cannot be recovered from the same snapshot", err)
		}
	}
	for _, path := range []string{filepath.Join("password-confirmed", "password-add-result.json.intent"), filepath.Join("totp-confirmed", "totp-setup-result.json")} {
		if _, err := os.Stat(filepath.Join(vault.root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("local credential commit did not retire confirmed recovery evidence", err)
		}
	}
	metadata, err := os.ReadFile(filepath.Join(vault.root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{integrityPassword, integritySeed, "synthetic-integration-key", "synthetic-private-value", "OLDQUARANTINEDSEED", "Retained-password9!", "stale-input-password", "fixture-rotated-refresh"} {
		if bytes.Contains(metadata, []byte(secret)) {
			t.Fatal("restored ordinary metadata contains a private credential or transient input")
		}
	}
}

func assertIntegrityAPIs(t *testing.T, handler http.Handler) {
	t.Helper()
	for _, scope := range []settings.Scope{settings.SMS, settings.Mail, settings.Sub2API, settings.Proxy} {
		data := integrityRequest(t, handler, http.MethodGet, "/api/integration-settings/"+string(scope), nil, http.StatusOK)
		var got settings.Snapshot
		if json.Unmarshal(data, &got) != nil || got.Revision != 1 {
			t.Fatal("protected configuration revision lost")
		}
		var wantRaw, gotRaw bytes.Buffer
		if json.Compact(&wantRaw, integritySettings) != nil || json.Compact(&gotRaw, got.Config) != nil || !bytes.Equal(wantRaw.Bytes(), gotRaw.Bytes()) {
			t.Fatal("protected settings lost secrets, explicit zero/false/empty, or unknown large integer")
		}
	}
	var cpa struct {
		Configured, HasAPIKey        bool
		CredentialPersistence, Error string
		Revision                     int64
		CooldownMinutes              int
	}
	if json.Unmarshal(integrityRequest(t, handler, http.MethodGet, "/api/cpa/settings", nil, 200), &cpa) != nil || !cpa.Configured || !cpa.HasAPIKey || cpa.CredentialPersistence != "secure" || cpa.Error != "" || cpa.Revision != 7 || cpa.CooldownMinutes != 0 {
		t.Fatal("CPA configuration lost its protected credential reference or explicit zero")
	}
	var pool struct {
		Enabled, Configured, Uncertain            bool
		Target, CredentialPersistence             string
		ConfigurationError, OpenAIRequestTimezone string
		CooldownMinutes                           int
		Revision                                  int64
		LastResult                                *sub2api.MonitorSummary
	}
	if json.Unmarshal(integrityRequest(t, handler, http.MethodGet, "/api/pool-monitor", nil, 200), &pool) != nil || !pool.Enabled || !pool.Configured || pool.Uncertain || pool.ConfigurationError != "" || pool.Target != "sub2api" || pool.CooldownMinutes != 0 || pool.Revision != 9 || pool.CredentialPersistence != "secure_or_environment" || pool.OpenAIRequestTimezone != "Asia/Shanghai" || pool.LastResult == nil || pool.LastResult.Checked != 3 || pool.LastResult.AutoUploadPaused != 1 {
		t.Fatal("shared monitor policy, credential reference or check history was not retained")
	}
	var insight struct {
		Snapshots map[string]accountinsight.Snapshot
	}
	if json.Unmarshal(integrityRequest(t, handler, http.MethodGet, "/api/account-insights?ids=active", nil, 200), &insight) != nil || insight.Snapshots["active"].LastAttempt.Status != "cancelled" || insight.Snapshots["active"].LastSuccessAt == nil || insight.Snapshots["active"].Automatic {
		t.Fatal("query history was lost or prior query resumed")
	}
	var quality struct {
		Snapshots map[string]accountquality.Snapshot
	}
	if json.Unmarshal(integrityRequest(t, handler, http.MethodGet, "/api/account-quality?ids=active,trash-current,trash-history,purging-history", nil, 200), &quality) != nil || len(quality.Snapshots) != 4 {
		t.Fatal("active or recoverable probe history lost")
	}
	for id, snapshot := range quality.Snapshots {
		if snapshot.LastAttempt.Status != "cancelled" || snapshot.LastResult == nil || snapshot.LastResult.RuleVersion != "candy-v1" || snapshot.LastResult.Total != 1 {
			t.Fatal("probe replay or historical single-question result changed", id)
		}
	}
	var status struct {
		Enabled, Drained      bool
		Jobs                  jobs.ActivitySnapshot
		Insights, Probes, SMS int
		RecycleRunning        bool
	}
	if json.Unmarshal(integrityRequest(t, handler, http.MethodGet, "/api/maintenance", nil, 200), &status) != nil || !status.Enabled || !status.Drained || !status.Jobs.AdmissionPaused || !status.Jobs.Drained || status.Insights+status.Probes+status.SMS != 0 || status.RecycleRunning {
		t.Fatal("restored maintenance did not pause and drain all work sources")
	}
	integrityRequest(t, handler, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "must-not-start@example.test"}, http.StatusServiceUnavailable)
	for _, path := range []string{"/api/account-insights/refresh", "/api/account-quality/refresh", "/api/sub2api/monitor/check", "/api/cpa/monitor/check"} {
		integrityRequest(t, handler, http.MethodPost, path, map[string]any{"ids": []string{"active"}}, http.StatusServiceUnavailable)
	}
}
