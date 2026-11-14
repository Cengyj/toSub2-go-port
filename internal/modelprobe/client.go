package modelprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	bpsResponsesURL   = "https://bps.openai.com/basispoints/api/responses"
	maxResponseBytes  = 1 << 20
)

// Probe makes one potentially billable inference request. It never refreshes a
// token, follows a redirect, solves a challenge, or retries an uncertain POST.
func (c *Client) Probe(ctx context.Context, creds protocol.InsightCredentials, opts Options) (Result, error) {
	started := time.Now()
	opts, err := normalizeOptions(opts)
	if err != nil {
		return Result{}, err
	}
	text, err := c.inferText(ctx, creds, opts, taskPrompt, taskInstructions, 0)
	if err != nil {
		return Result{}, err
	}
	score, err := scoreAnswers(text)
	if err != nil {
		return Result{}, withHTTPStatus(err, http.StatusOK)
	}
	if ctx != nil && ctx.Err() != nil {
		return Result{}, withHTTPStatus(contextFailure(ctx.Err()), http.StatusOK)
	}
	verdict := "normal"
	if score < opts.MinimumScore {
		verdict = "degraded"
	}
	// These verdicts describe this rule set only; neither proves a model's
	// identity, a general capability level, or an actual upstream downgrade.
	return Result{Model: opts.Model, RuleVersion: RuleVersion, Verdict: verdict,
		Score: score, Total: TotalQuestions, CheckedAt: time.Now().UTC(), DurationMS: time.Since(started).Milliseconds()}, nil
}

// inferText shares the closed inference transport between scored and manual
// prompts. It returns text only after the entire Responses stream has completed.
func (c *Client) inferText(ctx context.Context, creds protocol.InsightCredentials, opts Options, prompt, instructions string, maxOutputTokens int) (string, error) {
	return c.inferTextWithResponseLimit(ctx, creds, opts, prompt, instructions, maxOutputTokens, maxResponseBytes)
}

func (c *Client) inferTextWithResponseLimit(ctx context.Context, creds protocol.InsightCredentials, opts Options, prompt, instructions string, maxOutputTokens, responseLimit int) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := normalizeOptions(opts)
	if err != nil {
		return "", err
	}
	if prompt == "" || maxOutputTokens < 0 || maxOutputTokens > 32768 || responseLimit < 1 || responseLimit > pelicanMaxResponseBytes {
		return "", &Error{Code: "invalid_options"}
	}
	method, err := protocol.NormalizeAuthorizationMethod(creds.AuthorizationMethod)
	if err != nil {
		return "", &Error{Code: "invalid_credentials"}
	}
	if !validCredential(creds.AccessToken) || !validCredential(creds.AccountID) {
		return "", &Error{Code: "invalid_credentials"}
	}
	if c == nil || c.Transport == nil {
		return "", &Error{Code: "transport_unavailable"}
	}
	if _, unsafe := c.Transport.(*protocol.Client); unsafe {
		return "", &Error{Code: "unsafe_transport"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds)*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return "", contextFailure(ctx.Err())
	}
	endpoint, headers, payload, err := inferenceRequest(method, creds, opts, prompt, instructions, maxOutputTokens)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &Error{Code: "invalid_options"}
	}
	deadline, _ := ctx.Deadline()
	response, err := c.Transport.Do(ctx, transport.Request{
		Method: http.MethodPost, URL: endpoint, Body: body, Headers: headers,
		SingleAttempt: true, CaptureResponse: true, Timeout: time.Until(deadline), MaxResponseBytes: int64(responseLimit),
	})
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if ctx.Err() != nil {
		return "", withHTTPStatus(contextFailure(ctx.Err()), status)
	}
	if response != nil {
		if response.URL != nil && response.URL.String() != endpoint {
			return "", &Error{Code: "redirect_blocked", HTTPStatus: status}
		}
		if len(response.Body) > responseLimit {
			return "", &Error{Code: "response_too_large", HTTPStatus: status}
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return "", withHTTPStatus(contextFailure(err), status)
		case errors.Is(err, transport.ErrResponseTooLarge):
			return "", &Error{Code: "response_too_large", HTTPStatus: status}
		default:
			// A late connection error cannot invalidate an already fully read
			// response. Its status, type and complete stream still need to pass
			// every check below; a partial body is never enough to infer success.
			if response == nil || !response.BodyComplete || status != http.StatusOK {
				return "", &Error{Code: "network_error", HTTPStatus: status}
			}
		}
	}
	if response == nil {
		return "", &Error{Code: "network_error"}
	}
	if status != http.StatusOK {
		return "", httpFailure(status)
	}
	// Some valid upstream Responses streams omit Content-Type. An absent
	// header only defers classification to the strict SSE parser below;
	// an explicitly different media type remains an error.
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	if contentType != "" && strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) != "text/event-stream" {
		return "", &Error{Code: "unexpected_content_type", HTTPStatus: status}
	}
	text, err := parseStreamWithPolicyAndLimit(response.Body, opts.Model, method == protocol.AuthorizationMethodBPS, responseLimit)
	if ctx.Err() != nil {
		return "", withHTTPStatus(contextFailure(ctx.Err()), status)
	}
	return text, withHTTPStatus(err, status)
}

