package scripts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type localFixture struct {
	t                                        *testing.T
	root, home, state, bin, namespace, label string
	env                                      []string
}

func newLocalFixture(t *testing.T) *localFixture {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS launcher regression; launchctl/security/go are faked, but native stat/plutil are required")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &localFixture{t: t, root: filepath.Join(base, "repo & 'quoted'"), home: filepath.Join(base, "home & 'quoted'"), state: filepath.Join(base, "fake-state"), bin: filepath.Join(base, "fake-bin")}
	for _, p := range []string{filepath.Join(f.root, "scripts"), f.state, f.bin, filepath.Join(f.home, "Library", "Application Support")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, source, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(source), "local-macos.sh"))
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "scripts", "local-macos.sh"), string(b), 0700)
	f.write(filepath.Join(f.root, "source-preserve.txt"), "source remains intact", 0600)
	sum := sha256.Sum256([]byte(f.root))
	suffix := hex.EncodeToString(sum[:])[:16]
	f.namespace = "com.local.chatgpt-onboarding.credentials.local." + suffix
	f.label = "com.tosub2.local." + suffix
	f.env = []string{"PATH=" + f.bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + f.home, "FAKE_STATE=" + f.state, "FAKE_REPO=" + f.root, "FAKE_RUNTIME=" + f.runtime(), "FAKE_LABEL=" + f.label, "FAKE_UID=" + strconv.Itoa(os.Getuid()), "TOSUB2_LOCAL_PORT=49191"}
	f.tool("go", `#!/bin/sh
set -eu
printf 'build\n' >> "$FAKE_STATE/builds"
printf '%s\n' "$TMPDIR" "$GOTMPDIR" > "$FAKE_STATE/build-env"
out=
while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then shift; out=$1; fi
 shift
done
[ -n "$out" ]
printf '#!/bin/sh\nexit 0\n' > "$out"
`)
	f.tool("launchctl", `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_STATE/launchctl-calls"
target="gui/$FAKE_UID/$FAKE_LABEL"
case "$1" in
 print)
  [ "$2" != "gui/$FAKE_UID" ] || exit 0
  [ "$2" = "$target" ] || exit 99
  [ ! -f "$FAKE_STATE/inspect-error" ] || exit 5
  [ -f "$FAKE_STATE/loaded" ] || exit 113
  if [ -f "$FAKE_STATE/remaining" ]; then
   n=$(cat "$FAKE_STATE/remaining")
   if [ "$n" -eq 0 ]; then rm -f "$FAKE_STATE/remaining" "$FAKE_STATE/loaded"; exit 113; fi
   printf '%s\n' "$((n-1))" > "$FAKE_STATE/remaining"
  fi
  program="$FAKE_RUNTIME/bin/tosub2"
  [ ! -f "$FAKE_STATE/foreign-service" ] || program=/somebody/elses/tosub2
  printf 'path = %s\nprogram = %s\npid = %s\n' "$FAKE_RUNTIME/launchd/$FAKE_LABEL.plist" "$program" "$(cat "$FAKE_STATE/pid")"
  ;;
 bootstrap)
  [ "$2" = "gui/$FAKE_UID" ] || exit 99
  [ "$3" = "$FAKE_RUNTIME/launchd/$FAKE_LABEL.plist" ] || exit 99
  if [ -f "$FAKE_STATE/loaded" ]; then printf 'bootstrap-too-early\n' > "$FAKE_STATE/bootstrap-error"; exit 5; fi
  /bin/sleep 600 >/dev/null 2>&1 &
  pid=$!
  printf '%s\n' "$pid" > "$FAKE_STATE/pid"
  printf '%s\n' "$pid" >> "$FAKE_STATE/all-pids"
  : > "$FAKE_STATE/loaded"
  ;;
 bootout)
  [ "$2" = "$target" ] || exit 99
  if [ -f "$FAKE_STATE/block-bootout" ]; then exec /bin/sleep 600; fi
  [ ! -f "$FAKE_STATE/hold-loaded" ] || exit 0
  [ -f "$FAKE_STATE/loaded" ] || exit 113
  printf '3\n' > "$FAKE_STATE/remaining"
  pid=$(cat "$FAKE_STATE/pid")
  /bin/kill -TERM "$pid" 2>/dev/null || :
  ;;
 *) exit 99 ;;
esac
`)
	f.tool("ps", `#!/bin/sh
set -eu
pid=; kind=
while [ "$#" -gt 0 ]; do
 case "$1" in -p) shift; pid=$1 ;; -o) shift; kind=$1 ;; esac
 shift
done
[ -f "$FAKE_STATE/all-pids" ] && grep -Fx "$pid" "$FAKE_STATE/all-pids" >/dev/null || exit 1
case "$kind" in
 uid=) printf '%s\n' "$FAKE_UID" ;;
 command=) printf '%s --host=127.0.0.1 --port=49191\n' "$FAKE_RUNTIME/bin/tosub2" ;;
 *) exit 1 ;;
esac
`)
	f.tool("security", `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_STATE/security-calls"
code=44
if [ -s "$FAKE_STATE/security-results" ]; then
 code=$(head -1 "$FAKE_STATE/security-results")
 tail -n +2 "$FAKE_STATE/security-results" > "$FAKE_STATE/security-next"
 mv "$FAKE_STATE/security-next" "$FAKE_STATE/security-results"
fi
printf 'private-keychain-canary\n' >&2
exit "$code"
`)
	f.tool("curl", "#!/bin/sh\nprintf 'health\\n' >> \"$FAKE_STATE/health-calls\"\nexit 0\n")
	f.tool("lsof", `#!/bin/sh
set -eu
pid=
while [ "$#" -gt 0 ]; do
 if [ "$1" = -p ]; then shift; pid=$1; fi
 shift
done
if [ -z "$pid" ]; then
 [ ! -f "$FAKE_STATE/occupied-port" ] || { printf '77\n'; exit 0; }
 exit 1
fi
[ ! -f "$FAKE_STATE/no-owned-listener" ] || exit 1
[ -f "$FAKE_STATE/loaded" ] && [ "$pid" = "$(cat "$FAKE_STATE/pid")" ] || exit 1
printf '%s\n' "$pid"
`)
	f.tool("date", `#!/bin/sh
set -eu
if [ -f "$FAKE_STATE/fast-clock" ]; then
 now=$(cat "$FAKE_STATE/fast-clock")
 printf '%s\n' "$((now+1))" > "$FAKE_STATE/fast-clock"
 printf '%s\n' "$now"
else exec /bin/date "$@"; fi
`)
	f.tool("sleep", `#!/bin/sh
if [ -f "$FAKE_STATE/fast-clock" ] || [ -f "$FAKE_STATE/fast-sleep" ]; then exec /bin/sleep 0.001; fi
exec /bin/sleep "$@"
`)
	t.Cleanup(func() {
		if b, e := os.ReadFile(filepath.Join(f.state, "all-pids")); e == nil {
			for _, p := range strings.Fields(string(b)) {
				n, _ := strconv.Atoi(p)
				if n > 1 {
					process, _ := os.FindProcess(n)
					_ = process.Kill()
				}
			}
		}
	})
	return f
}
func (f *localFixture) write(path, text string, mode os.FileMode) {
	f.t.Helper()
	if e := os.WriteFile(path, []byte(text), mode); e != nil {
		f.t.Fatal(e)
	}
}
func (f *localFixture) tool(name, text string) { f.write(filepath.Join(f.bin, name), text, 0700) }
func (f *localFixture) runtime() string {
	return filepath.Join(f.home, "Library", "Application Support", "toSub2-local", strings.TrimPrefix(f.label, "com.tosub2.local."))
}
func (f *localFixture) run(args ...string) (string, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", append([]string{filepath.Join(f.root, "scripts", "local-macos.sh")}, args...)...)
	c.Env = f.env
	b, e := c.CombinedOutput()
	if ctx.Err() != nil {
		f.t.Fatalf("launcher exceeded local test deadline: %v %s", ctx.Err(), b)
	}
	return string(b), e
}
func (f *localFixture) ok(args ...string) string {
	f.t.Helper()
	s, e := f.run(args...)
	if e != nil {
		f.t.Fatalf("%v: %v\n%s", args, e, s)
	}
	return s
}
func (f *localFixture) content(path string) string {
	f.t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		f.t.Fatal(e)
	}
	return string(b)
}
func (f *localFixture) ownedRuntime() {
	for _, d := range []string{"", "bin", "data", "data/credentials", "logs", "tmp", "launchd"} {
		if e := os.MkdirAll(filepath.Join(f.runtime(), d), 0700); e != nil {
			f.t.Fatal(e)
		}
	}
	f.write(filepath.Join(f.runtime(), ".owner"), fmt.Sprintf("tosub2-local-runtime-v1\nproject=%s\nnamespace=%s\nlabel=%s\n", f.root, f.namespace, f.label), 0600)
}
func TestLocalMacOSLifecycleAndExactCleanup(t *testing.T) {
	f := newLocalFixture(t)
	if s := f.ok("start"); !strings.Contains(s, "127.0.0.1:49191") {
		t.Fatal(s)
	}
	plist := filepath.Join(f.runtime(), "launchd", f.label+".plist")
	text := f.content(plist)
	for _, want := range []string{f.namespace, "<key>TOSUB2_BROWSER_HEADLESS</key><string>0</string>", "<key>TOSUB2_BROWSER_NO_SANDBOX</key><string>0</string>", "<key>Umask</key><integer>63</integer>", "home &amp; &apos;quoted&apos;", "/data/credentials</string>", "/tmp/</string>", "--host=127.0.0.1", "--browser=true", "<key>KeepAlive</key><false/>"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing plist %q", want)
		}
	}
	if strings.Contains(text, "node") || strings.Contains(text, "python") {
		t.Fatal("runtime configuration added non-Go runtime")
	}
	err := filepath.Walk(f.runtime(), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if info.IsDir() || p == filepath.Join(f.runtime(), "bin", "tosub2") {
			mode = 0700
		}
		if info.Mode().Perm() != mode {
			t.Errorf("permissions %s = %o, want %o", p, info.Mode().Perm(), mode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "TOSUB2_LOCAL_PORT=49192")
	if s := f.ok("start"); !strings.Contains(s, "49191") {
		t.Fatal("repeat start changed running port:", s)
	}
	if s := f.content(filepath.Join(f.state, "builds")); s != "build\n" {
		t.Fatal("repeat start rebuilt:", s)
	}
	if s := f.ok("status"); !strings.Contains(s, "Running:") {
		t.Fatal(s)
	}
	started := time.Now()
	f.ok("restart")
	if time.Since(started) < 500*time.Millisecond {
		t.Fatal("restart did not wait for asynchronous bootout")
	}
	if _, e := os.Stat(filepath.Join(f.state, "bootstrap-error")); !os.IsNotExist(e) {
		t.Fatal("bootstrap raced old registration")
	}
	if s := f.ok("status"); !strings.Contains(s, "49192") {
		t.Fatal(s)
	}
	f.ok("stop")
	if s := f.ok("status"); !strings.Contains(s, "Stopped.") {
		t.Fatal(s)
	}
	f.write(filepath.Join(f.state, "security-results"), "0\n0\n44\n", 0600)
	f.ok("clean", "--yes")
	if _, e := os.Stat(f.runtime()); !os.IsNotExist(e) {
		t.Fatal("runtime still exists:", e)
	}
	want := "delete-generic-password -s " + f.namespace + "\n"
	if got := f.content(filepath.Join(f.state, "security-calls")); got != strings.Repeat(want, 3) {
		t.Fatalf("wrong Keychain scope: %q", got)
	}
	if f.content(filepath.Join(f.root, "source-preserve.txt")) != "source remains intact" {
		t.Fatal("source changed")
	}
	if s := f.ok("clean", "--yes"); !strings.Contains(s, "nothing changed") {
		t.Fatal(s)
	}
}

func TestLocalMacOSSafetyGuards(t *testing.T) {
	cases := []string{"root-symlink", "marker-symlink", "marker-mismatch", "marker-missing", "marker-hardlink", "bin", "data", "data/credentials", "logs", "tmp", "launchd", "binary-file", "plist-file", "log-file", "pid-file", "port-file"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLocalFixture(t)
			f.ownedRuntime()
			outside := filepath.Join(filepath.Dir(f.root), "outside")
			if e := os.Mkdir(outside, 0700); e != nil {
				t.Fatal(e)
			}
			canary := filepath.Join(outside, "keep")
			f.write(canary, "external data stays", 0600)
			marker := filepath.Join(f.runtime(), ".owner")
			switch name {
			case "root-symlink":
				if e := os.RemoveAll(f.runtime()); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(outside, f.runtime()); e != nil {
					t.Fatal(e)
				}
			case "marker-symlink":
				_ = os.Remove(marker)
				if e := os.Symlink(canary, marker); e != nil {
					t.Fatal(e)
				}
			case "marker-mismatch":
				f.write(marker, "another project", 0600)
			case "marker-missing":
				_ = os.Remove(marker)
			case "marker-hardlink":
				if e := os.Link(marker, filepath.Join(outside, "hardlink")); e != nil {
					t.Fatal(e)
				}
			default:
				target := filepath.Join(f.runtime(), name)
				switch name {
				case "binary-file":
					target = filepath.Join(f.runtime(), "bin", "tosub2")
				case "plist-file":
					target = filepath.Join(f.runtime(), "launchd", f.label+".plist")
				case "log-file":
					target = filepath.Join(f.runtime(), "logs", "stdout.log")
				case "pid-file":
					target = filepath.Join(f.runtime(), "service.pid")
				case "port-file":
					target = filepath.Join(f.runtime(), "port")
				}
				if e := os.RemoveAll(target); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(outside, target); e != nil {
					t.Fatal(e)
				}
			}
			if out, e := f.run("clean", "--yes"); e == nil {
				t.Fatalf("unsafe clean accepted: %s", out)
			}
			if f.content(canary) != "external data stays" {
				t.Fatal("external data changed")
			}
			for _, file := range []string{"security-calls", "launchctl-calls"} {
				if _, e := os.Stat(filepath.Join(f.state, file)); !os.IsNotExist(e) {
					t.Fatalf("guard reached external control %s: %v", file, e)
				}
			}
		})
	}
}

