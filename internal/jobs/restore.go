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
	"sort"
	"strings"
	"time"

	"tosub2-go-port/internal/credentials"
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
		if b := normalized[compact(tag)]; len(b) > 0 {
			if err := json.Unmarshal(b, value.Field(i).Addr().Interface()); err != nil {
				return nil, false
			}
		}
	}
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
	records := map[string]*Job{}
	if b, ok := raw["jobs"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("restore jobs: %w", err)
		}
		for _, b := range list {
			if j, ok := decodeLegacyJob(b, ""); ok && safeID(j.ID) {
				records[j.ID] = j
			}
		}
	} else if len(raw) > 0 {
		b, _ := json.Marshal(raw)
		if j, ok := decodeLegacyJob(b, ""); ok && safeID(j.ID) {
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
		b, _ := readRegularJSON(filepath.Join(m.store.Root, id, "job-meta.json"))
		j, ok := decodeLegacyJob(b, id)
		if current, exists := records[id]; exists {
			if ok && strings.EqualFold(current.Email, j.Email) && len(current.Input) == 0 {
				current.Input = j.Input
			}
			continue
		}
		if !ok {
			email, output, checkpoint := recoveryArtifacts(m.store.Root, id)
			if email == "" || (!output && !checkpoint) {
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
		records[id] = j
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) > 0 || len(m.jobs) > 0 {
		return errors.New("restore requires an empty, idle manager")
	}
	for id, j := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Never follow a directory symlink from aggregate metadata.
		if st, e := os.Lstat(filepath.Join(m.store.Root, id)); e == nil && (st.Mode()&os.ModeSymlink != 0 || !st.IsDir()) {
			continue
		}
		if err := m.prepareRestoredJob(ctx, id, j, m.vault); err != nil {
			return err
		}
		m.jobs[id] = j
		if j.Status == StatusQueued {
			m.pending = append(m.pending, j)
		}
	}
	// A reset quarantine is account-wide, including older records restored
	// before the result that established the uncertain mutation.
	for _, completed := range m.jobs {
		if !completed.totpRecoveryCleanup {
			continue
		}
		for _, related := range m.jobs {
			if emailKey(related.Email) == emailKey(completed.Email) && !related.ResetTOTP {
				related.TOTPRecoveryPending, related.TOTPCredentialInvalidated = false, false
				related.HasTOTPKey, related.TOTPKnownEnabled = true, true
			}
		}
	}
	for _, j := range m.jobs {
		if m.totpBlockedLocked(j.Email) {
			quarantineTOTP(j)
			j.Status, j.Error = StatusFailed, ErrTOTPRecoveryPending.Error()
			j.OperationStatus, j.OperationError = "failed", j.Error
			m.removePendingLocked(j.ID)
		}
	}
	sort.Slice(m.pending, func(i, j int) bool {
		a, b := m.pending[i], m.pending[j]
		if a.QueuedAt.Equal(b.QueuedAt) {
			if a.CreatedAt.Equal(b.CreatedAt) {
				return a.ID < b.ID
			}
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.QueuedAt.Before(b.QueuedAt)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	m.persistLocked()
	if m.lastPersistError == nil {
		for _, j := range m.jobs {
			if j.totpRecoveryCleanup && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
				if err := os.Remove(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				j.totpRecoveryCleanup = false
			}
		}
	}
	return m.lastPersistError
}

// prepareRestoredJob reads private artifacts and credentials without requiring
// the manager lock. Callers reserve the account before a live import.
func (m *Manager) prepareRestoredJob(ctx context.Context, id string, j *Job, vault credentials.Store) error {
	savedStatus, savedOperationStatus := j.Status, j.OperationStatus
	if resetOperation(j.Operation) {
		j.ResetTOTP = true
	}
	artifactEmail, output, checkpoint := recoveryArtifactsForEmail(m.store.Root, id, j.Email)
	if artifactEmail != "" && !strings.EqualFold(artifactEmail, j.Email) {
		output, checkpoint = false, false
	}
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	// The intent file can outlive a failed metadata acknowledgement or a
	// locked vault. Read its public safety markers before any recovery branch.
	if b, readErr := readRegularJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json")); readErr == nil {
		var result map[string]any
		if json.Unmarshal(b, &result) == nil && result["version"] == float64(1) && resultBool(result, "reset_totp_requested") && strings.EqualFold(resultString(result, "email"), j.Email) {
			applyTOTPResetResult(j, result, nil)
		}
	}
	artifactTime := time.Now().UTC()
	if output {
		if planType := outputPlanType(m.store.Root, id, j.Email); planType != "unknown" {
			j.PlanType = planType
		}
		if stat, err := os.Stat(filepath.Join(m.store.Root, id, "sub2api-import-oauth.json")); err == nil {
			artifactTime = stat.ModTime().UTC()
		}
		if j.FinishedAt == nil {
			finished := artifactTime
			j.FinishedAt = &finished
		}
	} else if checkpoint {
		if stat, err := os.Stat(filepath.Join(m.store.Root, id, "login-checkpoint.json")); err == nil {
			artifactTime = stat.ModTime().UTC()
		}
		j.FinishedAt = nil
		if phone := checkpointPhone(m.store.Root, id); phone != "" {
			j.CurrentPhone = phone
		}
		if checkpointPhoneRetryRequired(m.store.Root, id) {
			j.PhoneRetryRequired = true
		}
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
	finalizeGrant := checkpoint && checkpointStage(m.store.Root, id) == "tokens_received"
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
			recovered, e = recoverPrivateResult(ctx, m.store.Root, j, vault)
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
	if legacyURL, _ := j.Input["mailApiUrl"].(string); legacyURL != "" && !finalizeGrant && pendingRefresh == "" {
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
		if saved.TOTPResetPending {
			quarantineTOTP(j)
		}
		j.MailAPIURL = saved.MailAPIURL
		if saved.Password != "" {
			j.LoginMode = "password"
			j.HasPassword = true
		}
		if saved.TOTPSecret != "" && !saved.TOTPResetPending && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
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
		// checkpoint writes the durable grant locally and needs no login secret.
		j.Status, j.Operation = "resume_available", "resume"
		j.Error, j.LastError = "", ""
		j.Prompt = "OAuth 令牌已取得，可以继续保存授权文件"
	} else {
		restoreOperationState(j, output, checkpoint, recovered)
	}
	// Do not silently switch password/proxy identity after restart when secrets
	// could not be recovered. Explicit input/retry can resume this record.
	if j.Status == StatusQueued && ((j.HasPassword && saved.Password == "") || (j.HasTOTPKey && saved.TOTPSecret == "") || (j.ProxyConfigured && saved.ProxyURL == "")) {
		j.Status = "reauth_required"
		j.Prompt = "恢复所需凭据不可用，请重新提供登录资料"
	}
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		j.Status, j.Error = StatusFailed, ErrTOTPRecoveryPending.Error()
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
	return ctx.Err()
}

func readRegularJSON(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 32<<20 {
		return nil, errors.New("invalid private data file")
	}
	return os.ReadFile(path)
}
func recoveryArtifacts(root, id string) (email string, output, checkpoint bool) {
	return recoveryArtifactsForEmail(root, id, "")
}
func recoveryArtifactsForEmail(root, id, expectedEmail string) (email string, output, checkpoint bool) {
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
	if b, e := readRegularJSON(filepath.Join(root, id, "login-checkpoint.json")); e == nil {
		var x struct {
			Version int    `json:"version"`
			Email   string `json:"email"`
		}
		if json.Unmarshal(b, &x) == nil && x.Version == 1 && x.Email != "" && (expectedEmail == "" || strings.EqualFold(expectedEmail, x.Email)) && (email == "" || strings.EqualFold(email, x.Email)) {
			email = x.Email
			checkpoint = true
		}
	}
	return
}

func checkpointStage(root, id string) string {
	b, err := readRegularJSON(filepath.Join(root, id, "login-checkpoint.json"))
	if err != nil {
		return ""
	}
	var cp struct {
		Stage string `json:"stage"`
	}
	if json.Unmarshal(b, &cp) != nil {
		return ""
	}
	return cp.Stage
}

func checkpointPhone(root, id string) string {
	b, err := readRegularJSON(filepath.Join(root, id, "login-checkpoint.json"))
	if err != nil {
		return ""
	}
	var cp struct {
		OAuth struct {
			Phone string `json:"phone"`
		} `json:"oauth"`
	}
	if json.Unmarshal(b, &cp) != nil {
		return ""
	}
	return cp.OAuth.Phone
}

func checkpointPhoneRetryRequired(root, id string) bool {
	b, err := readRegularJSON(filepath.Join(root, id, "login-checkpoint.json"))
	if err != nil {
		return false
	}
	var cp struct {
		OAuth struct {
			PhoneRetryRequired bool `json:"phoneRetryRequired"`
		} `json:"oauth"`
	}
	return json.Unmarshal(b, &cp) == nil && cp.OAuth.PhoneRetryRequired
}

type privateResultRecovery struct{ password, totp, passwordError, totpError bool }

func restoreOperationState(j *Job, output, checkpoint bool, recovered privateResultRecovery) {
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		j.Status, j.Error = StatusFailed, ErrTOTPRecoveryPending.Error()
		j.Prompt = "2FA 重置未完成，已保留恢复资料；不会自动重放账号修改"
		return
	}
	if totpOperation(j.Operation) && j.Error == "" && j.TOTPWorkflowStatus == "succeeded" && j.ReloginAfterTOTP && j.TOTPReloginStatus == "succeeded" && output {
		j.Status, j.Stage, j.Error, j.Prompt = StatusSucceeded, "complete", "", "2FA 与重新授权流程已完成"
		if j.UploadAfterTOTP {
			j.Prompt = "2FA、重新授权和 Sub2API 同步已完成"
		}
		return
	}
	if j.ResetTOTP && recovered.totp {
		j.Status, j.Error, j.Prompt = StatusSucceeded, "", "已恢复并安全保存新的 2FA 密钥，未重新执行账号修改或退出"
		if !output {
			j.Status = "reauth_required"
			j.Prompt = "已恢复并安全保存新的 2FA 密钥；原登录会话已失效，请重新登录获取授权"
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
		case recovered.passwordError || recovered.totpError:
			j.Status, j.Error, j.LastError = StatusSucceeded, "", ""
			j.Prompt = "原授权文件仍可下载，设置结果需要重试安全保存"
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
	if checkpoint {
		if recovered.password || recovered.totp || savedStatus == "" || interrupted {
			j.Status, j.Error, j.LastError = "resume_available", "", ""
			j.Prompt = "登录检查点已保留，可以继续未完成的授权"
		}
		return
	}
	// Only a queue/preflight stage with no authorization state is safe to
	// automatically restart. A consumed OTP or remote account mutation is not.
	restartable := savedStatus == StatusQueued || savedStatus == "starting" || (savedStatus == "interrupted" && j.Stage == "start" && !isSuboperation(j.Operation))
	switch {
	case restartable && !recovered.password && !recovered.totp:
		j.Status = StatusQueued
	case savedStatus == StatusSucceeded && !isSuboperation(j.Operation):
		j.Status = "reauth_required"
	case interrupted || savedStatus == "":
		j.Status = StatusFailed
		j.Prompt = "上次流程因服务重启中断，可以重新授权"
		if j.Error == "" {
			j.Error = "上次流程未完成，未发现可恢复的登录检查点"
		}
	}
}

func recoverPrivateResult(ctx context.Context, root string, j *Job, vault credentials.Store) (recovered privateResultRecovery, err error) {
	if root == "" || j == nil || vault == nil {
		return recovered, nil
	}
	current, err := credentials.LoadContext(ctx, vault, j.Email)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return recovered, err
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
			if err = credentials.SaveContext(ctx, vault, j.Email, current); err != nil {
				recovered.passwordError = true
				j.PasswordAddError = "新密码恢复到系统凭据存储失败，私有结果已保留"
				return recovered, err
			}
			if err = ctx.Err(); err != nil {
				return recovered, err
			}
			recovered.password = true
			j.HasPassword = true
			j.PasswordAddedAt = x.AddedAt
			if err = os.Remove(passwordPath); err != nil {
				return recovered, err
			}
		}
	}
	totpPath := filepath.Join(root, j.ID, "totp-setup-result.json")
	if b, e := readRegularJSON(totpPath); e == nil {
		var result map[string]any
		if json.Unmarshal(b, &result) == nil && resultBool(result, "reset_totp_requested") {
			if result["version"] != float64(1) || !strings.EqualFold(resultString(result, "email"), j.Email) {
				quarantineTOTP(j)
				return recovered, errors.New("2FA 重置恢复结果与账号不匹配，已保留私有资料")
			}
			applyTOTPResetResult(j, result, nil)
			if _, memoryOnly := vault.(*credentials.MemoryStore); memoryOnly {
				quarantineTOTP(j)
				return recovered, errors.New("2FA 重置恢复需要持久安全凭据存储")
			}
			storedResetPending := current.TOTPResetPending
			if j.TOTPCredentialInvalidated || j.TOTPRecoveryPending {
				current.TOTPResetPending = true
			}
			secret := resultString(result, "secret")
			activated := resultBool(result, "activation_succeeded") && secret != ""
			if activated && !storedResetPending && current.TOTPSecret != "" && current.TOTPSecret != secret {
				quarantineTOTP(j)
				recovered.totpError = true
				return recovered, errors.New("已保存的有效 2FA 密钥与历史恢复结果不同，未覆盖当前凭据，恢复资料已保留")
			}
			// A local save failure can change the terminal status to unknown
			// without undoing the separate, confirmed remote verification fact.
			verified := activated && resultBool(result, "activation_verified")
			if activated {
				current.TOTPSecret = secret
				current.PendingTOTPSecret, current.PendingTOTPURI = "", ""
				current.TOTPResetPending = !verified
			}
			if activated || current.TOTPResetPending {
				if err = credentials.SaveContext(ctx, vault, j.Email, current); err != nil {
					quarantineTOTP(j)
					recovered.totpError = true
					return recovered, err
				}
				readback, readErr := credentials.LoadContext(ctx, vault, j.Email)
				if readErr != nil || !reflect.DeepEqual(current, readback) {
					quarantineTOTP(j)
					recovered.totpError = true
					return recovered, errors.New("2FA 重置恢复凭据读回不一致，私有结果已保留")
				}
			}
			if verified {
				j.TOTPRecoveryPending, j.TOTPCredentialInvalidated = false, false
				j.HasTOTPKey, j.TOTPKnownEnabled = true, true
				j.TOTPResetStatus, j.TOTPResetPhase, j.TOTPResetError = "activated", "credential_persisted", ""
				j.totpRecoveryCleanup, recovered.totp = true, true
			} else if activated || current.TOTPResetPending {
				quarantineTOTP(j)
			}
			return recovered, ctx.Err()
		}
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
			current.TOTPSecret = x.Secret
			current.PendingTOTPSecret, current.PendingTOTPURI = "", ""
			if err = credentials.SaveContext(ctx, vault, j.Email, current); err != nil {
				recovered.totpError = true
				j.TOTPSetupError = "2FA密钥恢复到系统凭据存储失败，私有结果已保留"
				return recovered, err
			}
			if err = ctx.Err(); err != nil {
				return recovered, err
			}
			recovered.totp = true
			j.HasTOTPKey = true
			j.TOTPKnownEnabled = true
			if err = os.Remove(totpPath); err != nil {
				return recovered, err
			}
		}
	}
	return recovered, ctx.Err()
}
