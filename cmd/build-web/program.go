package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type programBuild struct {
	directory string
	cross     bool
	command   func(context.Context, ...string) *exec.Cmd
}

func buildWebProgram(ctx context.Context, root string, check bool, program programBuild) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	release, err := acquireBuildLock(root)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := buildWebLocked(root, check); err != nil {
		return err
	}
	return compileProgram(ctx, root, program)
}

// Go's overlay does not isolate embed directory discovery. Compile a private
// module copy instead: an orphaned compiler can only read this build's source
// and write a candidate here, never consume the next dist or publish it.
func compileProgram(ctx context.Context, root string, program programBuild) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := program.directory
	if directory == "" {
		directory = "bin"
	}
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(root, directory)
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolvedOutput, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	for _, input := range []string{"cmd/tosub2", "internal"} {
		relative, err := filepath.Rel(filepath.Join(resolvedRoot, input), resolvedOutput)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("program output directory must be outside %s", input)
		}
	}
	staging, err := os.MkdirTemp(directory, ".tosub2-build-")
	if err != nil {
		return err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	source, err := snapshotProgram(root, staging)
	if err != nil {
		return err
	}
	notices, err := os.ReadFile(filepath.Join(root, "docs", "pure-go-runtime", "THIRD_PARTY_NOTICES.txt"))
	if err != nil {
		return err
	}
	const noticesName = "AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt"
	if err := os.WriteFile(filepath.Join(staging, noticesName), notices, 0644); err != nil {
		return err
	}
	targets := []string{""}
	if program.cross {
		targets = []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "windows/amd64"}
	}
	command := program.command
	if command == nil {
		command = func(ctx context.Context, args ...string) *exec.Cmd { return exec.CommandContext(ctx, "go", args...) }
	}
	var artifacts []string
	for _, target := range targets {
		name := "tosub2"
		environment := replaceBuildEnv(os.Environ(), "CGO_ENABLED", "0")
		environment = replaceBuildEnv(environment, "GOWORK", "off")
		if target != "" {
			goos, goarch, _ := strings.Cut(target, "/")
			environment = replaceBuildEnv(environment, "GOOS", goos)
			environment = replaceBuildEnv(environment, "GOARCH", goarch)
			name += "-" + goos + "-" + goarch
			if goos == "windows" {
				name += ".exe"
			}
		}
		cmd := command(ctx, "build", "-mod=readonly", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(staging, name), "./cmd/tosub2")
		cmd.Dir, cmd.Env = source, environment
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("compile %s: %w", name, err)
		}
		artifacts = append(artifacts, name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	removeStaging, err = publishProgramArtifacts(staging, directory, append(artifacts, noticesName), os.Rename)
	return err
}

func replaceBuildEnv(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, key) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func snapshotProgram(root, staging string) (string, error) {
	snapshot := filepath.Join(staging, "source")
	// These are the module inputs of ./cmd/tosub2, including its embedded web
	// and JS host files. Runtime data, Git state and unrelated binaries stay out.
	for _, name := range []string{"go.mod", "go.sum", "cmd/tosub2", "internal"} {
		source := filepath.Join(root, name)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) && name == "go.sum" {
			continue // A dependency-free module does not need a checksum file.
		}
		if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(snapshot, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("build input must be a regular file: %s", path)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0600)
		}); err != nil {
			return "", fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	return snapshot, nil
}

// Publish only after every target compiled. Ordinary rename failures restore
// the previous files; if restoration itself fails, keep its recovery material.
// A process crash between distinct artifact renames cannot be atomic.
func publishProgramArtifacts(staging, directory string, names []string, rename func(string, string) error) (removeStaging bool, result error) {
	type artifact struct {
		name                string
		backedUp, installed bool
	}
	previous := filepath.Join(staging, "previous")
	if err := os.Mkdir(previous, 0700); err != nil {
		return true, err
	}
	artifacts := make([]artifact, 0, len(names))
	rollback := func(cause error) (bool, error) {
		canRemove := true
		for i := len(artifacts) - 1; i >= 0; i-- {
			item := &artifacts[i]
			target := filepath.Join(directory, item.name)
			if item.installed {
				if err := os.Remove(target); err != nil {
					cause = errors.Join(cause, fmt.Errorf("remove candidate %s: %w", target, err))
					canRemove = false
					continue
				}
			}
			if item.backedUp {
				if err := rename(filepath.Join(previous, item.name), target); err != nil {
					cause = errors.Join(cause, fmt.Errorf("restore %s from %s: %w", target, previous, err))
					canRemove = false
				}
			}
		}
		return canRemove, cause
	}
	for _, name := range names {
		target := filepath.Join(directory, name)
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			return rollback(fmt.Errorf("program output must be a regular file: %s", target))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return rollback(err)
		}
		artifacts = append(artifacts, artifact{name: name})
		item := &artifacts[len(artifacts)-1]
		if err := rename(target, filepath.Join(previous, name)); err == nil {
			item.backedUp = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollback(fmt.Errorf("preserve %s: %w", target, err))
		}
		if err := rename(filepath.Join(staging, name), target); err != nil {
			return rollback(fmt.Errorf("publish %s: %w", target, err))
		}
		item.installed = true
	}
	return true, nil
}
