package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/transport"
)

// One observer, attached only during this Flow's explicit reset. Facts about
// the remote outcome come from the existing bound receipts, not this ledger.
type resetDiagnostics struct {
	ref                      string
	seq, attempts            int
	writes                   resetlog.Writes
	material                 string // only advanced after protected journal readback
	issue                    resetlog.Detail
	writeIssue, writeAttempt string
	lastAttempt, lastStage   string
}

// Adapters report the checkpoint that failed, not the provider's raw error
// (which may include a credential path or private response).
type ResetStorageFailure struct {
	Stage, Reason, Message string
	matches                func(error) bool
}

func (e *ResetStorageFailure) Error() string { return e.Message }

// Keep errors.Is usable for a caller that already holds the storage sentinel;
// do not expose the provider's path or plaintext through Unwrap or formatting.
func (e *ResetStorageFailure) Is(target error) bool { return e.matches != nil && e.matches(target) }

func journalFailure(reason string, cause error) error {
	return &ResetStorageFailure{Stage: "journal", Reason: reason, Message: "TOTP_RESET_STORAGE_BLOCKED: 操作记录写入或读回未确认，停止后续修改", matches: func(target error) bool { return target == ErrResetStorageBlocked || errors.Is(cause, target) }}
}

func (f *Flow) explainEnrollment(step string, result map[string]any) {
	if f.resetLog == nil || result == nil {
		return
	}
	factor := payload(result["factor"])
	for _, field := range []struct {
		key, label, kind string
		obj              map[string]any
	}{
		{"secret", "secret", "string", result}, {"session_id", "session_id", "string", result}, {"factor", "factor", "object", result},
		{"id", "factor.id", "string", factor}, {"factor_type", "factor.factor_type", "string", factor}, {"is_recovery", "factor.is_recovery", "boolean", factor},
	} {
		v, present := field.obj[field.key]
		actual := totpFieldType(v, present)
		if actual != field.kind || actual == "string" && str(v) == "" {
			f.resetNote(resetlog.Detail{Stage: step, Reason: "FIELD_INVALID", Source: "business_receipt", Field: field.label, ActualType: actual, ExpectedType: field.kind})
			return
		}
	}
}

func (f *Flow) resetNote(d resetlog.Detail) {
	l := f.resetLog
	if l == nil {
		return
	}
	l.seq++
	d.OpRef, d.Seq = l.ref, l.seq
	if d.AttemptRef == "" && l.lastStage == d.Stage {
		d.Related = l.lastAttempt
	}
	if safe := d.Safe(); safe != nil {
		if safe.Level() == "warning" || safe.Level() == "error" {
			l.issue = *safe
			if safe.Source == "transport" && safe.Access == "write" && safe.Stage != "login" {
				l.writeIssue, l.writeAttempt = safe.Reason, safe.AttemptRef
			}
		}
		f.emitData("totp_reset_diagnostic", "", safe.Message(), map[string]any{resetlog.DataKey: safe})
	}
}

func (f *Flow) decision(stage, reason, source string) {
	f.resetNote(resetlog.Detail{Stage: stage, Reason: reason, Source: source})
}

func (f *Flow) readAgain(stage string, remaining int) {
	f.resetNote(resetlog.Detail{Stage: stage, Reason: "READ_AGAIN", Source: "workflow", Remaining: &remaining})
}

