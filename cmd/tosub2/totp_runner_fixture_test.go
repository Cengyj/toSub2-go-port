package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

const (
	sessionAuditEmail  = "session-loss-audit@example.test"
	sessionAuditJobID  = "session-loss-audit"
	sessionAuditCookie = "synthetic-reset-session-cookie"
	sessionAuditOld    = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	sessionAuditNew    = "JBSWY3DPEHPK3PXP"
)

// This is a stateful, localhost-only model of an upstream service. Its policy
// that a missing/expired session needs email is an explicit test assumption,
// not a claim that the real service always requires email authentication.
type sessionAuditRemote struct {
	t                 *testing.T
	mu                sync.Mutex
	failure           string
	expireOnFault     bool
	sessionValid      bool
	factor            string
	requests          int
	infoReads         int
	loginRequests     int
	unauthenticated   int
	disableCalls      int
	enrollCalls       int
	activateCalls     int
	blockReadsOnFault bool
	blockReads        bool
	rotateOnDisable   bool
	currentCookie     string
	cookieName        string
	enrollmentExists  bool
	nextFactor        string
	nextSecret        string
	enrollmentSession string
}

type sessionAuditSnapshot struct {
	factor                                                                                        string
	requests, loginRequests, unauthenticated, disableCalls, enrollCalls, activateCalls, infoReads int
	enrollmentExists                                                                              bool
}

func (s *sessionAuditRemote) snapshot() sessionAuditSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sessionAuditSnapshot{s.factor, s.requests, s.loginRequests, s.unauthenticated, s.disableCalls, s.enrollCalls, s.activateCalls, s.infoReads, s.enrollmentExists}
}

