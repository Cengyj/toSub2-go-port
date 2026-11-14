package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

type fixtureDoer func(context.Context, transport.Request) (*transport.Response, error)

func (f fixtureDoer) Do(ctx context.Context, request transport.Request) (*transport.Response, error) {
	return f(ctx, request)
}

func testCredentials() protocol.InsightCredentials {
	return protocol.InsightCredentials{AccessToken: "synthetic-access-secret", AccountID: "synthetic-account", AuthorizationMethod: "chatgpt", Email: "fixture@example.invalid"}
}

func testAnswers() string {
	return "21"
}

func messageOutput(text string) map[string]any {
	return map[string]any{"id": "synthetic-message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func completedFixture(text string) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "synthetic-response", "object": "response", "status": "completed", "model": "synthetic-model",
		"output": []any{messageOutput(text)}, "error": nil, "incomplete_details": nil,
	}}
}

func eventBytes(event any) []byte {
	data, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return []byte("data: " + string(data) + "\n\n")
}

func successfulResponse(text string) *transport.Response {
	u, _ := url.Parse(codexResponsesURL)
	return &transport.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: eventBytes(completedFixture(text)), URL: u}
}

func requireProbeError(t *testing.T, result Result, err error, code string) *Error {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error = %v; want code %s", err, code)
	}
	if result != (Result{}) {
		t.Fatalf("unscored failure returned a result: %#v", result)
	}
	return failure
}

func TestProbeSingleAttemptRequestAndResultContract(t *testing.T) {
	calls := 0
	client := &Client{Transport: fixtureDoer(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		if request.URL != codexResponsesURL || request.Method != http.MethodPost || !request.SingleAttempt || request.RetryReadOnly || request.RetryBeforeSend || request.MaxResponseBytes != maxResponseBytes {
			t.Fatal("inference request violated the closed single-attempt transport contract")
		}
		if request.Headers.Get("Authorization") != "Bearer "+testCredentials().AccessToken || request.Headers.Get("Chatgpt-Account-Id") != testCredentials().AccountID || request.Headers.Get("Accept") != "text/event-stream" {
			t.Fatal("request did not retain the selected grant identity")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute || request.Timeout <= 0 || request.Timeout > time.Minute {
			t.Fatal("inference has no bounded cancellation budget")
		}
		var body map[string]any
		if json.Unmarshal(request.Body, &body) != nil || body["model"] != "synthetic-model" || body["stream"] != true || body["store"] != false || body["tool_choice"] != "none" || body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Fatal("request payload does not preserve explicit model and reasoning choice")
		}
		if strings.Contains(string(request.Body), testCredentials().AccessToken) || strings.Contains(string(request.Body), testCredentials().Email) || len(body["tools"].([]any)) != 0 {
			t.Fatal("synthetic prompt contains credentials or enables tools")
		}
		return successfulResponse(testAnswers()), nil
	})}
	result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model", ReasoningEffort: "high"})
	if err != nil || calls != 1 || result.Score != 1 || result.Total != 1 || result.Verdict != "normal" || result.Model != "synthetic-model" || result.RuleVersion != RuleVersion || result.CheckedAt.IsZero() || result.DurationMS < 0 {
		t.Fatalf("result=%#v error=%v calls=%d", result, err, calls)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "fixture@example") {
		t.Fatal("result leaked credential material")
	}
}

func TestProbeScoresOnlyCompleteValidAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, answer, verdict string
		minimum, score        int
	}{
		{"default-pass", testAnswers(), "normal", 0, 1},
		{"explicit-pass", " 21\n", "normal", 1, 1},
		{"common-wrong-answer", "29", "degraded", 0, 0},
		{"not-a-substring", "129", "degraded", 1, 0},
		{"valid-zero", "0", "degraded", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return successfulResponse(tc.answer), nil
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model", MinimumScore: tc.minimum})
			if err != nil || result.Score != tc.score || result.Verdict != tc.verdict {
				t.Fatalf("score result=%#v error=%v", result, err)
			}
		})
	}
}

