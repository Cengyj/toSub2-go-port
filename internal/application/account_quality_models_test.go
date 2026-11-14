package application

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func qualityModelResponse(method string) *transport.Response {
	body := `{"models":[{"slug":"gpt-synthetic","supported_reasoning_levels":[{"effort":"high"}]}]}`
	if method == jobs.AuthorizationMethodBPS {
		body = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-synthetic","efforts":[{"value":"high"}]}]}}`
	}
	return &transport.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func requireQualityModelsError(t *testing.T, err error, code string) {
	t.Helper()
	var failure *protocol.AccountModelsError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("wrong catalog failure: want=%s got=%v", code, err)
	}
}

func TestAccountQualityModelsExcludeNonProbeChoices(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			body := `{"models":[{"slug":"gpt-reserve"},{"slug":"gpt-6-chat-thinking"},{"slug":"codex-auto-review"},{"slug":"GPT-RESERVE"},{"slug":"gpt-live"}]}`
			if method == jobs.AuthorizationMethodBPS {
				body = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-reserve"},{"id":"gpt-6-chat-thinking"},{"id":"codex-auto-review"},{"id":"GPT-RESERVE"},{"id":"gpt-live"}]}}`
			}
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
					return &transport.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}, nil
				}), func() {}, nil
			}
			catalog, err := c.AccountModels(t.Context(), f.job.ID)
			if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "gpt-live" {
				t.Fatalf("unexpected probe catalog: %+v, %v", catalog, err)
			}
			for _, model := range catalog.SuggestedModels {
				switch strings.ToLower(model.ID) {
				case "gpt-reserve", "gpt-6-chat-thinking", "codex-auto-review":
					t.Fatalf("non-probe model remained in suggestions: %s", model.ID)
				}
			}
		})
	}
}

func TestProbeModelChoicesFiltersSuggestionsWithoutMutatingInput(t *testing.T) {
	models := []protocol.AccountModel{{ID: "gpt-reserve"}, {ID: "GPT-6-CHAT-THINKING"}, {ID: "codex-auto-review"}, {ID: "gpt-6.1-sol"}, {ID: "gpt-6.1-terra"}}
	before := append([]protocol.AccountModel(nil), models...)
	choices := probeModelChoices(models)
	if len(choices) != 2 || choices[0].ID != "gpt-6.1-sol" || choices[1].ID != "gpt-6.1-terra" || !reflect.DeepEqual(models, before) {
		t.Fatalf("unexpected filtered suggestions: %+v", choices)
	}
}

func TestAccountQualityModelsReadsDisabledPolicyWithoutMutationOrInference(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			beforeGrant, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			beforeSecrets, err := f.vault.Load(f.job.Email)
			if err != nil {
				t.Fatal(err)
			}
			beforeJob, _ := f.manager.Get(f.job.ID)
			beforeSettings, _ := c.Service.Settings()
			beforeRows, _ := c.Service.Snapshots([]string{f.job.ID})
			var calls, closed atomic.Int32
			c.transportFactory = func(proxy string) (protocol.Doer, func(), error) {
				if proxy != "" {
					t.Fatal("selected direct connection was replaced")
				}
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					calls.Add(1)
					if r.Method != http.MethodGet || len(r.Body) != 0 || !r.SingleAttempt || !strings.Contains(r.URL, "models") && !strings.Contains(r.URL, "include_models=true") {
						t.Fatal("catalog lookup dispatched inference or a different endpoint")
					}
					if r.Headers.Get("Authorization") != "Bearer synthetic-private-access" || r.Headers.Get("Chatgpt-Account-Id") != "synthetic-workspace" {
						t.Fatal("catalog lookup borrowed another account")
					}
					if _, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
						t.Fatal("catalog lookup did not hold account lease", err)
					}
					return qualityModelResponse(method), nil
				}), func() { closed.Add(1) }, nil
			}
			for i := 0; i < 2; i++ {
				catalog, err := c.AccountModels(t.Context(), f.job.ID)
				if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "gpt-synthetic" {
					t.Fatal("fresh disabled-policy catalog unavailable", err)
				}
			}
			if calls.Load() != 2 || closed.Load() != 2 {
				t.Fatal("model catalog cached, retried, or leaked transport")
			}
			afterGrant, _ := os.ReadFile(f.path)
			afterSecrets, _ := f.vault.Load(f.job.Email)
			afterJob, _ := f.manager.Get(f.job.ID)
			afterSettings, _ := c.Service.Settings()
			afterRows, _ := c.Service.Snapshots([]string{f.job.ID})
			// A fresh catalog updates display diagnostics, not operation history.
			beforeJob.AccountRequestObservations, afterJob.AccountRequestObservations = nil, nil
			if !bytes.Equal(beforeGrant, afterGrant) || !reflect.DeepEqual(beforeSecrets, afterSecrets) || !reflect.DeepEqual(beforeJob, afterJob) || !reflect.DeepEqual(beforeSettings, afterSettings) || !reflect.DeepEqual(beforeRows, afterRows) {
				t.Fatal("read-only model catalog mutated grant, secrets, account history, settings, or probe results")
			}
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("catalog leaked account lease", err)
			}
			lease.Release()
		})
	}
}

func TestAccountQualityModelsDiscardSupersededGrantAndReleaseOnFailure(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodBPS)
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement-access"
			f.write(t)
			return qualityModelResponse(jobs.AuthorizationMethodBPS), nil
		}), func() {}, nil
	}
	_, err := c.AccountModels(t.Context(), f.job.ID)
	requireQualityModelsError(t, err, "superseded")
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("superseded model read leaked lease", err)
	}
	lease.Release()
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			return &transport.Response{StatusCode: http.StatusForbidden}, errors.New("private upstream body read failure")
		}), func() {}, nil
	}
	_, err = c.AccountModels(t.Context(), f.job.ID)
	requireQualityModelsError(t, err, "forbidden")
	lease, err = f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("failed model read leaked lease", err)
	}
	lease.Release()
}

func TestAccountQualityModelsHonorCancellationAndAccountMutations(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "account-proxy"}[mutate], func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			entered := make(chan struct{})
			var closed atomic.Int32
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(ctx context.Context, _ transport.Request) (*transport.Response, error) {
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				}), func() { closed.Add(1) }, nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.AccountModels(ctx, f.job.ID); done <- err }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("model read not started")
			}
			if mutate {
				if _, err := f.manager.Update(f.job.ID, map[string]any{"proxyUrl": "http://synthetic-proxy.example.test:8080"}); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				requireQualityModelsError(t, err, "canceled")
			case <-time.After(2 * time.Second):
				t.Fatal("canceled model read did not exit")
			}
			if closed.Load() != 1 {
				t.Fatal("canceled model read leaked transport")
			}
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("canceled model read leaked lease", err)
			}
			lease.Release()
		})
	}
}

func TestAccountQualityModelsRejectUnavailableGrantBeforeNetwork(t *testing.T) {
	for _, scenario := range []string{"missing", "unsaved", "expired", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				t.Fatal("unavailable account opened model network transport")
				return nil, nil, nil
			}
			id, code := f.job.ID, "authorization_unavailable"
			switch scenario {
			case "missing":
				id, code = "missing", "not_found"
			case "unsaved":
				f.manager.UpdateResult(f.job.ID, false, "")
			case "expired":
				f.account["credentials"].(map[string]any)["expires_at"] = time.Now().Add(-time.Hour).Unix()
				f.write(t)
				code = "authorization_expired"
			case "busy":
				lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				code = "busy"
			}
			_, err := c.AccountModels(t.Context(), id)
			requireQualityModelsError(t, err, code)
		})
	}
}