func (s *sessionAuditRemote) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nextFactor, nextSecret, enrollmentSession := s.nextFactor, s.nextSecret, s.enrollmentSession
	if nextFactor == "" {
		nextFactor = "new-factor"
	}
	if nextSecret == "" {
		nextSecret = sessionAuditNew
	}
	if enrollmentSession == "" {
		enrollmentSession = "synthetic-enrollment-session"
	}
	cookieName := s.cookieName
	if cookieName == "" {
		cookieName = "audit-session-token"
	}
	cookie, _ := r.Cookie(cookieName)
	currentCookie := s.currentCookie
	if currentCookie == "" {
		currentCookie = sessionAuditCookie
	}
	authenticated := s.sessionValid && cookie != nil && cookie.Value == sessionAuditCookie
	if s.rotateOnDisable {
		rotation, _ := r.Cookie("audit-session-rotation")
		authenticated = authenticated && rotation != nil && rotation.Value == currentCookie
	}
	write := func(status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(value); err != nil {
			s.t.Errorf("encode fixture response: %v", err)
		}
	}
	// Only the test calls this oracle, using its independently held copy of
	// the synthetic cookie. It is not an imagined production recovery API.
	if r.URL.Path == "/audit/session-oracle" {
		status := http.StatusOK
		if !authenticated {
			status = http.StatusUnauthorized
		}
		write(status, map[string]any{"authenticated": authenticated, "email_required": !authenticated, "factor": s.factor})
		return
	}
	s.requests++
	if r.URL.Path == "/api/auth/signin/openai" {
		s.loginRequests++
	}
	if serveFreshLoginFixture(s.t, w, r, sessionAuditEmail) {
		return
	}
	if !authenticated {
		s.unauthenticated++
		write(http.StatusUnauthorized, map[string]any{"error": "email_verification_required"})
		return
	}
	fault := func(disconnect bool) {
		if s.blockReadsOnFault {
			s.blockReads = true
		}
		if s.expireOnFault {
			s.sessionValid = false
		}
		if disconnect {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				s.t.Errorf("hijack fixture connection: %v", err)
				return
			}
			_ = connection.Close()
		}
	}
	if s.blockReads && r.Method == http.MethodGet {
		fault(true)
		return
	}
	switch r.URL.Path {
	case "/":
		write(http.StatusOK, map[string]any{"accessToken": "synthetic-web-access-token-at-least-twenty-characters"})
	case "/api/auth/session":
		write(http.StatusOK, map[string]any{"account": map[string]any{"id": "synthetic-account"}, "user": map[string]any{"email": sessionAuditEmail}})
	case "/backend-api/accounts/mfa_info":
		s.infoReads++
		factors := []any{}
		if s.factor != "" {
			factors = append(factors, map[string]any{"id": s.factor, "factor_type": "totp", "is_recovery": false})
		}
		write(http.StatusOK, map[string]any{"mfa_enabled_v2": s.factor != "", "factors": map[string]any{"totp": factors}})
	case "/backend-api/accounts/mfa/user/disable_in_house":
		s.disableCalls++
		if s.factor == "" {
			s.t.Error("removal was replayed or addressed the wrong remote state")
		}
		// The remote mutation commits BEFORE loss or response-shape failure.
		s.factor = ""
		if s.rotateOnDisable {
			s.currentCookie = sessionAuditCookie + "-rotated"
			// A separate required session component models cookie rotation on
			// localhost HTTP without violating __Secure- HTTPS-only rules.
			http.SetCookie(w, &http.Cookie{Name: "audit-session-rotation", Value: s.currentCookie, Path: "/", HttpOnly: true})
		}
		switch s.failure {
		case "disable-response-lost":
			fault(true)
		case "disable-message-changed":
			fault(false)
			write(http.StatusOK, map[string]any{"message": "Factor successfully disabled"})
		case "disable-unconfirmed-read-blocked":
			fault(false)
			// The changed success wording is recognized. Use a genuinely
			// unknown body to exercise the interrupted reconciliation path.
			write(http.StatusOK, map[string]any{})
		default:
			write(http.StatusOK, map[string]any{"message": "Factor disabled"})
		}
	case "/backend-api/accounts/mfa/enroll":
		s.enrollCalls++
		if s.failure == "enroll-refused" {
			fault(false)
			write(http.StatusForbidden, map[string]any{"error": "fixture enrollment refused"})
			return
		}
		s.enrollmentExists = true
		if s.failure == "enroll-response-lost" {
			fault(true)
			return
		}
		write(http.StatusOK, map[string]any{"secret": nextSecret, "session_id": enrollmentSession, "factor": map[string]any{"id": nextFactor, "factor_type": "totp", "is_recovery": false}})
	case "/backend-api/accounts/mfa/user/activate_enrollment":
		s.activateCalls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["session_id"] != enrollmentSession || body["code"] == "" {
			s.t.Error("activation did not carry the enrolled factor and generated code")
		}
		// Activation also commits before the client receives any acknowledgement.
		s.factor = nextFactor
		if s.failure == "activation-response-lost" {
			fault(true)
			return
		}
		write(http.StatusOK, map[string]any{"success": true})
	default:
		s.t.Errorf("unexpected production request %s", r.URL.Path)
		write(http.StatusNotFound, map[string]any{"error": "unexpected fixture request"})
	}
}

func sessionAuditConfig(root, base string) config.Config {
	cfg := config.Default()
	cfg.OutputRoot, cfg.ChatGPTBase, cfg.AuthBase = root, base, base
	cfg.ProxyURL, cfg.ProxyURLs = "", nil
	cfg.SameProxyRetries = 0
	cfg.RequestTimeoutMS = 2000
	cfg.SentinelSDKURL = base + "/fixture-sdk.js"
	return cfg
}

func sessionAuditManager(t *testing.T, cfg config.Config, vault credentials.Store) (*jobs.Manager, *store.Store, *slog.Logger) {
	t.Helper()
	registerRunnerSyntheticProtection(t, cfg.OutputRoot)
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := jobs.NewManager(t.Context(), 1, st, logger)
	configureAccountOperationLease(cfg, m)
	m.SetCredentialStore(vault)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m, st, logger
}

