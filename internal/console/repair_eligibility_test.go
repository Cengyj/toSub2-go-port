package console

import (
	"encoding/json"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestPublicJobPreservesLegacyAutoRepairEligibility(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		automated, blocked, eligible bool
		input                        map[string]any
		requirements                 map[string]bool
		remove, reason               string
	}{
		{name: "manual", input: map[string]any{"mailApiUrl": "https://mail.example.test"}, reason: "手动输入"},
		{name: "password", automated: true, eligible: true, input: map[string]any{"password": "Synthetic-password9!"}, requirements: map[string]bool{"password": true}, reason: "所需资料仍可用"},
		{name: "mail-api", automated: true, eligible: true, input: map[string]any{"mailApiUrl": "https://mail.example.test"}, requirements: map[string]bool{"emailOtp": true}, reason: "所需资料仍可用"},
		{name: "missing-password", automated: true, input: map[string]any{"password": "Synthetic-password9!"}, requirements: map[string]bool{"password": true}, remove: "password", reason: "已保存的密码无法读取"},
		{name: "missing-email-api", automated: true, requirements: map[string]bool{"emailOtp": true}, reason: "缺少可自动收取邮箱验证码的 API"},
		{name: "missing-mfa", automated: true, input: map[string]any{"password": "Synthetic-password9!", "totpSecret": "JBSWY3DPEHPK3PXP"}, requirements: map[string]bool{"mfa": true}, remove: "totpSecret", reason: "已保存的 2FA 密钥无法读取"},
		{name: "missing-login-method", automated: true, reason: "缺少可自动登录的密码或邮件收码 API"},
		{name: "unrecoverable-totp", automated: true, input: map[string]any{"password": "Synthetic-password9!", "totpSecret": "JBSWY3DPEHPK3PXP"}, remove: "totpSecret", reason: "2FA 密钥在当前系统上无法恢复"},
		{name: "blocked", automated: true, blocked: true, input: map[string]any{"password": "Synthetic-password9!"}, reason: "账号已确认封禁、删除或永久停用"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			j, err := m.Submit(jobs.Spec{ID: "repair-eligibility", Email: "eligibility@example.test", Input: tc.input})
			if err != nil {
				t.Fatal(err)
			}
			m.RecordAuthorization(j.ID, tc.automated, "本次登录需要手动输入", tc.requirements)
			if tc.remove != "" {
				m.ConsumeInput(j.ID, tc.remove)
			}
			if tc.blocked {
				m.UpdateAutoRepair(j.ID, nil, "", "account_disabled", true)
			}
			current, _ := m.Get(j.ID)
			got := s.publicJob(current)
			reason, _ := got["autoRepairEligibilityReason"].(string)
			if got["autoRepairEligible"] != tc.eligible || !strings.Contains(reason, tc.reason) {
				t.Fatalf("repair eligibility contract missing: eligible=%v reason=%q", got["autoRepairEligible"], reason)
			}
			raw, _ := json.Marshal(got)
			for _, secret := range []string{"Synthetic-password9!", "JBSWY3DPEHPK3PXP", "https://mail.example.test"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("repair eligibility exposed private source data")
				}
			}
		})
	}
}