func (f *Flow) observeResetAttempt(method, rawURL string) func(*transport.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || f.resetCapture == nil {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	if origin != f.Options.AuthBase && origin != f.Options.ChatGPTBase {
		return nil
	}
	path, stage := u.Path, "login"
	var counter *resetlog.Count
	l := f.resetLog
	out := f.resetCapture.out
	switch path {
	case "/backend-api/accounts/mfa_info":
		stage = "initial_state"
		switch {
		case out["activation_attempted"] == true:
			stage = "verify_factor"
		case out["reset_totp_phase"] == "enroll_conflict" || out["reset_totp_phase"] == "initial_enroll_conflict":
			stage = "conflict_read"
		case out["disable_response_error"] != nil:
			stage = "verify_disable"
		case out["initial_enroll_result"] == "already_exists":
			stage = "conflict_read"
		}
	case "/backend-api/accounts/mfa/user/disable_in_house":
		stage, counter = "disable", &l.writes.Disable
	case "/backend-api/accounts/mfa/enroll":
		stage, counter = "enroll", &l.writes.Enroll
		if f.resetCapture.enrollmentStep == "initial_enroll" {
			stage, counter = "initial_enroll", &l.writes.Initial
		} else if f.resetCapture.enrollmentStep == "retry_enroll" {
			stage = "retry_enroll"
		}
	case "/backend-api/accounts/mfa/user/activate_enrollment":
		stage, counter = "activation", &l.writes.Activation
	case "/backend-api/accounts/logout_all":
		stage, counter = "logout", &l.writes.Logout
	case "/api/auth/session":
		stage = "identity"
	case "/":
	default:
		path = "/:login" // includes redirects: never expose codes or concrete paths
	}
	l.attempts++
	a := fmt.Sprintf("A%d", l.attempts)
	l.lastAttempt, l.lastStage = a, stage
	d := resetlog.Detail{Stage: stage, Source: "transport", AttemptRef: a, Method: method, Path: path, Access: "read"}
	if method != http.MethodGet && method != http.MethodHead {
		d.Access = "write"
	}
	start := time.Now()
	d.Started = resetlog.Stamp(start)
	if counter != nil {
		d.Reason, d.Dispatch, d.Receipt = "DISPATCH_INTENT", "possibly_sent", "not_received"
		f.resetNote(d)
	}
	// Exclude time spent publishing the intent from transport duration.
	start = time.Now()
	d.Started = resetlog.Stamp(start)
	return func(r *transport.Response, err error) {
		end := time.Now()
		ms := end.Sub(start).Milliseconds()
		d.Ended, d.DurationMS = resetlog.Stamp(end), &ms
		d.Reason, d.Dispatch, d.Receipt = "NO_RESPONSE", "possibly_sent", "not_received"
		switch {
		case r != nil:
			if counter != nil {
				counter.Received++
			}
			d.HTTP, d.Dispatch = r.StatusCode, "response_received"
			d.Reason, d.Receipt = "REQUEST_COMPLETE", "complete"
			if err != nil && !r.BodyComplete {
				d.Reason, d.Receipt = "INCOMPLETE_RESPONSE", "incomplete"
			}
			if stage != "login" && stage != "identity" && d.Receipt == "complete" && len(r.Body) <= maxResetReceiptBody {
				obj, ok := decodeTOTPResponse(r.Body)
				if ok {
					d.Business = resetBusinessCode(stage, obj)
					if d.HTTP >= 200 && d.HTTP < 300 && totpCrossedMutationSignals(stage, obj) {
						d.Reason = "CONTRADICTORY_RECEIPT"
					}
				}
			}
			switch {
			case isBrowserChallenge(r):
				d.Reason = "CHALLENGE_REQUIRED"
			case d.Business == "token_revoked":
				d.Reason = "SESSION_REVOKED"
			case d.Business == "recent_auth_required" || d.HTTP == 401 || d.HTTP == 403:
				d.Reason = "AUTH_REQUIRED"
			case d.HTTP == 429:
				d.Reason = "RATE_LIMITED"
			case d.HTTP >= 500:
				d.Reason = "SERVER_ERROR"
			case d.HTTP >= 400:
				d.Reason = "HTTP_REFUSED"
			}
		case transport.IsRequestDefinitelyUnsent(err):
			d.Reason, d.Dispatch = "REQUEST_UNSENT", "proven_unsent"
			if counter != nil {
				counter.Unsent++
			}
		default:
			if counter != nil {
				counter.Possible++
			}
			if errors.Is(err, context.Canceled) {
				d.Reason = "REQUEST_CANCELED"
			} else if errors.Is(err, context.DeadlineExceeded) {
				d.Reason = "REQUEST_TIMEOUT"
			}
		}
		f.resetNote(d)
	}
}

func resetBusinessCode(stage string, obj map[string]any) string {
	if isResetEnrollmentStep(stage) && totpEnrollmentAlreadyExists(obj) {
		return totpEnrollmentExistsCode
	}
	for _, v := range totpErrorFields(obj) {
		if o := payload(v); o != nil {
			v = o["code"]
		}
		switch v {
		case "token_revoked", "recent_auth_required", "invalid_request", "invalid_code":
			return v.(string)
		}
	}
	return ""
}

func (f *Flow) reportStateMatch(s totpLiveState, expected, old string) {
	if f.resetLog == nil || s.State != totpStateEnabled {
		return
	}
	count := len(s.Factors)
	d := resetlog.Detail{Stage: "verify_factor", Source: "state_read", Reason: "EXPECTED_FACTOR_NOT_VISIBLE", Expected: "F2", VisibleCount: &count, Match: "not_visible"}
	if s.matches(expected) {
		d.Reason, d.Match, d.Visible = "FACTOR_MATCHED", "matched", "F2"
		d.OldVisible = s.matches(old)
	} else if count == 1 {
		d.Visible = "F3"
		if s.matches(old) {
			d.Visible = "F1"
		}
	}
	f.resetNote(d)
}

// Built only after the runner knows whether the Manager's final commit succeeded.
// Do not infer a remote removal from the local old_factor_invalidated flag.
func resetOutcome(out map[string]any, issue resetlog.Detail, err error, committed bool) resetlog.Detail {
	d := resetlog.Detail{Terminal: true, Stage: "preflight", Reason: "PREFLIGHT_STOPPED", Source: "workflow", LastTrusted: "none", Pending: "not_performed", Removal: "not_performed", Activation: "not_performed", Match: "not_performed", Commit: "not_performed", Logout: "not_requested"}
	var storage *ResetStorageFailure
	if errors.As(err, &storage) {
		issue = resetlog.Detail{Stage: storage.Stage, Reason: storage.Reason, Source: "local_store"}
	}
	if errors.Is(err, context.Canceled) {
		issue.Reason = "REQUEST_CANCELED"
		if issue.Stage == "" {
			issue.Stage = "preflight"
		}
	}
	if out == nil {
		if issue.Reason != "" {
			d.Stage, d.Reason, d.Cause = issue.Stage, issue.Reason, issue.Reason
		}
		return d
	}
	d.Stage, d.Reason = "identity", "IDENTITY_UNVERIFIED"
	if str(out["account_id"]) != "" {
		d.LastTrusted, d.Stage, d.Reason = "identity", "initial_state", "TARGET_UNRESOLVED"
	}
	if out["reset_totp_attempted_at"] != nil && !resetRequestUnsent(out, "disable") {
		d.Removal = "unknown"
		if issue.Stage == "disable" && issue.Reason == "HTTP_REFUSED" {
			d.Removal = "refused"
			d.Stage, d.Reason = "disable", "HTTP_REFUSED"
		}
	}
	if acknowledgedResetRemoval(out) {
		d.Removal = "accepted"
	} else if out["old_factor_removal_confirmed_by_read"] == true {
		d.Removal = "state_read"
	}
	if d.Removal == "accepted" || d.Removal == "state_read" {
		d.LastTrusted, d.Stage, d.Reason = "old_removal", "enroll", "REMOVED_NOT_REPLACED"
	}
	if resetHasEnrollmentAttempt(out) {
		d.Stage, d.Reason = resetEnrollmentSource(out), "ENROLLMENT_UNCONFIRMED"
	}
	if _, _, _, valid := resetEnrollmentMaterial(out); valid {
		d.LastTrusted = "enrollment"
		d.Stage, d.Reason = "pending", "STORAGE_BLOCKED"
	}
	if out["pending_credential_readback"] == true {
		d.Pending, d.LastTrusted = "verified", "pending"
	}
	if out["activation_attempted"] == true && !resetRequestUnsent(out, "activation") {
		d.Activation, d.Stage, d.Reason = "unknown", "activation", "ACTIVATION_UNCONFIRMED"
		if body, ok := resetCompleteReceiptBody(out, "activation"); ok {
			_, sid, id, _ := resetEnrollmentMaterial(out)
			status := receiptStatus(payload(out["activation_receipt"])["http_status"])
			if status >= 400 && status < 500 && status != 408 || status >= 200 && status < 300 && classifyTOTPMutation("activation", body, id, sid) == totpMutationRejected {
				d.Activation = "refused"
			}
		}
	}
	if acknowledgedResetActivation(out) {
		d.Activation, d.LastTrusted = "accepted", "activation"
	}
	if issue.Reason != "" {
		d.Cause, d.Related = issue.Reason, issue.AttemptRef
		if d.Related == "" {
			d.Related = issue.Related
		}
		d.Business = issue.Business
		switch issue.Reason {
		case "EXPECTED_FACTOR_NOT_VISIBLE", "READ_BUDGET_EXHAUSTED", "TARGET_NOT_AUTHORIZED", "TARGET_UNRESOLVED", "REMOVAL_BUDGET_USED", "FORMAL_CONFLICT", "IDENTITY_UNVERIFIED", "STORAGE_WRITE_FAILED", "STORAGE_READBACK_FAILED", "STORAGE_COMMIT_FAILED", "STORAGE_BLOCKED", "REQUEST_CANCELED", "REQUEST_TIMEOUT", "AUTH_REQUIRED", "SESSION_REVOKED", "CHALLENGE_REQUIRED", "RATE_LIMITED", "SERVER_ERROR", "RESPONSE_INVALID", "FIELD_INVALID":
			d.Stage, d.Reason = issue.Stage, issue.Reason
		}
		d.Expected, d.Visible, d.VisibleCount = issue.Expected, issue.Visible, issue.VisibleCount
		if issue.Match != "" {
			d.Match = issue.Match
		}
		d.Field, d.ExpectedType, d.ActualType = issue.Field, issue.ExpectedType, issue.ActualType
		if issue.Stage == "pending" && (issue.Reason == "STORAGE_WRITE_FAILED" || issue.Reason == "STORAGE_READBACK_FAILED") {
			d.Pending = "failed"
		}
	}
	if out["logout_all_devices_requested"] == true {
		d.Logout = "not_performed"
		switch out["logout_all_devices_status"] {
		case "succeeded":
			d.Logout = "accepted"
		case "failed":
			d.Logout = "refused"
		case "unknown":
			d.Logout = "unknown"
		case "not_attempted":
			d.Logout = "proven_unsent"
		}
	}
	if verifiedResetActivation(out) {
		d.Stage = "local_commit"
		if out["activation_confirmed_by_read"] == true {
			d.Expected, d.Match, d.LastTrusted, d.Reason = "F2", "matched", "verify_factor", "LOCAL_COMMIT_REQUIRED"
			d.OldVisible = out["old_factor_also_visible"] == true
			d.Visible, d.VisibleCount = "F2", nil // never reuse an earlier failed read's count
			if n := receiptStatus(out["verified_ordinary_factor_count"]); n > 0 {
				d.VisibleCount = &n
			}
		} else {
			d.Reason, d.Match = "RECEIPT_LOCAL_COMMIT_REQUIRED", "unknown"
			if out["activation_list_check_skipped"] == true {
				d.Match = "skipped"
			}
		}
		if err != nil {
			d.Commit = "failed"
		}
		if !committed && out["verified_journal_readback"] != true && out["credential_persisted"] != true {
			d.Reason = "UNCERTAIN_COMMIT"
		}
	}
	if committed {
		d.Reason, d.Commit, d.LastTrusted, d.Stage = "RESET_COMPLETE", "committed", "local_commit", "local_commit"
		if d.OtherAuthenticatorVisible() {
			d.Reason = "RESET_SAVED_OTHER_VISIBLE"
		} else if d.Logout != "accepted" && d.Logout != "not_requested" {
			d.Reason = "RESET_COMPLETE_LOGOUT_UNCONFIRMED"
		} else if d.Activation == "accepted" && d.Match == "skipped" {
			d.Reason = "RESET_SAVED_BY_RECEIPT"
		}
	}
	return d
}

func (f *Flow) EmitResetOutcome(out map[string]any, err error, committed bool) {
	if l := f.resetLog; l != nil {
		d := resetOutcome(out, l.issue, err, committed)
		d.Writes = &l.writes
		d.Material = l.material
		d.WriteIssue, d.WriteAttempt = l.writeIssue, l.writeAttempt
		f.resetNote(d)
	}
}

// Describe what was actually read back, not what we hoped to persist.
func savedResetMaterial(out map[string]any) string {
	if str(out["secret"]) != "" || str(out["session_id"]) != "" || str(out["new_totp_factor_id"]) != "" {
		if _, _, _, ok := resetEnrollmentMaterial(out); ok {
			return "complete"
		}
		return "partial"
	}
	return "none"
}

// Local recovery has no client or transport. Only the caller's already
// validated protected record can supply historical facts; all counts are zero.
func LocalResetLog(operationID string, seq int, out map[string]any, err error, committed bool) *resetlog.Detail {
	if operationID == "" {
		operationID = randomUUID()
	}
	d := resetOutcome(out, resetlog.Detail{}, err, committed)
	d.OpRef, d.Seq, d.Writes = resetlog.OperationRef(operationID), seq, &resetlog.Writes{}
	if committed && d.Reason != "RESET_SAVED_OTHER_VISIBLE" {
		d.Reason = "LOCAL_COMPLETE"
	}
	if out != nil {
		d.Source = "protected_journal"
		d.Material = savedResetMaterial(out)
	}
	return d.Safe()
}

// Count logical journal intents/receipts, not lost transport attempts. Never
// infer new support claims for old operations from the absence of fields.
func InterruptedResetLog(out map[string]any, email string, seq int) *resetlog.Detail {
	if receiptStatus(out["support_schema"]) != 1 || out["operation_scope"] != "current_session" || out["reset_totp_requested"] != true || !mutationEvidenceVersion(out) || validateResetMutationEvidence(out) != nil || str(out["operation_id"]) == "" || !strings.EqualFold(str(out["email"]), email) {
		return nil
	}
	d := resetOutcome(out, resetlog.Detail{}, nil, false)
	d.Material = savedResetMaterial(out)
	if n := receiptStatus(out["support_seq"]); n >= 0 && n < 1e7 {
		seq = max(seq, n+1)
	}
	d.Source, d.OpRef, d.Seq = "protected_journal", resetlog.OperationRef(str(out["operation_id"])), seq
	d.Reason = "INTERRUPTED_RESET"
	var w resetlog.Writes
	for _, step := range []struct {
		name string
		c    *resetlog.Count
	}{{"initial_enroll", &w.Initial}, {"disable", &w.Disable}, {"enroll", &w.Enroll}, {"retry_enroll", &w.Enroll}, {"activation", &w.Activation}} {
		switch out[step.name+"_dispatch"] {
		case "proven_unsent":
			step.c.Unsent++
		case "may_have_been_sent":
			r := payload(out[step.name+"_receipt"])
			if r["operation_id"] == out["operation_id"] && r["step"] == step.name && str(r["attempt_id"]) != "" && r["attempt_id"] == out[step.name+"_attempt_id"] && receiptStatus(r["http_status"]) >= 100 {
				step.c.Received++
			} else {
				step.c.Possible++
				d.Stage, d.Reason = step.name, "INTERRUPTED_WRITE"
			}
		}
	}
	if str(out["logout_all_devices_attempted_at"]) != "" {
		switch d.Logout {
		case "accepted", "refused":
			w.Logout.Received = 1
		case "proven_unsent":
			w.Logout.Unsent = 1
		default:
			w.Logout.Possible = 1
		}
	}
	if d.Match == "skipped" && d.Activation == "accepted" {
		d.Stage, d.Reason = "local_commit", "RECEIPT_LOCAL_COMMIT_REQUIRED"
	} else if d.Match == "matched" {
		d.Stage, d.Reason = "local_commit", "LOCAL_COMMIT_REQUIRED"
	}
	d.JournalWrites = &w
	return d.Safe()
}