func sessionAuditPrepare(t *testing.T, cfg config.Config, m *jobs.Manager) *jobs.Job {
	t.Helper()
	j := sessionAuditImport(t, cfg, m)
	j, err := m.QueueOperation(j.ID, "reset-2fa", nil)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func sessionAuditImport(t *testing.T, cfg config.Config, m *jobs.Manager) *jobs.Job {
	t.Helper()
	j, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password", "totpSecret": sessionAuditOld}})
	if err != nil {
		t.Fatal(err)
	}

	return j
}

func sessionAuditJournal(t *testing.T, cfg config.Config) map[string]any {
	t.Helper()
	data, err := protocol.ReadTOTPResetJournalJSON(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	return journal
}

func sessionAuditAssertOrdinaryBlocked(t *testing.T, m *jobs.Manager, remote *sessionAuditRemote) {
	t.Helper()
	before := remote.snapshot()
	if _, err := m.QueueOperation(sessionAuditJobID, "relogin", nil); !errors.Is(err, jobs.ErrTOTPRecoveryRequired) {
		t.Fatal("ordinary re-login bypassed quarantine", err)
	}
	if _, err := m.LoadCredentials(sessionAuditJobID); !errors.Is(err, jobs.ErrTOTPRecoveryRequired) {
		t.Fatal("ordinary credential export bypassed quarantine", err)
	}
	if after := remote.snapshot(); after != before {
		t.Fatal("blocked operation made a network request")
	}
}

func sessionAuditExecute(t *testing.T, cfg config.Config, m *jobs.Manager, logger *slog.Logger, vault credentials.Store, observers ...func(jobs.JobEvent)) error {
	t.Helper()
	runner := makeJobRunnerWithCredentials(cfg, m, logger, vault, true)
	m.SetRunner(func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
		return runner(ctx, job, func(event jobs.JobEvent) {
			for _, observe := range observers {
				observe(event)
			}
			emit(event)
		})
	})
	m.Start()
	return sessionAuditWait(t, m)
}

// A running manager owns dispatch. Follow-up operations must wait for that
// scheduler instead of invoking the runner a second time from the test.
func sessionAuditWait(t *testing.T, m *jobs.Manager) error {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		j, ok := m.Get(sessionAuditJobID)
		if ok && j.FinishedAt != nil {
			if j.OperationStatus == "succeeded" {
				return nil
			}
			return errors.New(j.OperationError)
		}
		select {
		case <-deadline.C:
			t.Fatal("production manager did not finish the 2FA operation")
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func sessionAuditAssertCommitted(t *testing.T, cfg config.Config, m *jobs.Manager, vault credentials.Store, remote *sessionAuditRemote) {
	t.Helper()
	c, err := vault.Load(sessionAuditEmail)
	j, ok := m.Get(sessionAuditJobID)
	if err != nil || !ok || c.TOTPSecret != sessionAuditNew || c.TOTPResetPending || c.PendingTOTPSecret != "" || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated || !j.HasTOTPKey || !j.TOTPResetCommitted {
		t.Fatal("confirmed reset was not durably committed", err)
	}
	journal, err := protocol.ReadTOTPResetCompletion(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json"), sessionAuditEmail)
	if err != nil || journal["credential_persisted"] != true || journal["operation_id"] != c.TOTPResetOperationID {
		t.Fatal("completion evidence missing", err)
	}
	assertNoTaskCheckpoint(t, filepath.Join(cfg.OutputRoot, sessionAuditJobID, "login-checkpoint.json"))
	assertNoAccountSessions(t, cfg.OutputRoot)
	snapshot := remote.snapshot()
	if snapshot.factor != "new-factor" || snapshot.disableCalls != 1 || snapshot.enrollCalls != 1 || snapshot.activateCalls != 1 || snapshot.loginRequests != 1 || snapshot.unauthenticated != 0 {
		t.Fatalf("unexpected mutation replay or authentication: %+v", snapshot)
	}
}
