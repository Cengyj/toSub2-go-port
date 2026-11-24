package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"tosub2-go-port/internal/maintenance"
)

// This opt-in test registers only a temporary, uniquely named user launchd job.
// It never installs a LaunchAgent, starts a real runtime directory, or contacts
// an account service. Cleanup must confirm job/process exit before removing
// its files; an unconfirmed cleanup deliberately leaves that directory intact.
func TestNativeLaunchdConsoleLifecycle(t *testing.T) {
	if os.Getenv("TOSUB2_LAUNCHD_TEST") != "1" {
		t.Skip("set TOSUB2_LAUNCHD_TEST=1 for isolated native launchd acceptance")
	}
	for _, path := range []string{"/bin/launchctl", "/usr/bin/plutil", "/usr/bin/sandbox-exec", "/usr/bin/security", "/usr/sbin/lsof", "/usr/bin/pgrep"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("explicit launchd acceptance requires %s: %v", path, err)
		}
	}
	if os.Getuid() == 0 {
		t.Fatal("launchd acceptance requires a non-root GUI user")
	}
	currentUser, err := user.Current()
	if err != nil || currentUser.Uid != strconv.Itoa(os.Getuid()) || !filepath.IsAbs(currentUser.HomeDir) || currentUser.HomeDir == "/" {
		t.Fatal("native GUI user's home is unavailable", err)
	}
	// TestMain isolates ordinary tests by replacing HOME and publishing
	// TOSUB2_TEST_CACHE_NAMESPACE. This serial opt-in must use the real GUI user's
	// Keychain and cache namespace, matching its env -i launchd child. Keep the
	// shared harness unchanged and restore its HOME when this test finishes.
	home := currentUser.HomeDir
	t.Setenv("HOME", home)
	root, err := os.MkdirTemp("/tmp", "tosub2-launchd-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = os.Remove(root)
		t.Fatal(err)
	}
	unique := hex.EncodeToString(nonce[:])
	f := &launchdAcceptance{
		root: root, rootInfo: rootInfo, home: home, label: "com.tosub2.test." + unique,
		binary: filepath.Join(root, "tosub2"), plist: filepath.Join(root, "console.plist"),
		data: filepath.Join(root, "data"), credentialRoot: filepath.Join(root, "credentials"),
		service: "com.local.chatgpt-onboarding.credentials.local.launchd-test-" + unique,
		email:   "launchd-" + unique + "@synthetic.invalid", token: "synthetic-launchd-" + unique,
		pids: make(map[int]struct{}),
	}
	f.target = fmt.Sprintf("gui/%d/%s", os.Getuid(), f.label)
	t.Cleanup(func() { f.cleanup(t) })
	for _, dir := range []string{f.data, f.credentialRoot, filepath.Join(root, "tmp")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists, err := f.inspect(); err != nil || exists {
		t.Fatal("unique launchd target is unavailable", err)
	}
	for _, item := range f.keychainItems() {
		if err := f.keychainAbsent(item); err != nil {
			t.Fatal("synthetic Keychain identity must be absent before acceptance", err)
		}
	}
	f.keychainOwned = true
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "account requests are forbidden in launchd acceptance", http.StatusForbidden)
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	f.base = fmt.Sprintf("http://127.0.0.1:%d", f.port)
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", f.binary, "./cmd/tosub2")
	build.Dir = moduleRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOOS=darwin", "GOARCH="+runtime.GOARCH)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("production binary build failed: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	binarySHA := sha256.Sum256(binary)
	t.Logf("temporary production binary sha256=%x", binarySHA)
	template := filepath.Join(moduleRoot, "deploy", "com.tosub2.console.plist")
	templateBytes, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("temporary plist adapted from deployment template sha256=%x; changed fields=Label,ProgramArguments,WorkingDirectory,StandardOutPath,StandardErrorPath; production build CGO_ENABLED=0 GOWORK=off GOOS=darwin GOARCH=%s", sha256.Sum256(templateBytes), runtime.GOARCH)
	raw, err := f.run(10*time.Second, "/usr/bin/plutil", "-convert", "json", "-o", "-", template)
	if err != nil {
		t.Fatal("deployment plist is unreadable", err)
	}
	var plist map[string]any
	if json.Unmarshal(raw, &plist) != nil {
		t.Fatal("deployment plist is not a dictionary")
	}
	keepAlive, ok := plist["KeepAlive"].(map[string]any)
	if !ok || keepAlive["SuccessfulExit"] != false || plist["RunAtLoad"] != true || plist["Umask"] != float64(63) {
		t.Fatal("deployment plist must retain successful-exit suppression, RunAtLoad and private umask")
	}
	exitSeconds, exitOK := plist["ExitTimeOut"].(float64)
	throttleSeconds, throttleOK := plist["ThrottleInterval"].(float64)
	if !exitOK || !throttleOK || exitSeconds < 1 || exitSeconds > 60 || throttleSeconds < 1 || throttleSeconds > 30 {
		t.Fatal("deployment lifecycle deadlines are invalid for native acceptance")
	}
	f.stopDeadline = time.Duration(exitSeconds)*time.Second + 5*time.Second
	f.quietWindow = time.Duration(throttleSeconds)*time.Second + 500*time.Millisecond
	profile := filepath.Join(root, "network.sb")
	// Keep default file and Mach IPC access so the real user's Keychain works.
	// Darwin's network filter accepts localhost, not a literal IP. Deny only
	// external IP traffic; every application fixture URL remains 127.0.0.1.
	if err := os.WriteFile(profile, []byte("(version 1)\n(allow default)\n(deny network-outbound (require-all (remote ip \"*:*\") (require-not (remote ip \"localhost:*\"))))\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(5*time.Second, "/usr/bin/sandbox-exec", "-f", profile, "/usr/bin/true"); err != nil {
		t.Fatalf("sandbox policy validation failed before launchd registration: %v %s", err, output)
	}
	plist["Label"], plist["WorkingDirectory"] = f.label, root
	plist["StandardOutPath"], plist["StandardErrorPath"] = filepath.Join(root, "stdout.log"), filepath.Join(root, "stderr.log")
	plist["ProgramArguments"] = []string{
		"/usr/bin/env", "-i", "HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + filepath.Join(root, "tmp"),
		"TOSUB2_MAC_CREDENTIAL_ROOT=" + f.credentialRoot, "TOSUB2_MAC_KEYCHAIN_SERVICE=" + f.service,
		"TOSUB2_CONSOLE_TOKEN=" + f.token, "CHATGPT_BASE=" + upstream.URL, "AUTH_BASE=" + upstream.URL,
		"TOSUB2_SENTINEL_SDK_URL=" + upstream.URL + "/sdk.js", "/usr/bin/sandbox-exec", "-f", profile, f.binary,
		"--host=127.0.0.1", "--port=" + strconv.Itoa(f.port), "--output-root=" + f.data,
	}
	plistJSON, err := json.Marshal(plist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.plist, plistJSON, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(10*time.Second, "/usr/bin/plutil", "-convert", "xml1", f.plist); err != nil {
		t.Fatalf("temporary deployment plist conversion failed: %v %s", err, output)
	}
	f.assertReleased(t)
	if output, err := f.run(15*time.Second, "/bin/launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), f.plist); err != nil {
		t.Fatalf("native launchd bootstrap failed: %v %s", err, output)
	}
	firstPID := f.ready(t)
	f.assertAccounts(t, "")
	if release, err := maintenance.AcquireInstance(f.data); err == nil {
		_ = release()
		t.Fatal("running launchd console did not retain its instance lease")
	}
	status, body := f.request(t, http.MethodPost, "/api/jobs/batch", map[string]string{"text": f.email})
	var imported struct {
		OK      bool `json:"ok"`
		Created int  `json:"created"`
		Jobs    []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &imported) != nil || !imported.OK || imported.Created != 1 || len(imported.Jobs) != 1 || imported.Jobs[0].ID == "" {
		t.Fatalf("save-only synthetic import failed: status=%d body=%s", status, body)
	}
	id := imported.Jobs[0].ID
	f.assertAccounts(t, id)
	f.stop(t)
	f.assertMetadata(t, id)
	f.assertReleased(t)
	if output, err := f.run(10*time.Second, "/bin/launchctl", "kickstart", f.target); err != nil {
		t.Fatalf("explicit native launchd restart failed: %v %s", err, output)
	}
	secondPID := f.ready(t)
	if firstPID == secondPID {
		t.Fatal("kickstart did not create a new console process")
	}
	f.assertAccounts(t, id)
	f.stop(t)
	f.assertMetadata(t, id)
	f.assertReleased(t)
	if requests.Load() != 0 {
		t.Fatalf("save-only launchd acceptance issued %d forbidden account requests", requests.Load())
	}
	for _, item := range f.keychainItems() {
		if err := f.keychainAbsent(item); err != nil {
			t.Fatal("email-only import unexpectedly persisted a Keychain secret", err)
		}
	}
	t.Logf("native launchd accepted: label=%s pids=%d,%d exit_codes=0,0 upstream_requests=0 binary_sha256=%x", f.label, firstPID, secondPID, binarySHA)
}

type launchdAcceptance struct {
	root, home, binary, plist, data, credentialRoot, service, email, token, label, target, base string
	rootInfo                                                                                    os.FileInfo
	port                                                                                        int
	completedStops                                                                              int
	keychainOwned                                                                               bool
	pids                                                                                        map[int]struct{}
	stopDeadline, quietWindow                                                                   time.Duration
}

func (f *launchdAcceptance) run(timeout time.Duration, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = []string{"HOME=" + f.home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + filepath.Join(f.root, "tmp")}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func launchdValue(output []byte, name string) string {
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+" = "); ok {
			return value
		}
	}
	return ""
}

