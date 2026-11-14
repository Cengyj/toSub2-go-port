package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

// decodeLegacyJob normalizes only public fields. Secret-bearing legacy fields
// never enter the ordinary metadata model, including through unknown keys.
func decodeLegacyJob(b []byte, fallbackID string) (*Job, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil || len(raw) == 0 {
		return nil, false
	}
	normalized := map[string]json.RawMessage{}
	compact := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for k, v := range raw {
		normalized[compact(k)] = v
	}
	aliases := map[string]string{"attempt": "attempts", "lasterror": "error", "completedat": "finishedat", "lastoperationtype": "operation", "smsnumber": "currentphone", "lubanserviceid": "smsservicelabel", "autorepairpendingbackend": "autorepairbackend"}
	for old, newKey := range aliases {
		if _, ok := normalized[newKey]; !ok {
			normalized[newKey] = normalized[old]
		}
	}
	var j Job
	value := reflect.ValueOf(&j).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if tag == "accountRequestObservations" {
			// Optional display metadata cannot make an otherwise valid job vanish.
			var entries []json.RawMessage
			if json.Unmarshal(normalized[compact(tag)], &entries) == nil {
				for _, entry := range entries {
					var observation AccountRequestObservation
					if json.Unmarshal(entry, &observation) == nil {
						j.AccountRequestObservations = append(j.AccountRequestObservations, observation)
					}
				}
			}
			j.AccountRequestObservations = sanitizeAccountRequestObservations(j.AccountRequestObservations)
			continue
		}
		if b := normalized[compact(tag)]; len(b) > 0 {
			if tag == "manualExpiresAt" || tag == "subscriptionExpiresAt" {
				// These optional fields were previously unknown extensions. An
				// old non-string value must not discard an otherwise valid job.
				var text string
				if json.Unmarshal(b, &text) == nil {
					text, _ = NormalizeExpiry(text)
					value.Field(i).SetString(text)
				}
				continue
			}
			if err := json.Unmarshal(b, value.Field(i).Addr().Interface()); err != nil {
				return nil, false
			}
		}
	}
	if j.UploadTarget != UploadTargetSub2API && j.UploadTarget != UploadTargetCPA {
		j.UploadTarget = ""
	}
	_ = json.Unmarshal(normalized["totpintentreceipts"], &j.TOTPIntentReceipts)
	_ = json.Unmarshal(normalized["totpresetoperationid"], &j.TOTPResetOperationID)
	_ = json.Unmarshal(normalized["oauthresultid"], &j.OAuthResultID)
	restoreSubscriptionBinding(normalized["subscriptionplanbinding"], &j)
	// last_error historically stores the action failure, not a credential
	// storage warning. New warnings have a separate persisted field.
	j.LastError = ""
	_ = json.Unmarshal(normalized["credentialwarning"], &j.LastError)
	// Older metadata used one combined flag before explicit password/TOTP flags.
	// Retain required-input flags so a restarted queue never changes identity.
	var stored bool
	_ = json.Unmarshal(normalized["hasstoredcredentials"], &stored)
	if _, present := normalized["haspassword"]; !present && stored && j.LoginMode == "password" {
		j.HasPassword = true
	}
	if _, present := normalized["hastotpkey"]; !present && stored && !j.HasPassword {
		j.HasTOTPKey = true
	}
	if j.Operation == "" {
		_ = json.Unmarshal(normalized["queuedmode"], &j.Operation)
	}
	if j.Email == "" {
		return nil, false
	}
	// Legacy console metadata carried the mailbox URL/body. They are private
	// input only; migration moves them into the credential store below and
	// jobMetadata never serializes them into the Go aggregate.
	var mailURL, mailBody string
	_ = json.Unmarshal(normalized["mailapiurl"], &mailURL)
	_ = json.Unmarshal(normalized["mailrequestbody"], &mailBody)
	if u, err := url.Parse(strings.TrimSpace(mailURL)); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && len(mailBody) <= 64<<10 {
		j.Input = map[string]any{"mailApiUrl": strings.TrimSpace(mailURL), "mailRequestBody": mailBody}
		if mailBody != "" {
			j.Input["mailRequestMethod"] = "POST"
		}
	}
	if j.ID == "" {
		j.ID = fallbackID
	}
	if fallbackID != "" && j.ID != fallbackID {
		return nil, false
	}
	return &j, true
}

