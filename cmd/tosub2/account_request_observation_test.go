package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

func observationFixture(t *testing.T, operation string) (*jobs.Manager, *jobs.Job, *accountOperationObservation) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	j, err := m.Submit(jobs.Spec{ID: "account-observation", Email: "synthetic@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Operation = operation
	return m, j, newAccountOperationObservation(m, nil, j, "https://chatgpt.com", "https://auth.openai.com")
}

func observationBySource(t *testing.T, m *jobs.Manager, id, source string) []jobs.AccountRequestObservation {
	t.Helper()
	j, ok := m.Get(id)
	if !ok {
		t.Fatal("job disappeared")
	}
	var observations []jobs.AccountRequestObservation
	for _, observation := range j.AccountRequestObservations {
		if observation.Source == source {
			observations = append(observations, observation)
		}
	}
	return observations
}

func TestAccountOperationObservationPreservesFailedStatusesAcrossReadback(t *testing.T) {
	for _, operation := range []string{"relogin", "refresh", "reset-2fa", "add-password"} {
		t.Run(operation, func(t *testing.T) {
			m, job, o := observationFixture(t, operation)
			o.Observe("https://chatgpt.com/backend-api/accounts/change?private-token=CANARY", 403)
			o.Observe("https://chatgpt.com/backend-api/accounts/info", 503)
			o.Observe("https://chatgpt.com/backend-api/accounts/info", 200)
			o.Complete(t.Context(), errors.New("private body CANARY with HTTP 401"))
			got := observationBySource(t, m, job.ID, accountOperationSource(operation))
			if len(got) != 2 || got[0].HTTPStatus != 403 || got[1].HTTPStatus != 503 || got[0].Status != "failed" || got[1].Status != "failed" {
				t.Fatal("readback erased a rejected account request", got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "CANARY") || strings.Contains(string(b), "chatgpt.com") {
				t.Fatal("private diagnostic escaped")
			}
		})
	}
}

func TestAccountOperationObservationRetainsLatestResponseOrder(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		statuses, want []int
	}{
		{"newer-403-after-503", []int{503, 403}, []int{503, 403}},
		{"repeated-403-is-latest", []int{403, 503, 403}, []int{503, 403}},
		{"repeated-503-is-latest", []int{503, 403, 503}, []int{403, 503}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, job, observer := observationFixture(t, "refresh")
			for _, status := range scenario.statuses {
				observer.Observe("https://auth.openai.com/oauth/token", status)
			}
			observer.Complete(t.Context(), errors.New("synthetic request failure"))
			var statuses []int
			for _, observation := range observationBySource(t, m, job.ID, "refresh") {
				statuses = append(statuses, observation.HTTPStatus)
			}
			if !reflect.DeepEqual(statuses, scenario.want) {
				t.Fatalf("operation reordered observed statuses: got %v want %v", statuses, scenario.want)
			}
		})
	}
}

func TestAccountOperationObservationKeepsMatchedFailureInResponseOrder(t *testing.T) {
	m, job, observer := observationFixture(t, "refresh")
	parseFailure := &json.SyntaxError{}
	observer.Observe("https://auth.openai.com/oauth/token", 503)
	observer.ObserveResult("https://auth.openai.com/oauth/token", 200, parseFailure)
	observer.Observe("https://auth.openai.com/oauth/token", 403)
	observer.Complete(t.Context(), parseFailure)
	got := observationBySource(t, m, job.ID, "refresh")
	if len(got) != 3 || got[0].HTTPStatus != 503 || got[1].HTTPStatus != 200 || got[2].HTTPStatus != 403 || got[1].ErrorCode != "invalid_json" {
		t.Fatalf("matched parsing failure lost original response order: %+v", got)
	}
}

func TestAccountOperationObservationNeverInventsHTTPOrIncludesExternalServices(t *testing.T) {
	m, job, o := observationFixture(t, "relogin")
	for _, raw := range []string{"https://mail.example.test/request", "https://sms.example.test/request", "https://sub2api.example.test/admin", "https://chatgpt.com.attacker.test/", "https://user:pass@chatgpt.com/", "https://chatgpt.com:8443/", "http://chatgpt.com/"} {
		o.Observe(raw, 403)
	}
	failure := errors.New("HTTP 401 PRIVATE-TOKEN")
	o.ObserveResult("https://auth.openai.com/token", 0, failure)
	o.Complete(t.Context(), failure)
	got := observationBySource(t, m, job.ID, "authentication")
	if len(got) != 1 || got[0].HTTPStatus != 0 || got[0].ErrorCode != "request_failed" {
		t.Fatal("unobserved status became real HTTP", got)
	}
}

