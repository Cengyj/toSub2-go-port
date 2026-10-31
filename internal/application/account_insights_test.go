package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

type insightGrantFixture struct {
	manager  *jobs.Manager
	store    *store.Store
	vault    *credentials.MemoryStore
	job      *jobs.Job
	account  map[string]any
	document map[string]any
	path     string
}

func newInsightGrantFixture(t *testing.T, method string, proxy any) *insightGrantFixture {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(t.Context(), 1, st, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	input := map[string]any{"authorizationMethod": method, "password": "synthetic-private-password"}
	if proxy != nil {
		input["proxyUrl"] = proxy
	}
	job, err := m.Submit(jobs.Spec{ID: "insight-account", Email: "reader@example.test", ImportOnly: true, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	client := protocol.DefaultCodexClientID
	if method == jobs.AuthorizationMethodBPS {
		client = protocol.DefaultBPSClientID
	}
	account := map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{
		"client_id": client, "access_token": "synthetic-private-access", "refresh_token": "synthetic-private-refresh",
		"email": job.Email, "chatgpt_account_id": "synthetic-workspace",
	}, "extra": map[string]any{"email": job.Email, "account_id": "synthetic-workspace", "client_id": client}}
	f := &insightGrantFixture{manager: m, store: st, vault: vault, job: job, account: account,
		document: map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{account}},
		path:     filepath.Join(st.Root, job.ID, "sub2api-import-oauth.json")}
	f.write(t)
	return f
}

func (f *insightGrantFixture) write(t *testing.T) {
	t.Helper()
	if err := protocol.WritePrivateJSON(f.path, f.document); err != nil {
		t.Fatal(err)
	}
}
func (f *insightGrantFixture) load(t *testing.T) *AccountInsightGrant {
	t.Helper()
	grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(grant.Release)
	return grant
}
func insightJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(data) + ".synthetic"
}

func TestAccountInsightGrantReadOnlyIdentityAndSerialization(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f := newInsightGrantFixture(t, method, "")
			fields := f.account["credentials"].(map[string]any)
			fields["plan_type"] = "prolite"
			fields["access_token"] = insightJWT(t, map[string]any{"email": f.job.Email, "auth": map[string]any{"chatgpt_account_id": "synthetic-workspace", "chatgpt_plan_type": "plus"}})
			f.write(t)
			before, _ := f.manager.Get(f.job.ID)
			beforeMetadata, err := os.ReadFile(f.store.Path("job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			beforeGrant, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			beforeSecrets, err := f.vault.Load(f.job.Email)
			if err != nil {
				t.Fatal(err)
			}
			grant := f.load(t)
			if grant.Credentials.AuthorizationMethod != method || grant.Credentials.AccountID != "synthetic-workspace" || grant.Credentials.Email != f.job.Email || grant.Credentials.PlanType != "prolite" {
				t.Fatal("grant changed the selected authorization identity or explicit plan")
			}
			if !grant.ProxyOverride || grant.ProxyURL != "" || grant.ExpiresAt != nil || len(grant.IdentityKey) != 64 {
				t.Fatal("grant lost direct proxy override or invented expiry")
			}
			encoded, err := json.Marshal(grant)
			if err != nil || string(encoded) != "{}" {
				t.Fatal("private grant is serializable")
			}
			if err := grant.Validate(); err != nil {
				t.Fatal(err)
			}
			grant.Release()
			after, _ := f.manager.Get(f.job.ID)
			afterMetadata, metadataErr := os.ReadFile(f.store.Path("job-meta.json"))
			afterGrant, grantErr := os.ReadFile(f.path)
			afterSecrets, secretErr := f.vault.Load(f.job.Email)
			if metadataErr != nil || grantErr != nil || secretErr != nil || !reflect.DeepEqual(before, after) || !bytes.Equal(beforeMetadata, afterMetadata) || !bytes.Equal(beforeGrant, afterGrant) || !reflect.DeepEqual(beforeSecrets, afterSecrets) {
				t.Fatal("read-only query changed history, export or protected credentials")
			}
		})
	}
}

