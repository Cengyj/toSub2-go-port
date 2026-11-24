package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

var ErrTOTPExportUnavailable = errors.New("无法确认账号安全凭据状态，已停止资料导出和上传，请核实安全凭据存储后重试")

// CheckTOTPExport checks the live account boundary before exposing a saved
// authorization. It only reads recovery evidence; a web download or upload
// cannot finish a reset, commit credentials, or consume a retained journal.
func (m *Manager) CheckTOTPExport(id string) error {
	return m.CheckTOTPExportContext(m.rootCtx, id)
}

func (m *Manager) CheckTOTPExportContext(ctx context.Context, id string) error {
	if id == "" {
		return osErrNotFound(id)
	}
	return m.checkTOTPExportContext(ctx, id, "", false)
}

// CheckTOTPWorkflowExport retains every read-only recovery boundary while
// allowing a completed reset's own active workflow to upload its new grant.
// Callers must hold the cross-process account lease before using this gate.
func (m *Manager) CheckTOTPWorkflowExport(id string) error {
	return m.CheckTOTPWorkflowExportContext(m.rootCtx, id)
}

func (m *Manager) CheckTOTPWorkflowExportContext(ctx context.Context, id string) error {
	if id == "" {
		return osErrNotFound(id)
	}
	return m.checkTOTPExportContext(ctx, id, "", true)
}

// CheckTOTPExportEmail also covers manually supplied upload payloads which
// have no selected job IDs. Unknown external accounts need no local record.
func (m *Manager) CheckTOTPExportEmail(email string) error {
	return m.CheckTOTPExportEmailContext(m.rootCtx, email)
}

func (m *Manager) CheckTOTPExportEmailContext(ctx context.Context, email string) error {
	return m.checkTOTPExportContext(ctx, "", email, false)
}

// Capture account ownership and eligibility under the manager lock, perform
// protected reads outside it, then reject any changed same-email generation.
// A workflow's completed reset is checked against this one credential read;
// ordinary LoadCredentials deliberately rejects that still-active operation.
func (m *Manager) checkTOTPExportContext(ctx context.Context, id, email string, workflow bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRoot := context.AfterFunc(m.rootCtx, cancel)
	defer stopRoot()
	if err := readCtx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	snapshot, err := m.captureTOTPExportLocked(id, email, workflow)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	err = snapshot.check(readCtx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if readCtx.Err() != nil {
		return readCtx.Err()
	}
	if m.rootCtx.Err() != nil {
		return m.rootCtx.Err()
	}
	if !snapshot.matchesLocked(m) {
		return ErrCredentialReadSuperseded
	}
	return err
}

type totpExportSnapshot struct {
	email, root, ownUpload string
	vault                  credentials.Store
	vaultGeneration        uint64
	jobs                   map[string]totpExportJobSnapshot
}

type totpExportJobSnapshot struct {
	job     *Job
	version credentialReadVersion
	state   totpExportJobState
}

// These additional fields determine the narrow completed-reset exception.
// credentialReadVersion also fences in-place credential and operation changes.
type totpExportJobState struct {
	active                                                          *executionAttempt
	queued, resetRequested, activationVerified, hasKey, resultSaved bool
	uploadAfter, reloginAfter, logoutAfter                          bool
	status, phase, workflowStatus, step, relogin, upload, logout    string
	authorizationMethod                                             string
	attempts                                                        int
}

func (m *Manager) exportJobStateLocked(j *Job) totpExportJobState {
	return totpExportJobState{
		active: m.active[j.ID], queued: containsPending(m.pending, j.ID), resetRequested: j.ResetTOTP,
		activationVerified: j.TOTPActivationVerified, hasKey: j.HasTOTPKey, resultSaved: j.ResultSaved,
		uploadAfter: j.UploadAfterTOTP, reloginAfter: j.ReloginAfterTOTP, logoutAfter: j.LogoutAllDevicesAfterTOTP,
		status: j.Status, phase: j.TOTPResetPhase, workflowStatus: j.TOTPWorkflowStatus, step: j.TOTPWorkflowStep,
		relogin: j.TOTPReloginStatus, upload: j.TOTPUploadStatus, logout: j.LogoutAllDevicesStatus,
		authorizationMethod: j.AuthorizationMethod, attempts: j.Attempts,
	}
}

func (m *Manager) captureTOTPExportLocked(id, email string, workflow bool) (totpExportSnapshot, error) {
	s := totpExportSnapshot{vault: m.vault, vaultGeneration: m.vaultGeneration, jobs: make(map[string]totpExportJobSnapshot)}
	if id != "" {
		job := m.jobs[id]
		if job == nil {
			return s, osErrNotFound(id)
		}
		email = job.Email
	}
	s.email = email
	if m.rootCtx.Err() != nil {
		return s, m.rootCtx.Err()
	}
	if m.closing || m.deleting[emailKey(email)] || m.importing[emailKey(email)] {
		return s, ErrCredentialReadSuperseded
	}
	if m.store != nil {
		s.root = m.store.Root
	}
	for key, job := range m.jobs {
		if emailKey(job.Email) != emailKey(email) {
			continue
		}
		row := totpExportJobSnapshot{job: job, version: captureCredentialReadVersion(job), state: m.exportJobStateLocked(job)}
		s.jobs[key] = row
		v, state := row.version, row.state
		if workflow && key == id && v.operation == "reset-2fa" && state.resetRequested && state.active != nil && !state.queued &&
			state.workflowStatus == "running" && state.step == "upload" && state.uploadAfter && state.reloginAfter &&
			state.relogin == "succeeded" && state.upload == "running" && state.resultSaved && state.hasKey &&
			state.phase == "credential_persisted" && state.activationVerified && v.resetCommitted &&
			(!state.logoutAfter || state.logout == "succeeded") {
			s.ownUpload = id
		}
	}
	return s, nil
}

func (s totpExportSnapshot) matchesLocked(m *Manager) bool {
	if m.closing || m.vaultGeneration != s.vaultGeneration || m.deleting[emailKey(s.email)] || m.importing[emailKey(s.email)] {
		return false
	}
	root := ""
	if m.store != nil {
		root = m.store.Root
	}
	if root != s.root {
		return false
	}
	count := 0
	for id, job := range m.jobs {
		if emailKey(job.Email) != emailKey(s.email) {
			continue
		}
		count++
		row, exists := s.jobs[id]
		if !exists || row.job != job || !row.version.matches(job) || row.state != m.exportJobStateLocked(job) {
			return false
		}
	}
	return count == len(s.jobs)
}

func (s totpExportSnapshot) check(ctx context.Context) error {
	if _, err := s.localEvidence(ctx); err != nil {
		return err
	}
	if s.vault == nil {
		return ErrTOTPExportUnavailable
	}
	saved, err := credentials.LoadContext(ctx, s.vault, s.email)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrTOTPExportUnavailable
	}
	if saved.TOTPResetPending {
		return ErrTOTPRecoveryPending
	}
	if saved.PendingTOTPSecret != "" || saved.PendingTOTPURI != "" || saved.PendingTOTPFactorID != "" {
		return ErrTOTPSetupVerificationPending
	}
	// Journals may have appeared while the protected service was answering.
	// Read the current proof without performing a second credential lookup.
	proof, proofErr := s.localEvidence(ctx)
	if proofErr != nil {
		return proofErr
	}
	if s.ownUpload != "" && (err != nil || !totpCompletionMatchesVault(proof, saved)) {
		return ErrTOTPRecoveryPending
	}
	return nil
}