func TestLocalMacOSCleanupRetainsDataOnKeychainFailure(t *testing.T) {
	f := newLocalFixture(t)
	f.ownedRuntime()
	data := filepath.Join(f.runtime(), "data", "job-meta.json")
	f.write(data, "private fixture", 0600)
	f.write(filepath.Join(f.state, "security-results"), "0\n36\n", 0600)
	out, err := f.run("clean", "--yes")
	if err == nil || !strings.Contains(out, "Keychain cleanup failed") || strings.Contains(out, "private-keychain-canary") {
		t.Fatalf("unexpected failure: %v %s", err, out)
	}
	if f.content(data) != "private fixture" {
		t.Fatal("data removed after failed Keychain cleanup")
	}
	if _, e := os.Stat(filepath.Join(f.runtime(), ".owner")); e != nil {
		t.Fatal("marker lost:", e)
	}
	f.write(filepath.Join(f.state, "security-results"), "44\n", 0600)
	f.ok("clean", "--yes")
}

func TestLocalMacOSExistingBinaryNeedsNoGo(t *testing.T) {
	f := newLocalFixture(t)
	f.ownedRuntime()
	f.write(filepath.Join(f.runtime(), "bin", "tosub2"), "#!/bin/sh\nexit 0\n", 0700)
	if e := os.Remove(filepath.Join(f.bin, "go")); e != nil {
		t.Fatal(e)
	}
	f.ok("start")
	f.env = append(f.env, "TOSUB2_LOCAL_REBUILD=1")
	f.ok("start")
	if _, e := os.Stat(filepath.Join(f.state, "builds")); !os.IsNotExist(e) {
		t.Fatal("existing binary unexpectedly built")
	}
	f.ok("stop")
	out, e := f.run("start")
	if e == nil || !strings.Contains(out, "Go is required") {
		t.Fatalf("forced rebuild without Go: %v %s", e, out)
	}
	f.env = append(f.env, "TOSUB2_LOCAL_REBUILD=0")
	f.ok("start")
	f.ok("clean", "--yes")
}

