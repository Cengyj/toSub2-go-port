package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestFrontendInputNormalizesValuesAndClosesAcceptedPrompt(t *testing.T) {
	for _, tc := range []struct {
		action, protocolStage, status, value, want, prompt string
	}{
		{"email_otp", "email_otp", "email_otp", " \t529731\r\n", "529731", "正在验证邮箱验证码"},
		{"mfa_otp", "totp", "mfa_otp", " \t529732\r\n", "529732", "正在验证 2FA 验证码"},
		{"totp_setup_otp", "totp_setup", "totp_setup_otp", " \t529733\r\n", "529733", "正在激活新的 2FA"},
		{"password", "password", "password", " \tFixture-password!19 \t", " \tFixture-password!19 \t", "正在验证账号密码"},
		{"phone", "phone", "phone", " \t+12025550124\r\n", "+12025550124", "正在向 +12025550124 发送手机验证码"},
		{"phone_otp", "sms_otp", "phone_otp", " \t9731\r\n", "9731", "正在验证手机验证码"},
		{"resend_email", "email_otp", "email_otp", "ignored", "r", "正在重新发送邮箱验证码"},
		{"resend", "email_otp", "email_otp", "ignored", "r", "正在重新发送邮箱验证码"},
		{"resend_phone", "sms_otp", "phone_otp", "ignored", "r", "正在向 +12025550123 重新发送验证码"},
		{"change_phone", "sms_otp", "phone_otp", "ignored", "p", "正在返回手机号输入"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			server := httptest.NewServer(s.Handler())
			readInput := make(chan struct{})
			consumed := make(chan string, 1)
			m.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
				emit(jobs.JobEvent{Stage: tc.protocolStage, Status: jobs.StatusWaitingInput, Prompt: "等待用户输入", Data: map[string]any{"currentPhone": "+12025550123", "phoneError": "previous rejection"}})
				select {
				case <-readInput:
				case <-ctx.Done():
					return ctx.Err()
				}
				value, err := waitConsoleInput(ctx, m, j.ID, tc.status)
				if err != nil {
					return err
				}
				// Flow.ask emits running with the same stage after consuming input.
				emit(jobs.JobEvent{Stage: tc.protocolStage, Status: jobs.StatusRunning})
				consumed <- value
				<-ctx.Done()
				return ctx.Err()
			})
			t.Cleanup(func() {
				server.Close()
				s.Close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := m.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			m.Start()
			j, err := m.Submit(jobs.Spec{ID: "input-flow", Email: "input@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			waitConsoleState(t, m, j.ID, func(j *jobs.Job) bool { return j.Status == tc.status })
			inputPath := "/api/jobs/" + j.ID + "/input"
			status, body := consoleHTTPPost(t, server.URL, inputPath, map[string]any{"action": tc.action, "value": tc.value})
			if status != http.StatusOK {
				t.Fatalf("valid input rejected: status=%d body=%v", status, body)
			}
			public, ok := body["job"].(map[string]any)
			if !ok || public["status"] != "working" || public["prompt"] != tc.prompt {
				t.Fatalf("accepted input retained a prompt: %v", public)
			}
			if tc.action == "change_phone" && public["currentPhone"] != "" {
				t.Fatal("change-phone retained the old number")
			}
			status, _ = consoleHTTPPost(t, server.URL, inputPath, map[string]any{"action": tc.action, "value": tc.value})
			if status != http.StatusConflict {
				t.Fatalf("duplicate input accepted before consumption: %d", status)
			}
			close(readInput)
			select {
			case value := <-consumed:
				if value != tc.want {
					t.Fatalf("input normalization mismatch for %s", tc.action)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("runner did not consume accepted input")
			}
			current, _ := m.Get(j.ID)
			if current.Status != "working" || current.Prompt != tc.prompt {
				t.Fatalf("protocol running event reopened stale prompt: %s %q", current.Status, current.Prompt)
			}
			status, _ = consoleHTTPPost(t, server.URL, inputPath, map[string]any{"action": tc.action, "value": tc.value})
			if status != http.StatusConflict {
				t.Fatalf("duplicate input accepted during validation: %d", status)
			}
			if _, ok := m.ConsumeInput(j.ID, tc.status); ok {
				t.Fatal("duplicate submission seeded the next input prompt")
			}
			w := frontendRequest(s, "GET", "/api/jobs", nil, "test")
			var page struct {
				Stats map[string]int `json:"stats"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || page.Stats["active"] != 1 {
				t.Fatal("working task missing from active task count")
			}
			if tc.action == "password" || tc.action == "email_otp" || tc.action == "mfa_otp" || tc.action == "totp_setup_otp" || tc.action == "phone_otp" {
				metadata, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
				if err != nil {
					t.Fatal(err)
				}
				logs := frontendRequest(s, "GET", "/api/jobs/"+j.ID+"/logs", nil, "test")
				secret := strings.TrimSpace(tc.want)
				if strings.Contains(string(metadata), secret) || strings.Contains(w.Body.String(), secret) || strings.Contains(logs.Body.String(), secret) {
					t.Fatal("submitted credential or code escaped into public metadata")
				}
			}
		})
	}
}

func TestFrontendInputRejectsEmptyPasswordAndCodeControlCharacters(t *testing.T) {
	for _, tc := range []struct{ action, value string }{
		{"password", ""}, {"email_otp", "r"}, {"mfa_otp", "r"}, {"totp_setup_otp", "r"}, {"phone_otp", "r"}, {"phone_otp", "p"},
	} {
		t.Run(tc.action+"/"+tc.value, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			j, err := m.Submit(jobs.Spec{ID: "invalid-input", Email: "invalid@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			m.Emit(jobs.JobEvent{JobID: j.ID, Stage: tc.action, Status: jobs.StatusWaitingInput})
			w := frontendRequest(s, "POST", "/api/jobs/"+j.ID+"/input", map[string]any{"action": tc.action, "value": tc.value}, "test")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid input accepted: %d", w.Code)
			}
			current, _ := m.Get(j.ID)
			if current.Status != tc.action {
				t.Fatal("rejected input changed prompt")
			}
			if _, ok := m.ConsumeInput(j.ID, tc.action); ok {
				t.Fatal("rejected input queued a value")
			}
		})
	}
}
