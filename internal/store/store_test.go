package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestAtomicPermissionsAndPathBoundary(t *testing.T) {
	s := newTestStore(t)
	if err := os.WriteFile(s.Path("job-meta.json"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteJSON(context.Background(), "../job-meta.json", map[string]any{"version": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := s.ReadJSON(context.Background(), "job-meta.json", &got); err != nil || got["version"] != 1 {
		t.Fatalf("read %v: %v", got, err)
	}
	if runtime.GOOS != "windows" {
		file, _ := os.Stat(s.Path("job-meta.json"))
		dir, _ := os.Stat(s.Root)
		if file.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
			t.Fatalf("insecure file/directory permissions: %o/%o", file.Mode().Perm(), dir.Mode().Perm())
		}
	}
	private, err := os.Open(s.Path("job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if err = privatefile.Verify(private); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.Root), "job-meta.json")); !os.IsNotExist(err) {
		t.Fatal("write escaped store root")
	}
	assertNoTemporaryFiles(t, s)
}
func TestInvalidAndTrailingJSON(t *testing.T) {
	s := newTestStore(t)
	for _, raw := range []string{"", "{", "{\"ok\":true}broken", "{\"ok\":true} {}", "{\"ok\":true} null"} {
		if err := os.WriteFile(s.Path("bad.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var data any
		if err := s.ReadJSON(context.Background(), "bad.json", &data); err == nil {
			t.Fatalf("accepted malformed JSON %q", raw)
		}
	}
	if err := os.WriteFile(s.Path("good.json"), []byte("{\"ok\":true}\n \t"), 0600); err != nil {
		t.Fatal(err)
	}
	var data any
	if err := s.ReadJSON(context.Background(), "good.json", &data); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadJSON(context.Background(), "missing.json", &data); !IsNotFound(err) {
		t.Fatalf("missing file: %v", err)
	}
	f, err := os.Create(s.Path("oversized.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate((32 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := s.ReadJSON(context.Background(), "oversized.json", &data); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}

type cancelBeforeCommit struct {
	context.Context
	calls int
}

func (c *cancelBeforeCommit) Err() error {
	c.calls++
	if c.calls > 1 {
		return context.Canceled
	}
	return nil
}
func TestCanceledWritesAndMarshalErrorsPreserveTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.WriteBytes(context.Background(), "state.json", []byte(`{"old":true}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, attempt := range []func() error{
		func() error { return s.WriteJSON(ctx, "state.json", map[string]int{"new": 1}) },
		func() error {
			return s.WriteBytes(&cancelBeforeCommit{Context: context.Background()}, "state.json", []byte(`{"new":true}`))
		},
	} {
		if err := attempt(); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation: %v", err)
		}
	}
	if err := s.WriteJSON(context.Background(), "state.json", make(chan int)); err == nil {
		t.Fatal("unsupported JSON value accepted")
	}
	b, err := os.ReadFile(s.Path("state.json"))
	if err != nil || string(b) != `{"old":true}` {
		t.Fatalf("target replaced after failure: %s %v", b, err)
	}
	var data any
	if err = s.ReadJSON(ctx, "state.json", &data); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	assertNoTemporaryFiles(t, s)
}
func TestFailedRenameCleansTemporaryFile(t *testing.T) {
	s := newTestStore(t)
	if err := os.Mkdir(s.Path("directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteJSON(context.Background(), "directory.json", map[string]bool{"ok": true}); err == nil {
		t.Fatal("replaced a directory")
	}
	assertNoTemporaryFiles(t, s)
}
func TestConcurrentReadersSeeCompleteJSON(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	type record struct {
		Version int    `json:"version"`
		Payload string `json:"payload"`
	}
	payload := strings.Repeat("private-fixture", 1024)
	if err := s.WriteJSON(ctx, "checkpoint.json", record{0, payload}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 80; i++ {
				var got record
				if err := s.ReadJSON(ctx, "checkpoint.json", &got); err != nil {
					failures <- err
					return
				}
				if got.Payload != payload {
					failures <- fmt.Errorf("partial file")
					return
				}
			}
		}()
	}
	for i := 1; i <= 40; i++ {
		if err := s.WriteJSON(ctx, "checkpoint.json", record{i, payload}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	assertNoTemporaryFiles(t, s)
}
func TestCloneJSONDoesNotShareDataAndPreservesNumbers(t *testing.T) {
	original := map[string]any{"nested": map[string]any{"n": json.Number("9007199254740993")}}
	value, err := CloneJSON(original)
	if err != nil {
		t.Fatal(err)
	}
	clone := value.(map[string]any)["nested"].(map[string]any)
	if clone["n"].(json.Number).String() != "9007199254740993" {
		t.Fatal("integer precision lost")
	}
	clone["n"] = 1
	if original["nested"].(map[string]any)["n"].(json.Number).String() != "9007199254740993" {
		t.Fatal("clone aliases original")
	}
}
func assertNoTemporaryFiles(t *testing.T, s *Store) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(s.Root, ".*.tmp"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary files remain: %v %v", paths, err)
	}
}
