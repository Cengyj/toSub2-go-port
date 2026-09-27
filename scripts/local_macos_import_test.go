package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installImportFixture(f *localFixture, fail bool) {
	f.t.Helper()
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$TOSUB2_MAC_KEYCHAIN_SERVICE\" \"$TOSUB2_MAC_CREDENTIAL_ROOT\" \"$TMPDIR\" \"$@\" > \"$FAKE_STATE/import-call\"\n"
	if fail {
		script += "printf 'Imported=1 Failed=1\\n'\nexit 1\n"
	} else {
		script += "printf 'Imported=1 SkippedExisting=1\\n'\n"
	}
	f.write(filepath.Join(f.runtime(), "bin", "tosub2"), script, 0700)
}

func TestLocalMacOSCredentialImportIsOfflineAndIsolated(t *testing.T) {
	f := newLocalFixture(t)
	f.ownedRuntime()
	installImportFixture(f, false)
	source := filepath.Join(filepath.Dir(f.root), "legacy & 'quoted'")
	emails := filepath.Join(filepath.Dir(f.root), "accounts & 'quoted'.json")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	f.write(emails, "{\"jobs\":[{\"email\":\"owner@example.test\"}]}", 0600)
	if out := f.ok("import-credentials", source, emails); !strings.Contains(out, "Imported=1") {
		t.Fatal("import result not forwarded")
	}
	want := strings.Join([]string{f.namespace, filepath.Join(f.runtime(), "data", "credentials"), filepath.Join(f.runtime(), "tmp") + "/", "--import-legacy-credentials=" + source, "--credential-emails-file=" + emails, "--output-root=" + filepath.Join(f.runtime(), "data"), ""}, "\n")
	if f.content(filepath.Join(f.state, "import-call")) != want {
		t.Fatal("import lost namespace, path quoting, or explicit source arguments")
	}
	calls := f.content(filepath.Join(f.state, "launchctl-calls"))
	if strings.Contains(calls, "bootstrap") || strings.Contains(calls, "bootout") {
		t.Fatal("offline import started or stopped a service")
	}
	for _, path := range []string{filepath.Join(f.state, "security-calls"), filepath.Join(f.runtime(), "port"), filepath.Join(f.state, "builds")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("offline import unexpectedly controlled Keychain, service port or build")
		}
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("legacy directory changed")
	}
}

func TestLocalMacOSCredentialImportDoesNotInterruptRunningService(t *testing.T) {
	f := newLocalFixture(t)
	f.ok("start")
	installImportFixture(f, false)
	before := f.content(filepath.Join(f.state, "launchctl-calls"))
	out, err := f.run("import-credentials", "/explicit/legacy", "/explicit/accounts")
	if err == nil || !strings.Contains(out, "Stop this local service") {
		t.Fatal("live service import was not rejected clearly")
	}
	after := f.content(filepath.Join(f.state, "launchctl-calls"))
	delta := strings.TrimPrefix(after, before)
	if strings.Contains(delta, "bootout") || strings.Contains(delta, "bootstrap") {
		t.Fatal("import changed running service state")
	}
	if _, err := os.Stat(filepath.Join(f.state, "import-call")); !os.IsNotExist(err) {
		t.Fatal("import reached credential command while service was active")
	}
}

func TestLocalMacOSCredentialImportPropagatesFailureAndPreservesRuntime(t *testing.T) {
	f := newLocalFixture(t)
	f.ownedRuntime()
	installImportFixture(f, true)
	path := filepath.Join(f.runtime(), "data", "job-meta.json")
	f.write(path, "preserved-metadata", 0600)
	out, err := f.run("import-credentials", "/explicit/legacy", "/explicit/accounts")
	if err == nil || !strings.Contains(out, "Failed=1") {
		t.Fatal("partial import failure became success")
	}
	if f.content(path) != "preserved-metadata" {
		t.Fatal("failed import changed existing metadata")
	}
	if _, err := os.Stat(filepath.Join(f.runtime(), ".operation-lock")); !os.IsNotExist(err) {
		t.Fatal("failed import left its lifecycle lock")
	}
	if _, err := f.run("import-credentials", "/missing-email-argument"); err == nil {
		t.Fatal("incomplete import arguments were accepted")
	}
}