func TestLocalMacOSOwnedServiceAndListenerRequired(t *testing.T) {
	t.Run("occupied-port", func(t *testing.T) {
		f := newLocalFixture(t)
		f.write(filepath.Join(f.state, "occupied-port"), "", 0600)
		out, e := f.run("start")
		if e == nil || !strings.Contains(out, "already has a listener") {
			t.Fatalf("occupied: %v %s", e, out)
		}
		if _, e := os.Stat(filepath.Join(f.state, "builds")); !os.IsNotExist(e) {
			t.Fatal("built despite occupied port")
		}
	})
	t.Run("foreign-label", func(t *testing.T) {
		f := newLocalFixture(t)
		f.ownedRuntime()
		f.write(filepath.Join(f.state, "loaded"), "", 0600)
		f.write(filepath.Join(f.state, "pid"), strconv.Itoa(os.Getpid()), 0600)
		f.write(filepath.Join(f.state, "foreign-service"), "", 0600)
		out, e := f.run("clean", "--yes")
		if e == nil || !strings.Contains(out, "outside this owned runtime") {
			t.Fatalf("foreign label: %v %s", e, out)
		}
		calls := f.content(filepath.Join(f.state, "launchctl-calls"))
		if strings.Contains(calls, "bootout") {
			t.Fatal("unloaded foreign service")
		}
	})
	t.Run("healthy-other-listener", func(t *testing.T) {
		f := newLocalFixture(t)
		f.write(filepath.Join(f.state, "fast-sleep"), "", 0600)
		f.write(filepath.Join(f.state, "no-owned-listener"), "", 0600)
		out, e := f.run("start")
		if e == nil || !strings.Contains(out, "did not become healthy") {
			t.Fatalf("wrong listener: %v %s", e, out)
		}
		if _, e := os.Stat(filepath.Join(f.state, "health-calls")); !os.IsNotExist(e) {
			t.Fatal("sent readiness request without owned listener")
		}
		f.ok("clean", "--yes")
	})
}

