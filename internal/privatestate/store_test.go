package privatestate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
)

func testRegistry(t *testing.T) (*Registry, string, *credentials.MemoryStateProtector) {
	t.Helper()
	r, root := NewRegistry(), t.TempDir()
	p, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(root, p); err != nil {
		t.Fatal(err)
	}
	return r, root, p
}

func TestSealedStateRoundTripAndBinding(t *testing.T) {
	r, root, _ := testRegistry(t)
	ctx, path := context.Background(), filepath.Join(root, "account", "operation.json")
	value := map[string]any{"email": "fixture@example.test", "cookie": "COOKIE-CANARY", "seed": "SEED-CANARY", "operation_id": "op-1"}
	if err := r.WriteJSON(ctx, path, "totp-recovery", " Fixture@Example.test ", "op-1", value); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	for _, secret := range []string{"COOKIE-CANARY", "SEED-CANARY", "fixture@example.test"} {
		if bytes.Contains(onDisk, []byte(secret)) {
			t.Fatal("recovery state leaked plaintext")
		}
	}
	plain, binding, err := r.ReadJSONWithBinding(ctx, path, "totp-recovery", 4096)
	if err != nil || binding.Schema != Schema || binding.Identity != Identity("fixture@example.test") || binding.Operation != "op-1" {
		t.Fatalf("state binding did not roundtrip: %+v %v", binding, err)
	}
	var got map[string]any
	if json.Unmarshal(plain, &got) != nil || got["cookie"] != value["cookie"] || got["seed"] != value["seed"] {
		t.Fatal("private state did not roundtrip")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := privatefile.Verify(file); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(ctx, path, "totp-recovery", "fixture@example.test", "op-2", value); err != nil {
		t.Fatalf("next operation could not replace account checkpoint: %v", err)
	}
	if err := r.WriteJSON(ctx, path, "totp-recovery", "other@example.test", "op-3", value); err == nil {
		t.Fatal("existing state was reassigned to a different account")
	}
}

func TestStateRejectsTamperingWrongPurposeAndPathSwap(t *testing.T) {
	r, root, _ := testRegistry(t)
	ctx, path := context.Background(), filepath.Join(root, "journal.json")
	if err := r.WriteJSON(ctx, path, "totp", "fixture", "op-1", map[string]string{"secret": "canary"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadJSON(ctx, path, "session", 4096); err == nil {
		t.Fatal("state accepted a different purpose")
	}
	original, _ := os.ReadFile(path)
	other := filepath.Join(root, "other.json")
	if err := privatefile.Write(ctx, other, original); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadJSON(ctx, other, "totp", 4096); err == nil {
		t.Fatal("state accepted a different relative path")
	}
	for _, field := range []string{"schema", "purpose", "identity", "operation", "ciphertext", "private_state"} {
		t.Run(field, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(original, &fields); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "schema":
				fields[field] = float64(2)
			case "identity":
				fields[field] = Identity("other")
			case "ciphertext":
				fields[field] = "AAAA"
			case "private_state":
				delete(fields, field)
			default:
				fields[field] = "changed"
			}
			damaged, _ := json.Marshal(fields)
			if err := privatefile.Write(ctx, path, damaged); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReadJSON(ctx, path, "totp", 4096); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corruption accepted or treated as absent: %v", err)
			}
			if err := r.WriteJSON(ctx, path, "totp", "fixture", "op-1", map[string]string{"secret": "replacement"}); err == nil {
				t.Fatal("unreadable existing evidence was overwritten")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, damaged) {
				t.Fatal("failed write modified evidence")
			}
		})
	}
}

