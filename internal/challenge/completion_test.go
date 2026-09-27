package challenge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
)

type verificationBrowser struct {
	pending       []any
	pendingIndex  int
	evaluateError error
	navigate      func(context.Context) error
	clearance     string
	response      string
	sdkCalls      int
}

func (s *verificationBrowser) Navigate(ctx context.Context, _ string) error {
	if s.navigate != nil {
		return s.navigate(ctx)
	}
	return nil
}
func (s *verificationBrowser) Cookies() ([]map[string]any, error) {
	if s.clearance != "" {
		return []map[string]any{{"name": "cf_clearance", "value": s.clearance, "domain": "fixture.invalid", "path": "/"}}, nil
	}
	return nil, nil
}
func (s *verificationBrowser) SetCookies([]map[string]any) error { return nil }
func (s *verificationBrowser) Evaluate(ctx context.Context, script string) (any, error) {
	if strings.Contains(script, "const loader=") {
		s.sdkCalls++
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.evaluateError != nil {
		return nil, s.evaluateError
	}
	if strings.Contains(script, "const pick=") {
		return map[string]any{"token": s.response, "provider": "turnstile", "reason": "challenge response observed"}, nil
	}
	if strings.Contains(script, "const title=document.title") {
		if len(s.pending) == 0 {
			return nil, nil
		}
		index := s.pendingIndex
		if index < len(s.pending)-1 {
			s.pendingIndex++
		}
		return s.pending[index], nil
	}
	return nil, nil
}
func (s *verificationBrowser) EvaluateFrame(context.Context, string, string) (any, error) {
	return "", nil
}
func (s *verificationBrowser) HTML(context.Context) (string, error) { return "", nil }
func (s *verificationBrowser) Close() error                         { return nil }

// A URL-only response may belong to an old loader or a different target.
// Providing this deliberately must not make the legacy mock current-document aware.
func (s *verificationBrowser) ResourceDiagnostic(string) browser.NetworkDiagnostic {
	return browser.NetworkDiagnostic{Status: 200, ContentKind: "html"}
}

type currentVerificationBrowser struct {
	*verificationBrowser
	diagnostic browser.NetworkDiagnostic
	known      bool
}

func (s *currentVerificationBrowser) CurrentDocumentDiagnostic(ctx context.Context) (browser.NetworkDiagnostic, bool) {
	return s.diagnostic, s.known && ctx.Err() == nil
}

func TestVerificationDocumentReadinessRequiresProtectedReplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		diagnostic browser.NetworkDiagnostic
	}{
		{"html", browser.NetworkDiagnostic{Status: 200, ContentKind: "html"}},
		{"json", browser.NetworkDiagnostic{Status: 200, ContentKind: "json"}},
		{"challenge_disappeared_during_navigation", browser.NetworkDiagnostic{Status: 403, ContentKind: "html", Challenge: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, diagnostic: test.diagnostic, known: true}
			result, err := (BrowserSolver{Session: session, Timeout: time.Second}).SolveContext(context.Background(), "https://fixture.invalid/protected")
			if err != nil || !result.RetryReady || result.Solved || !result.SessionBound || result.Token != "" {
				t.Fatalf("document incorrectly treated as accepted verification: result=%+v err=%v", result, err)
			}
			if !strings.Contains(result.Reason, "protected request verification") || !strings.Contains(result.Reason, "DOM=clear") {
				t.Fatalf("missing replay requirement: %s", result.Reason)
			}
		})
	}
}

func TestVerificationObservedChallengeDisappearanceOnlyAllowsReplay(t *testing.T) {
	session := &verificationBrowser{pending: []any{true, false}}
	result, err := (BrowserSolver{Session: session, Timeout: time.Second}).SolveContext(context.Background(), "https://fixture.invalid/protected")
	if err != nil || !result.RetryReady || result.Solved || !result.SessionBound {
		t.Fatalf("DOM transition must only allow protected replay: result=%+v err=%v", result, err)
	}
}

func TestVerificationReadinessDoesNotTrustUnknownOrBlockedDocuments(t *testing.T) {
	for _, test := range []struct {
		name       string
		session    browser.BrowserSession
		diagnostic string
	}{
		{"url_cache_only", &verificationBrowser{pending: []any{false}}, "document=unknown, DOM=clear"},
		{"unknown_dom", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200}}, "DOM=unknown"},
		{"active_dom", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{true}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200}}, "DOM=pending"},
		{"network_failed", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200, NetworkError: "net::ERR_CONNECTION_RESET"}}, "net::ERR_CONNECTION_RESET"},
		{"unsafe_network_failure", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200, NetworkError: "https://private.invalid/?token=secret"}}, "unclassified"},
		{"network_blocked", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200, BlockedReason: "csp"}}, "blocked=csp"},
		{"cors_rejected", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200, CORSError: "DisallowedByMode"}}, "cors=DisallowedByMode"},
		{"old_loader", &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: false, diagnostic: browser.NetworkDiagnostic{Status: 200}}, "document=unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := (BrowserSolver{Session: test.session, Timeout: 25 * time.Millisecond}).SolveContext(context.Background(), "https://fixture.invalid/protected")
			if !errors.Is(err, ErrVerificationPending) || !errors.Is(err, context.DeadlineExceeded) || result.RetryReady || result.Solved || result.Token != "" {
				t.Fatalf("unsafe document became ready or timeout lost category: result=%+v err=%v", result, err)
			}
			if !strings.Contains(err.Error(), test.diagnostic) {
				t.Fatalf("missing safe diagnostic %q: %v", test.diagnostic, err)
			}
		})
	}
}

