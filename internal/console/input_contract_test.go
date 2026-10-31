package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
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
				for _, surface := range []struct {
					name string
					data []byte
				}{{"metadata", metadata}, {"page", w.Body.Bytes()}, {"logs", logs.Body.Bytes()}} {
					if matches := inputContractMatchingJSONPaths(surface.data, secret); len(matches) != 0 {
						t.Fatalf("submitted credential or code escaped into %s at JSON paths %v", surface.name, matches)
					}
				}
			}
		})
	}
}

// Check every key and scalar without printing credentials or contents. Only
// known time.Time DTO fields have their nondeterministic fractions normalized:
// a four-digit phone OTP can otherwise coincide with time.Now().Nanosecond().
func inputContractMatchingJSONPaths(data []byte, secret string) []string {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	paths := []string{}
	var visit func(string, string) error
	visit = func(path, schemaPath string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch v := token.(type) {
		case json.Delim:
			switch v {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return err
					}
					key, ok := keyToken.(string)
					if !ok || seen[key] {
						return fmt.Errorf("invalid or duplicate object key")
					}
					seen[key] = true
					displayKey := key
					if strings.Contains(key, secret) {
						displayKey = "<matching-key>"
						paths = append(paths, path+"."+displayKey)
					}
					childSchemaPath := schemaPath + "." + key
					if strings.ContainsAny(key, ".[]") {
						childSchemaPath = schemaPath + ".<non-DTO-key>"
					}
					if err := visit(path+"."+displayKey, childSchemaPath); err != nil {
						return err
					}
				}
			case '[':
				for index := 0; decoder.More(); index++ {
					if err := visit(fmt.Sprintf("%s[%d]", path, index), schemaPath+"[]"); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unexpected JSON delimiter")
			}
			_, err := decoder.Token()
			return err
		case string:
			if inputContractTimestampPath(schemaPath) {
				if timestamp, err := time.Parse(time.RFC3339Nano, v); err == nil {
					v = timestamp.Format(time.RFC3339)
				}
			}
			if strings.Contains(v, secret) {
				paths = append(paths, path)
			}
		case json.Number:
			if strings.Contains(v.String(), secret) {
				paths = append(paths, path)
			}
		}
		return nil
	}
	if err := visit("$", "$"); err != nil {
		return []string{"invalid-json"}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return []string{"invalid-json"}
	}
	sort.Strings(paths)
	return paths
}

func inputContractTimestampPath(path string) bool {
	switch path {
	case "$.updated_at",
		"$.jobs[].created_at", "$.jobs[].updated_at", "$.jobs[].queued_at",
		"$.jobs[].started_at", "$.jobs[].finished_at", "$.jobs[].last_operation_at",
		"$.jobs[].password_added_at", "$.jobs[].last_auth_automated_at",
		"$.jobs[].auto_repair_last_attempt_at", "$.jobs[].auto_repair_last_success_at",
		"$.jobs[].log_entries[].at", "$.jobs[].log_entries[].operationAt",
		"$.jobs[].createdAt", "$.jobs[].updatedAt", "$.jobs[].startedAt",
		"$.jobs[].completedAt", "$.jobs[].lastOperationAt", "$.jobs[].passwordAddedAt",
		"$.jobs[].autoRepairLastAttemptAt", "$.jobs[].autoRepairLastSuccessAt",
		"$.selection[].createdAt", "$.selection[].startedAt", "$.selection[].completedAt",
		"$.selection[].lastOperationAt", "$.selection[].passwordAddedAt",
		"$.selection[].autoRepairLastAttemptAt", "$.selection[].autoRepairLastSuccessAt",
		"$.entries[].at", "$.entries[].operationAt":
		return true
	}
	return false
}

func TestFrontendInputLeakageCheckNormalizesOnlyTypedTimestampFractions(t *testing.T) {
	const secret = "9731"
	const timestamp = "2026-10-31T06:45:03.973100001Z"
	job := map[string]any{}
	for _, field := range []string{
		"created_at", "updated_at", "queued_at", "started_at", "finished_at",
		"last_operation_at", "password_added_at", "last_auth_automated_at",
		"auto_repair_last_attempt_at", "auto_repair_last_success_at",
		"createdAt", "updatedAt", "startedAt", "completedAt", "lastOperationAt",
		"passwordAddedAt", "autoRepairLastAttemptAt", "autoRepairLastSuccessAt",
	} {
		job[field] = timestamp
	}
	entry := map[string]any{"at": timestamp, "operationAt": timestamp, "message": "正在验证手机验证码"}
	job["log_entries"] = []any{entry}
	selection := map[string]any{}
	for _, field := range []string{
		"createdAt", "startedAt", "completedAt", "lastOperationAt",
		"passwordAddedAt", "autoRepairLastAttemptAt", "autoRepairLastSuccessAt",
	} {
		selection[field] = timestamp
	}
	data, err := json.Marshal(map[string]any{
		"updated_at": timestamp, "jobs": []any{job},
		"selection": []any{selection}, "entries": []any{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), secret) {
		t.Fatal("fixture does not reproduce timestamp fraction collision")
	}
	if matches := inputContractMatchingJSONPaths(data, secret); len(matches) != 0 {
		t.Fatalf("typed timestamp fractions falsely reported as credentials: %v", matches)
	}

	for _, tc := range []struct {
		name string
		data string
	}{
		{"prompt", `{"jobs":[{"prompt":"submitted 9731"}]}`},
		{"input", `{"jobs":[{"input":{"phone_otp":"9731"}}]}`},
		{"escaped_text", `{"jobs":[{"lastError":"\u0039\u0037\u0033\u0031"}]}`},
		{"logs", `{"jobs":[{"logs":"synthetic submitted 9731"}]}`},
		{"log_timestamp_text", `{"entries":[{"message":"` + timestamp + `"}]}`},
		{"number", `{"code":9731}`},
		{"key", `{"credential9731":"redacted"}`},
		{"unknown_timestamp_path", `{"createdAt":"` + timestamp + `"}`},
		{"nested_timestamp_path", `{"debug":{"jobs":[{"updatedAt":"` + timestamp + `"}]}}`},
		{"lookalike_root_key", `{"jobs[].updatedAt":"` + timestamp + `"}`},
		{"lookalike_job_key", `{"jobs":[{"log_entries[].at":"` + timestamp + `"}]}`},
		{"invalid_timestamp", `{"jobs":[{"updatedAt":"9731"}]}`},
		{"timestamp_suffix", `{"jobs":[{"updatedAt":"` + timestamp + ` submitted"}]}`},
		{"timestamp_year", `{"jobs":[{"updatedAt":"9731-10-31T06:45:03Z"}]}`},
		{"duplicate_key", `{"jobs":[{"lastError":"9731","lastError":"safe"}]}`},
		{"trailing_document", `{"jobs":[]} {"code":"9731"}`},
		{"malformed_json", `{"code":"9731"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matches := inputContractMatchingJSONPaths([]byte(tc.data), secret)
			if len(matches) == 0 {
				t.Fatal("injected secret or invalid JSON was not detected")
			}
			if strings.Contains(strings.Join(matches, " "), secret) {
				t.Fatal("diagnostic JSON paths expose the matching secret")
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
