package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newLegacyLocalFixture(t *testing.T) *localFixture {
	t.Helper()
	f := newLocalFixture(t)
	f.runtimeRoot = f.runtime() + "-legacy"
	f.label += ".legacy"
	f.namespace += ".legacy"
	f.launcher = "local-legacy-macos.sh"
	f.env = append(f.env, "FAKE_RUNTIME="+f.runtime(), "FAKE_LABEL="+f.label)
	_, source, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(source), f.launcher))
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "scripts", f.launcher), string(b), 0700)
	if err := os.MkdirAll(filepath.Join(f.root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "bin", "tosub2"), "#!/bin/sh\nexit 0\n", 0700)
	for _, executable := range []string{"node", "python", "python3"} {
		f.tool(executable, "#!/bin/sh\nprintf 'unexpected helper invocation\\n' >> \"$FAKE_STATE/helper-calls\"\nexit 99\n")
	}
	return f
}

func TestLocalGoMacOSDeploysAndRestartsWithoutHelpers(t *testing.T) {
	f := newLegacyLocalFixture(t)
	f.ok("start")
	p := localPlist(t, filepath.Join(f.runtime(), "launchd", f.label+".plist"))
	for _, value := range p["ProgramArguments"].([]any) {
		arg := value.(string)
		if strings.HasPrefix(arg, "--legacy-") || strings.HasPrefix(arg, "--auth-transport=") || strings.HasPrefix(arg, "--browser") {
			t.Fatal("Go deployment configured removed runtime options", arg)
		}
	}
	for _, path := range []string{"helpers", "bin/node", "js-runtime"} {
		if _, err := os.Stat(filepath.Join(f.runtime(), path)); !os.IsNotExist(err) {
			t.Fatal("Go deployment staged removed helper files", path, err)
		}
	}
	for _, path := range []string{"helper-calls", "builds"} {
		if _, err := os.Stat(filepath.Join(f.state, path)); !os.IsNotExist(err) {
			t.Fatal("Go deployment unnecessarily invoked a build/helper", path, err)
		}
	}
	if err := os.Remove(filepath.Join(f.bin, "go")); err != nil {
		t.Fatal(err)
	}
	f.ok("restart")
	if _, err := os.Stat(filepath.Join(f.runtime(), "js-runtime")); !os.IsNotExist(err) {
		t.Fatal("restart created an obsolete host selector", err)
	}
	f.ok("clean", "--yes")
	if _, err := os.Stat(f.runtime()); !os.IsNotExist(err) {
		t.Fatal("Go deployment cleanup retained the owned runtime", err)
	}
}