func TestAccountOperationObservationUsesTypedHTTPAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"typed", &protocol.HTTPError{StatusCode: 401, Code: "PRIVATE-CANARY"}, 401, "unauthorized"},
		{"deadline", context.DeadlineExceeded, 0, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, job, o := observationFixture(t, "relogin")
			o.ObserveResult("https://auth.openai.com/authorize", tc.status, tc.err)
			o.Complete(t.Context(), tc.err)
			got := observationBySource(t, m, job.ID, "authentication")
			if len(got) != 1 || got[0].HTTPStatus != tc.status || got[0].ErrorCode != tc.code {
				t.Fatal("typed outcome lost", got)
			}
		})
	}
}

func TestAccountOperationObservationIgnoresExternalTypedHTTPError(t *testing.T) {
	for _, external := range []string{"mail", "sms", "sub2api"} {
		t.Run(external, func(t *testing.T) {
			m, job, o := observationFixture(t, "relogin")
			o.Observe("https://auth.openai.com/authorize", 200)
			err := &protocol.HTTPError{StatusCode: 503}
			o.ObserveResult("https://"+external+".example.test/verify", 503, err)
			o.Complete(t.Context(), err)
			got := observationBySource(t, m, job.ID, "authentication")
			if len(got) != 0 {
				t.Fatal("external response became account status", got)
			}
		})
	}
}

func TestAccountOperationObservationLogoutUnconfirmedResponseStatus(t *testing.T) {
	for _, status := range []int{0, 200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, job, o := observationFixture(t, "reset-2fa")
			o.Observe("https://chatgpt.com/backend-api/accounts/logout_all", status)
			o.LogoutResult(map[string]any{"logout_all_devices_status": "unknown"})
			o.Complete(t.Context(), nil)
			got := observationBySource(t, m, job.ID, "logout")
			if len(got) != 1 || got[0].HTTPStatus != status || got[0].Status != "failed" {
				t.Fatal("logout uncertainty lost response status", got)
			}
		})
	}
}

func TestAccountOperationObservationPreservesKnownSessionRejectionWithoutGuessingHTTP(t *testing.T) {
	m, job, o := observationFixture(t, "relogin")
	o.Observe("https://auth.openai.com/log-in", 200)
	o.Complete(t.Context(), fmt.Errorf("CODEX_AUTH_LOGIN_REQUIRED: %w", protocol.ErrSessionExpired))
	got := observationBySource(t, m, job.ID, "authentication")
	if len(got) != 1 || got[0].HTTPStatus != 0 || got[0].ErrorCode != "authorization_expired" {
		t.Fatal("confirmed session rejection lost or became an invented HTTP status", got)
	}
}

func TestAccountOperationObservationReportsResponseParsingAndBusinessFailures(t *testing.T) {
	for _, body := range []string{"{", `{"unexpected":true}`} {
		t.Run(body, func(t *testing.T) {
			m, job, o := observationFixture(t, "refresh")
			client := protocol.NewClient(refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return &transport.Response{StatusCode: 200, Body: []byte(body)}, nil
			}), nil, "auto")
			client.OnRequestResult = o.ObserveResult
			_, err := protocol.RefreshOAuthToken(t.Context(), client, "https://auth.openai.com", "synthetic-client", "synthetic-refresh")
			if err == nil {
				t.Fatal("invalid grant was accepted")
			}
			o.Complete(t.Context(), err)
			got := observationBySource(t, m, job.ID, "refresh")
			if len(got) != 1 || got[0].HTTPStatus != 200 || got[0].Status != "failed" {
				t.Fatal("actual successful HTTP status lost on invalid response", got)
			}
		})
	}
}

func TestAccountOperationObservationRefreshFallbackKeepsSourcesIndependent(t *testing.T) {
	m, job, o := observationFixture(t, "reauthorize")
	o.Observe("https://auth.openai.com/oauth/token", 401)
	o.Switch(t.Context(), "authentication", errors.New("grant rejected"))
	o.Observe("https://auth.openai.com/authorize", 200)
	o.Complete(t.Context(), nil)
	refresh := observationBySource(t, m, job.ID, "refresh")
	auth := observationBySource(t, m, job.ID, "authentication")
	if len(refresh) != 1 || refresh[0].HTTPStatus != 401 || refresh[0].Status != "failed" || len(auth) != 1 || auth[0].Status != "success" {
		t.Fatal("login overwrote refresh outcome", refresh, auth)
	}
}