func (m *Manager) Restore(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := m.store.ReadJSON(ctx, "job-meta.json", &raw); err != nil && !store.IsNotFound(err) {
		return fmt.Errorf("restore job metadata: %w", err)
	}
	var version int
	if len(raw["version"]) > 0 && json.Unmarshal(raw["version"], &version) != nil {
		return errors.New("invalid job metadata version")
	}
	if version > 2 || (version == 2 && len(raw["trash"]) == 0) {
		return errors.New("unsupported or incomplete job metadata version")
	}
	trash, trashedIDs, trashedEmails, err := decodeTrash(raw["trash"])
	if err != nil {
		return fmt.Errorf("restore trash: %w", err)
	}
	records := map[string]*Job{}
	if b, ok := raw["jobs"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("restore jobs: %w", err)
		}
		for _, b := range list {
			if j, ok := decodeLegacyJob(b, ""); ok && safeID(j.ID) && !trashedIDs[j.ID] && !trashedEmails[emailKey(j.Email)] {
				records[j.ID] = j
			}
		}
	} else if len(raw) > 0 {
		b, _ := json.Marshal(raw)
		if j, ok := decodeLegacyJob(b, ""); ok && safeID(j.ID) && !trashedIDs[j.ID] && !trashedEmails[emailKey(j.Email)] {
			records[j.ID] = j
		}
	}
	entries, err := os.ReadDir(m.store.Root)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !safeID(ent.Name()) {
			continue
		}
		id := ent.Name()
		if trashedIDs[id] {
			continue
		}
		b, _ := readRegularJSON(filepath.Join(m.store.Root, id, "job-meta.json"))
		j, ok := decodeLegacyJob(b, id)
		if current, exists := records[id]; exists {
			if ok && strings.EqualFold(current.Email, j.Email) && len(current.Input) == 0 {
				current.Input = j.Input
			}
			continue
		}
		if !ok {
			email, output, grantAvailable := recoveryArtifacts(m.store.Root, id)
			if email == "" || (!output && !grantAvailable) {
				if b, readErr := readRegularJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json")); readErr == nil {
					var result map[string]any
					if json.Unmarshal(b, &result) == nil && resultBool(result, "reset_totp_requested") {
						email = resultString(result, "email")
					}
				}
				if email == "" {
					continue
				}
			}
			j = &Job{ID: id, Email: email}
		}
		if !trashedEmails[emailKey(j.Email)] {
			records[id] = j
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) > 0 || len(m.jobs) > 0 || len(m.trash) > 0 {
		return errors.New("restore requires an empty, idle manager")
	}
	// Recovery can stop after any credential or artifact read. Keep the
	// complete batch private under the lock until metadata is committed, so
	// cancellation or a disk failure cannot expose a partial restored batch
	// or prevent a caller from retrying initialization on this manager.
	previousJobs, previousPending, previousTrash := m.jobs, m.pending, m.trash
	m.jobs, m.pending = make(map[string]*Job, len(records)), nil
	m.trash = trash
	committed := false
	defer func() {
		if !committed {
			m.jobs, m.pending, m.trash = previousJobs, previousPending, previousTrash
		}
	}()
	for id, j := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Never follow a directory symlink from aggregate metadata.
		if st, e := os.Lstat(filepath.Join(m.store.Root, id)); e == nil && (st.Mode()&os.ModeSymlink != 0 || !st.IsDir()) {
			continue
		}
		if err := m.prepareRestoredJob(ctx, id, j, m.vault, m.accountOperationLease); err != nil {
			return err
		}
		if j.Status == StatusQueued {
			// Startup restores account facts, never execution authority. Even
			// an old queued intent needs an explicit new action after restart.
			j.Status, j.OperationStatus = StatusFailed, "interrupted"
			j.Prompt = "服务重启中断了上次操作，请重新选择要执行的功能"
			j.OperationError = j.Prompt
		}
		m.jobs[id] = j
	}
	for _, j := range m.jobs {
		if m.totpBlockedLocked(j.Email) && !m.totpResetHistoricallyCommittedLocked(j) {
			quarantineTOTP(j)
			j.Status, j.Error = StatusFailed, totpRecoveryDiagnostic(j, nil)
			j.OperationStatus, j.OperationError = "failed", j.Error
			m.removePendingLocked(j.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		return m.lastPersistError
	}
	committed = true
	for _, j := range m.jobs {
		if err := m.cleanupRestoredResults(j); err != nil {
			return err
		}
	}
	return nil
}

// cleanupRestoredResults runs only after the public recovery outcome commits.
// A successful credential write remains valid if a later step fails; its
// private result must remain available until the full local recovery can retry.
func (m *Manager) cleanupRestoredResults(j *Job) error {
	remove := func(name string, pending *bool) error {
		if !*pending {
			return nil
		}
		if err := os.Remove(filepath.Join(m.store.Root, j.ID, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		*pending = false
		return nil
	}
	if err := remove("password-add-result.json", &j.passwordCleanup); err != nil {
		return err
	}
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		return nil
	}
	return remove("totp-setup-result.json", &j.totpSetupCleanup)
}

// prepareRestoredJob reads private artifacts and credentials without requiring
// the manager lock. Callers reserve the account before a live import and pass
// the account lease callback captured while holding the manager lock.
func (m *Manager) prepareRestoredJob(ctx context.Context, id string, j *Job, vault credentials.Store, acquire func(string) (func() error, error)) error {
	// Restoring from trash resumes visibility, not an interrupted workflow.
	// Keep that contract across later service restarts: the next explicit
	// admitted operation may use the retained artifacts and protected inputs.
	if j.RecoveryPaused {
		return ctx.Err()
	}
	if OAuthGrantPath(m.store.Root, j) == "" {
		clearAuthenticationInputCandidates(j)
		j.Status, j.OperationStatus, j.Stage = StatusFailed, "failed", "invalid_oauth_result"
		j.Error, j.OperationError, j.Prompt = "本地授权结果标识无效，请重新发起独立操作", "本地授权结果标识无效，请重新发起独立操作", "未读取旧授权结果或连接远端"
		return ctx.Err()
	}
	j.ManualExpiresAt, _ = NormalizeExpiry(j.ManualExpiresAt)
	j.SubscriptionExpiresAt, _ = NormalizeExpiry(j.SubscriptionExpiresAt)
	savedStatus, savedOperationStatus := j.Status, j.OperationStatus
	savedPrompt, savedFinishedAt := j.Prompt, j.FinishedAt
	preserveTerminalTOTP := savedStatus == StatusFailed && (j.TOTPRecoveryPending || j.TOTPCredentialInvalidated)
	wasAutomaticRepair := isAutomaticRepair(j)
	if resetOperation(j.Operation) {
		j.ResetTOTP = true
	}
	artifactEmail, output, grantAvailable := recoveryArtifactsForEmail(m.store.Root, id, j.Email, j.OAuthResultID)
	if artifactEmail != "" && !strings.EqualFold(artifactEmail, j.Email) {
		output, grantAvailable = false, false
	}
	method, err := NormalizeAuthorizationMethod(j.AuthorizationMethod)
	if err == nil && j.AuthorizationMethod == "" {
		method, err = restoredAuthorizationMethod(m.store.Root, id, j.Email, output, grantAvailable, j.OAuthResultID)
	}
	if err != nil {
		return fmt.Errorf("restore authorization method for job %s: %w", id, err)
	}
	j.AuthorizationMethod = method
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	clearAuthenticationInputCandidates(j)
	// The intent file can outlive a failed metadata acknowledgement or a
	// locked vault. Read its public safety markers before any recovery branch.
	if b, readErr := readRegularJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json")); readErr == nil {
		var result map[string]any
		if json.Unmarshal(b, &result) == nil && result["version"] == float64(1) && resultBool(result, "reset_totp_requested") && strings.EqualFold(resultString(result, "email"), j.Email) {
			applyRestoredTOTPResetResult(j, result)
			preserveTerminalTOTP = preserveTerminalTOTP || savedStatus == StatusFailed && (j.TOTPRecoveryPending || j.TOTPCredentialInvalidated)
		}
	}
	artifactTime := time.Now().UTC()
	if !output && j.SubscriptionPlanCheckedAt != nil {
		j.SubscriptionPlanStale = true
	}
	if output {
		applySavedSubscriptionMetadata(m.store.Root, j)
		if stat, err := os.Stat(filepath.Join(m.store.Root, id, "sub2api-import-oauth.json")); err == nil {
			artifactTime = stat.ModTime().UTC()
		}
		if j.FinishedAt == nil {
			finished := artifactTime
			j.FinishedAt = &finished
		}
	} else if grantAvailable {
		if stat, err := os.Stat(OAuthGrantPath(m.store.Root, j)); err == nil {
			artifactTime = stat.ModTime().UTC()
		}
		j.FinishedAt = nil
	}

	if j.PhoneRetryRequired {
		clearPhoneInputCandidates(j)
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = artifactTime
	}
	if j.UpdatedAt.IsZero() {
		j.UpdatedAt = j.CreatedAt
	}
	if j.LastOperationAt.IsZero() {
		j.LastOperationAt = j.CreatedAt
	}
	if j.QueuedAt.IsZero() && j.Status != StatusImported {
		j.QueuedAt = j.CreatedAt
	}
	j.ResultSaved = output
	finalizeGrant := grantAvailable
	pendingRefresh := pendingRefreshOperation(m.store.Root, id, j.Operation)
	var recovered privateResultRecovery
	var saved credentials.Credentials
	var loadErr error = os.ErrNotExist
	if vault != nil && ((!finalizeGrant && pendingRefresh == "") || j.ResetTOTP) {
		saved, loadErr = credentials.LoadContext(ctx, vault, j.Email)
		if err := ctx.Err(); err != nil {
			return err
		}
		if loadErr == nil || errors.Is(loadErr, os.ErrNotExist) {
			var e error
			recovered, e = recoverPrivateResult(ctx, m.store.Root, j, vault, acquire)
			if e != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if m.logger != nil {
					m.logger.Error("private result recovery failed", "jobId", id, "error", safeError(e))
				}
			}
			saved, loadErr = credentials.LoadContext(ctx, vault, j.Email)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A failed origin/lock/journal read can stop recovery before it classifies
	// the private result. Existing reset safety evidence must still prevent an
	// unrelated mailbox migration from writing outside the recovery lease.
	allowCredentialMigration := !recovered.passwordError && !recovered.totpError && !recovered.totpReadOnly && (recovered.totp || (!j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated && (!j.ResetTOTP || j.TOTPResetPhase == "credential_persisted")))
	if legacyURL, _ := j.Input["mailApiUrl"].(string); legacyURL != "" && !finalizeGrant && pendingRefresh == "" && allowCredentialMigration {
		// A newer protected mailbox record wins over the legacy copy. When
		// a store is locked, preserve the parsed copy in memory but never
		// overwrite credentials that could not be read.
		if saved.MailAPIURL == "" {
			saved.MailAPIURL = legacyURL
			saved.MailBody, _ = j.Input["mailRequestBody"].(string)
			saved.MailMethod, _ = j.Input["mailRequestMethod"].(string)
			if vault != nil && (loadErr == nil || errors.Is(loadErr, os.ErrNotExist)) {
				if e := credentials.SaveContext(ctx, vault, j.Email, saved); e != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					j.MailAPIError = "旧邮箱资料未能保存到系统凭据存储，仅在本次进程内保留"
				}
			} else {
				j.MailAPIError = "旧邮箱资料未能保存到系统凭据存储，仅在本次进程内保留"
			}
		}
		j.Input["mailApiUrl"], j.Input["mailRequestBody"] = saved.MailAPIURL, saved.MailBody
		j.Input["mailRequestMethod"], j.Input["mailRequestHeaders"] = saved.MailMethod, saved.MailHeaders
		j.MailAPIURL = saved.MailAPIURL
	}
	if loadErr == nil {
		if saved.TOTPResetPending && !m.totpResetHistoricallyCommittedLocked(j) {
			quarantineTOTP(j)
		}
		j.MailAPIURL = saved.MailAPIURL
		if saved.Password != "" && !recovered.passwordError {
			j.LoginMode = "password"
			j.HasPassword = true
		}
		if saved.TOTPSecret != "" && !saved.TOTPResetPending && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated && !recovered.totpError {
			j.TOTPKnownEnabled = true
			j.HasTOTPKey = true
		}
		if saved.ProxyURL != "" {
			j.ProxyConfigured = true
		}
	}
	if pendingRefresh != "" {
		j.Status, j.Operation, j.Stage = StatusFailed, pendingRefresh, refreshRecoveryStage
		j.Prompt = "刷新授权已保存在私有恢复文件中，可以重试完成本地保存"
	} else if finalizeGrant {
		// The authorization code has already been consumed. Finishing this
		// result writes the durable grant locally and needs no login secret.
		j.Status, j.Operation = "resume_available", "resume"
		j.Error, j.LastError = "", ""
		j.Prompt = "OAuth 令牌已取得，可以继续保存授权文件"
	} else {
		restoreOperationState(j, output, recovered)
	}
	// Do not silently switch password/proxy identity after restart when secrets
	// could not be recovered. Explicit input/retry can resume this record.
	interruptedWithoutResult := !finalizeGrant && pendingRefresh == "" && !j.ResetTOTP && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated &&
		!output && !recovered.password && !recovered.totp && !recovered.passwordError && !recovered.totpError &&
		(savedStatus == StatusQueued || isTransientStatus(savedStatus) || savedStatus == "interrupted") && j.Status == StatusFailed
	if interruptedWithoutResult && ((j.HasPassword && saved.Password == "") || (j.HasTOTPKey && saved.TOTPSecret == "") || (j.ProxyConfigured && saved.ProxyURL == "")) {
		j.Status = "reauth_required"
		j.Prompt = "恢复所需凭据不可用，请重新提供登录资料"
	}
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		j.Status, j.Error = StatusFailed, totpRecoveryDiagnostic(j, nil)
	}
	if j.TOTPWorkflowStatus == "pending" || j.TOTPWorkflowStatus == "running" {
		interruptTOTPWorkflow(j, "服务重启中断了流程，请核实已完成步骤；不会自动重放操作")
		j.Status, j.Error, j.Prompt = StatusFailed, j.TOTPWorkflowError, j.TOTPWorkflowError
	}
	if j.UploadAfterRelogin && (j.ReloginUploadStatus == "pending" || j.ReloginUploadStatus == "running") {
		interruptReloginUpload(j, "服务重启中断了自动上传，请核实同步结果；不会自动重放操作")
		if j.Status == StatusQueued {
			j.Status, j.Error, j.Prompt = StatusFailed, "服务重启中断了重新授权及自动上传，请重新选择操作", "服务重启中断了重新授权及自动上传，请重新选择操作"
		}
	}
	if j.TOTPWorkflowStatus != "" && j.ReloginAfterTOTP && j.TOTPReloginStatus != "succeeded" {
		j.ResultSaved = false
	}
	if j.TOTPWorkflowStatus != "" && j.LogoutAllDevicesAfterTOTP && j.TOTPReloginStatus != "succeeded" && (j.TOTPWorkflowStep == "logout" || j.TOTPWorkflowStep == "relogin" || j.LogoutAllDevicesStatus == "succeeded" || j.LogoutAllDevicesStatus == "unknown") {
		j.ResultSaved = false
	}
	restoreOperationOutcome(j, savedStatus, savedOperationStatus, recovered)
	if wasAutomaticRepair {
		unfinished := savedStatus == StatusQueued || isTransientStatus(savedStatus) || savedStatus == "interrupted" || savedOperationStatus == "queued" || savedOperationStatus == "running" || savedOperationStatus == "waiting"
		if savedOperationStatus == "skipped" || savedOperationStatus == "canceled" || savedStatus == StatusCanceled {
			clearAutoRepairTargets(j)
		} else if unfinished && !grantAvailable && pendingRefresh == "" && !(j.UploadTarget == UploadTargetCPA && j.ReloginAuthorizationSaved && output) {
			// A queued automatic intent may predate the current monitor policy,
			// including a skip whose metadata write failed before replacement.
			// Only a fresh monitor eligibility pass may admit another login when
			// no private result exists. Saved one-time
			// grants retain their targets and existing manual finalization route;
			// they must never be replaced by a newly started authorization.
			clearAutoRepairTargets(j)
			if !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
				setAutoRepairSkipped(j, "服务已重启，旧自动重登任务未继续；后续由巡检重新检查资格", time.Now().UTC())
			}
		}
	}
	if preserveTerminalTOTP && !recovered.totp {
		j.FinishedAt = savedFinishedAt
		if savedPrompt != "" {
			j.Prompt = savedPrompt
		}
	}
	if IsRemovedAuthenticationOperation(j.Operation) {
		j.Status, j.OperationStatus, j.Stage = StatusFailed, "failed", "manual_review_required"
		j.Error, j.OperationError, j.Prompt = "旧的独立 2FA 核实或恢复操作已移除，需要人工处理", "旧的独立 2FA 核实或恢复操作已移除，需要人工处理", "不会恢复旧会话或自动发送远端请求"
		if strings.EqualFold(strings.TrimSpace(j.Operation), "recover-2fa") && !m.totpResetHistoricallyCommittedLocked(j) {
			quarantineTOTP(j)
		}
	}
	return ctx.Err()
}

