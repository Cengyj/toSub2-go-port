package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

type qualityDoer func(context.Context, transport.Request) (*transport.Response, error)

func (f qualityDoer) Do(ctx context.Context, request transport.Request) (*transport.Response, error) {
	return f(ctx, request)
}

func qualitySyntheticResponse(request transport.Request, correct bool) *transport.Response {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(request.Body, &payload)
	answer := "21"
	if !correct {
		answer = "29"
	}
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "synthetic-response", "object": "response", "status": "completed", "model": payload.Model,
		"output": []any{map[string]any{"id": "synthetic-message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer}}}},
	}})
	return &transport.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("data: " + string(data) + "\n\n")}
}

func newQualityControllerFixture(t *testing.T, method string) (*insightGrantFixture, *AccountQualityController) {
	t.Helper()
	f := newInsightGrantFixture(t, method, "")
	f.manager.UpdateResult(f.job.ID, true, "")
	c, err := NewAccountQualityController(config.Config{OutputRoot: f.store.Root}, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			return qualitySyntheticResponse(r, true), nil
		}), func() {}, nil
	}
	c.Service.Start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return f, c
}

func enableQualityFixture(t *testing.T, c *AccountQualityController, edit func(*accountquality.Settings)) {
	t.Helper()
	settings, err := c.Service.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled = true
	settings.BeforeUpload = true
	settings.Model = "synthetic-model"
	settings.CooldownSeconds = 0
	if edit != nil {
		edit(&settings)
	}
	if _, err = c.Service.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
}

func TestAccountQualityDefaultOffMakesNoProbeButHoldsUploadReservation(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		t.Fatal("disabled policy constructed an inference transport")
		return nil, nil, nil
	}
	ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	if err := gate(ctx, f.account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		t.Fatal("disabled model policy released the upload account reservation", err)
	}
	cleanup()
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("disabled policy upload cleanup leaked the account reservation", err)
	}
	lease.Release()
	if err := c.WorkflowUploadCheck(t.Context(), f.job.ID, f.account); err != nil {
		t.Fatal("disabled workflow gate changed legacy behavior", err)
	}
}

func TestAccountQualityManualGatePreservesTokensAndHoldsLease(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSecrets, err := f.vault.Load(f.job.Email)
	if err != nil {
		t.Fatal(err)
	}
	var calls, closed atomic.Int32
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			calls.Add(1)
			return qualitySyntheticResponse(r, true), nil
		}), func() { closed.Add(1) }, nil
	}
	ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	if err := gate(ctx, f.account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		t.Fatal("upload session released account ownership before downstream upload", err)
	}
	if err := gate(ctx, f.account); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || closed.Load() != 2 {
		t.Fatalf("fresh result not reused or transport leaked: calls=%d closed=%d", calls.Load(), closed.Load())
	}
	cleanup()
	cleanup()
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("cleanup leaked its account lease", err)
	}
	lease.Release()
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	afterSecrets, err := f.vault.Load(f.job.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !reflect.DeepEqual(beforeSecrets, afterSecrets) {
		t.Fatal("reasoning probe changed saved authorization or protected secrets")
	}
	rows, _ := c.Service.Snapshots([]string{f.job.ID})
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "synthetic-private") {
		t.Fatal("quality snapshots exposed credentials")
	}
}

func TestAccountQualityGateBindsIncomingAndCurrentGrantIdentity(t *testing.T) {
	for _, scenario := range []string{"incoming-token", "incoming-workspace", "replaced-during-probe"} {
		t.Run(scenario, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			enableQualityFixture(t, c, nil)
			encoded, _ := json.Marshal(f.account)
			var incoming map[string]any
			_ = json.Unmarshal(encoded, &incoming)
			fields := incoming["credentials"].(map[string]any)
			if scenario == "incoming-token" {
				fields["access_token"] = "synthetic-other-access"
			}
			if scenario == "incoming-workspace" {
				fields["chatgpt_account_id"] = "synthetic-other-workspace"
			}
			calls := 0
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					calls++
					f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement-access"
					f.write(t)
					return qualitySyntheticResponse(r, true), nil
				}), func() {}, nil
			}
			ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
			defer cleanup()
			err := gate(ctx, incoming)
			var blocked *sub2api.AuthorizationGateError
			if !errors.As(err, &blocked) || sub2api.IsMutationOutcomeUnknown(err) {
				t.Fatal("mismatched grant did not stop upload safely", err)
			}
			want := 0
			if scenario == "replaced-during-probe" {
				want = 1
			}
			if calls != want {
				t.Fatalf("unexpected probe count %d", calls)
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			if rows[f.job.ID].LastResult != nil {
				t.Fatal("superseded probe published a passing result")
			}
		})
	}
}

