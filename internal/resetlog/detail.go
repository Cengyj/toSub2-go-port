// Package resetlog defines the public, display-only projection of reset evidence.
// It has no transport, credentials, or recovery dependencies.
package resetlog

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const DataKey = "reset_diagnostic"

// Received means HTTP responded, not that the action succeeded. Possible means
// transport could not prove whether it sent the request. Unsent is proven.
type Count struct {
	Received int `json:"received"`
	Possible int `json:"possible"`
	Unsent   int `json:"unsent"`
}
type Writes struct {
	Disable    Count `json:"remove"`
	Initial    Count `json:"initial_enroll"`
	Enroll     Count `json:"enroll"`
	Activation Count `json:"activate"`
	Logout     Count `json:"logout"`
}
type Detail struct {
	OpRef         string  `json:"op_ref"`
	Seq           int     `json:"seq"`
	AttemptRef    string  `json:"attempt_ref,omitempty"`
	Stage         string  `json:"stage"`
	Reason        string  `json:"reason_code"`
	Source        string  `json:"evidence_source"`
	Next          string  `json:"next_action"`
	Terminal      bool    `json:"terminal,omitempty"`
	Method        string  `json:"method,omitempty"`
	Path          string  `json:"path,omitempty"`
	Dispatch      string  `json:"dispatch,omitempty"`
	HTTP          int     `json:"http_status,omitempty"`
	Receipt       string  `json:"receipt,omitempty"`
	Business      string  `json:"business_code,omitempty"`
	Started       string  `json:"started_at,omitempty"`
	Ended         string  `json:"ended_at,omitempty"`
	DurationMS    *int64  `json:"duration_ms,omitempty"`
	Remaining     *int    `json:"read_attempts_remaining,omitempty"`
	Field         string  `json:"field,omitempty"`
	ExpectedType  string  `json:"expected_type,omitempty"`
	ActualType    string  `json:"actual_type,omitempty"`
	Expected      string  `json:"expected_factor,omitempty"`
	Visible       string  `json:"visible_factor,omitempty"`
	VisibleCount  *int    `json:"visible_count,omitempty"`
	LastTrusted   string  `json:"last_trusted_step,omitempty"`
	Removal       string  `json:"old_removal,omitempty"`
	Pending       string  `json:"pending_readback,omitempty"`
	Material      string  `json:"journal_enrollment_material,omitempty"`
	Activation    string  `json:"activation_receipt,omitempty"`
	Match         string  `json:"factor_match,omitempty"`
	Commit        string  `json:"local_commit,omitempty"`
	Logout        string  `json:"logout,omitempty"`
	Writes        *Writes `json:"writes_this_run,omitempty"`
	JournalWrites *Writes `json:"logical_writes_from_journal,omitempty"`
	OldVisible    bool    `json:"old_also_visible,omitempty"`
	Cause         string  `json:"cause_reason,omitempty"`
	WriteIssue    string  `json:"last_write_issue,omitempty"`
	WriteAttempt  string  `json:"write_issue_attempt,omitempty"`
	Related       string  `json:"related_attempt,omitempty"`
	Access        string  `json:"access,omitempty"`
}

// Only pass the local random operation UUID, never an account or credential.
func OperationRef(operationID string) string {
	sum := sha256.Sum256([]byte("reset display:" + operationID))
	return fmt.Sprintf("O%x", sum[:8])
}

type label struct{ message, next, level string }