func readRegularJSON(path string) ([]byte, error) {
	if filepath.Base(path) == "totp-setup-result.json" {
		return protocol.ReadTOTPResetJournalJSON(path)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 32<<20 {
		return nil, errors.New("invalid private data file")
	}
	return os.ReadFile(path)
}
func recoveryArtifacts(root, id string) (email string, output, grantAvailable bool) {
	return recoveryArtifactsForEmail(root, id, "")
}
func recoveryArtifactsForEmail(root, id, expectedEmail string, resultIDs ...string) (email string, output, grantAvailable bool) {
	if b, e := readRegularJSON(filepath.Join(root, id, "sub2api-import-oauth.json")); e == nil {
		var x struct {
			Type     string `json:"type"`
			Accounts []struct {
				Name        string `json:"name"`
				Credentials struct {
					Email string `json:"email"`
				} `json:"credentials"`
				Extra struct {
					Email string `json:"email"`
				} `json:"extra"`
			} `json:"accounts"`
		}
		if json.Unmarshal(b, &x) == nil && x.Type == "sub2api-data" && len(x.Accounts) > 0 {
			a := x.Accounts[0]
			email = strings.TrimSpace(a.Credentials.Email)
			if email == "" {
				email = strings.TrimSpace(a.Extra.Email)
			}
			if email == "" {
				email = strings.TrimPrefix(strings.TrimSpace(a.Name), "oauth---")
			}
			output = email != "" && (expectedEmail == "" || strings.EqualFold(email, expectedEmail))
			if !output {
				email = ""
			}
		}
	}
	resultID := ""
	if len(resultIDs) > 0 {
		resultID = resultIDs[0]
	}
	if grant, e := protocol.ReadSavedOAuthGrant(OAuthGrantPath(root, &Job{ID: id, OAuthResultID: resultID})); e == nil &&
		(expectedEmail == "" || strings.EqualFold(expectedEmail, grant.Email)) && (email == "" || strings.EqualFold(email, grant.Email)) {
		email, grantAvailable = grant.Email, true
	}
	return
}

func savedOAuthGrantAvailable(root, id, email, resultID string) bool {
	grant, err := protocol.ReadSavedOAuthGrant(OAuthGrantPath(root, &Job{ID: id, OAuthResultID: resultID}))
	return err == nil && strings.EqualFold(grant.Email, email)
}

type privateResultRecovery struct{ password, totp, passwordError, totpError, totpReadOnly bool }

func restoreOperationState(j *Job, output bool, recovered privateResultRecovery) {
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		j.Status, j.Error = StatusFailed, totpRecoveryDiagnostic(j, nil)
		j.Prompt = "2FA 重置未完成，已保留恢复资料；不会自动重放账号修改"
		return
	}
	if recovered.passwordError || recovered.totpError {
		j.Status = StatusFailed
		j.Error = j.TOTPSetupError
		if j.Error == "" {
			j.Error = j.PasswordAddError
		}
		j.Prompt = "设置结果需要重试安全保存，私有结果已保留；不会重新执行账号修改"
		if output {
			j.Status, j.Error, j.LastError = StatusSucceeded, "", ""
			j.Prompt = "原授权文件仍可下载，设置结果需要重试安全保存"
		}
		return
	}
	if totpOperation(j.Operation) && j.Error == "" && j.TOTPWorkflowStatus == "succeeded" && j.ReloginAfterTOTP && j.TOTPReloginStatus == "succeeded" && output {
		j.Status, j.Stage, j.Error, j.Prompt = StatusSucceeded, "complete", "", "2FA 与重新授权流程已完成"
		if j.UploadAfterTOTP {
			j.Prompt = "2FA、重新授权和 " + uploadTargetLabel(j) + " 同步已完成"
		}
		return
	}
	if j.ResetTOTP && recovered.totp {
		j.Status, j.Error, j.Prompt = StatusSucceeded, "", "已恢复并安全保存新的 2FA 密钥，未重新执行账号修改或退出"
		if !output {
			j.Status = "reauth_required"
			j.Prompt = "已恢复并安全保存新的 2FA 密钥；本地未保留可用授权文件，需要另行获取授权"
		}
		if j.SecurityActionReturnStatus == StatusImported {
			j.Status = StatusImported
			j.Prompt = "已恢复并安全保存新的 2FA 密钥，请选择要执行的功能"
		}
		return
	}
	if j.ResetTOTP && j.TOTPResetPhase != "credential_persisted" {
		j.Status, j.Prompt = StatusFailed, "服务重启中断了 2FA 重置，请核实状态后重新选择操作；不会自动续跑"
		if j.TOTPResetStatus == "" || j.TOTPResetStatus == "pending" {
			j.TOTPResetStatus = "failed"
		}
		return
	}
	savedStatus := j.Status
	if savedStatus == StatusImported {
		j.QueuedAt = time.Time{}
		return
	}
	if j.SecurityActionReturnStatus == StatusImported && isSuboperation(j.Operation) {
		j.Status, j.QueuedAt = StatusFailed, time.Time{}
		j.Prompt = "服务重启中断了所选操作，请重新选择要执行的功能"
		if recovered.password || recovered.totp {
			j.Status, j.Prompt = StatusImported, "已恢复设置结果，请选择要执行的功能"
		}
		return
	}
	terminal := savedStatus == StatusSucceeded || savedStatus == StatusFailed || savedStatus == StatusCanceled || savedStatus == "reauth_required" || savedStatus == "resume_available"
	interrupted := savedStatus != "" && !terminal
	passwordOperation := strings.HasPrefix(savedStatus, "password_add") || j.Operation == "password_add" || j.Operation == "add_password" || j.Operation == "add-password"
	if output {
		switch {
		case recovered.password || recovered.totp:
			j.Status, j.Error, j.LastError = StatusSucceeded, "", ""
			j.TOTPSetupError, j.PasswordAddError = "", ""
			j.Prompt = "已恢复并安全保存设置结果，原授权文件仍可下载"
		case passwordOperation && interrupted:
			j.Status, j.Error, j.LastError = StatusSucceeded, "", ""
			j.PasswordAddError = "服务重启中断了添加密码，请重新发起"
			j.Prompt = "原授权文件仍可下载，添加密码尚未完成"
		case savedStatus == "":
			j.Status = StatusSucceeded
		case interrupted:
			j.Status = StatusFailed
			j.Prompt = "上次操作因服务重启中断，旧授权文件仍可下载"
			if j.Error == "" {
				j.Error = "上次操作未完成，请手动重试"
			}
			if strings.HasPrefix(savedStatus, "totp_") || isSuboperation(j.Operation) {
				j.TOTPSetupError = "服务重启时 2FA 设置未完成"
			}
		}
		return
	}

	// An old queue/preflight record is not a new authorization to execute.
	// Retain its account data for an explicit action, including after a write
	// whose durability was uncertain to the submitting process.
	switch {
	case savedStatus == StatusSucceeded && !isSuboperation(j.Operation):
		j.Status = "reauth_required"
	case interrupted || savedStatus == "":
		j.Status = StatusFailed
		j.Prompt = "上次流程因服务重启中断，可以重新授权"
		if j.Error == "" {
			j.Error = "上次流程未完成，请重新发起独立登录"
		}
	}
}

