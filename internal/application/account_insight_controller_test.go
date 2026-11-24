package application

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestInsightControllerRejectsBeforeOpeningTransportAndReleasesOwnership(t *testing.T) {
	for _, scenario := range []string{"expired", "pool", "rotating", "identity", "canceled", "factory", "host-busy", "lock-unavailable", "superseded", "shared-reset", "shared-setup", "vault-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			f.manager.UpdateResult(f.job.ID, true, "")
			cfg := config.Config{OutputRoot: f.store.Root}
			want := ""
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "expired":
				f.account["credentials"].(map[string]any)["expires_at"] = time.Now().Add(-time.Hour).Unix()
				want = "authorization_expired"
			case "pool":
				cfg.ProxyURLs = []string{"http://synthetic.invalid:80"}
				want = "proxy_selection_required"
			case "rotating":
				cfg.ProxyURL = "http://session-{session}.invalid:80"
				want = "proxy_selection_required"
			case "identity":
				f.account["credentials"].(map[string]any)["email"] = "other@example.test"
				want = "identity_mismatch"
			case "canceled":
				cancel()
			case "factory":
				want = "transport_unavailable"
			case "host-busy":
				want = "account_busy"
				f.manager.SetAccountOperationLease(func(string) (func() error, error) { return nil, accountlock.ErrBusy })
			case "lock-unavailable":
				want = "ownership_unavailable"
				f.manager.SetAccountOperationLease(func(string) (func() error, error) { return nil, errors.New("synthetic private path must not escape") })
			case "superseded":
				want = "query_superseded"
				f.manager.SetAccountOperationLease(func(string) (func() error, error) {
					f.manager.SetCredentialStore(credentials.NewMemoryStore())
					return func() error { return nil }, nil
				})
			case "shared-reset", "shared-setup":
				want = "recovery_pending"
				saved, err := f.vault.Load(f.job.Email)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "shared-reset" {
					saved.TOTPResetPending = true
				} else {
					saved.PendingTOTPSecret = "JBSWY3DPEHPK3PXP"
				}
				if err := f.vault.Save(f.job.Email, saved); err != nil {
					t.Fatal(err)
				}
			case "vault-unavailable":
				want = "credentials_unavailable"
				f.manager.SetCredentialStore(nil)
			}
			f.write(t)
			c := NewAccountInsightController(cfg, f.manager)
			opened, closed := 0, 0
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				opened++
				return nil, func() { closed++ }, errors.New("synthetic secret must not escape")
			}
			_, _, release, err := c.AcquireSavedQuery(ctx, f.job.ID)
			if release != nil {
				release()
				t.Fatal("failed acquisition returned ownership")
			}
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			} else {
				var failure *accountinsight.QueryError
				if !errors.As(err, &failure) || failure.Code != want {
					t.Fatalf("want %s, got %v", want, err)
				}
			}
			wantCalls := 0
			if scenario == "factory" {
				wantCalls = 1
			}
			if opened != wantCalls || closed != wantCalls {
				t.Fatalf("transport lifecycle: open=%d close=%d", opened, closed)
			}
			f.manager.SetAccountOperationLease(nil)
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("failed query leaked ownership", err)
			}
			lease.Release()
		})
	}
}