var reasons = map[string]label{
	"INTERRUPTED_WRITE":                 {"进程中断；只找到写前意图，可能已发，未取得回执；不再发送", "review_receipt", "warning"},
	"INTERRUPTED_RESET":                 {"进程中断；按受保护记录列出最后可信步骤，未重放请求", "review_evidence", "warning"},
	"DISPATCH_INTENT":                   {"已记录写前意图，可能已发；未取得回执", "await_receipt", "info"},
	"UNCERTAIN_COMMIT":                  {"尚不能证明本地恢复记录可靠，请先检查安全存储", "check_storage", "warning"},
	"IDENTITY_VERIFIED":                 {"登录身份与目标账号一致，开始读取因子状态", "read_status", "info"},
	"IDENTITY_UNVERIFIED":               {"当前身份或账号未核对，停止修改", "review_identity", "warning"},
	"FORMAL_CONFLICT":                   {"正式登记提示已有因子；不再移除或登记", "review_binding", "warning"},
	"FIELD_INVALID":                     {"回执中的关键字段不完整或类型不符", "review_receipt", "warning"},
	"ENROLLMENT_BOUND":                  {"已取得本次登记的密钥、登记会话和普通因子绑定", "save_pending", "info"},
	"REQUEST_COMPLETE":                  {"已收到完整 HTTP 回包，业务结果另行判断", "classify_receipt", "info"},
	"CONTRADICTORY_RECEIPT":             {"回包同时表示激活和停用，不能作为成功凭据；仅核对状态，不重发修改", "read_status", "warning"},
	"HTTP_REFUSED":                      {"请求被拒绝，未把 HTTP 响应当作成功", "review_receipt", "warning"},
	"SESSION_REVOKED":                   {"业务授权已撤销；停止本会话的修改", "reauth_manually", "warning"},
	"AUTH_REQUIRED":                     {"需要重新认证或授权；本会话不再写入", "reauth_manually", "warning"},
	"CHALLENGE_REQUIRED":                {"服务端要求额外验证，未按空状态处理", "reauth_manually", "warning"},
	"RATE_LIMITED":                      {"请求被限流，未自动重发写请求", "review_receipt", "warning"},
	"SERVER_ERROR":                      {"服务端异常；写入结果不能由状态码确定", "review_receipt", "warning"},
	"REQUEST_TIMEOUT":                   {"请求超时，不能推断写入结果", "review_receipt", "warning"},
	"REQUEST_CANCELED":                  {"请求已取消，保留此前证据", "review_receipt", "warning"},
	"REQUEST_UNSENT":                    {"传输确认请求未发出", "check_connection", "warning"},
	"NO_RESPONSE":                       {"未收到 HTTP 响应，可能已经发出", "review_receipt", "warning"},
	"INCOMPLETE_RESPONSE":               {"回执不完整，未作为成功依据", "review_receipt", "warning"},
	"RESPONSE_INVALID":                  {"回包结构不可信，未采用成功字段", "review_receipt", "warning"},
	"READ_AGAIN":                        {"在原会话预算内继续只读核对", "read_status", "info"},
	"NO_ORDINARY_TOTP":                  {"状态显示没有普通 TOTP，跳过移除", "enroll_once", "info"},
	"OPTIONAL_STATE_MISSING":            {"缺少可选状态；允许一次登记，不代表已证明未启用", "enroll_once", "info"},
	"TARGET_BOUND":                      {"已固定本轮旧因子，不更换移除目标", "remove_bound_once", "info"},
	"TARGET_NOT_AUTHORIZED":             {"可见因子不匹配已授权的旧因子；本次核对后未再发送移除", "review_binding", "warning"},
	"TARGET_UNRESOLVED":                 {"无法确定普通旧因子；本次核对后未再发送移除", "review_binding", "warning"},
	"INITIAL_CONFLICT":                  {"初次登记明确被拒；只读核对已有绑定", "read_status", "info"},
	"CONFLICT_STATE_DISABLED":           {"登记提示已有验证器，复查显示未启用；不发移除请求，再登记一次", "enroll_once", "info"},
	"FIRST_CONFLICT_AFTER_REMOVE":       {"移除后的首次登记明确提示已有验证器；在原会话只读复查，不再移除", "read_status", "info"},
	"REMOVAL_BUDGET_USED":               {"复查仍显示验证器；本轮已移除过旧因子，不再移除或登记，请核对账号安全设置", "review_binding", "warning"},
	"REMOVAL_ACCEPTED":                  {"已取得所选旧因子的完整移除回执", "enroll_once", "info"},
	"REMOVAL_READ_MATCH":                {"只读状态确认停用；没有伪造移除回执", "enroll_once", "info"},
	"REMOVED_NOT_REPLACED":              {"旧因子移除已有证据，新因子尚未确认；不再移除", "review_binding", "warning"},
	"ENROLLMENT_UNCONFIRMED":            {"登记未取得可信的新材料，不再登记或激活", "review_receipt", "warning"},
	"PENDING_VERIFIED":                  {"新密钥已安全暂存并读回，尚未激活，暂不可领取", "activate_once", "info"},
	"ACTIVATION_ACCEPTED":               {"服务端已确认激活，尚需只读核对因子", "read_status", "info"},
	"ACTIVATION_RECEIPT_CONFIRMED":      {"网站已接受本次激活，本地最终保存尚未完成；跳过列表查询，进入本地保存", "commit_local", "info"},
	"ACTIVATION_UNCONFIRMED":            {"没有明确激活回执，不能补发激活", "review_binding", "warning"},
	"EXPECTED_FACTOR_NOT_VISIBLE":       {"当前查询未匹配本次登记的因子；未再次激活或移除", "review_binding", "warning"},
	"FACTOR_MATCHED":                    {"只读查询匹配本次因子；不代表账号只有一个验证器", "commit_local", "info"},
	"LOCAL_COMMIT_REQUIRED":             {"远端因子已核对，本地提交尚未确认；仅补本地保存", "complete_local_only", "warning"},
	"RECEIPT_LOCAL_COMMIT_REQUIRED":     {"本地提交尚未确认；可仅补本地保存，无需重新设置", "complete_local_only", "warning"},
	"STORAGE_WRITE_FAILED":              {"受保护存储写入失败，停止后续修改", "check_storage", "error"},
	"STORAGE_READBACK_FAILED":           {"受保护存储读回不一致，停止后续修改", "check_storage", "error"},
	"STORAGE_COMMIT_FAILED":             {"本地元数据未能提交，保留已有证据", "check_storage", "error"},
	"STORAGE_BLOCKED":                   {"本地持久化屏障未通过，不能继续修改", "check_storage", "error"},
	"READ_BUDGET_EXHAUSTED":             {"只读核对已用尽预算，保留最后可信证据", "review_binding", "warning"},
	"RESET_STOPPED":                     {"本轮未完成；核对已有证据后再处理", "review_evidence", "warning"},
	"RESET_COMPLETE":                    {"本次新因子已在远端核对，密钥读回和任务提交均已完成", "none", "success"},
	"RESET_SAVED_BY_RECEIPT":            {"网站已确认激活，新密钥已保存；未另查验证器列表", "none", "success"},
	"RESET_SAVED_OTHER_VISIBLE":         {"新密钥已核对并保存", "review_binding", "warning"},
	"LOCAL_COMPLETE":                    {"依据已绑定记录完成本地提交；本轮没有远端写入", "none", "success"},
	"RESET_COMPLETE_LOGOUT_UNCONFIRMED": {"本地提交已完成", "review_logout", "warning"},
	"PREFLIGHT_STOPPED":                 {"重置未开始，请核对本地存储和操作准入", "check_preflight", "warning"},
}
var beijing = time.FixedZone("UTC+08:00", 8*3600)

