package sub2api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadCreationNeedsConfirmedOutcome(t *testing.T) {
	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{invalid fixture-private-value`, `{"data":null}`, `{"created":"invalid"}`,
		`{"created":1.5}`, `{"created":-1}`, `{"created":1,"failed":"invalid"}`,
		`{"created":1,"success_count":0}`, `{"data":{"message":"fixture-private-message"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"data":{"items":[],"pages":0,"total":0}}`)
					return
				}
				writes.Add(1)
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
			got := result.(map[string]any)
			if err == nil || writes.Load() != 1 || got["ok"] != false || got["created"] != float64(0) || got["success"] != float64(0) {
				t.Fatalf("unconfirmed creation reported success or replayed: writes=%d result=%v error=%v", writes.Load(), got, err)
			}
			if !errors.Is(err, ErrMutationOutcomeUnknown) || !IsMutationOutcomeUnknown(err) || got["unknown"] != float64(1) || got["failed"] != float64(0) {
				t.Fatalf("unknown result lost its classification: %v %v", got, err)
			}
		})
	}
}

func TestUploadUpdateBusinessRejectionStopsBeforeCreation(t *testing.T) {
	for _, body := range []string{`{"ok":false}`, `{"success":false}`, `{"data":{"ok":false}}`, `{"ok":true,"data":{"success":false}}`} {
		t.Run(body, func(t *testing.T) {
			var updates, creates atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/admin/accounts":
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
				case r.Method == http.MethodGet:
					_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}}`)
				case r.Method == http.MethodPut:
					updates.Add(1)
					_, _ = fmt.Fprint(w, body)
				case r.Method == http.MethodPost:
					creates.Add(1)
					_, _ = fmt.Fprint(w, `{"created":1}`)
				}
			}))
			defer server.Close()
			result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access"), uploadFixtureAccount("new@example.test", "fixture-access")))
			got := result.(map[string]any)
			if err == nil || updates.Load() != 1 || creates.Load() != 0 || got["ok"] != false || got["updated"] != float64(0) || got["success"] != float64(0) {
				t.Fatalf("business rejection reported success or continued upload: updates=%d creates=%d result=%v error=%v", updates.Load(), creates.Load(), got, err)
			}
			if !errors.Is(err, ErrMutationRejected) || IsMutationOutcomeUnknown(err) || got["failed"] != float64(1) || got["not_attempted"] != float64(1) {
				t.Fatalf("business refusal lost failed or pending counts: %v %v", got, err)
			}
		})
	}
}

func TestUploadCreationOutcomeCompatibilityAndPartialCounts(t *testing.T) {
	for _, tc := range []struct {
		body                     string
		created, failed, unknown int
		want                     error
	}{
		{`{"ok":true}`, 2, 0, 0, nil},
		{`{"success":true,"data":null}`, 2, 0, 0, nil},
		{`{"data":{"success":2,"failed":0,"ids":[1,2]}}`, 2, 0, 0, nil},
		{`{"created":2,"data":{"created":2}}`, 2, 0, 0, nil},
		{`{"ok":true,"failed":1}`, 1, 1, 0, ErrMutationRejected},
		{`{"ok":false}`, 0, 2, 0, ErrMutationRejected},
		{`{"data":{"created":1,"failed":1}}`, 1, 1, 0, ErrMutationRejected},
		{`{"ok":false,"created":1,"failed":1}`, 1, 1, 0, ErrMutationRejected},
		{`{"ok":false,"failed":1}`, 0, 1, 1, ErrMutationOutcomeUnknown},
		{`{"created":1,"failed":0}`, 1, 0, 1, ErrMutationOutcomeUnknown},
		{`{"created":1}`, 1, 0, 1, ErrMutationOutcomeUnknown},
		{`{"created":2,"failed":1}`, 0, 0, 2, ErrMutationOutcomeUnknown},
		{`{"ok":false,"created":2}`, 0, 0, 2, ErrMutationOutcomeUnknown},
		{`{"created":2,"data":{"created":1}}`, 0, 0, 2, ErrMutationOutcomeUnknown},
		{`{"created":2,"ids":[1,1]}`, 0, 0, 2, ErrMutationOutcomeUnknown},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.body), &value); err != nil {
				t.Fatal(err)
			}
			got, err := uploadCreationResult(value, 2)
			if got.created != tc.created || got.failed != tc.failed || got.unknown != tc.unknown || !errors.Is(err, tc.want) {
				t.Fatalf("counts=%+v err=%v", got, err)
			}
		})
	}
}

func TestUploadFailureBeforeMutationIsKnownUnchanged(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
	if !IsRemoteStatus(err, http.StatusServiceUnavailable) || IsMutationOutcomeUnknown(err) || writes.Load() != 0 {
		t.Fatalf("lookup failure became an unknown write: writes=%d err=%v", writes.Load(), err)
	}
}

