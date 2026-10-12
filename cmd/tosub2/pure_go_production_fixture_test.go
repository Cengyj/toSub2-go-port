package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
)

// The harness serves only local synthetic TLS endpoints. Authentication and
// worker execution happen in the separately built production executable.
func runPureGoProductionFixture(t *testing.T, binary string, cfg config.Config, scenario, email, password string, wantsMFA bool, certificate *x509.Certificate) {
	t.Helper()
	if runtime.GOOS != "linux" || !filepath.IsAbs(binary) {
		t.Fatal("production dependency-free fixture requires an absolute Linux executable")
	}
	for _, name := range []string{"node", "python", "python3", "chrome", "chromium"} {
		if _, err := os.Stat("/" + name); err == nil {
			t.Fatal("scratch fixture unexpectedly contains an external authentication runtime")
		}
	}
	privateRoot := t.TempDir()
	caPath := filepath.Join(privateRoot, "fixture-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal("could not prepare private fixture CA")
	}
	checkpointPath := filepath.Join(cfg.OutputRoot, "legacy-login", "login-checkpoint.json")
	outputPath := filepath.Join(cfg.OutputRoot, "legacy-login", "sub2api-import-oauth.json")
	args := []string{
		"--debug-auth",
		"--email=" + email, "--output-root=" + cfg.OutputRoot,
		"--chatgpt-base=" + cfg.ChatGPTBase, "--auth-base=" + cfg.AuthBase, "--sentinel-sdk-url=" + cfg.SentinelSDKURL,
		"--request-timeout-ms=10000", "--connection-retries=0", "--proxy-max-attempts=1",
		"--checkpoint=" + checkpointPath, "--output-mode=both", "--out=" + filepath.Join(privateRoot, "session.json"), "--sub2api-out=" + outputPath,
	}
	if !strings.HasPrefix(scenario, "new-") {
		args = append(args, "--resume-checkpoint="+checkpointPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/no-external-executables", "TMPDIR=" + privateRoot, "SSL_CERT_FILE=" + caPath, "SSL_CERT_DIR=/no-system-certificate-directory", "CHATGPT_LOGIN_PASSWORD=" + password}
	if strings.Contains(scenario, "email") {
		cmd.Env = cmd.Env[:len(cmd.Env)-1]
		codes := "123456\n"
		if strings.Contains(scenario, "rejection") {
			codes = "000000\n123456\n"
			if strings.HasSuffix(scenario, "resend") {
				codes = "000000\nr\n123456\n"
			}
		}
		cmd.Stdin = strings.NewReader(codes)
	}
	if wantsMFA {
		cmd.Env = append(cmd.Env, "CHATGPT_TOTP_SECRET=JBSWY3DPEHPK3PXP")
		cmd.Stdin = strings.NewReader("654321\n")
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal("production executable could not start")
	}
	var observedWorkers atomic.Int32
	var foreignWorkers atomic.Bool
	stop, observed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(observed)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			inspectProductionChildren(binary, cmd.Process.Pid, &observedWorkers, &foreignWorkers)
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	err := cmd.Wait()
	close(stop)
	<-observed
	if err != nil {
		// Remote bodies, process arguments and exception text are never emitted.
		code := regexp.MustCompile(`GO_JS_RUNTIME: [A-Z0-9_]{1,80}`).FindString(output.String())
		t.Fatalf("production CLI failed: timeout=%t safe_code=%q", ctx.Err() != nil, code)
	}
	if foreignWorkers.Load() {
		t.Fatal("production process launched a worker from another executable")
	}
	if scenario != "legacy-authenticated-checkpoint" && scenario != "legacy-email-accepted-checkpoint" && observedWorkers.Load() == 0 {
		t.Fatal("production process did not expose a same-executable worker")
	}
	if strings.Count(output.String(), "[legacy_session_start]") != 1 {
		t.Fatal("production CLI restarted the authentication session")
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Contains(data, []byte("fixture-refresh")) {
		t.Fatal("production CLI did not save the synthetic OAuth output")
	}
	entries, err := os.ReadDir(privateRoot)
	if err != nil {
		t.Fatal("could not verify private worker cleanup")
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tosub2-js-") || strings.HasPrefix(entry.Name(), "tosub2-node-") {
			t.Fatal("production worker left a private input file")
		}
	}
	t.Logf("production CLI passed: same_executable_worker=%t external_worker=false private_inputs_removed=true", observedWorkers.Load() > 0)
}

func inspectProductionChildren(binary string, parentPID int, observed *atomic.Int32, foreign *atomic.Bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		path := filepath.Join("/proc", entry.Name())
		status, err := os.ReadFile(filepath.Join(path, "status"))
		if err != nil {
			continue
		}
		isChild := false
		for _, line := range strings.Split(string(status), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "PPid:" && fields[1] == strconv.Itoa(parentPID) {
				isChild = true
				break
			}
		}
		if !isChild {
			continue
		}
		executable, err := os.Readlink(filepath.Join(path, "exe"))
		if err != nil {
			continue
		}
		arguments, err := os.ReadFile(filepath.Join(path, "cmdline"))
		if err != nil {
			continue
		}
		parts := bytes.Split(arguments, []byte{0})
		if executable != binary || len(parts) < 3 || string(parts[1]) != "--auth-js-worker" {
			foreign.Store(true)
		} else {
			observed.Add(1)
		}
	}
}