func TestAccountInsightGrantRejectsConflictingIdentityAndInvalidDocuments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*insightGrantFixture)
		want   error
	}{
		{"missing-access", func(f *insightGrantFixture) { delete(f.account["credentials"].(map[string]any), "access_token") }, ErrInsightGrantUnavailable},
		{"missing-client", func(f *insightGrantFixture) {
			delete(f.account["credentials"].(map[string]any), "client_id")
			delete(f.account["extra"].(map[string]any), "client_id")
		}, ErrInsightGrantIdentity},
		{"client-conflict", func(f *insightGrantFixture) {
			f.account["extra"].(map[string]any)["client_id"] = protocol.DefaultBPSClientID
		}, ErrInsightGrantIdentity},
		{"method-conflict", func(f *insightGrantFixture) {
			f.account["credentials"].(map[string]any)["client_id"] = protocol.DefaultBPSClientID
			f.account["extra"].(map[string]any)["client_id"] = protocol.DefaultBPSClientID
		}, ErrInsightGrantIdentity},
		{"email-conflict", func(f *insightGrantFixture) { f.account["extra"].(map[string]any)["email"] = "other@example.test" }, ErrInsightGrantIdentity},
		{"email-missing", func(f *insightGrantFixture) {
			delete(f.account["credentials"].(map[string]any), "email")
			delete(f.account["extra"].(map[string]any), "email")
		}, ErrInsightGrantIdentity},
		{"workspace-conflict", func(f *insightGrantFixture) { f.account["extra"].(map[string]any)["account_id"] = "other-workspace" }, ErrInsightGrantIdentity},
		{"workspace-missing", func(f *insightGrantFixture) {
			delete(f.account["credentials"].(map[string]any), "chatgpt_account_id")
			delete(f.account["extra"].(map[string]any), "account_id")
		}, ErrInsightGrantIdentity},
		{"wrong-field-type", func(f *insightGrantFixture) { f.account["credentials"].(map[string]any)["email"] = 123 }, ErrInsightGrantIdentity},
		{"wrong-document", func(f *insightGrantFixture) { f.document["type"] = "private-canary" }, ErrInsightGrantUnavailable},
		{"future-version", func(f *insightGrantFixture) { f.document["version"] = 100 }, ErrInsightGrantUnavailable},
		{"multiple-accounts", func(f *insightGrantFixture) { f.document["accounts"] = []any{f.account, f.account} }, ErrInsightGrantUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			test.mutate(f)
			f.write(t)
			grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
			if grant != nil || !errors.Is(err, test.want) || strings.Contains(err.Error(), "private") && !errors.Is(err, ErrInsightProxyUnavailable) {
				t.Fatal("unsafe grant was admitted or error lost its safe category", err)
			}
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("failed reader leaked its lease", err)
			}
			lease.Release()
		})
	}
	for _, conflict := range []string{"email", "workspace", "client"} {
		t.Run("access-"+conflict, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			claims := map[string]any{"email": f.job.Email, "chatgpt_account_id": "synthetic-workspace", "client_id": protocol.DefaultCodexClientID}
			switch conflict {
			case "email":
				claims["email"] = "other@example.test"
			case "workspace":
				claims["chatgpt_account_id"] = "other-workspace"
			case "client":
				claims["client_id"] = protocol.DefaultBPSClientID
			}
			f.account["credentials"].(map[string]any)["access_token"] = insightJWT(t, claims)
			f.write(t)
			if grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID); grant != nil || !errors.Is(err, ErrInsightGrantIdentity) {
				t.Fatal("access identity conflict was accepted", err)
			}
		})
	}
}

