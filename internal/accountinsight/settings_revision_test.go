package accountinsight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestInsightRestoredSettingsRevisionCannotWrap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	disk, err := store.New(filepath.Join(root, "account-insights"))
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Revision = ^uint64(0) - 1
	if err := disk.WriteJSON(ctx, "settings.json", settings); err != nil {
		t.Fatal(err)
	}
	f := &fixture{}
	s, err := New(root, f.provider())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	next := s.Settings()
	next.IntervalMinutes = 30
	got, err := s.Update(ctx, next)
	if err != nil || got.Revision != ^uint64(0) || got.IntervalMinutes != 30 {
		t.Fatalf("last representable revision did not retain compatibility: revision=%d err=%v", got.Revision, err)
	}
	before, err := os.ReadFile(disk.Path("settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	next = got
	next.IntervalMinutes = 60
	got, err = s.Update(ctx, next)
	if !errors.Is(err, ErrConflict) || got.Revision != ^uint64(0) || got.IntervalMinutes != 30 {
		t.Fatalf("exhausted revision changed committed configuration: revision=%d interval=%d err=%v", got.Revision, got.IntervalMinutes, err)
	}
	after, err := os.ReadFile(disk.Path("settings.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected update changed durable settings", err)
	}
	next.Revision = 0
	if _, err := s.Update(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatal("an old zero revision became eligible after exhaustion", err)
	}
	restored, err := New(root, f.provider())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(ctx) })
	if restored.Settings().Revision != ^uint64(0) || restored.Settings().IntervalMinutes != 30 {
		t.Fatal("restart lost the last committed configuration")
	}
}
