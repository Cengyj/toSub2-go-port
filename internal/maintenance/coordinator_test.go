package maintenance

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

func TestMaintenanceDrainsAcceptedRequestsAndLocalCLITasks(t *testing.T) {
	root := t.TempDir()
	c := New(root)
	defer c.Close()
	request, err := c.Enter()
	if err != nil {
		t.Fatal(err)
	}
	cli, err := AcquireTask(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Change(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enter(); !errors.Is(err, ErrPaused) {
		t.Fatal("new request admitted", err)
	}
	if _, err := AcquireTask(root); !errors.Is(err, ErrPaused) {
		t.Fatal("new CLI admitted", err)
	}
	if drained, err := c.HoldDrained(true); drained || err != nil {
		t.Fatal("active request ignored", drained, err)
	}
	request()
	if drained, err := c.HoldDrained(true); drained || err != nil {
		t.Fatal("active CLI ignored", drained, err)
	}
	input, err := c.Continue()
	if err != nil {
		t.Fatal("existing input blocked", err)
	}
	input()
	if err := cli(); err != nil {
		t.Fatal(err)
	}
	if drained, err := c.HoldDrained(false); drained || err != nil {
		t.Fatal("active local service ignored", drained, err)
	}
	if drained, err := c.HoldDrained(true); !drained || err != nil {
		t.Fatal("drain failed", drained, err)
	}
	if _, err := c.Continue(); !errors.Is(err, ErrPaused) {
		t.Fatal("late cancellation crossed confirmed drain", err)
	}
	if err := c.Change(t.Context(), false, 1); err != nil {
		t.Fatal(err)
	}
	cli, err = AcquireTask(root)
	if err != nil {
		t.Fatal(err)
	}
	cli()
	if New(root).Status().Enabled {
		t.Fatal("confirmed exit did not survive restart")
	}
}

func TestMaintenancePersistenceFailuresKeepAdmissionPaused(t *testing.T) {
	root := t.TempDir()
	c := New(root)
	defer c.Close()
	if err := c.Change(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	write := c.write
	c.write = func(context.Context, string, []byte) error { return errors.New("synthetic before replacement") }
	if err := c.Change(t.Context(), false, 1); !errors.Is(err, ErrUnavailable) || !c.Status().Enabled {
		t.Fatal("failed exit opened admission", err)
	}
	c.write = func(ctx context.Context, path string, data []byte) error {
		if err := write(ctx, path, data); err != nil {
			return err
		}
		return privatefile.ErrDurabilityUncertain
	}
	if err := c.Change(t.Context(), false, 1); !errors.Is(err, ErrUncertain) || !c.Status().Enabled || !c.Status().Uncertain {
		t.Fatal("unknown exit opened admission", err)
	}
	if drained, _ := c.HoldDrained(true); drained {
		t.Fatal("unknown persistence declared drained")
	}
	c.write = write
	if err := c.Change(t.Context(), false, 2); err != nil || c.Status().Enabled {
		t.Fatal("explicit local confirmation failed", err)
	}
}

func TestMaintenanceCorruptOrIncompleteModeFailsClosed(t *testing.T) {
	for _, raw := range []string{`{`, `{"schema":1,"revision":1}`, `{"schema":1,"revision":1,"enabled":null}`, `{"schema":1,"revision":1,"enabled":false,"enabled":true}`, `{"schema":2,"revision":1,"enabled":false}`} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "maintenance.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		c := New(root)
		if !c.Status().Enabled || c.Status().Error == "" {
			t.Fatal("corrupt mode enabled admission")
		}
		if _, err := AcquireTask(root); !errors.Is(err, ErrUnavailable) {
			t.Fatal("CLI ignored corrupt mode", err)
		}
		if err := c.Change(t.Context(), false, 0); !errors.Is(err, ErrUnavailable) {
			t.Fatal("corrupt evidence overwritten", err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "maintenance.json"))
		if string(got) != raw {
			t.Fatal("original mode not preserved")
		}
	}
}

func TestMaintenanceInstanceAndSharedLeaseIdentity(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireInstance(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireInstance(filepath.Join(root, ".")); err == nil {
		t.Fatal("duplicate writer admitted")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	one, err := AcquireTask(root)
	if err != nil {
		t.Fatal(err)
	}
	defer one()
	two, err := AcquireTask(root)
	if err != nil {
		t.Fatal("independent CLI tasks were globally serialized", err)
	}
	two()
}

func TestOfflineMaintenanceExcludesOnlineWritersAndRetainsPause(t *testing.T) {
	root := t.TempDir()
	c := New(root)
	defer c.Close()
	for _, acquire := range []func(string) (func() error, error){AcquireInstance, AcquireTask} {
		release, err := acquire(root)
		if err != nil {
			t.Fatal(err)
		}
		if offline, err := AcquireOffline(root); err == nil {
			offline()
			t.Fatal("offline mutation admitted alongside an online writer")
		}
		release()
	}
	if err := c.Change(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	if drained, err := c.HoldDrained(true); err != nil || !drained {
		t.Fatal("drain failed", err)
	}
	if offline, err := AcquireOffline(root); err == nil {
		offline()
		t.Fatal("offline mutation crossed an existing drain fence")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	offline, err := AcquireOffline(root)
	if err != nil {
		t.Fatal("explicit offline command required reopening online admission", err)
	}
	if instance, err := AcquireInstance(root); err == nil {
		instance()
		t.Fatal("console started during offline mutation")
	}
	if task, err := AcquireTask(root); err == nil {
		task()
		t.Fatal("CLI started during offline mutation")
	}
	if err := offline(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "maintenance.json"))
	if err != nil || string(before) != string(after) || !New(root).Status().Enabled {
		t.Fatal("offline admission changed durable maintenance policy", err)
	}
	instance, err := AcquireInstance(root)
	if err != nil {
		t.Fatal("failed offline admission retained an instance lease", err)
	}
	instance()
}

func TestMaintenanceMissingRootAliasRetainsLeaseIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; shared lease process fixture remains portable")
	}
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "new", "data")
	aliased := filepath.Join(alias, "new", "data")
	one, err := AcquireInstance(root)
	if err != nil {
		t.Fatal(err)
	}
	defer one()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if release, err := AcquireInstance(aliased); err == nil {
		release()
		t.Fatal("parent alias and newly created root escaped the original lease")
	}
}

func TestMaintenanceSharedLeaseProcessExit(t *testing.T) {
	if root := os.Getenv("TOSUB2_MAINTENANCE_CHILD_ROOT"); root != "" {
		if _, err := AcquireTask(root); err != nil {
			os.Exit(23)
		}
		_, _ = os.Stdout.WriteString("lease acquired\n")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		// Deliberately omit release: process loss must free the OS lease.
		os.Exit(0)
	}
	root := t.TempDir()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMaintenanceSharedLeaseProcessExit$", "-test.count=1")
	child.Env = append(os.Environ(), "TOSUB2_MAINTENANCE_CHILD_ROOT="+root)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); _ = child.Process.Kill() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "lease acquired\n" {
		t.Fatal("child did not acquire work lease", err)
	}
	if offline, err := AcquireOffline(root); err == nil {
		offline()
		t.Fatal("offline mutation ignored a CLI in another process")
	}
	other, err := AcquireTask(root)
	if err != nil {
		t.Fatal("shared lease globally serialized CLI tasks", err)
	}
	other()
	c := New(root)
	defer c.Close()
	if err := c.Change(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	if drained, err := c.HoldDrained(true); err != nil || drained {
		t.Fatal("another process was ignored", drained, err)
	}
	input.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if drained, err := c.HoldDrained(true); err != nil || !drained {
		t.Fatal("exited process retained work lease", drained, err)
	}
}