func TestAccountInsightGrantExpirationUsesOnlyKnownAccessAndExportValues(t *testing.T) {
	const past, future int64 = 1700000000, 2000000000
	tests := []struct {
		name                 string
		exported             any
		token, idToken, want int64
	}{
		{"unknown", nil, 0, 0, 0}, {"id-token-is-not-access-expiry", nil, 0, past, 0},
		{"access-expiry", nil, past, future, past}, {"export-expiry", past, 0, future, past},
		{"earliest-export", past, future, 0, past}, {"earliest-access", future, past, 0, past},
		{"export-string", "1700000000", 0, 0, past}, {"export-rfc3339", "2023-11-14T22:13:20Z", 0, 0, past},
		{"malformed-expiry", "unknown", 0, 0, 0}, {"negative-expiry", -1, 0, 0, 0}, {"fractional-expiry", 123.5, 0, 0, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			fields := f.account["credentials"].(map[string]any)
			fields["expires_at"] = test.exported
			fields["access_token"] = insightJWT(t, map[string]any{"exp": test.token})
			fields["id_token"] = insightJWT(t, map[string]any{"exp": test.idToken})
			f.write(t)
			grant := f.load(t)
			if grant.TokenExpiresAt != test.token || (test.want == 0 && grant.ExpiresAt != nil) || (test.want != 0 && (grant.ExpiresAt == nil || grant.ExpiresAt.Unix() != test.want)) {
				t.Fatal("grant invented or extended access expiry")
			}
		})
	}
}

func TestAccountInsightGrantRejectsPendingUnsafeOrReplacedFiles(t *testing.T) {
	for _, scenario := range []string{"journal", "missing", "directory", "oversize", "trailing-json", "symlink", "directory-symlink", "root-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			root := f.store.Root
			want := ErrInsightGrantUnavailable
			switch scenario {
			case "journal":
				if err := protocol.WritePrivateJSON(filepath.Join(filepath.Dir(f.path), ".sub2api-import-oauth.json.refresh-grant-recovery.json"), map[string]any{"synthetic": true}); err != nil {
					t.Fatal(err)
				}
				want = ErrInsightRecoveryPending
			case "missing":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.path, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(f.path, bytes.Repeat([]byte("x"), (2<<20)+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing-json":
				file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("{}")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal("append failed", writeErr, closeErr)
				}
			case "symlink":
				actual := filepath.Join(filepath.Dir(f.path), "synthetic-private.json")
				if err := os.Rename(f.path, actual); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, f.path); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				dir := filepath.Dir(f.path)
				actual := filepath.Join(f.store.Root, "moved-private")
				if err := os.Rename(dir, actual); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, dir); err != nil {
					t.Fatal(err)
				}
			case "root-symlink":
				root = filepath.Join(t.TempDir(), "linked-root")
				if err := os.Symlink(f.store.Root, root); err != nil {
					t.Fatal(err)
				}
			}
			if grant, err := LoadAccountInsightGrant(t.Context(), root, f.manager, f.job.ID); grant != nil || !errors.Is(err, want) {
				t.Fatal("unsafe or pending grant accepted", err)
			}
		})
	}
	t.Run("changed-grant", func(t *testing.T) {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
		grant := f.load(t)
		f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement-access"
		f.write(t)
		if !errors.Is(grant.Validate(), jobs.ErrAccountQuerySuperseded) {
			t.Fatal("replaced grant accepted a stale result")
		}
		grant.Release()
		replacement := f.load(t)
		if replacement.IdentityKey == grant.IdentityKey {
			t.Fatal("replacement grant reused stale identity key")
		}
	})
	t.Run("pending-after-read", func(t *testing.T) {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
		grant := f.load(t)
		if err := protocol.WritePrivateJSON(filepath.Join(filepath.Dir(f.path), ".sub2api-import-oauth.json.refresh-grant-recovery.json"), map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(grant.Validate(), jobs.ErrAccountQuerySuperseded) {
			t.Fatal("pending rotation accepted stale query")
		}
	})
	if runtime.GOOS != "windows" {
		t.Run("public-permissions", func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			if err := os.Chmod(f.path, 0644); err != nil {
				t.Fatal(err)
			}
			if grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID); grant != nil || !errors.Is(err, ErrInsightGrantUnavailable) {
				t.Fatal("non-private authorization accepted", err)
			}
		})
	}
}

