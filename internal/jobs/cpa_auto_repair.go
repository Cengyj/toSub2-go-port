package jobs

import (
	"errors"
	"fmt"
	"strings"

	"tosub2-go-port/internal/accountplan"
)

// SetCPAAutoRepairPolicy keeps CPA execution decisions independent of Sub2API.
// Applications set this before starting the scheduler, even when disabled.
func (m *Manager) SetCPAAutoRepairPolicy(policy AutoRepairPolicy) {
	copy := copyAutoRepairPolicy(policy)
	m.mu.Lock()
	m.cpaAutoRepairPolicy = &copy
	m.mu.Unlock()
	m.wakeScheduler()
}

func isCPAAutomaticRepair(job *Job) bool {
	return isAutomaticRepair(job) && job.UploadTarget == UploadTargetCPA
}

// QueueCPAAutoRepair freezes one matching remote identity and the prepared CPA
// configuration for a relogin/upload workflow under the scheduler lock. The
// console must verify the remote file, recoverable login and current settings.
func (m *Manager) QueueCPAAutoRepair(id, backend string, values map[string]any, remotePlans ...string) (*Job, error) {
	accountID, _ := values["cpaRepairAccountId"].(string)
	if accountID == "" || accountID != strings.TrimSpace(accountID) || len(accountID) > 512 {
		return nil, errors.New("CPA 自动修复缺少已核对的远端账号身份")
	}
	if values["cpaConfig"] == nil {
		return nil, errors.New("CPA 自动修复缺少上传配置")
	}
	frozen := map[string]any{
		"uploadAfterRelogin": true,
		"uploadTarget":       UploadTargetCPA,
		"cpaConfig":          cloneValue(values["cpaConfig"]),
		"cpaRepairAccountId": accountID,
	}
	return m.queueAutoRepair(id, nil, backend, frozen, remotePlans...)
}

func (m *Manager) cpaAutoRepairPolicyReasonLocked(job *Job, grantPlan string, uploading bool) string {
	if !isCPAAutomaticRepair(job) {
		return ""
	}
	action := "排队的自动重登"
	if uploading {
		action = "新授权的自动上传"
	}
	policy := m.cpaAutoRepairPolicy
	if policy == nil || !policy.Enabled {
		return "CPA 自动巡检已关闭，已跳过" + action
	}
	if policy.Backend == "" || policy.Backend != job.AutoRepairBackend {
		return "CPA 配置已变更或不可用，已跳过" + action
	}
	if accountplan.AutomaticRepairExcluded(job.PlanType, job.AutoRepairQueuedRemotePlanTypes, policy.ExcludedPlanTypes) ||
		uploading && grantPlan != "" && accountplan.AutomaticRepairExcluded(grantPlan, job.AutoRepairQueuedRemotePlanTypes, policy.ExcludedPlanTypes) {
		return "当前本地、新授权或远端套餐已列入排除范围，已跳过" + action
	}
	return ""
}

// ErrAutoRepairUploadPaused reports that the new grant was retained while
// current policy or the frozen remote identity prevented its optional upload.
var ErrAutoRepairUploadPaused = errors.New("自动修复上传已暂停")

func (m *Manager) pauseCPAAutoRepairUploadLocked(job *Job, reason string) error {
	job.AutoRepairSkippedReason = reason
	job.AutoRepairSyncPaused = true
	m.persistLocked()
	if m.lastPersistError != nil {
		return fmt.Errorf("%w：%s；暂停状态未确认保存", ErrAutoRepairUploadPaused, reason)
	}
	return fmt.Errorf("%w：%s", ErrAutoRepairUploadPaused, reason)
}

// CheckCPAAutoRepairUpload is called immediately before writing the new grant
// to CPA, after any slow quality check. Manual uploads are unaffected.
func (m *Manager) CheckCPAAutoRepairUpload(id, grantPlan, accountID string, currentRemotePlans ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return osErrNotFound(id)
	}
	if !isCPAAutomaticRepair(job) {
		return nil
	}
	if reason := m.cpaAutoRepairPolicyReasonLocked(job, grantPlan, true); reason != "" {
		return m.pauseCPAAutoRepairUploadLocked(job, reason)
	}
	if len(currentRemotePlans) > 0 && (accountplan.AutomaticRepairExcluded(job.PlanType, currentRemotePlans, m.cpaAutoRepairPolicy.ExcludedPlanTypes) || accountplan.AutomaticRepairExcluded(grantPlan, currentRemotePlans, m.cpaAutoRepairPolicy.ExcludedPlanTypes)) {
		return m.pauseCPAAutoRepairUploadLocked(job, "CPA 当前远端套餐已列入排除范围，已保留新授权并跳过自动上传")
	}
	expected, _ := job.Input["cpaRepairAccountId"].(string)
	if expected == "" || accountID != expected {
		return m.pauseCPAAutoRepairUploadLocked(job, "新授权的账号身份与原 CPA 文件不一致，已保留新授权，请手动核实")
	}
	return nil
}

// PauseCPAAutoRepairUpload retains the new grant when the last remote read no
// longer confirms that its original file is eligible for automatic repair.
func (m *Manager) PauseCPAAutoRepairUpload(id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return osErrNotFound(id)
	}
	if !isCPAAutomaticRepair(job) {
		return ErrAutoRepairSuperseded
	}
	return m.pauseCPAAutoRepairUploadLocked(job, safeError(errors.New(reason)))
}