func TestProbeRejectsUnsupportedIdentityAndUnsafeInputsBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*protocol.InsightCredentials, *Options)
	}{
		{"bps-missing-token", "invalid_credentials", func(c *protocol.InsightCredentials, _ *Options) { c.AuthorizationMethod, c.AccessToken = "bps", "" }},
		{"unknown-method", "invalid_credentials", func(c *protocol.InsightCredentials, _ *Options) { c.AuthorizationMethod = "unknown" }},
		{"missing-token", "invalid_credentials", func(c *protocol.InsightCredentials, _ *Options) { c.AccessToken = "" }},
		{"missing-account", "invalid_credentials", func(c *protocol.InsightCredentials, _ *Options) { c.AccountID = "" }},
		{"header-injection", "invalid_credentials", func(c *protocol.InsightCredentials, _ *Options) { c.AccessToken = "synthetic\r\nX-Test: injected" }},
		{"missing-model", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.Model = "" }},
		{"model-as-url", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.Model = "https://evil.invalid/" }},
		{"model-injection", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.Model = "model\nsecret" }},
		{"bad-effort", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.ReasoningEffort = "unbounded" }},
		{"bad-score", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.MinimumScore = 2 }},
		{"bad-timeout", "invalid_options", func(_ *protocol.InsightCredentials, o *Options) { o.TimeoutSeconds = MaxTimeoutSeconds + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })}
			creds, opts := testCredentials(), Options{Model: "synthetic-model"}
			tc.change(&creds, &opts)
			result, err := client.Probe(t.Context(), creds, opts)
			requireProbeError(t, result, err, tc.code)
			if calls != 0 {
				t.Fatal("invalid or unsupported input reached the inference transport")
			}
		})
	}
	for _, client := range []*Client{nil, {}, {Transport: &protocol.Client{}}} {
		result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
		if err == nil || result != (Result{}) {
			t.Fatal("unsafe or absent transport was accepted")
		}
	}
}

func TestProbeNeverRetriesFailuresOrEchoesUpstreamSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		err        error
	}{
		{"disconnect", "network_error", 0, errors.New("Authorization: Bearer synthetic-secret https://evil.invalid/?token=private")},
		{"server-error", "http_error", 503, nil},
		{"rate-limit", "rate_limited", 429, nil},
		{"unauthorized", "unauthorized", 401, nil},
		{"forbidden", "forbidden", 403, nil},
		{"redirect", "redirect_blocked", 307, nil},
		{"oversize-error", "response_too_large", 200, transport.ErrResponseTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return &transport.Response{StatusCode: tc.status, Header: http.Header{"Location": {"https://evil.invalid/"}}, Body: []byte("synthetic-secret token=private")}, tc.err
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			failure := requireProbeError(t, result, err, tc.code)
			if calls != 1 || failure.HTTPStatus != tc.status || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "evil.invalid") {
				t.Fatalf("failure leaked raw data or retried: calls=%d failure=%#v", calls, failure)
			}
		})
	}
	if strings.Contains((&Error{Code: "synthetic-secret"}).Error(), "synthetic-secret") {
		t.Fatal("unknown error category leaked arbitrary caller data")
	}
}

func TestProbeRejectsResponseSubstitutionAndOversizeBodies(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*transport.Response)
	}{
		{"redirect-followed", "redirect_blocked", func(r *transport.Response) { r.URL, _ = url.Parse("https://evil.invalid/responses") }},
		{"oversize", "response_too_large", func(r *transport.Response) { r.Body = make([]byte, maxResponseBytes+1) }},
		{"html", "unexpected_content_type", func(r *transport.Response) {
			r.Header.Set("Content-Type", "text/html")
			r.Body = []byte("<html>synthetic challenge</html>")
		}},
		{"json-not-sse", "unexpected_content_type", func(r *transport.Response) { r.Header.Set("Content-Type", "application/json") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				response := successfulResponse(testAnswers())
				tc.change(response)
				return response, nil
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			requireProbeError(t, result, err, tc.code)
			if calls != 1 {
				t.Fatal("unexpected second inference request")
			}
		})
	}
}

func TestProbeCancellationAndDeadlineStayUnscored(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })}
	result, err := client.Probe(ctx, testCredentials(), Options{Model: "synthetic-model"})
	requireProbeError(t, result, err, "canceled")
	if calls != 0 {
		t.Fatal("already canceled operation was dispatched")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	client.Transport = fixtureDoer(func(ctx context.Context, _ transport.Request) (*transport.Response, error) {
		cancel()
		<-ctx.Done()
		return successfulResponse(testAnswers()), nil
	})
	result, err = client.Probe(ctx, testCredentials(), Options{Model: "synthetic-model"})
	requireProbeError(t, result, err, "canceled")
	client.Transport = fixtureDoer(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || request.Timeout > time.Second {
			t.Fatal("configured timeout was not applied to context and transport")
		}
		return nil, context.DeadlineExceeded
	})
	result, err = client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model", TimeoutSeconds: 1})
	requireProbeError(t, result, err, "timeout")
}