func TestAccountQualityCacheTracksGrantAndEffectiveTransport(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	var calls atomic.Int32
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			calls.Add(1)
			return qualitySyntheticResponse(r, true), nil
		}), func() {}, nil
	}
	check := func() {
		ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
		defer cleanup()
		if err := gate(ctx, f.account); err != nil {
			t.Fatal(err)
		}
	}
	check()
	check()
	if calls.Load() != 1 {
		t.Fatal("same committed grant did not reuse its fresh observation")
	}
	f.account["credentials"].(map[string]any)["access_token"] = "synthetic-new-committed-access"
	f.write(t)
	check()
	if calls.Load() != 2 {
		t.Fatal("new committed grant reused old model observation")
	}
	c.cfg.AuthTLSProfile = "chrome131"
	check()
	if calls.Load() != 3 {
		t.Fatal("changed effective TLS identity reused old observation")
	}
	grant := f.load(t)
	defer grant.Release()
	base := c.qualityIdentity(grant)
	copyGrant := *grant
	copyGrant.ProxyOverride = false
	copyGrant.ProxyURL = ""
	c.cfg.ProxyURL = "http://proxy-a.invalid:8080"
	first := c.qualityIdentity(&copyGrant)
	c.cfg.ProxyURL = "http://proxy-b.invalid:8080"
	second := c.qualityIdentity(&copyGrant)
	if first == second || first == base {
		t.Fatal("effective proxy was excluded from model observation identity")
	}
	if c.qualityIdentity(grant) != base {
		t.Fatal("global proxy override changed an explicitly direct grant")
	}
}

func TestAccountQualityGateHonorsCallerCancellationAndConcurrentCleanup(t *testing.T) {
	for _, mode := range []string{"already-canceled", "in-flight-caller", "concurrent-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			enableQualityFixture(t, c, nil)
			started := make(chan struct{})
			var calls, closed atomic.Int32
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(ctx context.Context, _ transport.Request) (*transport.Response, error) {
					calls.Add(1)
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				}), func() { closed.Add(1) }, nil
			}
			_, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
			defer cleanup()
			callCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "already-canceled" {
				cancel()
				if err := gate(callCtx, f.account); !errors.Is(err, context.Canceled) {
					t.Fatal("caller cancellation was ignored", err)
				}
				if calls.Load() != 0 {
					t.Fatal("canceled caller dispatched inference")
				}
				return
			}
			finished := make(chan error, 1)
			go func() { finished <- gate(callCtx, f.account) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("probe did not start")
			}
			if mode == "in-flight-caller" {
				cancel()
			} else {
				var wg sync.WaitGroup
				for range 3 {
					wg.Add(1)
					go func() { defer wg.Done(); cleanup() }()
				}
				done := make(chan struct{})
				go func() { wg.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("concurrent cleanup deadlocked")
				}
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("canceled probe allowed upload")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled probe did not stop")
			}
			cleanup()
			if calls.Load() != 1 || closed.Load() != 1 {
				t.Fatal("cancellation retried inference or leaked transport")
			}
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("canceled session retained the account lease", err)
			}
			lease.Release()
		})
	}
}

func TestAccountQualityWorkflowBorrowsActiveOwnershipWithoutReentry(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	finished := make(chan error, 1)
	f.manager.SetRunner(func(ctx context.Context, j *jobs.Job, _ func(jobs.JobEvent)) error {
		if _, err := f.manager.ReserveAccountQuery(ctx, j.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
			finished <- errors.New("workflow did not own account")
			return nil
		}
		err := c.WorkflowUploadCheck(ctx, j.ID, f.account)
		finished <- err
		return err
	})
	f.manager.Start()
	if _, err := f.manager.QueueOperation(f.job.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("borrowed workflow gate failed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workflow gate recursively waited on its own account ownership")
	}
}

func TestAccountQualityWorkflowPolicyReadHonorsCancellation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			if enabled {
				enableQualityFixture(t, c, nil)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := c.WorkflowUploadCheck(ctx, f.job.ID, f.account); !errors.Is(err, context.Canceled) {
				t.Fatal("workflow policy read ignored caller cancellation", err)
			}
		})
	}
}

