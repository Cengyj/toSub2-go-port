package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestUploadFreshAuthorizationRecoversErrorAccount(t *testing.T) {
	for _, clientID := range []string{protocol.DefaultCodexClientID, protocol.DefaultBPSClientID} {
		t.Run(clientID, func(t *testing.T) {
			remote := uploadFixtureAccount("owner@example.test", "fixture-old-access")
			remote["id"], remote["status"], remote["schedulable"] = 12, "error", false
			remote["error_message"] = "upstream returned 401"
			local := uploadFixtureAccount("owner@example.test", "fixture-new-access")
			local["credentials"].(map[string]any)["client_id"] = clientID
			steps := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				steps = append(steps, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "total": 1, "pages": 1}})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
					writeUploadFixture(t, w, map[string]any{"data": remote})
				case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body) != 2 || body["credentials"].(map[string]any)["client_id"] != clientID || body["extra"].(map[string]any)["openai_request_timezone"] != DefaultOpenAIRequestTimezone {
						t.Error("recovery changed the authorization merge contract")
					}
					writeUploadFixture(t, w, map[string]any{"ok": true})
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/12/clear-error":
					writeUploadFixture(t, w, map[string]any{"ok": true})
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/12/schedulable":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["schedulable"] != true {
						t.Error("error account was not restored to scheduling")
					}
					writeUploadFixture(t, w, map[string]any{"ok": true})
				default:
					t.Errorf("unexpected upload request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(t.Context(), uploadFixturePayload(local))
			want := []string{"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts/12", "PUT /api/v1/admin/accounts/12", "POST /api/v1/admin/accounts/12/clear-error", "POST /api/v1/admin/accounts/12/schedulable"}
			if err != nil || !reflect.DeepEqual(steps, want) {
				t.Fatalf("credential upload did not recover remote error state: steps=%v err=%v", steps, err)
			}
			public := PublicUploadResult(result)
			if public["updated"] != float64(1) || public["success"] != float64(1) || !reflect.DeepEqual(public["account_ids"], []int64{12}) {
				t.Fatalf("completed recovery lost its outcome: %v", public)
			}
		})
	}
}

func TestUploadRecoveryUsesDetailStatusAndPreservesAdministrativeChoices(t *testing.T) {
	for _, tc := range []struct {
		listStatus, detailStatus string
		wantRecovery             bool
	}{
		{"error", "disabled", false},
		{"error", "active", false},
		{"error", "", false},
		{"error", "expired", false},
		{"active", "error", true},
	} {
		t.Run(tc.listStatus+"-to-"+tc.detailStatus, func(t *testing.T) {
			remote := uploadFixtureAccount("owner@example.test", "fixture-old-access")
			remote["id"], remote["status"], remote["schedulable"] = 12, tc.listStatus, false
			detail := cloneMap(remote)
			detail["status"] = tc.detailStatus
			var updates, lifecycle atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "total": 1, "pages": 1}})
				case r.Method == http.MethodGet:
					writeUploadFixture(t, w, map[string]any{"data": detail})
				case r.Method == http.MethodPut:
					updates.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body) != 2 || body["credentials"] == nil || body["extra"] == nil {
						t.Error("credential update attempted to overwrite lifecycle settings")
					}
					writeUploadFixture(t, w, map[string]any{"ok": true})
				case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/clear-error") || strings.HasSuffix(r.URL.Path, "/schedulable")):
					lifecycle.Add(1)
					writeUploadFixture(t, w, map[string]any{"ok": true})
				default:
					t.Error("existing account was not updated in place")
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-new-access")))
			want := int32(0)
			if tc.wantRecovery {
				want = 2
			}
			if err != nil || updates.Load() != 1 || lifecycle.Load() != want {
				t.Fatalf("detail status policy failed: updates=%d lifecycle=%d err=%v", updates.Load(), lifecycle.Load(), err)
			}
		})
	}
}