func TestLocalMacOSStopBudgetPreservesRuntime(t *testing.T) {
	for _, mode := range []string{"hold-loaded", "block-bootout"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalFixture(t)
			f.ok("start")
			f.write(filepath.Join(f.state, mode), "", 0600)
			f.write(filepath.Join(f.state, "fast-clock"), "1000\n", 0600)
			out, e := f.run("clean", "--yes")
			if e == nil || !strings.Contains(out, "30 seconds") {
				t.Fatalf("stop budget: %v %s", e, out)
			}
			if _, e := os.Stat(filepath.Join(f.runtime(), ".owner")); e != nil {
				t.Fatal("timeout removed runtime:", e)
			}
			if _, e := os.Stat(filepath.Join(f.state, "security-calls")); !os.IsNotExist(e) {
				t.Fatal("timeout proceeded to Keychain cleanup")
			}
			_ = os.Remove(filepath.Join(f.state, mode))
			_ = os.Remove(filepath.Join(f.state, "fast-clock"))
			f.ok("clean", "--yes")
		})
	}
}

func TestLocalMacOSApplicationSupportBoundary(t *testing.T) {
	t.Run("dedicated-parent-symlink", func(t *testing.T) {
		f := newLocalFixture(t)
		outside := filepath.Join(filepath.Dir(f.root), "outside-parent")
		if e := os.Mkdir(outside, 0700); e != nil {
			t.Fatal(e)
		}
		keep := filepath.Join(outside, "keep")
		f.write(keep, "unrelated files", 0600)
		if e := os.Symlink(outside, filepath.Dir(f.runtime())); e != nil {
			t.Fatal(e)
		}
		for _, args := range [][]string{{"start"}, {"status"}, {"clean", "--yes"}} {
			out, e := f.run(args...)
			if e == nil || !strings.Contains(out, "symlinked runtime parent") {
				t.Fatalf("%v: %v %s", args, e, out)
			}
		}
		if f.content(keep) != "unrelated files" {
			t.Fatal("outside parent modified")
		}
		if _, e := os.Stat(filepath.Join(f.state, "launchctl-calls")); !os.IsNotExist(e) {
			t.Fatal("unsafe parent reached launchctl")
		}
	})
	t.Run("dedicated-parent-owner", func(t *testing.T) {
		f := newLocalFixture(t)
		f.ownedRuntime()
		f.tool("stat", "#!/bin/sh\nif [ \"$1\" = -f ] && [ \"$2\" = '%u' ] && [ \"$3\" = \"$(dirname \"$FAKE_RUNTIME\")\" ]; then printf '0\\n'; else exec /usr/bin/stat \"$@\"; fi\n")
		out, e := f.run("clean", "--yes")
		if e == nil || !strings.Contains(out, "belongs to another user") {
			t.Fatalf("foreign parent: %v %s", e, out)
		}
		if _, e := os.Stat(filepath.Join(f.runtime(), ".owner")); e != nil {
			t.Fatal("foreign parent was removed")
		}
	})
	t.Run("empty-parent-removal", func(t *testing.T) {
		f := newLocalFixture(t)
		f.ownedRuntime()
		support := filepath.Dir(filepath.Dir(f.runtime()))
		before, e := os.Stat(support)
		if e != nil {
			t.Fatal(e)
		}
		f.ok("clean", "--yes")
		if _, e := os.Stat(filepath.Dir(f.runtime())); !os.IsNotExist(e) {
			t.Fatal("empty dedicated parent retained:", e)
		}
		after, e := os.Stat(support)
		if e != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
			t.Fatal("Application Support was removed/replaced/repermissioned:", e)
		}
		if _, e := os.Stat(filepath.Join(f.home, "Library")); e != nil {
			t.Fatal("Library was removed:", e)
		}
	})
	t.Run("sibling-runtime-preserved", func(t *testing.T) {
		f := newLocalFixture(t)
		f.ownedRuntime()
		sibling := filepath.Join(filepath.Dir(f.runtime()), "another-project")
		if e := os.Mkdir(sibling, 0700); e != nil {
			t.Fatal(e)
		}
		keep := filepath.Join(sibling, "job-meta.json")
		f.write(keep, "sibling data", 0600)
		f.ok("clean", "--yes")
		if f.content(keep) != "sibling data" {
			t.Fatal("sibling data changed")
		}
		if _, e := os.Stat(f.runtime()); !os.IsNotExist(e) {
			t.Fatal("own runtime retained:", e)
		}
		if _, e := os.Stat(filepath.Dir(f.runtime())); e != nil {
			t.Fatal("nonempty parent removed:", e)
		}
	})
	t.Run("project-local-directory-untouched", func(t *testing.T) {
		f := newLocalFixture(t)
		f.ownedRuntime()
		old := filepath.Join(f.root, ".local-run")
		if e := os.Mkdir(old, 0700); e != nil {
			t.Fatal(e)
		}
		keep := filepath.Join(old, "migration-owned-by-root")
		f.write(keep, "do not migrate automatically", 0600)
		f.ok("clean", "--yes")
		if f.content(keep) != "do not migrate automatically" {
			t.Fatal("old project runtime changed")
		}
	})
}
