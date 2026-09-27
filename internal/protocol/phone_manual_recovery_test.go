package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/transport"
)

// The fixture deliberately returns real HTTP errors. Waiting on an input must
// leave the existing number and checkpoint intact without replaying a write.
func TestPhoneHTTPFailureWaitsForManualRecovery(t *testing.T) {
	for _, operation := range []string{"send", "validate", "resend"} {
		for _, failure := range []struct {
			name   string
			status int
		}{
			{"400", 400}, {"401", 401}, {"403", 403}, {"404", 404},
			{"405", 405}, {"408", 408}, {"409", 409}, {"410", 410},
			{"422", 422}, {"429", 429}, {"500", 500}, {"503", 503},
			{"request_timeout", 0}, {"connection_closed", 0},
			{"truncated_body", 0}, {"invalid_json", 0},
		} {
			for _, action := range []string{"retry", "cancel", "change"} {
				t.Run(fmt.Sprintf("%s/%s/%s", operation, failure.name, action), func(t *testing.T) {
					const originalPhone = "+12025550123"
					const changedPhone = "+12025550124"
					var sends, validates atomic.Int32
					var allow atomic.Bool
					failResponse := func(w http.ResponseWriter, r *http.Request) {
						switch failure.name {
						case "request_timeout":
							select {
							case <-r.Context().Done():
							case <-time.After(250 * time.Millisecond):
							}
						case "connection_closed":
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = conn.Close()
						case "truncated_body":
							w.Header().Set("Content-Length", "100")
							_, _ = w.Write([]byte("{}"))
						case "invalid_json":
							_, _ = w.Write([]byte("{invalid"))
						default:
							w.WriteHeader(failure.status)
							_, _ = w.Write([]byte("{\"error\":{\"message\":\"fixture service unavailable\"}}"))
						}
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/add-phone":
							_, _ = w.Write([]byte("{}"))
						case "/api/accounts/add-phone/send":
							n := sends.Add(1)
							if !allow.Load() && (operation == "send" || operation == "resend" && n > 1) {
								failResponse(w, r)
								return
							}
							_, _ = w.Write([]byte("{\"continue_url\":\"/phone-verification\"}"))
						case "/api/accounts/phone-otp/validate":
							validates.Add(1)
							if !allow.Load() && operation == "validate" {
								failResponse(w, r)
								return
							}
							_, _ = w.Write([]byte("{\"verified\":true}"))
						default:
							t.Errorf("unexpected local request: %s", r.URL.Path)
							w.WriteHeader(404)
						}
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					prompts, inputs, done := make(chan string, 4), make(chan string), make(chan error, 1)
					var mu sync.Mutex
					var saved Checkpoint
					deferred := false
					client := NewClient(transport.NewClient(nil), nil, "")
					client.Timeout = 100 * time.Millisecond
					flow := NewFlow(client, FlowOptions{
						AuthBase: server.URL, Phone: originalPhone, Email: "fixture@example.test",
						Input: func(ctx context.Context, stage, _ string) (string, error) {
							prompts <- stage
							select {
							case value := <-inputs:
								return value, nil
							case <-ctx.Done():
								return "", ctx.Err()
							}
						},
						EventData: func(stage, _, _ string, data map[string]any) {
							if data["phoneRetryRequired"] == true && data["currentPhone"] == originalPhone {
								mu.Lock()
								deferred = true
								mu.Unlock()
							}
						},
						SaveCheckpoint: func(c Checkpoint) error {
							b, err := json.Marshal(c)
							if err != nil {
								return err
							}
							mu.Lock()
							defer mu.Unlock()
							return json.Unmarshal(b, &saved)
						},
					})
					go func() {
						_, err := flow.bindPhone(ctx, map[string]any{"addPhoneUrl": server.URL + "/add-phone", "state": "fixture-state"}, false)
						done <- err
					}()
					awaitPrompt := func(want string) {
						t.Helper()
						select {
						case stage := <-prompts:
							if stage != want {
								t.Fatalf("prompt = %s; want %s", stage, want)
							}
						case err := <-done:
							t.Fatalf("flow exited before manual %s input: %v", want, err)
						case <-ctx.Done():
							t.Fatal("timed out waiting for manual input")
						}
					}
					if operation != "send" {
						awaitPrompt("sms_otp")
						if operation == "resend" {
							inputs <- "r"
						} else {
							inputs <- "123456"
						}
					}
					stage := "sms_otp"
					if operation == "send" {
						stage = "phone"
					}
					awaitPrompt(stage)
					mu.Lock()
					checkpoint, sawDeferred := saved, deferred
					mu.Unlock()
					wantStage := "phone_otp"
					if operation == "send" {
						wantStage = "phone_required"
					}
					if !sawDeferred || checkpoint.Stage != wantStage || checkpoint.OAuth["phone"] != originalPhone || checkpoint.OAuth["phoneRetryRequired"] != true || checkpoint.OAuth["state"] != "fixture-state" {
						t.Fatalf("deferred state/checkpoint not retained: deferred=%v checkpoint=%+v", sawDeferred, checkpoint)
					}
					before := [2]int32{sends.Load(), validates.Load()}
					time.Sleep(25 * time.Millisecond)
					if after := [2]int32{sends.Load(), validates.Load()}; after != before {
						t.Fatalf("writes replayed without manual input: %v => %v", before, after)
					}
					if action == "cancel" {
						cancel()
						if err := <-done; !errors.Is(err, context.Canceled) {
							t.Fatalf("cancel result: %v", err)
						}
						if after := [2]int32{sends.Load(), validates.Load()}; after != before {
							t.Fatalf("cancel replayed writes: %v => %v", before, after)
						}
						return
					}
					allow.Store(true)
					wantCounts := before
					if action == "change" {
						if operation != "send" {
							inputs <- "p"
							awaitPrompt("phone")
						}
						inputs <- changedPhone
						wantCounts[0]++
						awaitPrompt("sms_otp")
					} else if operation == "send" {
						inputs <- originalPhone
						wantCounts[0]++
						awaitPrompt("sms_otp")
					} else if operation == "resend" {
						inputs <- "r"
						wantCounts[0]++
						awaitPrompt("sms_otp")
					}
					inputs <- "654321"
					wantCounts[1]++
					if err := <-done; err != nil {
						t.Fatalf("manual recovery failed: %v", err)
					}
					if got := [2]int32{sends.Load(), validates.Load()}; !reflect.DeepEqual(got, wantCounts) {
						t.Fatalf("manual action write count = %v; want %v", got, wantCounts)
					}
				})
			}
		}
	}
}