func TestUploadRecoveryFailurePreservesUpdatedGrantAndStopsBatch(t *testing.T) {
	for _, step := range []string{"clear-error", "schedulable"} {
		for _, response := range []string{"rejected", "server-error", "invalid-json", "truncated", "lost-response"} {
			t.Run(step+"/"+response, func(t *testing.T) {
				remote := uploadFixtureAccount("owner@example.test", "fixture-old-access")
				remote["id"], remote["status"], remote["schedulable"] = 12, "error", false
				var writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
						writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "total": 1, "pages": 1}})
					case r.Method == http.MethodGet:
						writeUploadFixture(t, w, map[string]any{"data": remote})
					default:
						writes.Add(1)
						if strings.HasSuffix(r.URL.Path, "/batch") {
							t.Error("a failed recovery continued to account creation")
						}
						if strings.HasSuffix(r.URL.Path, "/"+step) {
							switch response {
							case "rejected":
								writeUploadFixture(t, w, map[string]any{"success": false, "message": "fixture-private-value"})
							case "server-error":
								w.WriteHeader(http.StatusServiceUnavailable)
							case "invalid-json":
								_, _ = fmt.Fprint(w, "invalid fixture-private-value")
							case "truncated":
								w.Header().Set("Content-Length", "1000")
								_, _ = fmt.Fprint(w, "fixture-private-value")
							case "lost-response":
								conn, _, err := w.(http.Hijacker).Hijack()
								if err != nil {
									t.Error(err)
									return
								}
								_ = conn.Close()
							}
							return
						}
						writeUploadFixture(t, w, map[string]any{"ok": true})
					}
				}))
				defer server.Close()
				result, err := New(Config{BaseURL: server.URL, APIKey: "fixture"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-new-access"), uploadFixtureAccount("new@example.test", "fixture-new-access")))
				public := PublicUploadResult(result)
				wantWrites := int32(2)
				wantStep := "清除错误状态"
				if step == "schedulable" {
					wantWrites, wantStep = 3, "恢复调度"
				}
				unknown := response != "rejected"
				if err == nil || IsMutationOutcomeUnknown(err) != unknown || writes.Load() != wantWrites || public["updated"] != float64(1) || public["success"] != float64(0) || public["not_attempted"] != float64(1) || public["ok"] != false || !reflect.DeepEqual(public["account_ids"], []int64{12}) {
					t.Fatalf("recovery failure lost progress or was replayed: writes=%d result=%v err=%v", writes.Load(), public, err)
				}
				if unknown && (public["unknown"] != float64(1) || public["failed"] != float64(0) || !errors.Is(err, ErrMutationOutcomeUnknown)) || !unknown && (public["failed"] != float64(1) || public["unknown"] != float64(0) || !errors.Is(err, ErrMutationRejected)) {
					t.Fatalf("recovery classification lost its cause: result=%v err=%v", public, err)
				}
				safe := SafeError(err)
				encoded, _ := json.Marshal(public)
				if !strings.Contains(safe, "授权已更新") || !strings.Contains(safe, wantStep) || strings.Contains(string(encoded)+safe+err.Error(), "fixture-private") {
					t.Fatalf("recovery explanation lost its stage or exposed a remote response: %s", safe)
				}
			})
		}
	}
}

type uploadRecoveryRoundTrip func(*http.Request) (*http.Response, error)

func (f uploadRecoveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type uploadRecoveryCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b uploadRecoveryCancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestUploadCancellationBetweenRecoveryStepsPreservesConfirmedWrites(t *testing.T) {
	for _, after := range []string{"update", "clear-error"} {
		t.Run(after, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := New(Config{BaseURL: "https://sub2.example.test", APIKey: "fixture"})
			writes := 0
			client.http.Transport = uploadRecoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true}`
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
					body = `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`
				} else if r.Method == http.MethodGet {
					body = `{"data":{"id":12,"platform":"openai","type":"oauth","status":"error","schedulable":false,"credentials":{"email":"owner@example.test"}}}`
				} else {
					writes++
				}
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
				if after == "update" && r.Method == http.MethodPut || strings.HasSuffix(r.URL.Path, "/"+after) {
					// The response is completely read and validated before Close;
					// cancel exactly between confirmed and not-yet-started writes.
					response.Body = uploadRecoveryCancelBody{ReadCloser: response.Body, cancel: cancel}
				}
				return response, nil
			})
			result, err := client.Upload(ctx, uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-new-access"), uploadFixtureAccount("new@example.test", "fixture-new-access")))
			public := PublicUploadResult(result)
			wantWrites := map[string]int{"update": 1, "clear-error": 2}[after]
			if !errors.Is(err, context.Canceled) || IsMutationOutcomeUnknown(err) || writes != wantWrites || public["updated"] != float64(1) || public["success"] != float64(0) || public["failed"] != float64(1) || public["unknown"] != float64(0) || public["not_attempted"] != float64(1) || !reflect.DeepEqual(public["account_ids"], []int64{12}) {
				t.Fatalf("canceled recovery lost confirmed progress or started another write: writes=%d result=%v err=%v", writes, public, err)
			}
		})
	}
}
