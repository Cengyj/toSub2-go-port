package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
)

// The child inherits TestMain's isolated cache namespace. It uses the same OS
// account lock as another CLI, without sharing a Manager or opening a network.
func TestAccountQueryProcessFixture(t *testing.T) {
	mode := os.Getenv("TOSUB2_QUERY_PROCESS_MODE")
	if mode == "" {
		return
	}
	if mode != "hold" && mode != "probe" {
		t.Fatal("invalid account-query process fixture mode")
	}
	release, err := accountlock.Acquire(os.Getenv("TOSUB2_QUERY_PROCESS_ORIGIN"), os.Getenv("TOSUB2_QUERY_PROCESS_EMAIL"))
	if mode == "probe" && errors.Is(err, accountlock.ErrBusy) {
		fmt.Fprintln(os.Stdout, "busy")
		return
	}
	if err != nil {
		t.Fatal("fixture account lock unavailable", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error("fixture account lock release failed", err)
		}
	}()
	if mode == "probe" {
		fmt.Fprintln(os.Stdout, "acquired")
		return
	}
	fmt.Fprintln(os.Stdout, "held")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal("fixture control pipe failed", err)
	}
}

func accountQueryProcessCommand(ctx context.Context, mode, origin, email string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAccountQueryProcessFixture$", "-test.count=1", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "TOSUB2_QUERY_PROCESS_MODE="+mode, "TOSUB2_QUERY_PROCESS_ORIGIN="+origin, "TOSUB2_QUERY_PROCESS_EMAIL="+email)
	return cmd
}

func holdAccountQueryProcessLock(t *testing.T, origin, email string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cmd := accountQueryProcessCommand(ctx, "hold", origin, email)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	var waitErr error
	finish := func() {
		t.Helper()
		once.Do(func() {
			_ = input.Close()
			waitErr = cmd.Wait()
			cancel()
		})
		if waitErr != nil {
			t.Errorf("account-lock fixture did not exit cleanly: %v %s", waitErr, stderr.String())
		}
	}
	t.Cleanup(finish)
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "held\n" {
		finish()
		t.Fatalf("account-lock fixture did not acquire ownership: %q %v", line, err)
	}
	return finish
}

func assertAccountQueryProcessLock(t *testing.T, origin, email, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := accountQueryProcessCommand(ctx, "probe", origin, email).CombinedOutput()
	if err != nil {
		t.Fatalf("account-lock probe failed: %v %s", err, output)
	}
	line, _, _ := strings.Cut(string(output), "\n")
	if line != want {
		t.Fatalf("other process observed account lock %q, want %q", line, want)
	}
}

func TestAccountQueryProcessPeerBlocksAdmissionWithoutLeakingReservation(t *testing.T) {
	const origin = "https://query-peer.example.test"
	m, _ := accountQueryManager(t)
	m.SetAccountOperationLease(func(email string) (func() error, error) { return accountlock.Acquire(origin, email) })
	job := accountQueryJob(t, m, "query-peer-held", "process-owner@example.test")
	other := accountQueryJob(t, m, "query-peer-other", "independent@example.test")
	stopPeer := holdAccountQueryProcessLock(t, "https://QUERY-PEER.example.test:443/other", " PROCESS-OWNER@EXAMPLE.TEST ")
	lease, err := m.ReserveAccountQuery(t.Context(), job.ID)
	if lease != nil {
		lease.Release()
	}
	if lease != nil || !errors.Is(err, ErrAccountQueryBusy) {
		t.Fatalf("query ignored another process's account ownership: %v", err)
	}
	if activity := m.ActivitySnapshot(); activity.Queries != 0 {
		t.Fatal("failed OS lock acquisition leaked an in-process reservation")
	}
	accountQueryLease(t, m, t.Context(), other.ID).Release()
	stopPeer()
	accountQueryLease(t, m, t.Context(), job.ID).Release()
}

func TestAccountQueryProcessCancellationRetainsOwnershipUntilRelease(t *testing.T) {
	for _, scope := range []string{"configured", "default"} {
		t.Run(scope, func(t *testing.T) {
			m, _ := accountQueryManager(t)
			origin, peerOrigin := "https://query-cancel.example.test", "https://QUERY-CANCEL.example.test:443/settings"
			if scope == "configured" {
				m.SetAccountOperationLease(func(email string) (func() error, error) { return accountlock.Acquire(origin, email) })
			} else {
				origin, peerOrigin = "https://chatgpt.com", "https://CHATGPT.COM:443/settings"
			}
			job := accountQueryJob(t, m, "query-cancel-process", "query-cancel@example.test")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lease := accountQueryLease(t, m, ctx, job.ID)
			assertAccountQueryProcessLock(t, peerOrigin, " QUERY-CANCEL@EXAMPLE.TEST ", "busy")
			cancel()
			if lease.Valid() || lease.Context().Err() == nil {
				t.Fatal("canceled query retained a valid grant")
			}
			assertAccountQueryProcessLock(t, peerOrigin, job.Email, "busy")
			lease.Release()
			lease.Release()
			assertAccountQueryProcessLock(t, peerOrigin, job.Email, "acquired")
			accountQueryLease(t, m, t.Context(), job.ID).Release()
		})
	}
}