func TestDeferredPhoneCheckpointResumesWithoutAutomaticWrites(t *testing.T) {
	for _, stage := range []string{"phone_required", "phone_otp"} {
		t.Run(stage, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{}"))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			input := make(chan string, 1)
			var deferred atomic.Bool
			flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{
				AuthBase: server.URL, Email: "fixture@example.test", Phone: "+12025550123",
				Input: func(ctx context.Context, stage, _ string) (string, error) {
					input <- stage
					<-ctx.Done()
					return "", ctx.Err()
				},
				EventData: func(_, _, _ string, data map[string]any) {
					if data["phoneRetryRequired"] == true && data["currentPhone"] == "+12025550123" {
						deferred.Store(true)
					}
				},
			})
			checkpoint := Checkpoint{Version: 1, Stage: stage, Email: flow.Options.Email, AuthBase: server.URL, Cookies: []transport.Cookie{}, OAuth: map[string]any{
				"addPhoneUrl": server.URL + "/add-phone", "state": "fixture-state", "codeVerifier": "fixture-verifier", "phone": "+12025550123", "phoneRetryRequired": true,
			}}
			if stage == "phone_otp" {
				checkpoint.OAuth["phoneReferer"] = server.URL + "/phone-verification"
			}
			if err := flow.Restore(checkpoint); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := flow.ResumeOAuth(ctx, checkpoint); done <- err }()
			want := "phone"
			if stage == "phone_otp" {
				want = "sms_otp"
			}
			select {
			case got := <-input:
				if got != want {
					t.Fatalf("input = %s; want %s", got, want)
				}
			case err := <-done:
				t.Fatalf("resumption exited before manual input: %v", err)
			case <-ctx.Done():
				t.Fatal("manual input not reached")
			}
			time.Sleep(25 * time.Millisecond)
			if writes.Load() != 0 || !deferred.Load() {
				t.Fatalf("restart replayed a phone write or lost pause: writes=%d paused=%v", writes.Load(), deferred.Load())
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel = %v", err)
			}
		})
	}
}

