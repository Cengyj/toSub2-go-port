package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This only bounds pipe draining after exit/cancellation. Each browser's
// existing context deadline and virtual-time budget remain unchanged.
const frontendBrowserDrainDelay = time.Second

var errFrontendBrowserCleanup = errors.New("browser fixture cleanup failed")

func frontendBrowserProcessResult(commandErr, cleanupErr error) error {
	if cleanupErr != nil {
		return errors.Join(commandErr, errFrontendBrowserCleanup, cleanupErr)
	}
	return commandErr
}

func runFrontendBrowserCommand(cmd *exec.Cmd) error {
	tree, err := prepareFrontendBrowserProcessTree(cmd)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return frontendBrowserProcessResult(err, tree.terminate(nil))
	}
	if err := tree.attach(cmd.Process); err != nil {
		_ = tree.terminate(cmd.Process)
		return frontendBrowserProcessResult(errors.Join(err, cmd.Wait()), tree.finalError(cmd.Process))
	}
	waitErr := cmd.Wait()
	_ = tree.terminate(cmd.Process)
	return frontendBrowserProcessResult(waitErr, tree.finalError(cmd.Process))
}

// Every descendant acknowledges a private local connection and inherits the
// parent's stdout/stderr. TCP probes establish that the actual process stopped,
// without interpreting a PID that the OS may have reused as a live fixture.
func TestFrontendBrowserProcessFixture(t *testing.T) {
	address := os.Getenv("TOSUB2_BROWSER_TREE_ADDRESS")
	if address == "" {
		return
	}
	role := os.Getenv("TOSUB2_BROWSER_TREE_ROLE")
	childRole := map[string]string{"parent": "child", "child": "grandchild"}[role]
	if childRole != "" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(executable, "-test.run=^TestFrontendBrowserProcessFixture$")
		child.Env = frontendBrowserFixtureEnv(address, childRole)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
	}
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		os.Exit(4)
	}
	defer connection.Close()
	if json.NewEncoder(connection).Encode(struct {
		Role string
		PID  int
	}{role, os.Getpid()}) != nil {
		os.Exit(5)
	}
	message := make([]byte, 1)
	for {
		if _, err = io.ReadFull(connection, message); err != nil || message[0] == 'X' {
			os.Exit(0)
		}
		if _, err = connection.Write(message); err != nil {
			os.Exit(0)
		}
	}
}

func frontendBrowserFixtureEnv(address, role string) []string {
	var environment []string
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "TOSUB2_BROWSER_TREE_ADDRESS=") && !strings.HasPrefix(item, "TOSUB2_BROWSER_TREE_ROLE=") {
			environment = append(environment, item)
		}
	}
	return append(environment, "TOSUB2_BROWSER_TREE_ADDRESS="+address, "TOSUB2_BROWSER_TREE_ROLE="+role)
}

func TestFrontendBrowserProcessTreeCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "parent-exit"} {
		t.Run(mode, func(t *testing.T) { verifyFrontendBrowserProcessCleanup(t, mode) })
	}
}

func verifyFrontendBrowserProcessCleanup(t *testing.T, mode string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if mode == "deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFrontendBrowserProcessFixture$")
	cmd.Env = frontendBrowserFixtureEnv(listener.Addr().String(), "parent")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	done := make(chan error, 1)
	go func() { done <- runFrontendBrowserCommand(cmd) }()
	other := exec.Command(executable, "-test.run=^TestFrontendBrowserProcessFixture$")
	other.Env = frontendBrowserFixtureEnv(listener.Addr().String(), "unrelated")
	if err := other.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	connections := map[string]net.Conn{}
	var waitResult error
	var waited bool
	t.Cleanup(func() {
		cancel()
		for _, connection := range connections {
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			_, _ = connection.Write([]byte{'X'})
			_ = connection.Close()
		}
		_ = other.Process.Kill()
		_ = other.Wait()
		if !waited {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("fixture command did not finish cleanup")
			}
		}
	})
	for range 4 {
		_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
		connection, err := listener.Accept()
		if err != nil {
			t.Fatal("Go process fixture did not initialize:", err)
		}
		var hello struct {
			Role string
			PID  int
		}
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		if err := json.NewDecoder(connection).Decode(&hello); err != nil || hello.PID <= 1 || connections[hello.Role] != nil {
			_ = connection.Close()
			t.Fatal("invalid fixture process identity")
		}
		connections[hello.Role] = connection
	}
	for _, role := range []string{"parent", "child", "grandchild", "unrelated"} {
		if !frontendBrowserFixtureResponds(connections[role]) {
			t.Fatalf("fixture %s was not alive before cleanup", role)
		}
	}
	started := time.Now()
	switch mode {
	case "cancel":
		cancel()
	case "deadline":
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("deadline fixture was cancelled without reaching its deadline")
		}
		started = time.Now()
	case "parent-exit":
		_, _ = connections["parent"].Write([]byte{'X'})
	}
	select {
	case waitResult = <-done:
		waited = true
	case <-time.After(5 * time.Second):
		t.Fatal("owned tree or inherited output pipes exceeded cleanup budget")
	}
	if cmd.WaitDelay != frontendBrowserDrainDelay {
		t.Error("browser output drain does not have the shared finite deadline")
	}
	if waitResult == nil {
		t.Error("cancelled or unclean parent exit was reported as success")
	}
	if errors.Is(waitResult, errFrontendBrowserCleanup) {
		t.Fatalf("owned process cleanup was not confirmed: %v", waitResult)
	}
	if mode == "parent-exit" && !errors.Is(waitResult, exec.ErrWaitDelay) {
		t.Errorf("inherited output pipe did not report its drain deadline: %v", waitResult)
	}
	for _, role := range []string{"parent", "child", "grandchild"} {
		if frontendBrowserFixtureResponds(connections[role]) {
			t.Errorf("owned %s survived %s", role, mode)
		}
	}
	if !frontendBrowserFixtureResponds(connections["unrelated"]) {
		t.Error("cleanup stopped an unrelated fixture process")
	}
	t.Logf("owned parent/child/grandchild stopped; unrelated process alive; cleanup_ms=%d", time.Since(started).Milliseconds())
}

func frontendBrowserFixtureResponds(connection net.Conn) bool {
	if connection == nil {
		return false
	}
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write([]byte{'P'}); err != nil {
		return false
	}
	var value [1]byte
	_, err := io.ReadFull(connection, value[:])
	return err == nil && value[0] == 'P'
}

func TestFrontendBrowserProcessStartFailureAndEarlyCancellation(t *testing.T) {
	for _, mode := range []string{"missing-executable", "already-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			executable := filepath.Join(t.TempDir(), "missing-browser-executable")
			if mode == "already-cancelled" {
				var err error
				executable, err = os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			cmd := exec.CommandContext(ctx, executable)
			if err := runFrontendBrowserCommand(cmd); err == nil || cmd.Process != nil {
				t.Fatal("unstarted fixture reported success or created a process")
			}
		})
	}
}

func TestFrontendBrowserProcessCleanExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFrontendBrowserProcessFixture$")
	if err := runFrontendBrowserCommand(cmd); err != nil {
		t.Fatalf("clean browser fixture exit failed: %v", err)
	}
}

func TestFrontendBrowserProcessCancellationRacesStart(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		ctx, cancel := context.WithCancel(t.Context())
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFrontendBrowserProcessFixture$")
		var ready sync.WaitGroup
		ready.Add(1)
		done := make(chan error, 1)
		go func() { ready.Done(); done <- runFrontendBrowserCommand(cmd) }()
		ready.Wait()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("early cancellation did not release process ownership")
		}
	}
}