func TestRequiredProtectionAndLegacyMigration(t *testing.T) {
	r, root := NewRegistry(), t.TempDir()
	ctx, path := context.Background(), filepath.Join(root, "state.json")
	legacy := map[string]string{"cookie": "legacy-canary"}
	if err := r.WriteJSON(ctx, path, "purpose", "fixture", "op", legacy); err != nil {
		t.Fatal(err)
	}
	if err := r.Require(root); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(ctx, path, "purpose", "fixture", "op", legacy); !errors.Is(err, ErrProtectionRequired) {
		t.Fatalf("required root wrote plaintext: %v", err)
	}
	plain, binding, err := r.ReadJSONWithBinding(ctx, path, "purpose", 4096)
	if err != nil || binding.Schema != 0 || !bytes.Contains(plain, []byte("legacy-canary")) {
		t.Fatalf("legacy state no longer readable: %v", err)
	}
	p, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(root, p); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(ctx, path, "purpose", "fixture", "op", legacy); err != nil {
		t.Fatal(err)
	}
	disk, _ := os.ReadFile(path)
	if bytes.Contains(disk, []byte("legacy-canary")) {
		t.Fatal("normal write did not migrate legacy JSON to sealed state")
	}
	unregistered := NewRegistry()
	if _, err := unregistered.ReadJSON(ctx, path, "purpose", 4096); !errors.Is(err, ErrProtectionRequired) {
		t.Fatalf("unregistered reader downgraded sealed state: %v", err)
	}
	if _, err := r.ReadJSON(ctx, filepath.Join(root, "absent.json"), "purpose", 4096); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real missing file not identified: %v", err)
	}
}

func TestMalformedOversizedAndNonPrivateStateRemainErrors(t *testing.T) {
	r, root, _ := testRegistry(t)
	ctx, path := context.Background(), filepath.Join(root, "state.json")
	for _, data := range [][]byte{[]byte(`{"broken":`), []byte(`null`), []byte(`{"cookie":"too-large"}`)} {
		if err := privatefile.Write(ctx, path, data); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ReadJSON(ctx, path, "purpose", 8); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid state treated as absent or valid: %v", err)
		}
	}
}

func TestRegistryRejectsSymlinkEscapesAndSeparatesRoots(t *testing.T) {
	r, root, _ := testRegistry(t)
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	if err := r.WriteJSON(ctx, filepath.Join(root, "linked", "state.json"), "purpose", "fixture", "op", map[string]string{"secret": "canary"}); err == nil {
		t.Fatal("state escaped through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink destination was written")
	}
	other, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(outside, other); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state.json")
	if err := r.WriteJSON(ctx, path, "purpose", "fixture", "op", map[string]string{"secret": "canary"}); err != nil {
		t.Fatal(err)
	}
	sealed, _ := os.ReadFile(path)
	if err := privatefile.Write(ctx, filepath.Join(outside, "state.json"), sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadJSON(ctx, filepath.Join(outside, "state.json"), "purpose", 4096); err == nil {
		t.Fatal("one registered root used another root's key")
	}
}

type absentKeyProtector struct{ credentials.StateProtector }

func (p absentKeyProtector) OpenContext(context.Context, string, []byte, []byte) ([]byte, error) {
	return nil, os.ErrNotExist
}

func TestMissingKeyIsNotMissingState(t *testing.T) {
	r, root, p := testRegistry(t)
	path, ctx := filepath.Join(root, "state.json"), context.Background()
	if err := r.WriteJSON(ctx, path, "purpose", "fixture", "op", map[string]string{"secret": "canary"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(root, absentKeyProtector{p}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadJSON(ctx, path, "purpose", 4096); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key absence hid an existing recovery record: %v", err)
	}
}

func TestRegistryRepeatedConcurrentRegistration(t *testing.T) {
	r, root, p := testRegistry(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Register(root, p); err != nil {
				t.Error(err)
			}
			if err := r.Require(root); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var nilProtector *credentials.MemoryStateProtector
	if err := r.Register(root, nilProtector); !errors.Is(err, ErrProtectionRequired) {
		t.Fatal("typed nil accepted")
	}
	if err := r.WriteJSON(context.Background(), filepath.Join(root, "state.json"), "purpose", "fixture", "op", map[string]string{"ok": "yes"}); err != nil {
		t.Fatal(err)
	}
}