func Stamp(t time.Time) string { return t.In(beijing).Format(time.RFC3339Nano) }
func stamp(s string) string {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return ""
	}
	return Stamp(t)
}
func one(s, set string) string {
	for _, v := range strings.Fields(set) {
		if s == v {
			return s
		}
	}
	return ""
}

var opPattern = regexp.MustCompile(`^O[0-9a-f]{16}$`)
var attemptPattern = regexp.MustCompile(`^A[1-9][0-9]{0,6}$`)
var aliasPattern = regexp.MustCompile(`^F[1-9][0-9]{0,3}$`)

const stages = "preflight login identity initial_state conflict_read disable verify_disable initial_enroll enroll retry_enroll activation verify_factor pending local_commit journal logout"

// OtherAuthenticatorVisible describes one matched read, not all valid seeds or sessions.
func (d Detail) OtherAuthenticatorVisible() bool {
	return d.Match == "matched" && (d.OldVisible || d.VisibleCount != nil && *d.VisibleCount > 1)
}

// Safe is also used at the archive/API boundary. No maps, arbitrary strings,
// URLs, provider identifiers, error messages or headers can pass through it.
func (d Detail) Safe() *Detail {
	if !opPattern.MatchString(d.OpRef) || d.Seq < 1 || reasons[d.Reason].message == "" || one(d.Stage, stages) == "" {
		return nil
	}
	switch d.Reason {
	case "RESET_SAVED_BY_RECEIPT":
		if !d.Terminal || d.Commit != "committed" || d.LastTrusted != "local_commit" || d.Pending != "verified" || d.Activation != "accepted" || d.Match != "skipped" || d.Visible != "" || d.VisibleCount != nil || d.OldVisible {
			return nil
		}
	case "RESET_SAVED_OTHER_VISIBLE":
		if !d.OtherAuthenticatorVisible() {
			return nil
		}
		fallthrough
	case "RESET_COMPLETE":
		if d.Match != "matched" {
			return nil
		}
		fallthrough
	case "LOCAL_COMPLETE", "RESET_COMPLETE_LOGOUT_UNCONFIRMED":
		if !d.Terminal || d.Commit != "committed" {
			return nil
		}
	}
	d.Next = reasons[d.Reason].next
	if reasons[d.Cause].message == "" {
		d.Cause = ""
	}
	if reasons[d.WriteIssue].level != "warning" || !attemptPattern.MatchString(d.WriteAttempt) {
		d.WriteIssue, d.WriteAttempt = "", ""
	}
	if !attemptPattern.MatchString(d.Related) {
		d.Related = ""
	}
	d.Access = one(d.Access, "read write")
	d.Source = one(d.Source, "transport business_receipt state_read credential_readback local_store workflow protected_journal")
	if !attemptPattern.MatchString(d.AttemptRef) {
		d.AttemptRef = ""
	}
	if !aliasPattern.MatchString(d.Expected) {
		d.Expected = ""
	}
	if !aliasPattern.MatchString(d.Visible) {
		d.Visible = ""
	}
	d.Method = one(d.Method, "GET HEAD POST")
	d.Path = one(d.Path, "/ /:login /api/auth/session /backend-api/accounts/mfa_info /backend-api/accounts/mfa/user/disable_in_house /backend-api/accounts/mfa/enroll /backend-api/accounts/mfa/user/activate_enrollment /backend-api/accounts/logout_all")
	d.Started, d.Ended = stamp(d.Started), stamp(d.Ended)
	d.Dispatch = one(d.Dispatch, "response_received possibly_sent proven_unsent")
	d.Receipt = one(d.Receipt, "complete incomplete not_received")
	if d.HTTP < 100 || d.HTTP > 599 {
		d.HTTP = 0
	}
	d.Business = one(d.Business, "totp_already_enrolled token_revoked recent_auth_required invalid_request invalid_code unrecognized")
	d.Field = one(d.Field, "factor factor.id factor.factor_type factor.is_recovery secret session_id success factors factors.totp mfa_enabled_v2 body")
	const types = "object array string boolean number absent null invalid"
	d.ExpectedType = one(d.ExpectedType, types)
	d.ActualType = one(d.ActualType, types)
	d.LastTrusted = one(d.LastTrusted, "none identity old_removal enrollment pending activation verify_factor local_commit")
	d.Removal = one(d.Removal, "accepted state_read refused unknown not_performed not_applicable")
	d.Pending = one(d.Pending, "verified unknown failed not_performed")
	d.Material = one(d.Material, "none partial complete")
	d.Activation = one(d.Activation, "accepted unknown refused not_performed")
	d.Match = one(d.Match, "matched not_visible unknown not_performed skipped")
	d.Commit = one(d.Commit, "committed failed unknown not_performed")
	d.Logout = one(d.Logout, "accepted refused unknown not_requested not_performed proven_unsent")
	// Keep the record immutable when published to subscribers or a cloned job.
	if d.Writes != nil {
		w := *d.Writes
		d.Writes = &w
	}
	if d.Remaining != nil {
		if *d.Remaining < 0 || *d.Remaining > 4 {
			return nil
		}
		n := *d.Remaining
		d.Remaining = &n
	}
	if d.JournalWrites != nil {
		w := *d.JournalWrites
		d.JournalWrites = &w
	}
	if d.VisibleCount != nil {
		n := *d.VisibleCount
		d.VisibleCount = &n
	}
	if d.DurationMS != nil {
		n := *d.DurationMS
		d.DurationMS = &n
	}
	return &d
}
func (d Detail) Level() string { return reasons[d.Reason].level }
func (d Detail) Message() string {
	s := reasons[d.Reason].message
	if d.Reason == "EXPECTED_FACTOR_NOT_VISIBLE" && d.Expected != "" && d.Visible != "" {
		s = fmt.Sprintf("本次预期 %s，当前查询显示 %s。%s", d.Expected, d.Visible, s)
	}
	if d.Field != "" {
		s += fmt.Sprintf("（%s：预期 %s，实际 %s）", d.Field, d.ExpectedType, d.ActualType)
	}
	if d.Terminal {
		if d.Reason == "RESET_COMPLETE_LOGOUT_UNCONFIRMED" && d.Match == "matched" {
			s = "新密钥已核对并保存"
		}
		receiptOnly := d.Activation == "accepted" && d.Match == "skipped"
		if receiptOnly && d.Commit == "committed" {
			s = reasons["RESET_SAVED_BY_RECEIPT"].message
			if d.Reason == "LOCAL_COMPLETE" {
				s += "；本轮只补本地保存，未联网"
			}
		} else if d.Activation == "accepted" && d.Match != "matched" {
			s = "服务端已确认激活；" + s
		}
		if d.Removal == "accepted" || d.Removal == "state_read" {
			if d.Match != "matched" && !receiptOnly && d.Commit != "committed" && d.Reason != "RECEIPT_LOCAL_COMMIT_REQUIRED" && d.Reason != "REMOVED_NOT_REPLACED" {
				s = "旧因子移除已有证据，新因子尚未确认。" + s
			}
		}
		if d.Pending == "verified" && d.Commit != "committed" && d.Match != "matched" {
			if receiptOnly {
				s += "；待提交的新密钥已安全保留"
			} else {
				s += "；待核对材料已安全保留"
			}
		}
		if d.Material == "partial" {
			s += "；受保护记录只读回部分登记材料，不足以激活"
		}
		if d.Commit == "committed" && (d.Match == "matched" || receiptOnly) {
			s += "；可在“账号信息 → 2FA 密钥”查看"
			switch d.Removal {
			case "accepted":
				s += "；已收到旧验证器 F1 的移除回执"
			case "state_read":
				s += "；移除回执未确认，当时查询显示已停用"
			}
		}
		if d.OldVisible {
			s += "；核对新密钥时仍能看到 F1，不能确认它已失效"
			if d.VisibleCount != nil && *d.VisibleCount > 2 {
				s += "；列表中另有其他验证器，本次没有移除它们"
			}
		} else if d.OtherAuthenticatorVisible() {
			s += "；核对新密钥时还看到其他验证器，本次没有移除它们"
		} else if d.Match == "matched" && d.Commit == "committed" && d.VisibleCount != nil && *d.VisibleCount == 1 && (d.Removal == "accepted" || d.Removal == "state_read") {
			s += "；当时只看到新验证器，不能证明所有旧密钥已失效"
		}
		if d.Reason == "RESET_SAVED_OTHER_VISIBLE" && d.Source == "protected_journal" {
			s += "；本轮只补本地保存，未联网"
		}
		if d.Commit == "committed" {
			switch d.Logout {
			case "accepted":
				s += "；退出请求已接受，其他设备是否已退出未核实"
			case "unknown":
				s += "；未确认退出结果，不重发退出请求"
			case "refused":
				s += "；退出请求被拒绝"
			case "proven_unsent":
				s += "；退出请求确定未发送"
			case "not_performed":
				s += "；未记录退出回执，不能确定是否已发送"
			case "":
				if d.Reason == "RESET_COMPLETE_LOGOUT_UNCONFIRMED" {
					s += "；未记录退出结果"
				}
			}
		}
		if d.Reason == "EXPECTED_FACTOR_NOT_VISIBLE" || d.Reason == "FORMAL_CONFLICT" {
			s += "。下一步：核对因子绑定"
		}
		if d.Reason == "TARGET_UNRESOLVED" || d.Reason == "TARGET_NOT_AUTHORIZED" {
			if d.Removal == "not_performed" {
				s += "；未移除任何验证器，请在账号安全设置中核对"
			} else {
				s += "；请在账号安全设置中核对"
			}
		}
		s = "本轮结果：" + s
	}
	if d.AttemptRef != "" {
		if d.HTTP != 0 {
			s += fmt.Sprintf("（HTTP %d）", d.HTTP)
		} else {
			s += "（未收到 HTTP 响应）"
		}
	}
	if d.Remaining != nil {
		s += fmt.Sprintf("，还可只读 %d 次", *d.Remaining)
	}
	return s
}