// The ordinary export and the workflow exception share the same protocol
// validators as local recovery. This function has only immutable metadata;
// protected journal reads cannot hold the scheduler lock or mutate evidence.
func (s totpExportSnapshot) localEvidence(ctx context.Context) (map[string]any, error) {
	var ownProof map[string]any
	for id, row := range s.jobs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		v, state := row.version, row.state
		if v.resetPending || v.invalidated || id != s.ownUpload && resetOperation(v.operation) && (state.queued || state.active != nil) {
			return nil, ErrTOTPRecoveryPending
		}
		if s.root == "" || !safeID(id) {
			continue
		}
		path := filepath.Join(s.root, id, "totp-setup-result.json")
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, ErrTOTPRecoveryPending
		}
		proof, err := protocol.ReadTOTPResetCompletionContext(ctx, path, v.email)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		committed := err == nil && v.resetCommitted && state.activationVerified && state.phase == "credential_persisted" &&
			v.resetOperation != "" && resultString(proof, "operation_id") == v.resetOperation
		if err == nil && !v.resetCommitted || !committed && protocol.ValidateTOTPResetStartContext(ctx, path, v.email) != nil {
			return nil, ErrTOTPRecoveryPending
		}
		if id == s.ownUpload && committed {
			ownProof = proof
		}
	}
	if s.ownUpload != "" && ownProof == nil {
		return nil, ErrTOTPRecoveryPending
	}
	return ownProof, nil
}

// TOTPExportPending projects local account state for the UI without opening
// the credential store. Actual exports must still call CheckTOTPExport.
func (m *Manager) TOTPExportPending(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return false
	}
	return m.totpExportPendingLocked(j.Email)
}

func (m *Manager) totpExportPendingLocked(email string) bool {
	return m.totpExportPendingExceptLocked(email, "")
}

func (m *Manager) totpExportPendingExceptLocked(email, exceptID string) bool {
	if m.totpBlockedLocked(email) || m.unresolvedTOTPJournalLocked(email) {
		return true
	}
	for _, related := range m.jobs {
		if related.ID != exceptID && emailKey(related.Email) == emailKey(email) && resetOperation(related.Operation) && (containsPending(m.pending, related.ID) || m.active[related.ID] != nil) {
			return true
		}
	}
	return false
}