func TestLocalGoMacOSMigratesObsoleteHelpersAndKeepsData(t *testing.T) {
	f := newLegacyLocalFixture(t)
	f.ok("start")
	f.ok("stop")
	for _, directory := range []string{"helpers/src/cloudflare-ctf/node_modules", "helpers/.venv/bin"} {
		if err := os.MkdirAll(filepath.Join(f.runtime(), directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"bin/node", "helpers/src/cloudflare-ctf/sentinel_runtime.cjs", "helpers/src/tls_transport.py", "helpers/.venv/bin/python"} {
		f.write(filepath.Join(f.runtime(), path), "obsolete fixture only", 0700)
	}
	f.write(filepath.Join(f.runtime(), "js-runtime"), "node\n", 0600)
	data := filepath.Join(f.runtime(), "data", "job-meta.json")
	f.write(data, "private fixture data remains", 0600)
	f.ok("restart")
	for _, path := range []string{"helpers", "bin/node", "js-runtime"} {
		if _, err := os.Stat(filepath.Join(f.runtime(), path)); !os.IsNotExist(err) {
			t.Fatal("Go migration retained deployed helper files", path, err)
		}
	}
	if f.content(data) != "private fixture data remains" {
		t.Fatal("Go migration changed account data")
	}
	if _, err := os.Stat(filepath.Join(f.state, "helper-calls")); !os.IsNotExist(err) {
		t.Fatal("Go migration invoked an external helper", err)
	}
	f.ok("clean", "--yes")
}

func TestLocalGoMacOSCleanupWaitsForGoWorkerExit(t *testing.T) {
	f := newLegacyLocalFixture(t)
	f.ok("start")
	f.write(filepath.Join(f.state, "go-worker-running"), "", 0600)
	f.write(filepath.Join(f.state, "fast-clock"), "1000\n", 0600)
	out, err := f.run("clean", "--yes")
	if err == nil || !strings.Contains(out, "30 seconds") {
		t.Fatalf("live Go worker cleanup: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.runtime(), "bin", "tosub2")); err != nil {
		t.Fatal("cleanup removed the live worker's executable", err)
	}
	if _, err := os.Stat(filepath.Join(f.state, "security-calls")); !os.IsNotExist(err) {
		t.Fatal("cleanup reached Keychain while a Go worker remained alive")
	}
	_ = os.Remove(filepath.Join(f.state, "go-worker-running"))
	_ = os.Remove(filepath.Join(f.state, "fast-clock"))
	f.ok("clean", "--yes")
}

func localPlist(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("parse launchd plist: %v %s", err, b)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestLocalGoMacOSCleanupPreservesSiblingVariant(t *testing.T) {
	f := newLegacyLocalFixture(t)
	siblingRuntime := strings.TrimSuffix(f.runtime(), "-legacy")
	if err := os.MkdirAll(filepath.Join(siblingRuntime, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(siblingRuntime, "data", "job-meta.json")
	f.write(canary, "sibling variant stays", 0600)
	f.ok("start")
	p := localPlist(t, filepath.Join(f.runtime(), "launchd", f.label+".plist"))
	env := p["EnvironmentVariables"].(map[string]any)
	if env["TOSUB2_MAC_KEYCHAIN_SERVICE"] != f.namespace {
		t.Fatal("Go service lost credential isolation", env)
	}
	if _, ok := env["CHROME_PATH"]; ok {
		t.Fatal("Go service configured Chrome")
	}
	f.ok("clean", "--yes")
	if _, err := os.Stat(f.runtime()); !os.IsNotExist(err) {
		t.Fatal("owned runtime retained", err)
	}
	if f.content(canary) != "sibling variant stays" {
		t.Fatal("Go cleanup modified sibling variant")
	}
	want := "delete-generic-password -s " + f.namespace + "\n"
	if got := f.content(filepath.Join(f.state, "security-calls")); got != want {
		t.Fatalf("cleanup reached another Keychain namespace: %q", got)
	}
}

func TestLocalGoMacOSMigrationWaitsForObsoleteHelperExit(t *testing.T) {
	for _, runningMarker := range []string{"helper-running", "obsolete-python-running"} {
		t.Run(runningMarker, func(t *testing.T) {
			f := newLegacyLocalFixture(t)
			f.ok("start")
			f.write(filepath.Join(f.state, runningMarker), "", 0600)
			f.write(filepath.Join(f.state, "fast-clock"), "1000\n", 0600)
			out, err := f.run("clean", "--yes")
			if err == nil || !strings.Contains(out, "30 seconds") {
				t.Fatalf("live obsolete helper cleanup: %v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(f.runtime(), "bin", "tosub2")); err != nil {
				t.Fatal("cleanup removed files before obsolete worker exited", err)
			}
			if _, err := os.Stat(filepath.Join(f.state, "security-calls")); !os.IsNotExist(err) {
				t.Fatal("cleanup reached Keychain while obsolete helper remains alive")
			}
			_ = os.Remove(filepath.Join(f.state, runningMarker))
			_ = os.Remove(filepath.Join(f.state, "fast-clock"))
			f.ok("clean", "--yes")
		})
	}
}
