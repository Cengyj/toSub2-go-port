package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

type accountRequestCompleter interface {
	Complete(...jobs.AccountRequestObservation) error
}

type accountRequestAttempt struct {
	source     string
	recorder   accountRequestCompleter
	requested  bool
	lastStatus int
	failures   []accountRequestFailure
}

type accountRequestFailure struct {
	status int
	err    error
}

// Account observations are a display-only side channel. They cannot alter
// retries, saved credentials, recovery decisions or the operation's error.
type accountOperationObservation struct {
	mu              sync.Mutex
	manager         *jobs.Manager
	logger          *slog.Logger
	jobID           string
	origins         map[string]bool
	chatOrigin      string
	current, logout accountRequestAttempt
	logoutOutcome   string
}

func accountOperationSource(operation string) string {
	op := strings.ToLower(strings.TrimSpace(operation))
	switch {
	case strings.Contains(op, "totp"), strings.Contains(op, "2fa"):
		return "totp"
	case strings.Contains(op, "password"):
		return "password"
	case op == "refresh" || op == "regenerate" || op == "reauthorize":
		return "refresh"
	default:
		return "authentication"
	}
}

func accountRequestOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if scheme == "http" {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func newAccountOperationObservation(manager *jobs.Manager, logger *slog.Logger, job *jobs.Job, chatBase, authBase string) *accountOperationObservation {
	if chatBase == "" {
		chatBase = "https://chatgpt.com"
	}
	if authBase == "" {
		authBase = "https://auth.openai.com"
	}
	o := &accountOperationObservation{manager: manager, logger: logger, jobID: job.ID, origins: make(map[string]bool), chatOrigin: accountRequestOrigin(chatBase)}
	for _, base := range []string{chatBase, authBase} {
		if origin := accountRequestOrigin(base); origin != "" {
			o.origins[origin] = true
		}
	}
	o.current = o.begin(accountOperationSource(job.Operation))
	return o
}

func (o *accountOperationObservation) begin(source string) accountRequestAttempt {
	return accountRequestAttempt{source: source, recorder: o.manager.BeginAccountRequest(o.jobID, source)}
}

// Observe accepts only the configured account service origins. Mail, SMS,
// sentinel assets and Sub2API management requests are not account outcomes.
func (o *accountOperationObservation) Observe(rawURL string, status int) {
	o.ObserveResult(rawURL, status, nil)
}

func (o *accountOperationObservation) ObserveResult(rawURL string, status int, requestErr error) {
	if o == nil {
		return
	}
	origin := accountRequestOrigin(rawURL)
	if !o.origins[origin] || status != 0 && (status < 100 || status > 599) {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	attempt := &o.current
	if origin == o.chatOrigin && u.Path == "/backend-api/accounts/logout_all" {
		if o.logout.source == "" {
			o.logout = o.begin("logout")
		}
		attempt = &o.logout
	}
	attempt.requested = true
	attempt.lastStatus = status
	// A later successful read must not erase an earlier rejected mutation.
	if status >= 400 || requestErr != nil {
		// Keep the actual response order, including error identities, in a
		// bounded in-memory trail. No private error text is persisted.
		if len(attempt.failures) == 32 {
			attempt.failures = attempt.failures[1:]
		}
		attempt.failures = append(attempt.failures, accountRequestFailure{status: status, err: requestErr})
	}
}

// Switch separates a failed refresh grant from its explicitly requested
// re-login fallback. A later login success must not overwrite that grant.
func (o *accountOperationObservation) Switch(ctx context.Context, source string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current.source == source {
		return
	}
	o.finish(ctx, o.current, err, true)
	o.current = o.begin(source)
}

func (o *accountOperationObservation) LogoutResult(result map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// This is a closed workflow state, never a server message or result body.
	status, _ := result["logout_all_devices_status"].(string)
	switch status {
	case "succeeded", "failed", "unknown", "not_attempted", "skipped":
		o.logoutOutcome = status
	}
}

// CompleteOperation is deferred by the production runner. A panic must keep
// its original behavior and must never commit a success tombstone.
func (o *accountOperationObservation) CompleteOperation(ctx context.Context, err *error) {
	if failure := recover(); failure != nil {
		panic(failure)
	}
	o.Complete(ctx, *err)
}

func (o *accountOperationObservation) Complete(ctx context.Context, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finish(ctx, o.current, err, true)
	if o.logout.requested {
		var logoutErr error
		if o.logoutOutcome != "succeeded" {
			logoutErr = errors.New("logout outcome unconfirmed")
		}
		o.finish(ctx, o.logout, logoutErr, true)
	}
}

func accountRequestFailureCode(status int, err error) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if status >= 400 {
		return "http_error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, protocol.ErrSessionExpired) {
		return "authorization_expired"
	}
	var invalidJSON *json.SyntaxError
	var invalidType *json.UnmarshalTypeError
	if errors.As(err, &invalidJSON) || errors.As(err, &invalidType) {
		return "invalid_json"
	}
	var transportErr *transport.TransportError
	if errors.As(err, &transportErr) {
		return "transport_error"
	}
	return "request_failed"
}

func (o *accountOperationObservation) finish(ctx context.Context, attempt accountRequestAttempt, err error, allowSuccess bool) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) || !attempt.requested || attempt.recorder == nil {
		return
	}
	if err == nil && !allowSuccess {
		return
	}
	observations := []jobs.AccountRequestObservation{{Status: "success", HTTPStatus: attempt.lastStatus}}
	if err != nil {
		// Filter the observed trail in its original order. A later mail/SMS
		// or local error must not borrow an earlier account HTTP 200.
		var statuses []int
		for _, failure := range attempt.failures {
			if failure.status >= 400 || failure.err != nil && errors.Is(err, failure.err) {
				statuses = append(statuses, failure.status)
			}
		}
		// Confirmed authentication redirects/callbacks carry a protocol
		// sentinel, not an invented 401 or a textual error-code guess.
		if errors.Is(err, protocol.ErrSessionExpired) {
			statuses = append(statuses, 0)
		}
		// Logout has its own closed workflow outcome, including a 2xx body
		// that failed confirmation. It cannot be attributed to later steps.
		if attempt.source == "logout" {
			statuses = append(statuses, attempt.lastStatus)
		}
		if len(statuses) == 0 {
			return
		}
		observations = nil
		for _, status := range statuses {
			observations = append(observations, jobs.AccountRequestObservation{Status: "failed", HTTPStatus: status, ErrorCode: accountRequestFailureCode(status, err)})
		}
	}
	if saveErr := attempt.recorder.Complete(observations...); saveErr != nil && !errors.Is(saveErr, jobs.ErrAccountRequestSuperseded) && o.logger != nil {
		o.logger.Warn("account request display status could not be persisted", "jobId", o.jobID, "source", attempt.source)
	}
}
