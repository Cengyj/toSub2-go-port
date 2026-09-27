package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConsoleProcessBindFailureIsNonzero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("target-host console subprocess test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConsoleProcessHelper$")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TOSUB2_PROCESS_FIXTURE=1", "TOSUB2_PROCESS_PORT=" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "TOSUB2_PROCESS_ROOT=" + t.TempDir()}
	output, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !bytes.Contains(output, []byte("HTTP listener initialization failed")) {
		t.Fatalf("bind failure must exit nonzero immediately: %v %s", err, output)
	}
}

// A test-only subprocess calls the same production main entry point with only
// system utilities on PATH, without the bundled Node/Vite/Go build runtime.
func TestConsoleProcessHelper(t *testing.T) {
	if os.Getenv("TOSUB2_PROCESS_FIXTURE") != "1" {
		t.Skip("test-only subprocess entry")
	}
	os.Args = []string{os.Args[0], "--host=127.0.0.1", "--port=" + os.Getenv("TOSUB2_PROCESS_PORT"), "--output-root=" + os.Getenv("TOSUB2_PROCESS_ROOT")}
	main()
	os.Exit(0)
}

type consoleProcess struct {
	command *exec.Cmd
	done    chan error
	base    string
	token   string
	logPath string
}

func startConsoleProcess(t *testing.T, root string) *consoleProcess {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal lifecycle requires macOS/Linux; Windows service lifecycle needs target-host acceptance")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	p := &consoleProcess{base: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan error, 1), logPath: filepath.Join(t.TempDir(), "console.log")}
	log, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	p.command = exec.Command(os.Args[0], "-test.run=^TestConsoleProcessHelper$")
	p.command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TOSUB2_PROCESS_FIXTURE=1", "TOSUB2_PROCESS_PORT=" + strconv.Itoa(port), "TOSUB2_PROCESS_ROOT=" + root, "TOSUB2_BROWSER_ENABLED=0", "CHATGPT_BASE=http://127.0.0.1:1", "AUTH_BASE=http://127.0.0.1:1"}
	p.command.Stdout, p.command.Stderr = log, log
	if err = p.command.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	go func() { err := p.command.Wait(); _ = log.Close(); p.done <- err; close(p.done) }()
	t.Cleanup(func() { _ = p.command.Process.Kill(); <-p.done })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, e := client.Get(p.base + "/api/bootstrap")
		if e == nil {
			var v struct {
				Token string `json:"token"`
			}
			e = json.NewDecoder(response.Body).Decode(&v)
			_ = response.Body.Close()
			if e == nil && response.StatusCode == 200 && v.Token != "" {
				p.token = v.Token
				return p
			}
		}
		select {
		case e := <-p.done:
			b, _ := os.ReadFile(p.logPath)
			t.Fatalf("console exited before ready: %v %s", e, b)
		case <-time.After(30 * time.Millisecond):
		}
	}
	t.Fatal("console did not become ready")
	return nil
}

func (p *consoleProcess) request(t *testing.T, method, path string, body any, authorized bool) (int, http.Header, []byte) {
	t.Helper()
	var content []byte
	if body != nil {
		var err error
		content, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, p.base+path, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if authorized {
		request.Header.Set("X-Console-Token", p.token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, data
}

func (p *consoleProcess) stop(t *testing.T, requested ...os.Signal) {
	t.Helper()
	signal := os.Signal(syscall.SIGTERM)
	if len(requested) > 0 {
		signal = requested[0]
	}
	if err := p.command.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			b, _ := os.ReadFile(p.logPath)
			t.Fatalf("shutdown failed: %v %s", err, b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown exceeded deadline")
	}
	b, _ := os.ReadFile(p.logPath)
	if !bytes.Contains(b, []byte("toSub2 stopped")) {
		t.Fatal("shutdown completion missing")
	}
	for _, secret := range []string{"process-fixture-access", "process-fixture-refresh", "process-fixture-id-token"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("private output leaked to console logs")
		}
	}
}

func TestConsoleProcessRestoreDownloadDeleteAndRestart(t *testing.T) {
	root := t.TempDir()
	ids := make([]string, 27)
	for i := range ids {
		ids[i] = fmt.Sprintf("fixture-%02d", i)
		dir := filepath.Join(root, ids[i])
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		output := map[string]any{"type": "sub2api-data", "version": 1, "proxies": []any{}, "accounts": []any{map[string]any{"name": fmt.Sprintf("smoke-%02d", i), "platform": "openai", "type": "oauth", "credentials": map[string]any{"email": fmt.Sprintf("process-%02d@smoke.invalid", i), "access_token": "process-fixture-access", "refresh_token": "process-fixture-refresh", "id_token": "process-fixture-id-token"}}}}
		data, _ := json.Marshal(output)
		if err := os.WriteFile(filepath.Join(dir, "sub2api-import-oauth.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := startConsoleProcess(t, root)
	status, _, body := p.request(t, "GET", "/", nil, false)
	if status != 200 || !bytes.Contains(body, []byte("/assets/")) {
		t.Fatal("embedded production React missing")
	}
	status, _, _ = p.request(t, "GET", "/api/jobs", nil, false)
	if status != 401 && status != 403 {
		t.Fatalf("unauthorized list: %d", status)
	}
	for page, want := range map[int]int{1: 20, 2: 7} {
		status, _, body = p.request(t, "GET", fmt.Sprintf("/api/jobs?page=%d", page), nil, true)
		var doc struct {
			Jobs       []map[string]any `json:"jobs"`
			Pagination map[string]any   `json:"pagination"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if status != 200 || len(doc.Jobs) != want || doc.Pagination["totalAll"] != float64(27) {
			t.Fatalf("page %d count=%d status=%d", page, len(doc.Jobs), status)
		}
		if bytes.Contains(body, []byte("process-fixture-")) {
			t.Fatal("private export token leaked in public API")
		}
	}
	status, _, body = p.request(t, "POST", "/api/jobs/query", map[string]any{"emails": []string{"process-26@smoke.invalid"}}, true)
	var query struct {
		Jobs []map[string]any `json:"jobs"`
	}
	_ = json.Unmarshal(body, &query)
	if status != 200 || len(query.Jobs) != 1 || query.Jobs[0]["id"] != ids[26] {
		t.Fatal("email filtering failed")
	}
	status, headers, body := p.request(t, "POST", "/api/jobs/download-batch", map[string]any{"ids": ids}, true)
	var export struct {
		Accounts []map[string]any `json:"accounts"`
	}
	_ = json.Unmarshal(body, &export)
	if status != 200 || len(export.Accounts) != 27 || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("batch export failed: status=%d count=%d", status, len(export.Accounts))
	}
	p.stop(t, os.Interrupt)
	next := startConsoleProcess(t, root)
	if next.token == p.token {
		t.Fatal("startup token reused across restart")
	}
	status, _, body = next.request(t, "POST", "/api/jobs/delete-batch", map[string]any{"ids": ids}, true)
	var deleted map[string]any
	_ = json.Unmarshal(body, &deleted)
	if status != 200 || deleted["deleted"] != float64(27) {
		t.Fatalf("delete 27 tasks failed: status=%d %s", status, body)
	}
	next.stop(t)
	last := startConsoleProcess(t, root)
	status, _, body = last.request(t, "GET", "/api/jobs", nil, true)
	var empty struct {
		Jobs []any `json:"jobs"`
	}
	_ = json.Unmarshal(body, &empty)
	if status != 200 || len(empty.Jobs) != 0 {
		t.Fatal("deleted jobs reappeared after restart")
	}
	last.stop(t)
}