func TestInsightControllerFixedProxyReadOnlyAcquisition(t *testing.T) {
	f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
	f.manager.UpdateResult(f.job.ID, true, "")
	c := NewAccountInsightController(config.Config{OutputRoot: f.store.Root, ProxyURL: "http://fallback.invalid:80", ProxyURLs: []string{"http://pool.invalid:80"}}, f.manager)
	opened, closed, requests := 0, 0, 0
	c.transportFactory = func(proxy string) (protocol.Doer, func(), error) {
		opened++
		if proxy != "" {
			t.Fatal("explicit direct connection was replaced")
		}
		return qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			requests++
			return nil, errors.New("not called")
		}), func() { closed++ }, nil
	}
	before, _ := f.manager.Get(f.job.ID)
	beforeGrant, _ := os.ReadFile(f.path)
	beforeSecrets, _ := f.vault.Load(f.job.Email)
	provider := c.Provider(func() string { return "Asia/Shanghai" })
	accounts := provider.Accounts()
	if len(accounts) != 1 || !accounts[0].CanQuery || provider.Timezone() != "Asia/Shanghai" {
		t.Fatal("query eligibility changed", accounts)
	}
	if opened != 0 {
		t.Fatal("inventory opened transport")
	}
	_, _, release, err := c.AcquireSavedQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		t.Fatal("query failed to hold account ownership", err)
	}
	release()
	release()
	after, _ := f.manager.Get(f.job.ID)
	afterGrant, _ := os.ReadFile(f.path)
	afterSecrets, _ := f.vault.Load(f.job.Email)
	if opened != 1 || closed != 1 || requests != 0 || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeGrant, afterGrant) || !reflect.DeepEqual(beforeSecrets, afterSecrets) {
		t.Fatal("acquisition changed account, credential, grant or transport lifecycle")
	}
}

func TestInsightControllerDropsStaleDispatchAndDiagnostics(t *testing.T) {
	for _, moment := range []string{"before", "during"} {
		t.Run(moment, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
			f.manager.UpdateResult(f.job.ID, true, "")
			if err := f.manager.BeginAccountRequest(f.job.ID, "usage").Complete(jobs.AccountRequestObservation{Status: "failed", HTTPStatus: 401}); err != nil {
				t.Fatal(err)
			}
			before, _ := f.manager.Get(f.job.ID)
			replace := func() {
				f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replaced-access"
				f.write(t)
			}
			c := NewAccountInsightController(config.Config{OutputRoot: f.store.Root}, f.manager)
			requests := 0
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					requests++
					if r.Method != http.MethodGet || !r.SingleAttempt || r.URL != "https://chatgpt.com/backend-api/wham/usage" {
						t.Fatal("query dispatched unexpected operation")
					}
					replace()
					return &transport.Response{StatusCode: 403, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"synthetic"}`)}, nil
				}), func() {}, nil
			}
			lease, err := c.Provider(nil).Acquire(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if moment == "before" {
				replace()
			}
			result := lease.Query(lease.Context, protocol.InsightsOptions{SkipComponents: []string{"subscription", "bps", "resetCredits"}})
			if len(result.Errors) == 0 || lease.Validate() == nil {
				t.Fatal("stale grant remained valid")
			}
			want := 1
			if moment == "before" {
				want = 0
			}
			if requests != want {
				t.Fatalf("query replayed: requests=%d want=%d", requests, want)
			}
			after, _ := f.manager.Get(f.job.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("stale response changed diagnostics or operation history")
			}
		})
	}
}

func TestReadOnlyTransportRequiresExplicitNativeMode(t *testing.T) {
	// A native mode name in the fingerprint profile field is not permission to
	// change transport. Unsupported exact profiles must fail without fallback.
	for _, name := range []string{transport.NativeProfile, "synthetic-unsupported-profile"} {
		f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
		g := f.load(t)
		cfg := config.Config{AuthTLSProfile: name}
		doer, release, err := openReadOnlyTransport(cfg, g, nil)
		if release != nil {
			release()
		}
		if doer != nil || err == nil || err.Code != readOnlyTransportUnavailable {
			t.Fatalf("unsupported exact identity %q switched transport", name)
		}
		cfg.TLSProfile = transport.NativeProfile
		doer, release, err = openReadOnlyTransport(cfg, g, nil)
		if err != nil {
			t.Fatal("explicit native mode unavailable", err)
		}
		release()
		if _, ok := doer.(*transport.Client); !ok {
			t.Fatal("explicit native mode was replaced")
		}
		g.Release()
	}
}
