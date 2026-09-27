package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

// The real embedded React, HTTP API and JobManager run here. The remote account
// runner is deterministic; production protocol/browser flows have separate E2E
// coverage in runner_browser_test.go. All credentials are synthetic.
func TestChromiumConsoleAccountActionsAndDownloads(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for React account controls and real downloads")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cfg := config.Default()
	cfg.OutputRoot = t.TempDir()
	cfg.StartupToken = "ui-operation-fixture"
	cfg.MaxActiveJobs = 20
	server := httptest.NewUnstartedServer(nil)
	cfg.Host = "127.0.0.1"
	cfg.Port = server.Listener.Addr().(*net.TCPAddr).Port
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.NewManager(ctx, 20, st, logger)
	manager.SetCredentialStore(credentials.NewMemoryStore())
	manager.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		ask := func(stage string, keys ...string) (string, error) {
			emit(jobs.JobEvent{JobID: j.ID, Stage: stage, Status: jobs.StatusWaitingInput, Prompt: "Fixture awaiting authorized input"})
			for {
				if v, ok := manager.ConsumeInput(j.ID, keys...); ok {
					return v, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		switch j.Operation {
		case "setup-2fa", "setup-totp":
			manager.UpdateTOTPSetup(j.ID, "JBSWY3DPEHPK3PXP", "otpauth://totp/Fixture?secret=JBSWY3DPEHPK3PXP", "", false)
			v, e := ask("totp_setup", "totp_setup_otp", "totp_setup")
			if e != nil {
				return e
			}
			if v != "654321" {
				return fmt.Errorf("fixture setup code rejected")
			}
			manager.UpdateTOTPSetup(j.ID, "JBSWY3DPEHPK3PXP", "", "", true)
			return nil
		case "add-password":
			manager.UpdatePasswordAdded(j.ID, true, "")
			return nil
		case "regenerate", "reauthorize", "refresh":
		default:
			v, e := ask("email_otp", "email_otp")
			if e != nil {
				return e
			}
			if v != "123456" {
				return fmt.Errorf("fixture email code rejected")
			}
			if j.Operation != "relogin" {
				if _, e = ask("totp", "mfa_otp", "totp"); e != nil {
					return e
				}
				for {
					phone, e := ask("phone", "phone")
					if e != nil {
						return e
					}
					manager.UpdateSMS(j.ID, "", "", phone, "idle", "")
					code, e := ask("sms_otp", "phone_otp", "sms_otp")
					if e != nil {
						return e
					}
					if code == "p" {
						continue
					}
					for code == "r" {
						code, e = ask("sms_otp", "phone_otp", "sms_otp")
						if e != nil {
							return e
						}
					}
					if code != "789012" {
						return fmt.Errorf("fixture SMS code rejected")
					}
					break
				}
			}
		}
		data := map[string]any{"type": "sub2api-data", "version": 1, "proxies": []any{}, "accounts": []any{map[string]any{"name": j.Email, "platform": "openai", "type": "oauth", "credentials": map[string]any{"email": j.Email, "access_token": "ui-private-access", "refresh_token": "ui-private-refresh"}}}}
		if err := protocol.WritePrivateJSON(filepath.Join(cfg.OutputRoot, j.ID, "sub2api-import-oauth.json"), data); err != nil {
			return err
		}
		manager.UpdateResult(j.ID, true, "Fixture authorized output ready")
		return nil
	})
	manager.Start()
	defer manager.Shutdown(context.Background())
	api := console.NewServer(cfg, manager, staticFiles, logger)
	defer api.Close()
	var privateSeedRequests atomic.Int32
	apiHandler := api.Handler()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/totp-setup") {
			if r.Header.Get("X-Console-Token") != cfg.StartupToken {
				t.Error("private seed retry lost console authentication")
			}
			// The original UI reread its setup seed on every job poll. The
			// separate private endpoint must recover without changing stages.
			switch privateSeedRequests.Add(1) {
			case 1:
				http.Error(w, "synthetic temporary failure", http.StatusServiceUnavailable)
				return
			case 2:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"secret":""}`)
				return
			}
		}
		apiHandler.ServeHTTP(w, r)
	})
	server.Start()
	defer server.Close()
	pool := browser.NewManager(chrome, 1)
	defer pool.Close()
	session, err := pool.Start(ctx, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	downloadDir := t.TempDir()
	// A separate CDP browser connection configures downloads into a test-owned
	// directory, so synthetic exports never land in the user's Downloads folder.
	portData, err := os.ReadFile(filepath.Join(session.Dir, "DevToolsActivePort"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(string(portData)), "\n")
	if len(parts) < 2 {
		t.Fatal("missing browser debugging endpoint")
	}
	socket, _, err := websocket.DefaultDialer.DialContext(ctx, "ws://127.0.0.1:"+parts[0]+parts[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = socket.WriteJSON(map[string]any{"id": 1, "method": "Browser.setDownloadBehavior", "params": map[string]any{"behavior": "allow", "downloadPath": downloadDir}}); err != nil {
		t.Fatal(err)
	}
	var commandResult map[string]any
	if err = socket.ReadJSON(&commandResult); err != nil || commandResult["error"] != nil {
		t.Fatalf("download configuration failed: %v", err)
	}
	defer socket.Close()
	if err = session.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	evaluate := func(js string) any {
		t.Helper()
		v, e := session.Evaluate(ctx, js)
		if e != nil {
			body, _ := session.Evaluate(ctx, "document.body.innerText.slice(0,3500)")
			t.Fatalf("UI expression %s failed: %v; page=%v", js, e, body)
		}
		return v
	}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	wait := func(js string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if v, e := session.Evaluate(ctx, js); e == nil && v == true {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		body, _ := session.Evaluate(ctx, "document.body.innerText.slice(0,4000)")
		t.Fatalf("UI condition %s failed; body=%v", js, body)
	}
	setValue := func(selector, value string) {
		t.Helper()
		evaluate("(()=>{const e=document.querySelector(" + quote(selector) + ");if(!e)throw new Error('input missing');e.focus();e.select();return true})()")
		if e := session.InsertText(ctx, value); e != nil {
			t.Fatal(e)
		}
		evaluate("new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(()=>r(true))))")
	}
	click := func(selector string) {
		t.Helper()
		wait("!!document.querySelector(" + quote(selector) + ")&&!document.querySelector(" + quote(selector) + ").disabled")
		evaluate("(()=>{const e=document.querySelector(" + quote(selector) + ");if(!e||e.disabled)throw new Error('button unavailable');e.click();return true})()")
	}
	clickText := func(label string) {
		t.Helper()
		evaluate("(()=>{const e=[...document.querySelectorAll('button')].find(e=>e.textContent.trim()===" + quote(label) + ");if(!e||e.disabled)throw new Error('button unavailable');e.click();return true})()")
	}
	submit := func(selector, value, title string) {
		t.Helper()
		wait("!!document.querySelector(" + quote(selector) + ")")
		wait("!document.querySelector('.inline-entry .text-action:disabled')")
		setValue(selector, value)
		click("button[title='" + title + "']")
	}
	wait("!!document.querySelector('.add-form button[type=submit]')&&!document.querySelector('.add-form button[type=submit]').disabled")
	evaluate("window.confirm=()=>true;window.__uiErrors=[];window.addEventListener('error',()=>window.__uiErrors.push('error'));window.addEventListener('unhandledrejection',()=>window.__uiErrors.push('rejection'));true")
	setValue("input[aria-label='邮箱地址']", "controls@example.test")
	evaluate("document.querySelector('.add-form').requestSubmit();true")
	submit("input[aria-label='6 位邮箱验证码']", "123456", "提交邮箱验证码")
	submit("input[aria-label='6 位 2FA 验证码']", "654321", "提交 2FA 验证码")
	submit("input[aria-label='+60123456789']", "+12025550123", "发送手机验证码")
	wait("!!document.querySelector('.status-phone_otp')")
	clickText("换号")
	submit("input[aria-label='+60123456789']", "+12025550124", "发送手机验证码")
	wait("!!document.querySelector('.status-phone_otp')")
	clickText("重发")
	submit("input[aria-label='+12025550124 的验证码']", "789012", "提交手机验证码")
	wait("document.querySelectorAll('.status-completed').length===1")
	// Download through the actual React Blob/link code and Chrome download manager.
	click(".job-row .download-button")
	waitFiles := func(want int) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			files, _ := os.ReadDir(downloadDir)
			n := 0
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".json") {
					b, e := os.ReadFile(filepath.Join(downloadDir, f.Name()))
					var out map[string]any
					if e == nil && json.Unmarshal(b, &out) == nil && out["type"] == "sub2api-data" {
						n++
					}
				}
			}
			if n == want {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		files, _ := os.ReadDir(downloadDir)
		names := []string{}
		for _, file := range files {
			names = append(names, file.Name())
		}
		page, _ := session.Evaluate(ctx, "document.body.innerText.slice(0,3500)")
		t.Fatalf("Chrome did not save compatible JSON download; files=%v page=%v", names, page)
	}
	waitFiles(1)
	clickText("本页全选")
	wait("document.body.innerText.includes('跨页已选 1 条')")
	clickText("批量下载")
	waitFiles(2)
	click("button[title='设置 2FA']")
	wait("!!document.querySelector('.totp-setup-secret code')&&document.querySelector('.totp-setup-secret code').textContent==='JBSWY3DPEHPK3PXP'")
	if privateSeedRequests.Load() != 3 {
		t.Fatal("private seed did not recover from failed and pending responses")
	}
	submit("input[aria-label='新 2FA 的 6 位验证码']", "654321", "激活新的 2FA")
	wait("!!document.querySelector('.status-completed')&&!document.querySelector(\"button[title='设置 2FA']\")")
	click("button[title='添加密码']")
	wait("!!document.querySelector('.status-completed')&&!document.querySelector(\"button[title='添加密码']\")")
	click(".job-row .regenerate-button")
	wait("document.querySelector('.operation-time-cell').textContent.includes('重新授权')&&!!document.querySelector('.status-completed')")
	click(".job-row .relogin-button")
	submit("input[aria-label='6 位邮箱验证码']", "123456", "提交邮箱验证码")
	wait("!!document.querySelector('.status-completed')")
	// Explicit cancellation, retry and stop-all exercise the same row controls.
	setValue("input[aria-label='邮箱地址']", "cancel@example.test")
	evaluate("document.querySelector('.add-form').requestSubmit();true")
	wait("!!document.querySelector('.status-email_otp')")
	click(".status-email_otp button[title='取消任务']")
	wait("!!document.querySelector('.status-canceled')")
	click(".status-canceled .retry-button")
	wait("!!document.querySelector('.status-email_otp')")
	clickText("停止全部")
	wait("!!document.querySelector('.status-canceled')&&!document.querySelector('.status-email_otp')")
	if evaluate("window.__uiErrors.length") != float64(0) {
		t.Fatal("React runtime error")
	}
	if privateSeedRequests.Load() != 3 {
		t.Fatal("private seed requests continued after success or setup completion")
	}
	meta, err := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"JBSWY3DPEHPK3PXP", "123456", "789012", "ui-private-access", "ui-private-refresh"} {
		if strings.Contains(string(meta), secret) {
			t.Fatal("private UI data leaked into job metadata")
		}
	}
}