func TestAccountQualityCachedPassCannotBypassCurrentGrantExpiry(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	var calls atomic.Int32
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			calls.Add(1)
			return qualitySyntheticResponse(r, true), nil
		}), func() {}, nil
	}
	grant := f.load(t)
	future := time.Now().Add(time.Hour)
	grant.ExpiresAt = &future
	if err := c.checkGrant(t.Context(), f.job.ID, grant, f.account); err != nil {
		t.Fatal("initial valid grant did not pass", err)
	}
	// Move only the transient expiration boundary; the committed identity and
	// fresh cached observation remain unchanged, as when wall time passes expiry.
	past := time.Now().Add(-time.Hour)
	grant.ExpiresAt = &past
	if err := c.checkGrant(t.Context(), f.job.ID, grant, f.account); err == nil {
		t.Fatal("a fresh cached passing score bypassed the current grant's expiry")
	}
	if calls.Load() != 1 {
		t.Fatal("expired authorization dispatched another inference request")
	}
}

func TestAccountQualitySessionSerializesConcurrentChecks(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return qualitySyntheticResponse(r, true), nil
		}), func() {}, nil
	}
	ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	finished := make(chan error, 2)
	go func() { finished <- gate(ctx, f.account) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first check did not start")
	}
	go func() { finished <- gate(ctx, f.account) }()
	close(release)
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal("concurrent callback lost its valid account lease", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent callback deadlocked")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent session callbacks duplicated inference")
	}
}

func TestAccountQualitySessionSupersessionCancelsDownstreamBeforeNewWorkflow(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, c, nil)
	ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	if err := gate(ctx, f.account); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	f.manager.SetRunner(func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { started <- struct{}{}; return nil })
	f.manager.Start()
	if _, err := f.manager.QueueOperation(f.job.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("new account operation did not cancel downstream upload context")
	}
	select {
	case <-started:
		t.Fatal("new workflow started before downstream session cleanup")
	default:
	}
	cleanup()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("session cleanup did not release queued workflow")
	}
}

func TestAccountQualityFailedAndDegradedProbesNeverReachSub2(t *testing.T) {
	for _, scenario := range []string{"bps-forbidden", "network", "incomplete", "degraded"} {
		t.Run(scenario, func(t *testing.T) {
			method := jobs.AuthorizationMethodChatGPT
			if scenario == "bps-forbidden" {
				method = jobs.AuthorizationMethodBPS
			}
			f, c := newQualityControllerFixture(t, method)
			enableQualityFixture(t, c, nil)
			before, _ := os.ReadFile(f.path)
			var probes, remoteCalls atomic.Int32
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					probes.Add(1)
					if scenario == "network" {
						return nil, errors.New("synthetic-private-network-message")
					}
					response := qualitySyntheticResponse(r, scenario != "degraded")
					if scenario == "bps-forbidden" {
						response.StatusCode = http.StatusForbidden
					}
					if scenario == "incomplete" {
						response.Body = []byte("data: [DONE]\n\n")
					}
					return response, nil
				}), func() {}, nil
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				remoteCalls.Add(1)
				http.Error(w, "unexpected upload", 500)
			}))
			defer remote.Close()
			ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
			defer cleanup()
			client := sub2api.New(sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", BeforeAuthorizationSync: gate})
			payload := map[string]any{"accounts": []any{f.account}}
			_, err := client.Upload(ctx, payload)
			if err == nil || remoteCalls.Load() != 0 || sub2api.IsMutationOutcomeUnknown(err) {
				t.Fatal("failed preflight reached Sub2 or lost known non-mutation outcome", err)
			}
			if probes.Load() != 1 {
				t.Fatal("probe request was repeated or skipped")
			}
			after, _ := os.ReadFile(f.path)
			if !bytes.Equal(before, after) {
				t.Fatal("blocked upload removed saved authorization")
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			row := rows[f.job.ID]
			if scenario == "degraded" {
				if row.LastResult == nil || row.LastResult.Verdict != "degraded" {
					t.Fatal("scored failure lost its rule verdict")
				}
			} else if row.LastResult != nil {
				t.Fatal("unsupported or unknown response was turned into a numeric score")
			}
		})
	}
}

