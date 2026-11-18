package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"tosub2-go-port/internal/resetlog"
)

func TestResetLogBoundaryAcceptsOnlyTypedEvidence(t *testing.T) {
	marker := "synthetic-private-echo-canary"
	d := &resetlog.Detail{OpRef: "O0123456789abcdef", Seq: 4, Reason: "HTTP_REFUSED", Stage: "disable", Source: "transport", Path: "https://fixture.invalid/?token=" + marker, Business: marker, Visible: marker, Next: marker}
	for _, payload := range []any{d, map[string]any{"reason_code": "RESET_COMPLETE", "token": marker}, &resetlog.Detail{OpRef: d.OpRef, Seq: 1, Stage: "local_commit", Reason: "RESET_COMPLETE"}} {
		e := SanitizeJobEvent(JobEvent{Prompt: strings.Repeat(marker, 200), Data: map[string]any{resetlog.DataKey: payload, "error": marker, "cookie": marker}})
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), marker) || strings.Contains(string(b), "https://") {
			t.Fatal("arbitrary metadata escaped to public event")
		}
		if payload == d {
			if !strings.Contains(string(b), `"reason_code":"HTTP_REFUSED"`) {
				t.Fatal("long text erased structured cause")
			}
		} else if len(e.Data) != 0 || e.Prompt != "" {
			t.Fatal("malformed evidence affected public output")
		}
	}
	var e LogEntry
	if err := json.Unmarshal([]byte(`{"stage":"totp_reset_complete","message":"历史记录","reset_diagnostic":"old extension"}`), &e); err != nil || e.Diagnostic != nil || e.Message != "历史记录" {
		t.Fatal("optional field lost legacy record", err)
	}
}

func TestResetSavedWithOtherVisibleNeedsBoundEvidence(t *testing.T) {
	base := resetlog.Detail{OpRef: "O0123456789abcdef", Seq: 3, Stage: "local_commit", Reason: "RESET_SAVED_OTHER_VISIBLE", Terminal: true, Match: "matched", Commit: "committed"}
	count := 2
	valid := base
	valid.VisibleCount = &count
	if d := valid.Safe(); d == nil || d.Level() != "warning" || d.Next != "review_binding" {
		t.Fatal("valid warning was lost")
	}
	for _, edit := range []func(*resetlog.Detail){
		func(d *resetlog.Detail) { d.VisibleCount = nil },
		func(d *resetlog.Detail) { d.Terminal = false },
		func(d *resetlog.Detail) { d.Commit = "unknown" },
		func(d *resetlog.Detail) { d.Match = "not_visible" },
	} {
		d := valid
		edit(&d)
		if d.Safe() != nil {
			t.Fatal("missing facts promoted to a saved key or a second authenticator")
		}
	}
	base.Reason = "RESET_COMPLETE" // old stored log: no list count was recorded
	if d := base.Safe(); d == nil || d.VisibleCount != nil || d.OldVisible || d.Reason != base.Reason {
		t.Fatal("legacy record reinterpreted as verification of the old key")
	}
	base.Reason, base.Match, base.Logout = "RESET_COMPLETE_LOGOUT_UNCONFIRMED", "", "not_performed"
	if d := base.Safe(); d == nil || !strings.Contains(d.Message(), "本地提交已完成") || !strings.Contains(d.Message(), "未记录退出回执") || strings.Contains(d.Message(), "新密钥已核对") || strings.Contains(d.Message(), "未执行退出") {
		t.Fatal("legacy commit alone was presented as a factor read or proof logout was unsent")
	}
	base.Match, base.Logout = "matched", "proven_unsent"
	if d := base.Safe(); d == nil || !strings.Contains(d.Message(), "新密钥已核对并保存") || !strings.Contains(d.Message(), "退出请求确定未发送") {
		t.Fatal("positive factor and dispatch evidence was lost")
	}
}

func TestReceiptOnlySuccessRequiresReceiptKeyAndNoListClaim(t *testing.T) {
	base := resetlog.Detail{OpRef: "O0123456789abcdef", Seq: 9, Terminal: true, Stage: "local_commit", Reason: "RESET_SAVED_BY_RECEIPT", LastTrusted: "local_commit", Pending: "verified", Commit: "committed", Activation: "accepted", Match: "skipped"}
	if safe := base.Safe(); safe == nil || !strings.Contains(safe.Message(), "未另查验证器列表") {
		t.Fatal("receipt-only message unavailable")
	}
	for _, change := range []func(*resetlog.Detail){
		func(d *resetlog.Detail) { d.Activation = "unknown" }, func(d *resetlog.Detail) { d.Pending = "" },
		func(d *resetlog.Detail) { d.LastTrusted = "activation" }, func(d *resetlog.Detail) { d.Commit = "failed" },
		func(d *resetlog.Detail) { d.Match = "matched" }, func(d *resetlog.Detail) { d.Visible = "F2" },
		func(d *resetlog.Detail) { d.OldVisible = true }, func(d *resetlog.Detail) { n := 0; d.VisibleCount = &n },
	} {
		d := base
		change(&d)
		if d.Safe() != nil {
			t.Fatal("a missing or conflicting fact was promoted to success")
		}
	}
}