func TestVerificationPlainHTTPRejectionStopsWithoutWaiting(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		session := &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: status, ContentKind: "json"}}
		started := time.Now()
		result, err := (BrowserSolver{Session: session, Timeout: time.Second}).SolveContext(context.Background(), "https://fixture.invalid/protected")
		if !errors.Is(err, ErrVerificationRejected) || errors.Is(err, ErrVerificationPending) || errors.Is(err, context.DeadlineExceeded) || result.RetryReady || result.Solved || time.Since(started) > 500*time.Millisecond {
			t.Fatalf("HTTP %d became a manual challenge timeout: result=%+v err=%v", status, result, err)
		}
		if !strings.Contains(err.Error(), "DOM=clear") || !strings.Contains(err.Error(), "content=json") {
			t.Fatalf("missing denial diagnostic: %v", err)
		}
	}
}

func TestVerificationProviderChallengeKeepsPendingWithSafeDiagnostics(t *testing.T) {
	session := &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{true}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 403, ContentKind: "html", Challenge: true}}
	result, err := (BrowserSolver{Session: session, Timeout: 25 * time.Millisecond}).SolveContext(context.Background(), "https://fixture.invalid/protected?token=secret")
	if !errors.Is(err, ErrVerificationPending) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrVerificationRejected) || result.RetryReady || result.Solved {
		t.Fatalf("provider gate classified as permanent rejection: result=%+v err=%v", result, err)
	}
	for _, diagnostic := range []string{"HTTP 403", "content=html", "provider-challenge", "DOM=pending"} {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Fatalf("missing %q in %v", diagnostic, err)
		}
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "fixture.invalid") {
		t.Fatalf("verification error exposed protected URL: %v", err)
	}
}

func TestVerificationStaleClearanceDoesNotClaimSolved(t *testing.T) {
	session := &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}, clearance: "revoked-cookie"}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 200}}
	result, err := (BrowserSolver{Session: session}).SolveContext(context.Background(), "https://fixture.invalid/protected")
	if err != nil || result.Solved || !result.RetryReady || result.Token != "" {
		t.Fatalf("pre-existing clearance claimed fresh approval: result=%+v err=%v", result, err)
	}
	session.diagnostic.Status = 403
	result, err = (BrowserSolver{Session: session}).SolveContext(context.Background(), "https://fixture.invalid/protected")
	if !errors.Is(err, ErrVerificationRejected) || result.Solved || result.RetryReady {
		t.Fatalf("stale cookie concealed refused document: result=%+v err=%v", result, err)
	}
}

func TestVerificationCancellationRetainsCauseAndCategory(t *testing.T) {
	for _, test := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			session := &verificationBrowser{navigate: func(ctx context.Context) error { return ctx.Err() }}
			result, err := (BrowserSolver{Session: session}).SolveContext(ctx, "https://fixture.invalid/protected")
			if !errors.Is(err, ErrVerificationPending) || !errors.Is(err, ctx.Err()) || result.RetryReady || result.Solved {
				t.Fatalf("navigation cancellation lost category or cause: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestVerificationErrorsNeverIncludeRawBrowserOrPageDetails(t *testing.T) {
	const secret = "https://private.invalid/?password=secret&token=access-token"
	for _, navigates := range []bool{true, false} {
		base := &verificationBrowser{evaluateError: errors.New(secret)}
		if navigates {
			base.navigate = func(context.Context) error { return errors.New(secret) }
		}
		session := &currentVerificationBrowser{verificationBrowser: base, known: true, diagnostic: browser.NetworkDiagnostic{Status: 403, ContentKind: secret, NetworkError: secret, BlockedReason: secret, CORSError: secret}}
		result, err := (BrowserSolver{Session: session, Timeout: 25 * time.Millisecond}).SolveContext(context.Background(), secret)
		if err == nil || strings.Contains(err.Error(), "private.invalid") || strings.Contains(err.Error(), "secret") || strings.Contains(result.Reason, "access-token") {
			t.Fatalf("raw browser details leaked: result=%+v err=%v", result, err)
		}
	}
}

func TestSentinelRefusedCurrentDocumentDoesNotExecuteSDK(t *testing.T) {
	session := &currentVerificationBrowser{verificationBrowser: &verificationBrowser{pending: []any{false}}, known: true, diagnostic: browser.NetworkDiagnostic{Status: 403, ContentKind: "json"}}
	result, err := (BrowserSolver{Session: session}).Sentinel(context.Background(), "https://fixture.invalid/login", "https://fixture.invalid/loader.js", "authorize_continue")
	if !errors.Is(err, ErrVerificationRejected) || result.Solved || session.sdkCalls != 0 {
		t.Fatalf("refused document executed SDK: result=%+v err=%v calls=%d", result, err, session.sdkCalls)
	}
	if !strings.Contains(err.Error(), "SENTINEL_PAGE_REJECTED") || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "DOM=clear") {
		t.Fatalf("missing Sentinel refusal diagnostic: %v", err)
	}
}