func inferenceRequest(method string, creds protocol.InsightCredentials, opts Options, prompt, instructions string, maxOutputTokens int) (string, http.Header, map[string]any, error) {
	headers := http.Header{
		"Content-Type": {"application/json"}, "Accept": {"text/event-stream"},
		"Authorization": {"Bearer " + creds.AccessToken}, "Chatgpt-Account-Id": {creds.AccountID},
	}
	message := func(role, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}}
	}
	if method == protocol.AuthorizationMethodBPS {
		// Basispoints has a native request schema: use its text-only whitelist,
		// not Codex's tools/reasoning/instructions fields. These wire identities
		// match sub2api b50754c's newExcelBPSRequestTo and basispoints.Prepare.
		headers.Set("X-Openai-Account-Id", creds.AccountID)
		headers.Set("X-Basispoints-Auth-Mode", "chatgpt")
		headers.Set("Origin", "https://bps.openai.com")
		headers.Set("User-Agent", "Mozilla/5.0")
		headers.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
		headers.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
		effort := opts.ReasoningEffort
		switch effort {
		case "", "medium":
			effort = "medium"
		case "none", "minimal":
			effort = "low"
		case "xhigh", "max", "ultra":
			effort = "xhigh"
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, nil, &Error{Code: "transport_unavailable"}
		}
		input := []any{}
		if instructions != "" {
			input = append(input, message("developer", instructions))
		}
		input = append(input, message("developer", "Answer as assistant text. Do not call Excel, Office, workbook, connector, or other tools."), message("user", prompt))
		return bpsResponsesURL, headers, map[string]any{
			"model": opts.Model, "model_selection": "explicit", "stream": true, "store": false,
			"input": input, "reasoning_effort": effort,
			"context_management": []any{map[string]any{"type": "compaction", "compact_threshold": 920000}},
			"metadata":           map[string]any{"task_id": hex.EncodeToString(nonce[:16]), "turn_id": hex.EncodeToString(nonce[16:]), "agent_iteration": "1"},
		}, nil
	}
	headers.Set("Originator", "codex-tui")
	payload := map[string]any{
		"model": opts.Model, "stream": true, "store": false, "instructions": instructions,
		"input": []any{message("user", prompt)}, "tools": []any{}, "tool_choice": "none", "parallel_tool_calls": false,
	}
	if opts.ReasoningEffort != "" {
		payload["reasoning"] = map[string]any{"effort": opts.ReasoningEffort}
	}
	if maxOutputTokens > 0 {
		payload["max_output_tokens"] = maxOutputTokens
	}
	return codexResponsesURL, headers, payload, nil
}

func normalizeOptions(opts Options) (Options, error) {
	opts.Model = strings.TrimSpace(opts.Model)
	if len(opts.Model) == 0 || len(opts.Model) > 128 {
		return Options{}, &Error{Code: "invalid_options"}
	}
	for _, r := range opts.Model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return Options{}, &Error{Code: "invalid_options"}
		}
	}
	switch opts.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return Options{}, &Error{Code: "invalid_options"}
	}
	if opts.MinimumScore == 0 {
		opts.MinimumScore = DefaultMinimumScore
	}
	if opts.TimeoutSeconds == 0 {
		opts.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if opts.MinimumScore < 1 || opts.MinimumScore > TotalQuestions || opts.TimeoutSeconds < 1 || opts.TimeoutSeconds > MaxTimeoutSeconds {
		return Options{}, &Error{Code: "invalid_options"}
	}
	return opts, nil
}

func validCredential(value string) bool {
	if value == "" || len(value) > 32768 {
		return false
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func contextFailure(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "timeout"}
	}
	return &Error{Code: "canceled"}
}

func httpFailure(status int) *Error {
	code := "http_error"
	switch {
	case status >= 300 && status < 400:
		code = "redirect_blocked"
	case status == http.StatusUnauthorized:
		code = "unauthorized"
	case status == http.StatusForbidden:
		code = "forbidden"
	case status == http.StatusTooManyRequests:
		code = "rate_limited"
	}
	return &Error{Code: code, HTTPStatus: status}
}