func TestMutationOutcomeClassificationDoesNotHideJoinedUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"refused", fmt.Errorf("upload: %w", ErrMutationRejected), false},
		{"unknown", ErrMutationOutcomeUnknown, true},
		{"4xx", &RemoteError{StatusCode: 403}, false},
		{"5xx", &RemoteError{StatusCode: 503}, true},
		{"lookup-5xx", &uploadPreparationError{cause: &RemoteError{StatusCode: 503}}, false},
		{"joined-known", errors.Join(ErrMutationRejected, &RemoteError{StatusCode: 403}), false},
		{"joined-unknown", errors.Join(ErrMutationRejected, ErrMutationOutcomeUnknown), true},
		{"joined-read-and-write", errors.Join(&uploadPreparationError{cause: &RemoteError{StatusCode: 503}}, &RemoteError{StatusCode: 503}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMutationOutcomeUnknown(tc.err); got != tc.want {
				t.Fatalf("unknown=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestUploadPreservesCompletedUpdateBeforeUnknownCreation(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/admin/accounts":
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
		case r.Method == http.MethodGet:
			_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}}`)
		case r.Method == http.MethodPut:
			writes.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		case r.Method == http.MethodPost:
			writes.Add(1)
			_, _ = fmt.Fprint(w, `{"data":{"access_token":"fixture-private-value"}}`)
		}
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access"), uploadFixtureAccount("new@example.test", "fixture-access")))
	public := PublicUploadResult(result)
	if !errors.Is(err, ErrMutationOutcomeUnknown) || writes.Load() != 2 || public["updated"] != float64(1) || public["success"] != float64(1) || public["unknown"] != float64(1) || public["not_attempted"] != float64(0) {
		t.Fatalf("partial completion lost: %v %v", public, err)
	}
	encoded, _ := json.Marshal(public)
	if strings.Contains(string(encoded)+SafeError(err), "fixture-private") {
		t.Fatal("unknown upload leaked remote response")
	}
}

func TestSyncAccountCredentialsStopsOnBusinessRejection(t *testing.T) {
	for _, step := range []string{"update", "clear-error", "schedulable"} {
		t.Run(step, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"data":{"id":12,"credentials":{"email":"owner@example.test"}}}`)
					return
				}
				writes.Add(1)
				if step == "update" && r.Method == http.MethodPut || strings.HasSuffix(r.URL.Path, "/"+step) {
					_, _ = fmt.Fprint(w, `{"success":false,"message":"fixture-private-value"}`)
					return
				}
				_, _ = fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).SyncAccountCredentials(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "fixture-access"}, "session")
			want := map[string]int32{"update": 1, "clear-error": 2, "schedulable": 3}[step]
			if !errors.Is(err, ErrMutationRejected) || IsMutationOutcomeUnknown(err) || writes.Load() != want || strings.Contains(SafeError(err), "fixture-private") {
				t.Fatalf("rejected lifecycle step continued or lost classification: writes=%d err=%v", writes.Load(), err)
			}
		})
	}
}

func FuzzUploadCreationOutcome(f *testing.F) {
	for _, seed := range []string{`null`, `{"ok":true}`, `{"success":false}`, `{"created":1,"failed":1}`, `{"data":{"success":2,"ids":[1,2]}}`, `{"created":1.5}`, `{"data":{"created":[]}}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var value any
		if json.Unmarshal([]byte(input), &value) != nil {
			return
		}
		got, err := uploadCreationResult(value, 2)
		if got.created < 0 || got.failed < 0 || got.unknown < 0 || got.created+got.failed+got.unknown != 2 || len(got.ids) > got.created {
			t.Fatalf("outcome counters lost their bounds: %+v", got)
		}
		if err == nil && (got.created != 2 || got.failed != 0 || got.unknown != 0) {
			t.Fatalf("partial outcome reported success: %+v", got)
		}
	})
}

func TestSyncAccountCredentialsKeepsUnknownLifecycleResultVisible(t *testing.T) {
	for _, step := range []string{"update", "clear-error", "schedulable"} {
		for _, response := range []string{"server-error", "invalid-json", "truncated"} {
			t.Run(step+"/"+response, func(t *testing.T) {
				var writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						_, _ = fmt.Fprint(w, `{"data":{"id":12,"credentials":{"email":"owner@example.test"}}}`)
						return
					}
					writes.Add(1)
					if step == "update" && r.Method == http.MethodPut || strings.HasSuffix(r.URL.Path, "/"+step) {
						switch response {
						case "server-error":
							w.WriteHeader(http.StatusServiceUnavailable)
						case "truncated":
							w.Header().Set("Content-Length", "1000")
						}
						_, _ = fmt.Fprint(w, "invalid fixture-private-value")
						return
					}
					_, _ = fmt.Fprint(w, `{}`)
				}))
				defer server.Close()
				err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).SyncAccountCredentials(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "fixture-access"}, "session")
				want := map[string]int32{"update": 1, "clear-error": 2, "schedulable": 3}[step]
				if !errors.Is(err, ErrMutationOutcomeUnknown) || !IsMutationOutcomeUnknown(err) || writes.Load() != want || SafeError(err) != ErrMutationOutcomeUnknown.Error() || strings.Contains(err.Error(), "fixture-private") {
					t.Fatalf("unknown lifecycle result was hidden, replayed or leaked: writes=%d err=%v", writes.Load(), err)
				}
				if response == "server-error" && !IsRemoteStatus(err, http.StatusServiceUnavailable) {
					t.Fatal("unknown result lost the HTTP status")
				}
			})
		}
	}
}