func recoverPrivateResult(ctx context.Context, root string, j *Job, vault credentials.Store, acquire func(string) (func() error, error)) (recovered privateResultRecovery, err error) {
	if root == "" || j == nil || vault == nil {
		return recovered, nil
	}
	// Startup restores facts only. A recorded activation is eligible for an
	// explicit local completion, never for automatic login or remote replay.
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		recovered.totpReadOnly = true
		quarantineTOTP(j)
	}
	totpPath := filepath.Join(root, j.ID, "totp-setup-result.json")
	var lockedTOTP []byte
	totpObserved := false
	if before, readErr := readRegularJSON(totpPath); readErr == nil {
		totpObserved = true
		// A sibling password result also writes the whole credential value.
		// Reserve the ordinary account lease before reading or changing it.
		if acquire == nil {
			acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
		}
		release, lockErr := acquire(j.Email)
		if lockErr != nil {
			if release != nil {
				_ = release()
			}
			quarantineTOTP(j)
			recovered.totpReadOnly = true
			return recovered, lockErr
		}
		if release == nil {
			quarantineTOTP(j)
			recovered.totpReadOnly = true
			return recovered, errors.New("本地恢复没有取得有效的普通账号操作锁")
		}
		defer release()
		latest, readErr := readRegularJSON(totpPath)
		if readErr != nil || string(latest) != string(before) {
			quarantineTOTP(j)
			recovered.totpReadOnly, recovered.totpError = true, true
			return recovered, errors.New("2FA 操作记录已变化，未覆盖凭据，请人工核实")
		}
		lockedTOTP = latest
	} else if !errors.Is(readErr, os.ErrNotExist) {
		quarantineTOTP(j)
		recovered.totpReadOnly, recovered.totpError = true, true
		return recovered, errors.New("2FA 私有操作记录无法读取，未修改凭据，资料已保留")
	}
	current, err := credentials.LoadContext(ctx, vault, j.Email)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return recovered, err
	}
	if current.TOTPResetPending {
		recovered.totpReadOnly = true
	}
	if totpObserved {
		latest, readErr := readRegularJSON(totpPath)
		if readErr != nil || string(latest) != string(lockedTOTP) {
			quarantineTOTP(j)
			recovered.totpReadOnly, recovered.totpError = true, true
			return recovered, errors.New("2FA 操作记录读取失败或已变化，未修改凭据")
		}
	}
	if totpObserved {
		var result map[string]any
		if json.Unmarshal(lockedTOTP, &result) != nil {
			quarantineTOTP(j)
			recovered.totpReadOnly, recovered.totpError = true, true
			return recovered, errors.New("2FA 操作记录无法解析，未修改凭据")
		}
		if resultBool(result, "reset_totp_requested") {
			recovered.totpReadOnly = true
			if result["version"] != float64(1) || !strings.EqualFold(resultString(result, "email"), j.Email) {
				quarantineTOTP(j)
				return recovered, errors.New("2FA 操作记录与账号不匹配，已保留私有资料")
			}
			applyRestoredTOTPResetResult(j, result)
			if protocol.ValidateTOTPResetStart(totpPath, j.Email) == nil && !current.TOTPResetPending && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
				return recovered, ctx.Err()
			}
			completion, proofErr := protocol.ReadTOTPResetCompletion(totpPath, j.Email)
			bound := proofErr == nil && totpCompletionMatchesVault(completion, current) &&
				j.TOTPResetOperationID != "" && j.TOTPResetOperationID == resultString(completion, "operation_id")
			if proofErr == nil && j.TOTPResetCommitted && j.TOTPResetOperationID == resultString(completion, "operation_id") &&
				!j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
				j.TOTPActivationVerified = true
				j.TOTPResetStatus, j.TOTPResetPhase = "activated", "credential_persisted"
				j.HasTOTPKey, j.TOTPKnownEnabled = true, true
				return recovered, ctx.Err()
			}
			j.TOTPResetCommitted = false
			quarantineTOTP(j)
			if bound {
				j.TOTPActivationVerified = true
				j.TOTPResetError = "新 2FA 已确认；需要显式完成本地保存，不会重新登录或修改远端"
				return recovered, ctx.Err()
			}
			recovered.totpError = true
			return recovered, errors.New("2FA 激活证据或凭据绑定不完整，未修改凭据，需要人工处理")
		}
	}
	if recovered.totpReadOnly || j.ResetTOTP && j.TOTPResetCommitted {
		// Neither missing evidence nor old success metadata can release quarantine.
		j.TOTPResetCommitted = false
		quarantineTOTP(j)
		recovered.totpReadOnly, recovered.totpError = true, true
		return recovered, errors.New("2FA 私有操作证据缺失或不匹配，未修改凭据")
	}
	passwordPath := filepath.Join(root, j.ID, "password-add-result.json")
	if b, e := readRegularJSON(passwordPath); e == nil {
		var x struct {
			Version  int        `json:"version"`
			Email    string     `json:"email"`
			Password string     `json:"password"`
			AddedAt  *time.Time `json:"added_at"`
		}
		if json.Unmarshal(b, &x) == nil && x.Version <= 1 && strings.EqualFold(x.Email, j.Email) && len(x.Password) >= 12 {
			current.Password = x.Password
			current.PendingPassword = ""
			if err = saveRecoveredCredentials(ctx, vault, j.Email, current); err != nil {
				recovered.passwordError = true
				j.HasPassword = false
				j.PasswordAddError = "新密码恢复到系统凭据存储失败或读回不一致，私有结果已保留"
				return recovered, err
			}
			if err = ctx.Err(); err != nil {
				return recovered, err
			}
			recovered.password = true
			j.HasPassword = true
			j.PasswordAddedAt = x.AddedAt
			j.passwordCleanup = true
		}
	}
	if b, e := readRegularJSON(totpPath); e == nil {
		var x struct {
			Version   int    `json:"version"`
			Email     string `json:"email"`
			Secret    string `json:"secret"`
			Activated bool   `json:"activation_succeeded"`
		}
		if current.TOTPResetPending || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
			// An older setup result must never replace a quarantined reset seed.
			return recovered, nil
		}
		if json.Unmarshal(b, &x) == nil && x.Version <= 1 && strings.EqualFold(x.Email, j.Email) && x.Activated && x.Secret != "" {
			if current.TOTPResetOperationID != "" && current.TOTPSecret != x.Secret {
				recovered.totpError = true
				j.TOTPSetupError = "历史 2FA 设置结果与当前已重置密钥不同，未覆盖凭据，私有结果已保留"
				return recovered, errors.New(j.TOTPSetupError)
			}
			current.TOTPSecret = x.Secret
			current.PendingTOTPSecret, current.PendingTOTPURI = "", ""
			if err = saveRecoveredCredentials(ctx, vault, j.Email, current); err != nil {
				recovered.totpError = true
				j.HasTOTPKey = false
				j.TOTPSetupError = "2FA密钥恢复到系统凭据存储失败或读回不一致，私有结果已保留"
				return recovered, err
			}
			if err = ctx.Err(); err != nil {
				return recovered, err
			}
			recovered.totp = true
			j.HasTOTPKey = true
			j.TOTPKnownEnabled = true
			j.totpSetupCleanup = true
		}
	}
	return recovered, ctx.Err()
}

// A store acknowledgement alone does not authorize removing the only private
// result. Read back the complete value, including unrelated protected fields.
func saveRecoveredCredentials(ctx context.Context, vault credentials.Store, email string, expected credentials.Credentials) error {
	if err := credentials.SaveContext(ctx, vault, email, expected); err != nil {
		return err
	}
	actual, err := credentials.LoadContext(ctx, vault, email)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("恢复凭据读回与完整保存内容不一致")
	}
	return nil
}
