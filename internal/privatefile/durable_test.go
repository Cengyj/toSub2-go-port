package privatefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDurableWriteFailureBeforeAndAfterReplacement(t *testing.T) {
	for _, stage := range []string{"replace", "directory"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("injected storage failure")
			ops := platformDurableOps()
			if stage == "replace" {
				ops.replace = func(string, string) error { return cause }
			} else {
				ops.syncDir = func(path string) error {
					if path == dir {
						return cause
					}
					return nil
				}
			}
			err := writeDurable(context.Background(), path, []byte("new"), ops)
			if !errors.Is(err, cause) || errors.Is(err, ErrDurabilityUncertain) != (stage == "directory") {
				t.Fatalf("wrong failure classification: %v", err)
			}
			got, _ := os.ReadFile(path)
			want := "old"
			if stage == "directory" {
				want = "new"
			}
			if string(got) != want {
				t.Fatalf("file is %q, want %q", got, want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("temporary private file was left behind")
			}
		})
	}
}

func TestDurableWriteCancellationAndPermissions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var flushed bool
	ops := platformDurableOps()
	ops.replace = func(from, to string) error {
		cancel()
		return replaceDurable(from, to)
	}
	ops.syncDir = func(string) error {
		if ctx.Err() != nil {
			flushed = true
		}
		return nil
	}
	if err := writeDurable(ctx, path, []byte("new"), ops); err != nil || !flushed {
		t.Fatalf("post-replacement cancellation skipped durability: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Verify(file); err != nil {
		t.Fatal(err)
	}
	if err := writeDurable(ctx, path, []byte("canceled"), ops); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatal("canceled write replaced the file")
	}
}

func TestDurableWriteFlushesNewParentEntries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "one", "two")
	var synced []string
	ops := platformDurableOps()
	ops.syncDir = func(path string) error { synced = append(synced, path); return nil }
	if err := writeDurable(context.Background(), filepath.Join(dir, "state"), []byte("new"), ops); err != nil {
		t.Fatal(err)
	}
	if len(synced) < 3 || !reflect.DeepEqual(synced[len(synced)-3:], []string{root, filepath.Join(root, "one"), dir}) {
		t.Fatalf("new directory entries not flushed in order: %v", synced)
	}
}

func TestDurableWriteRetriesUncertainParentCreation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "one", "two", "state")
	cause := errors.New("injected parent flush failure")
	ops := platformDurableOps()
	ops.syncDir = func(path string) error {
		if path == root {
			return cause
		}
		return nil
	}
	if err := writeDurable(context.Background(), path, []byte("secret"), ops); !errors.Is(err, cause) || errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("failure before file replacement misclassified: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state was published below an uncertain parent")
	}
	var confirmed bool
	ops.syncDir = func(path string) error {
		if path == root {
			confirmed = true
		}
		return nil
	}
	if err := writeDurable(context.Background(), path, []byte("secret"), ops); err != nil || !confirmed {
		t.Fatalf("retry did not confirm previous mkdir: %v", err)
	}
}

func TestDurableRemoveRetryConfirmsAnUncertainAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected remove directory flush failure")
	ops := platformDurableOps()
	ops.syncDir = func(string) error { return cause }
	if err := removeDurable(context.Background(), path, ops); !errors.Is(err, ErrDurabilityUncertain) || !errors.Is(err, cause) {
		t.Fatalf("wrong failure classification: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed directory flush should still expose deletion")
	}
	var called bool
	ops.syncDir = func(string) error { called = true; return nil }
	if err := removeDurable(context.Background(), path, ops); err != nil || !called {
		t.Fatalf("retry did not confirm absence durably: %v", err)
	}
}