func TestTemporaryPhoneStatusPreservesTerminalSessionErrors(t *testing.T) {
	for _, code := range []string{"invalid_auth_step", "session_expired", "invalid_state", "login_required", ""} {
		for _, status := range []int{429, 503} {
			t.Run(fmt.Sprintf("%s/%d", code, status), func(t *testing.T) {
				var sends, prompts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						sends.Add(1)
						w.WriteHeader(status)
						errorBody := map[string]any{"code": code}
						if code == "" {
							errorBody["message"] = "sign-in session no longer valid"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"error": errorBody})
						return
					}
					_, _ = w.Write([]byte("{}"))
				}))
				defer server.Close()
				flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{
					AuthBase: server.URL, Email: "fixture@example.test", Phone: "+12025550123",
					Input: func(context.Context, string, string) (string, error) {
						prompts.Add(1)
						return "", errors.New("unexpected manual retry")
					},
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := flow.bindPhone(ctx, map[string]any{"addPhoneUrl": server.URL + "/add-phone"}, false)
				var responseError *HTTPError
				if !errors.As(err, &responseError) || responseError.Code != code || sends.Load() != 1 || prompts.Load() != 0 {
					t.Fatalf("terminal session error was retried: err=%v sends=%d prompts=%d", err, sends.Load(), prompts.Load())
				}
			})
		}
	}
}

type phoneManualFailureDoer func(context.Context, transport.Request) (*transport.Response, error)

func (d phoneManualFailureDoer) Do(ctx context.Context, r transport.Request) (*transport.Response, error) {
	return d(ctx, r)
}

// Request-level sentinels do not establish that the task ended. Chromium may
// also have received headers already, so preserve the session and pause even
// when its error chain contains both a network error and a request timeout.
func TestPhoneRequestSentinelsWaitForManualRecovery(t *testing.T) {
	failures := []struct {
		name string
		err  error
	}{
		{"request_deadline", context.DeadlineExceeded},
		{"request_cancel", context.Canceled},
		{"EOF", io.EOF},
		{"unexpected_EOF", io.ErrUnexpectedEOF},
		{"browser_network", browser.ErrNetworkRequest},
		{"response_started", errors.Join(browser.ErrNetworkRequest, browser.ErrResponseStarted, context.DeadlineExceeded)},
		{"transport_request", &transport.TransportError{}},
		{"body_read", errors.New("HTTP response body read failed")},
	}
	for _, operation := range []string{"send", "validate", "resend"} {
		for _, failure := range failures {
			t.Run(operation+"/"+failure.name, func(t *testing.T) {
				const originalPhone = "+12025550123"
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				sends, validations, targets := 0, 0, 0
				doer := phoneManualFailureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					if request.Method == http.MethodPost {
						fail := false
						if pathOf(request.URL) == "/api/accounts/add-phone/send" {
							sends++
							fail = operation == "send" || operation == "resend" && sends > 1
						} else {
							validations++
							fail = operation == "validate"
						}
						if fail {
							targets++
							return nil, failure.err
						}
					}
					return &transport.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte("{}")}, nil
				})
				var saved Checkpoint
				inputCount := 0
				deferred := false
				flow := NewFlow(NewClient(doer, nil, ""), FlowOptions{
					AuthBase: "https://fixture.example.test", Phone: originalPhone,
					EventData: func(_, _, _ string, data map[string]any) {
						if data["phoneRetryRequired"] == true && data["currentPhone"] == originalPhone {
							deferred = true
						}
					},
					SaveCheckpoint: func(c Checkpoint) error { saved = c; return nil },
					Input: func(inputCtx context.Context, stage, _ string) (string, error) {
						inputCount++
						if operation != "send" && inputCount == 1 {
							if operation == "resend" {
								return "r", nil
							}
							return "123456", nil
						}
						wantStage, wantCheckpoint := "sms_otp", "phone_otp"
						if operation == "send" {
							wantStage, wantCheckpoint = "phone", "phone_required"
						}
						if inputCtx.Err() != nil || stage != wantStage || !deferred || targets != 1 ||
							saved.Stage != wantCheckpoint || saved.OAuth["phone"] != originalPhone || saved.OAuth["phoneRetryRequired"] != true {
							t.Errorf("manual request recovery state lost: task=%v stage=%s paused=%v targets=%d checkpoint=%+v", inputCtx.Err(), stage, deferred, targets, saved)
						}
						before := [2]int{sends, validations}
						time.Sleep(25 * time.Millisecond)
						if got := [2]int{sends, validations}; got != before {
							t.Errorf("uncertain request was replayed: %v => %v", before, got)
						}
						cancel()
						return "", inputCtx.Err()
					},
				})
				_, err := flow.bindPhone(ctx, map[string]any{"addPhoneUrl": "https://fixture.example.test/add-phone", "state": "fixture-state"}, false)
				if !errors.Is(err, context.Canceled) || !deferred || targets != 1 {
					t.Fatalf("request error ended task before manual input: err=%v paused=%v targets=%d", err, deferred, targets)
				}
			})
		}
	}
}