func (f *launchdAcceptance) inspect() ([]byte, bool, error) {
	output, err := f.run(5*time.Second, "/bin/launchctl", "print", f.target)
	if err != nil {
		if bytes.Contains(output, []byte(`Could not find service "`+f.label+`"`)) {
			return nil, false, nil
		}
		return output, false, fmt.Errorf("cannot inspect exact launchd target: %w", err)
	}
	if launchdValue(output, "path") != f.plist || launchdValue(output, "program") != "/usr/bin/env" || !bytes.Contains(output, []byte("\n\t\t"+f.binary+"\n")) {
		return output, true, errors.New("launchd target does not match this test's plist and executable")
	}
	if pid, _ := strconv.Atoi(launchdValue(output, "pid")); pid > 0 {
		f.pids[pid] = struct{}{}
	}
	return output, true, nil
}

func (f *launchdAcceptance) processesExited() bool {
	for pid := range f.pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			return false
		}
	}
	return true
}

func (f *launchdAcceptance) ready(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, exists, err := f.inspect()
		if err != nil || !exists {
			t.Fatal("launchd job vanished or changed ownership during startup", err)
		}
		pid, _ := strconv.Atoi(launchdValue(state, "pid"))
		if pid > 0 && launchdValue(state, "state") == "running" {
			listeners, err := f.run(3*time.Second, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP:"+strconv.Itoa(f.port), "-sTCP:LISTEN", "-Fpn")
			if err == nil && bytes.Contains(listeners, []byte("p"+strconv.Itoa(pid)+"\n")) && bytes.Contains(listeners, []byte(fmt.Sprintf("n127.0.0.1:%d\n", f.port))) {
				status, body := f.request(t, http.MethodGet, "/api/bootstrap", nil)
				var bootstrap struct {
					Token string `json:"token"`
				}
				if status != http.StatusOK || json.Unmarshal(body, &bootstrap) != nil || bootstrap.Token != f.token {
					t.Fatal("listener bootstrap identity did not match the synthetic launchd job")
				}
				if status, _ := f.request(t, http.MethodGet, "/api/health", nil); status != http.StatusOK {
					t.Fatal("native launchd health check failed", status)
				}
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("native launchd console did not acquire its own healthy loopback listener")
	return 0
}

func (f *launchdAcceptance) request(t *testing.T, method, path string, value any) (int, []byte) {
	t.Helper()
	var body []byte
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), method, f.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Console-Token", f.token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("native launchd HTTP request failed", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, content
}

func (f *launchdAcceptance) assertAccounts(t *testing.T, expectedID string) {
	t.Helper()
	status, body := f.request(t, http.MethodGet, "/api/jobs", nil)
	var page struct {
		Jobs []struct{ ID, Email, Status, Operation string } `json:"jobs"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatal("native launchd account list failed")
	}
	if expectedID == "" {
		if len(page.Jobs) != 0 {
			t.Fatal("isolated launchd startup was not empty")
		}
		return
	}
	if len(page.Jobs) != 1 || page.Jobs[0].ID != expectedID || page.Jobs[0].Email != f.email || page.Jobs[0].Status != "imported" || page.Jobs[0].Operation != "" {
		t.Fatalf("imported account was lost or started an operation: %s", body)
	}
}

func (f *launchdAcceptance) stop(t *testing.T) {
	t.Helper()
	if _, exists, err := f.inspect(); err != nil || !exists {
		t.Fatal("cannot signal a launchd job without confirmed ownership", err)
	}
	if output, err := f.run(5*time.Second, "/bin/launchctl", "kill", "SIGTERM", f.target); err != nil {
		t.Fatalf("SIGTERM dispatch failed: %v %s", err, output)
	}
	deadline := time.Now().Add(f.stopDeadline)
	var quietSince time.Time
	for time.Now().Before(deadline) {
		state, exists, err := f.inspect()
		if err != nil || !exists {
			t.Fatal("launchd ownership changed during graceful shutdown", err)
		}
		if launchdValue(state, "pid") == "" && launchdValue(state, "last exit code") == "0" && launchdValue(state, "state") != "running" && f.processesExited() {
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
			if time.Since(quietSince) >= f.quietWindow {
				f.completedStops++
				output, err := os.ReadFile(filepath.Join(f.root, "stderr.log"))
				if err != nil || bytes.Count(output, []byte("toSub2 stopped")) != f.completedStops {
					t.Fatal("clean launchd exit did not log shutdown completion", err)
				}
				return
			}
		} else {
			if !quietSince.IsZero() {
				t.Fatal("KeepAlive restarted a successfully stopped console")
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("launchd did not retain a stopped job with exit code zero")
}

func (f *launchdAcceptance) assertMetadata(t *testing.T, id string) {
	t.Helper()
	path := filepath.Join(f.data, "job-meta.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("synthetic metadata is missing or not private", err)
	}
	data, err := os.ReadFile(path)
	var snapshot struct {
		Version int
		Jobs    []struct{ ID, Email, Status, Operation string }
	}
	if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Version != 2 || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].ID != id || snapshot.Jobs[0].Email != f.email || snapshot.Jobs[0].Status != "imported" || snapshot.Jobs[0].Operation != "" {
		t.Fatal("graceful launchd exit did not preserve the import-only metadata", err)
	}
}

func (f *launchdAcceptance) assertReleased(t *testing.T) {
	t.Helper()
	release, err := maintenance.AcquireOffline(f.data)
	if err != nil {
		t.Fatal("stopped launchd console retained its instance or work lease", err)
	}
	if err := release(); err != nil {
		t.Fatal("local lease verification did not release its handles", err)
	}
}

type launchdKeychainItem struct{ account, service string }

func (f *launchdAcceptance) keychainItems() []launchdKeychainItem {
	return []launchdKeychainItem{
		{f.email, f.service},
		{f.email, fmt.Sprintf("com.local.chatgpt-onboarding.credential-migration-v1.%x", sha256.Sum256([]byte(f.service)))},
		{fmt.Sprintf("%x", sha256.Sum256([]byte(f.email))), f.service + ".recovery-state-v1"},
	}
}

func (f *launchdAcceptance) keychainAbsent(item launchdKeychainItem) error {
	_, err := f.run(5*time.Second, "/usr/bin/security", "find-generic-password", "-a", item.account, "-s", item.service)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil
	}
	if err == nil {
		return errors.New("exact synthetic Keychain item was unexpectedly present")
	}
	return fmt.Errorf("exact synthetic Keychain item was present or could not be checked: %w", err)
}

func (f *launchdAcceptance) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() {
		for _, name := range []string{"stdout.log", "stderr.log"} {
			if file, err := os.Open(filepath.Join(f.root, name)); err == nil {
				data, _ := io.ReadAll(io.LimitReader(file, 64<<10))
				_ = file.Close()
				t.Logf("synthetic launchd %s: %s", name, data)
			}
		}
	}
	_, exists, err := f.inspect()
	if err != nil {
		t.Errorf("retained %s: cannot confirm launchd ownership before cleanup: %v", f.root, err)
		return
	}
	if exists {
		if output, err := f.run(40*time.Second, "/bin/launchctl", "bootout", f.target); err != nil {
			t.Errorf("retained %s: exact launchd bootout failed: %v %s", f.root, err, output)
			return
		}
	}
	if _, exists, err := f.inspect(); err != nil || exists {
		t.Errorf("retained %s: exact launchd target is still registered: %v", f.root, err)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for !f.processesExited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !f.processesExited() {
		t.Errorf("retained %s: a PID observed under this exact launchd job still exists", f.root)
		return
	}
	// Query only the unique executable path, never signal a process based on a
	// substring or an old PID. launchd bootout owns process termination.
	output, err := f.run(5*time.Second, "/usr/bin/pgrep", "-f", "^"+regexp.QuoteMeta(f.binary)+"( |$)")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(bytes.TrimSpace(output)) != 0 {
		t.Errorf("retained %s: executable exit was not confirmed: %v", f.root, err)
		return
	}
	if f.keychainOwned {
		items := f.keychainItems()
		// Recovery keys use hashed accounts recorded below this isolated root.
		// Inspecting these filenames is not a Keychain namespace enumeration.
		entries, err := os.ReadDir(filepath.Join(f.credentialRoot, "recovery-state-keyrefs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retained %s: cannot inspect synthetic key references: %v", f.root, err)
			return
		}
		for _, entry := range entries {
			account := strings.TrimSuffix(entry.Name(), ".json")
			decoded, err := hex.DecodeString(account)
			if !entry.Type().IsRegular() || err != nil || len(decoded) != sha256.Size || entry.Name() != account+".json" {
				t.Errorf("retained %s: unknown synthetic recovery key reference", f.root)
				return
			}
			items = append(items, launchdKeychainItem{account, f.service + ".recovery-state-v1"})
		}
		for _, item := range items {
			_, err := f.run(5*time.Second, "/usr/bin/security", "delete-generic-password", "-a", item.account, "-s", item.service)
			if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 44) {
				t.Errorf("retained %s: exact synthetic Keychain cleanup failed: %v", f.root, err)
				return
			}
			if err := f.keychainAbsent(item); err != nil {
				t.Errorf("retained %s: synthetic Keychain cleanup was not confirmed: %v", f.root, err)
				return
			}
		}
	}
	current, err := os.Lstat(f.root)
	if err != nil || !os.SameFile(f.rootInfo, current) || current.Mode()&os.ModeSymlink != 0 {
		t.Errorf("retained %s: temporary directory ownership changed", f.root)
		return
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Error("synthetic launchd directory cleanup failed", err)
		return
	}
	t.Log("native launchd cleanup confirmed: exact job absent, executable exited, synthetic Keychain items absent, temporary directory removed; OS lock files retained")
}
