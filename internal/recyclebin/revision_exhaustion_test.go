package recyclebin

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestRetentionRevisionExhaustionPreservesPolicyAndRestart(t *testing.T) {
	disk, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	initial := Settings{SchemaVersion: 1, Revision: math.MaxUint64 - 1, RetentionDays: 0}
	if err := disk.WriteJSON(t.Context(), "recycle-bin-settings.json", initial); err != nil {
		t.Fatal(err)
	}
	service, err := New(disk.Root, &fakeRepository{})
	if err != nil {
		t.Fatal(err)
	}
	last, err := service.Update(t.Context(), Settings{SchemaVersion: 1, Revision: initial.Revision, RetentionDays: 7})
	if err != nil || last.Revision != math.MaxUint64 || last.RetentionDays != 7 {
		t.Fatal("last representable policy update did not commit", err)
	}
	path := filepath.Join(disk.Root, "recycle-bin-settings.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := service.Update(t.Context(), last)
	if err != nil || unchanged != last {
		t.Fatal("unchanged policy unnecessarily consumed a revision", err)
	}
	rejected, err := service.Update(t.Context(), Settings{SchemaVersion: 1, Revision: last.Revision, RetentionDays: 0})
	if !errors.Is(err, ErrConflict) || rejected != last || service.Settings() != last {
		t.Fatalf("exhausted revision changed the active policy: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("exhausted revision replaced persistent policy", err)
	}
	reopened, err := New(disk.Root, &fakeRepository{})
	if err != nil || reopened.Settings() != last {
		t.Fatal("rejected update left a policy that cannot restart", err)
	}
}