func TestUnspecifiedCandyReasoningEffortHasNoHiddenOverride(t *testing.T) {
	calls := 0
	client := &Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		var payload map[string]any
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["reasoning"]; exists {
			t.Error("unspecified reasoning effort became a hidden override")
		}
		return successfulResponse(testAnswers()), nil
	})}
	result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
	if err != nil || result.Verdict != "normal" || calls != 1 {
		t.Fatalf("probe result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestProbeBPSUsesNativeTextRequestWithSelectedGrant(t *testing.T) {
	creds := testCredentials()
	creds.AuthorizationMethod = protocol.AuthorizationMethodBPS
	calls := 0
	client := &Client{Transport: fixtureDoer(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		if request.URL != bpsResponsesURL || request.Method != http.MethodPost || !request.SingleAttempt || request.RetryReadOnly || request.RetryBeforeSend || request.MaxResponseBytes != maxResponseBytes {
			t.Fatal("BPS inference left its closed single-attempt route")
		}
		for key, want := range map[string]string{
			"Authorization": "Bearer " + creds.AccessToken, "Chatgpt-Account-Id": creds.AccountID,
			"X-Openai-Account-Id": creds.AccountID, "X-Basispoints-Auth-Mode": "chatgpt",
			"Origin": "https://bps.openai.com", "Accept": "text/event-stream",
			"X-Openai-Internal-Basispoints-Client-Product":       "basispoints-excel-plugin",
			"X-Openai-Internal-Basispoints-Client-Agent-Profile": "excel",
		} {
			if request.Headers.Get(key) != want {
				t.Fatalf("BPS selected grant header %s is missing", key)
			}
		}
		if request.Headers.Get("Originator") != "" || request.Headers.Get("Cookie") != "" || request.Headers.Get("X-Codex-Turn-State") != "" {
			t.Fatal("Codex identity leaked onto the BPS host")
		}
		var body map[string]any
		if json.Unmarshal(request.Body, &body) != nil || len(body) != 8 || body["model"] != "synthetic-model" || body["model_selection"] != "explicit" || body["reasoning_effort"] != "xhigh" || body["stream"] != true || body["store"] != false {
			t.Fatal("BPS native schema or requested model was changed")
		}
		for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls", "instructions", "reasoning", "max_output_tokens"} {
			if _, exists := body[key]; exists {
				t.Fatalf("unsupported Responses field %s was sent to BPS", key)
			}
		}
		input := body["input"].([]any)
		if len(input) != 3 || input[0].(map[string]any)["role"] != "developer" || input[1].(map[string]any)["role"] != "developer" || input[2].(map[string]any)["role"] != "user" || input[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != taskPrompt {
			t.Fatal("BPS prompt did not retain the native developer/user message layout")
		}
		metadata := body["metadata"].(map[string]any)
		if len(metadata["task_id"].(string)) != 32 || len(metadata["turn_id"].(string)) != 32 || metadata["task_id"] == metadata["turn_id"] || metadata["agent_iteration"] != "1" {
			t.Fatal("BPS per-request conversation identifiers are not independent")
		}
		if strings.Contains(string(request.Body), creds.AccessToken) || strings.Contains(string(request.Body), creds.Email) || strings.Contains(string(request.Body), creds.AccountID) {
			t.Fatal("BPS prompt leaked selected grant secrets or identity")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute || request.Timeout <= 0 {
			t.Fatal("BPS inference lost its deadline")
		}
		response := successfulResponse(testAnswers())
		response.URL, _ = url.Parse(bpsResponsesURL)
		return response, nil
	})}
	result, err := client.Probe(t.Context(), creds, Options{Model: "synthetic-model", ReasoningEffort: "ultra"})
	if err != nil || calls != 1 || result.Score != TotalQuestions || result.Total != TotalQuestions || result.Verdict != "normal" {
		t.Fatalf("BPS scored result=%#v error=%v calls=%d", result, err, calls)
	}
}

func TestProbeBPSFailuresStayUnscoredWithoutRetryOrCrossRouteFallback(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"forbidden", "forbidden"}, {"unauthorized", "unauthorized"}, {"network", "network_error"},
		{"codex-response", "redirect_blocked"}, {"incomplete", "incomplete"}, {"html", "unexpected_content_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := testCredentials()
			creds.AuthorizationMethod = protocol.AuthorizationMethodBPS
			calls := 0
			client := &Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				calls++
				if request.URL != bpsResponsesURL {
					t.Fatal("BPS failure fell back to the Codex route")
				}
				response := successfulResponse(testAnswers())
				response.URL, _ = url.Parse(bpsResponsesURL)
				switch tc.name {
				case "forbidden":
					response.StatusCode = http.StatusForbidden
				case "unauthorized":
					response.StatusCode = http.StatusUnauthorized
				case "network":
					return nil, errors.New("synthetic-private-network-error")
				case "codex-response":
					response.URL, _ = url.Parse(codexResponsesURL)
				case "incomplete":
					response.Body = []byte("data: [DONE]\n\n")
				case "html":
					response.Header.Set("Content-Type", "text/html")
					response.Body = []byte("<html>synthetic challenge</html>")
				}
				return response, nil
			})}
			result, err := client.Probe(t.Context(), creds, Options{Model: "synthetic-model"})
			requireProbeError(t, result, err, tc.code)
			if calls != 1 || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("BPS failure retried or leaked upstream diagnostics")
			}
		})
	}
}

