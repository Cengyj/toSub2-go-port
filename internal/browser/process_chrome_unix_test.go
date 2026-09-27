//go:build !windows

package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type browserProcessSnapshot struct {
	pid, parent, group int
	command            string
	state              string
}

func chromiumProcessSnapshot(t *testing.T) map[int]browserProcessSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,stat=,args=").Output()
	if err != nil {
		t.Fatal("cannot inspect Chromium process ownership:", err)
	}
	result := map[int]browserProcessSnapshot{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		group, e3 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		result[pid] = browserProcessSnapshot{pid: pid, parent: parent, group: group, state: fields[3], command: strings.Join(fields[4:], " ")}
	}
	return result
}

func TestChromiumOwnedDescendantsExitAfterCancellation(t *testing.T) {
	checkChromiumOwnedDescendantsExit(t, false)
}

func TestChromiumOwnedDescendantsExitAfterUnexpectedParentExit(t *testing.T) {
	checkChromiumOwnedDescendantsExit(t, true)
}

func checkChromiumOwnedDescendantsExit(t *testing.T, parentExit bool) {
	t.Helper()
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for actual Chrome process-tree validation")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chrome not installed; set CHROME_PATH")
	}
	before := chromiumProcessSnapshot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := NewManager(path, 1)
	defer m.Close()
	s, err := m.Start(ctx, "", "", "", "process-lifecycle-fixture")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<!doctype html><html><body>authorized local lifecycle fixture</body></html>")
	}))
	defer srv.Close()
	if err = s.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Evaluate(ctx, "document.body.textContent"); err != nil {
		t.Fatal(err)
	}
	snapshot := chromiumProcessSnapshot(t)
	owned := map[int]browserProcessSnapshot{s.cmd.Process.Pid: snapshot[s.cmd.Process.Pid]}
	for changed := true; changed; {
		changed = false
		for pid, process := range snapshot {
			_, parentOwned := owned[process.parent]
			_, found := owned[pid]
			if !found && (parentOwned || process.group == s.cmd.Process.Pid || strings.Contains(process.command, s.Dir)) {
				owned[pid] = process
				changed = true
			}
		}
	}
	if len(owned) < 3 {
		t.Fatalf("fixture did not observe real Chrome descendants: %d", len(owned)-1)
	}
	escaped := 0
	for _, process := range owned {
		if process.group != s.cmd.Process.Pid {
			escaped++
		}
	}
	// A newly detached crash handler without a profile marker cannot safely be
	// owned merely because it is named Chrome. Do not terminate it by name.
	// Other test packages or the user may have started it concurrently. Observe
	// it separately for diagnostics, without claiming ownership or asserting
	// its lifetime is controlled by this Session.
	detachedCandidates := map[int]browserProcessSnapshot{}
	for pid, process := range snapshot {
		_, existed := before[pid]
		_, known := owned[pid]
		if !existed && !known && strings.Contains(process.command, "chrome_crashpad_handler") {
			detachedCandidates[pid] = process
		}
	}
	t.Logf("observed %d attributable Chrome descendants; %d outside dedicated PGID; %d new detached crash-handler candidates", len(owned)-1, escaped, len(detachedCandidates))
	if parentExit {
		// Kill only the exact root process created by this Session. Its
		// descendants must be cleaned by the production Wait/Close path.
		if err = s.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.processDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Chrome parent did not exit")
		}
	} else {
		cancel()
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after := chromiumProcessSnapshot(t)
		remaining := 0
		resourceFreeZombies := 0
		for pid, original := range owned {
			if process, exists := after[pid]; exists && (process.group == original.group || process.command == original.command) {
				stopped, err := linuxZombieReleasedResources(process)
				if err != nil {
					t.Fatalf("cannot prove descendant released its resources: %v", err)
				}
				if stopped {
					resourceFreeZombies++
				} else {
					remaining++
				}
			}
		}
		if remaining == 0 {
			if resourceFreeZombies > 0 {
				t.Logf("%d owned descendants are verified Linux zombies with no file descriptors, mappings, or command line; stopped but awaiting parent/init reaping", resourceFreeZombies)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d attributable Chrome descendants survived close; no unrelated process was killed", remaining)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if len(detachedCandidates) > 0 {
		observationDeadline := time.Now().Add(200 * time.Millisecond)
		for {
			after := chromiumProcessSnapshot(t)
			visible := 0
			for pid, original := range detachedCandidates {
				if process, exists := after[pid]; exists && process.command == original.command {
					visible++
				}
			}
			if visible == 0 || time.Now().After(observationDeadline) {
				t.Logf("informational only: %d of %d unattributed detached crash-handler candidates still visible; Session ownership and cleanup of these candidates are not verified", visible, len(detachedCandidates))
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("browser profile survived close")
	}
}

// Docker without --init can leave already-dead orphaned children in /proc. A
// ps state alone is insufficient: inspect the actual kernel state and prove
// that the process retains no descriptors/address space before classifying it
// as stopped. Any live process or unreadable evidence continues to fail.
func linuxZombieReleasedResources(process browserProcessSnapshot) (bool, error) {
	if runtime.GOOS != "linux" || !strings.HasPrefix(process.state, "Z") {
		return false, nil
	}
	base := filepath.Join("/proc", strconv.Itoa(process.pid))
	readZombie := func() (bool, error) {
		status, err := os.ReadFile(filepath.Join(base, "status"))
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "State:") {
				fields := strings.Fields(line)
				return len(fields) > 1 && fields[1] == "Z", nil
			}
		}
		return false, errors.New("Linux process state unavailable")
	}
	zombie, err := readZombie()
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil || !zombie {
		return false, err
	}
	files, err := os.ReadDir(filepath.Join(base, "fd"))
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(files) != 0 {
		return false, nil
	}
	for _, name := range []string{"maps", "cmdline"} {
		data, err := os.ReadFile(filepath.Join(base, name))
		if os.IsNotExist(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if len(data) != 0 {
			return false, nil
		}
	}
	// Recheck after resource inspection so PID reuse or a stale ps record can
	// never turn a currently running process into a successful cleanup.
	zombie, err = readZombie()
	if os.IsNotExist(err) {
		return true, nil
	}
	return zombie, err
}

func TestLinuxProcessSnapshotRequiresActualResourceFreeZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc for actual zombie resource verification")
	}
	if stopped, err := linuxZombieReleasedResources(browserProcessSnapshot{pid: os.Getpid(), state: "Z"}); err != nil || stopped {
		t.Fatalf("stale ps Z marker incorrectly accepted for live process: stopped=%t err=%v", stopped, err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(time.Second)
	for {
		stopped, err := linuxZombieReleasedResources(browserProcessSnapshot{pid: cmd.Process.Pid, state: "Z"})
		if err != nil {
			t.Fatal(err)
		}
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not become a verifiable resource-free zombie")
		}
		time.Sleep(time.Millisecond)
	}
}
