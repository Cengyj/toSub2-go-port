package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeBuildFixture(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}

func buildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeBuildFixture(t, root, "web/src/main.jsx", "import './style.css'; console.log('synthetic build');")
	writeBuildFixture(t, root, "web/src/style.css", "body { color: green; }")
	writeBuildFixture(t, root, "web/index.html", `<html><head></head><body><script src="/src/main.jsx"></script></body></html>`)
	for _, vendor := range []string{"react", "react-dom", "scheduler", "lucide-react"} {
		writeBuildFixture(t, root, "web/vendor/"+vendor+"/LICENSE", "synthetic license: "+vendor)
	}
	return root
}

func TestBuildWebPreparationFailurePreservesPreviousOutput(t *testing.T) {
	root := buildFixture(t)
	writeBuildFixture(t, root, "web/dist/index.html", "previous working output")
	writeBuildFixture(t, root, "cmd/tosub2/web", "blocked destination parent")
	if err := buildWeb(root); err == nil {
		t.Fatal("build accepted an inaccessible embedded destination")
	}
	got, err := os.ReadFile(filepath.Join(root, "web/dist/index.html"))
	if err != nil || string(got) != "previous working output" {
		t.Fatalf("failed build replaced previous output: %q, %v", got, err)
	}
}

func TestInstallOutputsRollsBackBothTreesOnSecondInstallFailure(t *testing.T) {
	root := t.TempDir()
	writeBuildFixture(t, root, "source/index.html", "new")
	writeBuildFixture(t, root, "public/index.html", "previous public")
	writeBuildFixture(t, root, "embedded/index.html", "previous embedded")
	targets := []string{filepath.Join(root, "public"), filepath.Join(root, "embedded")}
	failure := errors.New("synthetic rename failure")
	err := installOutputs(filepath.Join(root, "source"), targets, func(from, to string) error {
		if filepath.Base(from) == "next" && to == targets[1] {
			return failure
		}
		return os.Rename(from, to)
	})
	if !errors.Is(err, failure) {
		t.Fatalf("lost installation failure: %v", err)
	}
	for i, want := range []string{"previous public", "previous embedded"} {
		got, err := os.ReadFile(filepath.Join(targets[i], "index.html"))
		if err != nil || string(got) != want {
			t.Fatalf("rollback output %d: %q, %v", i, got, err)
		}
	}
	leftovers, err := filepath.Glob(filepath.Join(root, ".dist-install-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("successful rollback left staging trees: %v, %v", leftovers, err)
	}
}

func TestBuildWebCheckDetectsStaleMissingAndExtraFilesWithoutRepairing(t *testing.T) {
	root := buildFixture(t)
	if err := buildWeb(root); err != nil {
		t.Fatal(err)
	}
	if err := buildWebMode(root, true); err != nil {
		t.Fatal(err)
	}
	writeBuildFixture(t, root, "web/dist/untracked.js", "unexpected generated file")
	if err := buildWebMode(root, true); err == nil {
		t.Fatal("check accepted extra file")
	}
	if _, err := os.Stat(filepath.Join(root, "web/dist/untracked.js")); err != nil {
		t.Fatal("check removed extra file", err)
	}
	if err := buildWeb(root); err != nil {
		t.Fatal(err)
	}
	writeBuildFixture(t, root, "web/src/style.css", "body { color: blue; }")
	if err := buildWebMode(root, true); err == nil {
		t.Fatal("check accepted stale assets")
	}
	if err := buildWeb(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "cmd/tosub2/web/dist/index.html")); err != nil {
		t.Fatal(err)
	}
	if err := buildWebMode(root, true); err == nil {
		t.Fatal("check accepted interrupted publication")
	}
	if _, err := os.Stat(filepath.Join(root, "cmd/tosub2/web/dist/index.html")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check repaired missing output", err)
	}
}

func TestInstallOutputsRetainsBackupWhenRollbackFails(t *testing.T) {
	root := t.TempDir()
	writeBuildFixture(t, root, "source/index.html", "new")
	writeBuildFixture(t, root, "public/index.html", "previous public")
	writeBuildFixture(t, root, "embedded/index.html", "previous embedded")
	targets := []string{filepath.Join(root, "public"), filepath.Join(root, "embedded")}
	installErr, restoreErr := errors.New("install failed"), errors.New("restore failed")
	err := installOutputs(filepath.Join(root, "source"), targets, func(from, to string) error {
		if filepath.Base(from) == "next" && to == targets[1] {
			return installErr
		}
		if filepath.Base(from) == "previous" && to == targets[0] {
			return restoreErr
		}
		return os.Rename(from, to)
	})
	if !errors.Is(err, installErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("lost errors: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(root, ".dist-install-*", "previous", "index.html"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("lost recovery backup: %v, %v", backups, err)
	}
	got, err := os.ReadFile(backups[0])
	if err != nil || string(got) != "previous public" {
		t.Fatalf("damaged recovery backup: %q, %v", got, err)
	}
}