func TestInferTextBPSNormalizesOnlySupportedReasoningEfforts(t *testing.T) {
	for _, tc := range []struct{ requested, wire string }{
		{"", "medium"}, {"none", "low"}, {"minimal", "low"}, {"low", "low"}, {"medium", "medium"},
		{"high", "high"}, {"xhigh", "xhigh"}, {"max", "xhigh"}, {"ultra", "xhigh"},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			creds := testCredentials()
			creds.AuthorizationMethod = protocol.AuthorizationMethodBPS
			client := &Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				var body map[string]any
				if json.Unmarshal(request.Body, &body) != nil || body["reasoning_effort"] != tc.wire {
					t.Fatal("unsupported reasoning effort reached BPS")
				}
				if _, unsupported := body["max_output_tokens"]; unsupported {
					t.Fatal("unsupported output-token field reached the BPS native schema")
				}
				input := body["input"].([]any)
				if input[len(input)-1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "synthetic manual prompt" {
					t.Fatal("generic inference replaced the manual prompt with scoring tasks")
				}
				response := successfulResponse("synthetic manual answer")
				response.URL, _ = url.Parse(bpsResponsesURL)
				return response, nil
			})}
			text, err := client.inferText(t.Context(), creds, Options{Model: "synthetic-model", ReasoningEffort: tc.requested}, "synthetic manual prompt", "synthetic instructions", 8192)
			if err != nil || text != "synthetic manual answer" {
				t.Fatalf("manual BPS response failed: %v", err)
			}
		})
	}
}

func TestBPSPelicanUsesNativeTextRouteAndReturnsInertSVG(t *testing.T) {
	creds := testCredentials()
	creds.AuthorizationMethod = protocol.AuthorizationMethodBPS
	drawing := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 480"><circle cx="160" cy="320" r="72"/></svg>`
	calls := 0
	client := Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		var body map[string]any
		if request.URL != bpsResponsesURL || !request.SingleAttempt || json.Unmarshal(request.Body, &body) != nil || body["model_selection"] != "explicit" {
			t.Fatal("BPS drawing bypassed its native single-attempt route")
		}
		for _, key := range []string{"tools", "tool_choice", "instructions", "max_output_tokens"} {
			if _, exists := body[key]; exists {
				t.Fatal("manual drawing sent unsupported BPS fields")
			}
		}
		input := body["input"].([]any)
		if input[len(input)-1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != PelicanPrompt {
			t.Fatal("manual drawing replaced its pelican prompt with automatic scoring")
		}
		response := successfulResponse(drawing)
		response.URL, _ = url.Parse(bpsResponsesURL)
		return response, nil
	})}
	result, err := client.Pelican(t.Context(), creds, Options{Model: "synthetic-model", ReasoningEffort: "high"})
	if err != nil || calls != 1 || result.SVG != drawing || result.Model != "synthetic-model" {
		t.Fatal("BPS completed SVG did not pass manual drawing validation", err)
	}
}
