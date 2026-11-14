package accountlock

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"tosub2-go-port/internal/testsupport"
)

// Exercise Acquire itself: isolation must not bypass the production OS lock,
// and a fixture subprocess must still conflict with its owning test process.
func TestDefaultAccountLockNamespacesPreserveChildContention(t *testing.T) {
	const origin, email = "https://chatgpt.com", "test-namespace@example.test"
	const childEnv = "TOSUB2_DEFAULT_ACCOUNT_LOCK_CHILD"
	if mode := os.Getenv(childEnv); mode != "" {
		release, err := Acquire(origin, email)
		if release != nil {
			defer release()
		}
		if mode == "busy" && !errors.Is(err, ErrBusy) || mode == "independent" && err != nil {
			t.Fatalf("test-process namespace lost its contention boundary: mode=%s err=%v", mode, err)
		}
		return
	}
	release, err := Acquire(origin, email)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, mode := range []string{"busy", "independent"} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDefaultAccountLockNamespacesPreserveChildContention$", "-test.count=1")
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if mode != "independent" || !strings.EqualFold(key, testsupport.CacheNamespaceEnv) {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, childEnv+"="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("namespace child %s failed: %v\n%s", mode, err, output)
		}
	}
}