func TestSavedAuthorizationUploadRequiresConfiguredGateWhenPolicyIsEnabled(t *testing.T) {
	for _, scenario := range []string{"missing-disabled", "enabled-no-hook", "invalid-policy", "configured-hook"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
			var reads, writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads.Add(1)
					_, _ = io.WriteString(w, `{"data":{"items":[],"pages":1,"total":0}}`)
				} else {
					writes.Add(1)
					_, _ = io.WriteString(w, `{"created":1}`)
				}
			}))
			defer remote.Close()
			job, err := f.manager.QueueOperation(f.job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"}})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "enabled-no-hook" {
				c, err := NewAccountQualityController(config.Config{OutputRoot: f.store.Root}, f.manager)
				if err != nil {
					t.Fatal(err)
				}
				enableQualityFixture(t, c, nil)
			}
			if scenario == "invalid-policy" {
				dir := filepath.Join(f.store.Root, "account-quality")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "configured-hook" {
				f.manager.SetAuthorizationSyncCheck(func(context.Context, string, map[string]any) error {
					return &sub2api.AuthorizationGateError{Reason: "degraded"}
				})
			}
			before, _ := os.ReadFile(f.path)
			err = UploadSavedAuthorizationAtOrigin(t.Context(), f.store.Root, "https://chatgpt.com", f.manager, job, nil)
			if scenario == "missing-disabled" {
				if err != nil || writes.Load() != 1 || reads.Load() == 0 {
					t.Fatal("missing disabled policy broke existing upload", err)
				}
			} else {
				var gate *sub2api.AuthorizationGateError
				if !errors.As(err, &gate) || reads.Load() != 0 || writes.Load() != 0 || sub2api.IsMutationOutcomeUnknown(err) {
					t.Fatal("active or unreadable gate policy was bypassed", err)
				}
			}
			after, _ := os.ReadFile(f.path)
			if !bytes.Equal(before, after) {
				t.Fatal("upload policy fallback changed committed authorization")
			}
		})
	}
}

func TestAccountQualityBPSActiveAndUploadChecksUseNativeSavedGrant(t *testing.T) {
	for _, scenario := range []string{"active-check", "upload-check"} {
		t.Run(scenario, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodBPS)
			enableQualityFixture(t, c, nil)
			before, _ := os.ReadFile(f.path)
			beforeSecrets, err := f.vault.Load(f.job.Email)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var invalidRoute atomic.Bool
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					calls.Add(1)
					creds := f.account["credentials"].(map[string]any)
					if r.URL != "https://bps.openai.com/basispoints/api/responses" || !r.SingleAttempt || r.Headers.Get("Authorization") != "Bearer "+creds["access_token"].(string) || r.Headers.Get("Chatgpt-Account-Id") != creds["chatgpt_account_id"].(string) || r.Headers.Get("X-Basispoints-Auth-Mode") != "chatgpt" {
						invalidRoute.Store(true)
						return nil, errors.New("synthetic incorrect BPS route")
					}
					return qualitySyntheticResponse(r, true), nil
				}), func() {}, nil
			}
			if scenario == "active-check" {
				receipt, err := c.Service.Refresh(t.Context(), []string{f.job.ID}, false)
				if err != nil || len(receipt.Accepted) != 1 || len(receipt.Skipped) != 0 {
					t.Fatal("saved BPS grant was ineligible for an explicit check", err)
				}
				deadline := time.Now().Add(time.Second)
				for {
					rows, _ := c.Service.Snapshots([]string{f.job.ID})
					if rows[f.job.ID].LastResult != nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("BPS explicit check did not publish its result")
					}
					time.Sleep(5 * time.Millisecond)
				}
			} else {
				ctx, check, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
				defer cleanup()
				if err := check(ctx, f.account); err != nil {
					t.Fatal("passing BPS check did not permit the upload gate", err)
				}
				if err := check(ctx, f.account); err != nil {
					t.Fatal("fresh BPS result was not reusable for the same grant", err)
				}
			}
			if calls.Load() != 1 || invalidRoute.Load() {
				t.Fatal("BPS check mixed identities, retried, or bypassed native inference")
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			if result := rows[f.job.ID].LastResult; result == nil || result.Verdict != "normal" {
				t.Fatal("complete BPS answer did not become a scored result")
			}
			after, _ := os.ReadFile(f.path)
			afterSecrets, err := f.vault.Load(f.job.Email)
			if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(beforeSecrets, afterSecrets) {
				t.Fatal("BPS check modified the saved grant or protected secrets")
			}
		})
	}
}