func TestAccountOperationObservationLogoutDoesNotHideSuccessfulTOTP(t *testing.T) {
	m, job, o := observationFixture(t, "reset-2fa")
	o.Observe("https://chatgpt.com/backend-api/accounts/mfa/user/activate_enrollment", 200)
	o.Observe("https://chatgpt.com/backend-api/accounts/logout_all", 503)
	o.LogoutResult(map[string]any{"logout_all_devices_status": "unknown", "logout_all_devices_error": "PRIVATE-CANARY"})
	o.Complete(t.Context(), nil)
	totp, logout := observationBySource(t, m, job.ID, "totp"), observationBySource(t, m, job.ID, "logout")
	if len(totp) != 1 || totp[0].Status != "success" || len(logout) != 1 || logout[0].HTTPStatus != 503 || logout[0].Status != "failed" {
		t.Fatal("optional logout corrupted TOTP result", totp, logout)
	}
}

func TestAccountOperationObservationCanceledAndLocalSuccessPreserveFailure(t *testing.T) {
	for _, mode := range []string{"canceled", "local"} {
		t.Run(mode, func(t *testing.T) {
			m, job, o := observationFixture(t, "reset-2fa")
			o.Observe("https://chatgpt.com/backend-api/accounts/mfa", 403)
			o.Complete(t.Context(), errors.New("rejected"))
			before := observationBySource(t, m, job.ID, "totp")
			next := newAccountOperationObservation(m, nil, job, "https://chatgpt.com", "https://auth.openai.com")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode != "local" {
				next.Observe("https://chatgpt.com/backend-api/accounts/mfa_info", 200)
			}
			if mode == "canceled" {
				cancel()
			}
			next.Complete(ctx, nil)
			if after := observationBySource(t, m, job.ID, "totp"); !reflect.DeepEqual(before, after) {
				t.Fatal("non-completion cleared earlier failure", after)
			}
		})
	}
}

type failedObservationRecorder struct{ calls int }

func (r *failedObservationRecorder) Complete(...jobs.AccountRequestObservation) error {
	r.calls++
	return errors.New("PRIVATE-PERSISTENCE-CANARY")
}

func TestAccountOperationObservationPersistenceIsBestEffort(t *testing.T) {
	_, _, o := observationFixture(t, "relogin")
	recorder := &failedObservationRecorder{}
	o.current.recorder = recorder
	var logs bytes.Buffer
	o.logger = slog.New(slog.NewTextHandler(&logs, nil))
	o.Observe("https://auth.openai.com/authorize", 200)
	o.Complete(t.Context(), nil)
	if recorder.calls != 1 || strings.Contains(logs.String(), "PRIVATE-PERSISTENCE-CANARY") {
		t.Fatal("display persistence changed or leaked operation outcome", logs.String())
	}
}

func TestAccountOperationObservationPanicNeverClearsEarlierFailure(t *testing.T) {
	m, job, o := observationFixture(t, "relogin")
	o.Observe("https://auth.openai.com/authorize", 403)
	o.Complete(t.Context(), errors.New("rejected"))
	before := observationBySource(t, m, job.ID, "authentication")
	next := newAccountOperationObservation(m, nil, job, "https://chatgpt.com", "https://auth.openai.com")
	var caught any
	func() {
		defer func() { caught = recover() }()
		func() (err error) {
			defer next.CompleteOperation(t.Context(), &err)
			next.Observe("https://auth.openai.com/authorize", 200)
			panic("synthetic-panic")
		}()
	}()
	if caught != "synthetic-panic" || !reflect.DeepEqual(before, observationBySource(t, m, job.ID, "authentication")) {
		t.Fatal("panic changed operation outcome or cleared a failure")
	}
}
func TestProductionAccountOperationObservations(t *testing.T) {
	for _, operation := range []string{"relogin", "refresh", "setup-2fa", "add-password"} {
		t.Run(operation, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.CLI.NativeHTTP = true
			cfg.ProxyURL, cfg.ProxyURLs, cfg.SameProxyRetries = "", nil, 0
			registerRunnerSyntheticProtection(t, cfg.OutputRoot)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden","private":"BODY-CANARY"}`))
			}))
			defer server.Close()
			cfg.ChatGPTBase, cfg.AuthBase = server.URL, server.URL
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			m := jobs.NewManager(t.Context(), 1, st, logger)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			job, err := m.Submit(jobs.Spec{ID: "production-observation", Email: "first@example.test", ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}})
			if err != nil {
				t.Fatal(err)
			}
			job.Operation = operation
			if operation == "refresh" {
				refreshRotationFixture(t, filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"))
			}
			vault := credentials.NewMemoryStore()
			err = makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(t.Context(), job, func(jobs.JobEvent) {})
			if err == nil {
				t.Fatal("rejected request became successful")
			}
			got := observationBySource(t, m, job.ID, accountOperationSource(operation))
			if len(got) != 1 || got[0].HTTPStatus != http.StatusForbidden || got[0].Status != "failed" {
				t.Fatal("production source failed to expose observed status", got, err)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "CANARY") || strings.Contains(logs.String(), "CANARY") {
				t.Fatal("private response leaked")
			}
		})
	}
}