func TestAccountInsightGrantProxySourcesAndInvalidation(t *testing.T) {
	t.Run("configured-input", func(t *testing.T) {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "http://synthetic-user:synthetic-pass@proxy.example.test:8080")
		grant := f.load(t)
		if !grant.ProxyOverride || grant.ProxyURL != "http://synthetic-user:synthetic-pass@proxy.example.test:8080" || grant.proxyFromVault {
			t.Fatal("captured proxy was replaced")
		}
		if _, err := f.manager.Update(f.job.ID, map[string]any{"proxyUrl": ""}); err != nil {
			t.Fatal(err)
		}
		if grant.Context().Err() == nil || !errors.Is(grant.Validate(), jobs.ErrAccountQuerySuperseded) {
			t.Fatal("proxy change did not invalidate the query")
		}
	})
	t.Run("global-fallback", func(t *testing.T) {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
		grant := f.load(t)
		if grant.ProxyOverride || grant.ProxyURL != "" {
			t.Fatal("missing account proxy prevented global configuration")
		}
	})
	t.Run("invalid-input", func(t *testing.T) {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, 123)
		if grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID); grant != nil || !errors.Is(err, ErrInsightProxyUnavailable) {
			t.Fatal("invalid proxy silently fell back", err)
		}
	})
	for _, scenario := range []string{"saved", "missing", "empty", "unavailable", "changed"} {
		t.Run("vault-"+scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "http://proxy.example.test:8080")
			f.manager.ConsumeInput(f.job.ID, "proxyUrl")
			switch scenario {
			case "missing":
				if err := f.vault.Delete(f.job.Email); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := f.vault.Save(f.job.Email, credentials.Credentials{}); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				f.manager.SetCredentialStore(nil)
			}
			grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
			if scenario == "missing" || scenario == "empty" || scenario == "unavailable" {
				if grant != nil || !errors.Is(err, ErrInsightProxyUnavailable) {
					t.Fatal("required proxy was silently lost", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(grant.Release)
			if !grant.proxyFromVault || !grant.ProxyOverride || grant.ProxyURL != "http://proxy.example.test:8080" {
				t.Fatal("protected proxy was not selected")
			}
			if scenario == "changed" {
				if err := f.vault.Save(f.job.Email, credentials.Credentials{ProxyURL: "http://replacement.example.test:8080"}); err != nil {
					t.Fatal(err)
				}
				if !errors.Is(grant.Validate(), jobs.ErrAccountQuerySuperseded) {
					t.Fatal("changed protected proxy accepted stale query")
				}
			}
		})
	}
}

func TestAccountInsightGrantVaultCancellationDoesNotHoldSchedulerLock(t *testing.T) {
	f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "http://proxy.example.test:8080")
	f.manager.ConsumeInput(f.job.ID, "proxyUrl")
	vault := &insightBlockingVault{entered: make(chan struct{})}
	f.manager.SetCredentialStore(vault)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		grant, err := LoadAccountInsightGrant(ctx, f.store.Root, f.manager, f.job.ID)
		if grant != nil {
			grant.Release()
		}
		done <- err
	}()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("query did not reach protected store")
	}
	listed := make(chan struct{})
	go func() { _ = f.manager.List(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("credential lookup held scheduler lock")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("credential cancellation was lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential query did not cancel")
	}
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("canceled loader leaked lease", err)
	}
	lease.Release()
}

type insightBlockingVault struct{ entered chan struct{} }

func (v *insightBlockingVault) Load(string) (credentials.Credentials, error) {
	return credentials.Credentials{}, credentials.ErrUnavailable
}
func (v *insightBlockingVault) Save(string, credentials.Credentials) error {
	return credentials.ErrUnavailable
}
func (v *insightBlockingVault) Delete(string) error { return credentials.ErrUnavailable }
func (v *insightBlockingVault) LoadContext(ctx context.Context, _ string) (credentials.Credentials, error) {
	close(v.entered)
	<-ctx.Done()
	return credentials.Credentials{}, ctx.Err()
}
func (v *insightBlockingVault) SaveContext(context.Context, string, credentials.Credentials) error {
	return credentials.ErrUnavailable
}
func (v *insightBlockingVault) DeleteContext(context.Context, string) error {
	return credentials.ErrUnavailable
}
