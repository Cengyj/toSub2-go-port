package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type programFixture struct{ Root, Output, Control string }

func programFixtureCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestProgramCompilerFixture$", "--"}, args...)...)
}

func readProgramFixture(t *testing.T) programFixture {
	t.Helper()
	value := os.Getenv("TOSUB2_PROGRAM_BUILD_FIXTURE")
	if value == "" {
		t.Skip("subprocess fixture only")
	}
	var fixture programFixture
	if err := json.Unmarshal([]byte(value), &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func waitProgramFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if value, err := os.ReadFile(path); err == nil {
			return value
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("build fixture did not reach boundary", filepath.Base(path))
	return nil
}

// Control-file existence is the interprocess signal consumed by waitProgramFile.
// Publish only a closed, complete file; WriteFile exposes an empty file before
// its contents are written and can release the reader with partial JSON.
func publishProgramFile(t *testing.T, path string, value []byte) {
	t.Helper()
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		t.Fatal(err)
	}
}

func TestProgramCompilerFixture(t *testing.T) {
	fixture := readProgramFixture(t)
	var output string
	for i, arg := range os.Args {
		if arg == "-o" && i+1 < len(os.Args) {
			output = os.Args[i+1]
		}
	}
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if source == fixture.Root || output == "" {
		t.Fatal("compiler did not receive an immutable input and candidate output")
	}
	publishProgramFile(t, filepath.Join(fixture.Control, "ready"), []byte(source))
	waitProgramFile(t, filepath.Join(fixture.Control, "release"))
	assets := map[string]string{}
	embedded := filepath.Join(source, "cmd/tosub2/web/dist")
	if err := filepath.WalkDir(embedded, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(embedded, path)
		if err != nil {
			return err
		}
		assets[filepath.ToSlash(relative)] = string(data)
		return nil
	}); err != nil {
		t.Fatal("compiler snapshot was removed while reading", err)
	}
	data, err := json.Marshal(assets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, data, 0700); err != nil {
		t.Fatal(err)
	}
	environment, err := json.Marshal(map[string]string{"CGO_ENABLED": os.Getenv("CGO_ENABLED"), "GOOS": os.Getenv("GOOS"), "GOARCH": os.Getenv("GOARCH"), "source": source})
	if err != nil {
		t.Fatal(err)
	}
	publishProgramFile(t, filepath.Join(fixture.Control, filepath.Base(output)+".environment"), environment)
	publishProgramFile(t, filepath.Join(fixture.Control, "finished"), data)
}

func TestProgramCompilerFailureFixture(t *testing.T) {
	readProgramFixture(t)
	t.Fatal("injected compiler failure")
}

func TestProgramOwnerFixture(t *testing.T) {
	fixture := readProgramFixture(t)
	if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: fixture.Output, command: programFixtureCommand}); err != nil {
		t.Fatal(err)
	}
}

func newProgramFixture(t *testing.T) programFixture {
	t.Helper()
	fixture := programFixture{Root: buildFixture(t), Output: t.TempDir(), Control: t.TempDir()}
	writeBuildFixture(t, fixture.Root, "go.mod", "module buildfixture\n\ngo 1.26.0\n")
	if err := os.Mkdir(filepath.Join(fixture.Root, "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	writeBuildFixture(t, fixture.Root, "docs/pure-go-runtime/THIRD_PARTY_NOTICES.txt", "synthetic runtime notices")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOSUB2_PROGRAM_BUILD_FIXTURE", string(data))
	return fixture
}

func releaseProgramFixture(t *testing.T, fixture programFixture) {
	t.Helper()
	publishProgramFile(t, filepath.Join(fixture.Control, "release"), []byte("continue"))
}

func TestProgramBuildHoldsWorkspaceLockWhileCompilerReadsEmbeddedAssets(t *testing.T) {
	fixture := newProgramFixture(t)
	done := make(chan error, 1)
	go func() {
		done <- buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: fixture.Output, command: programFixtureCommand})
	}()
	waitProgramFile(t, filepath.Join(fixture.Control, "ready"))
	for _, check := range []bool{false, true} {
		if err := buildWebMode(fixture.Root, check); !errors.Is(err, errBuildBusy) {
			t.Errorf("frontend can change while compiler reads embed: check=%v err=%v", check, err)
		}
	}
	if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: t.TempDir(), command: programFixtureCommand}); !errors.Is(err, errBuildBusy) {
		t.Error("second complete build did not stop at workspace admission", err)
	}
	releaseProgramFixture(t, fixture)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := waitProgramFile(t, filepath.Join(fixture.Control, "finished"))
	got, err := os.ReadFile(filepath.Join(fixture.Output, "tosub2"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("compiler result was not published", err)
	}
	if err := buildWebMode(fixture.Root, true); err != nil {
		t.Fatal("complete build leaked its lock", err)
	}
}

func TestKilledBuildOwnerCannotPublishOrMixLaterEmbeddedAssets(t *testing.T) {
	fixture := newProgramFixture(t)
	const prior = "previous executable"
	if err := os.WriteFile(filepath.Join(fixture.Output, "tosub2"), []byte(prior), 0700); err != nil {
		t.Fatal(err)
	}
	owner := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProgramOwnerFixture$")
	owner.Env = os.Environ()
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	source := string(waitProgramFile(t, filepath.Join(fixture.Control, "ready")))
	oldIndex, err := os.ReadFile(filepath.Join(fixture.Root, "web/dist/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	writeBuildFixture(t, fixture.Root, "web/src/style.css", "body { color: purple; }")
	if err := buildWeb(fixture.Root); err != nil {
		t.Fatal("OS lock remained after killed owner", err)
	}
	releaseProgramFixture(t, fixture)
	data := waitProgramFile(t, filepath.Join(fixture.Control, "finished"))
	var captured map[string]string
	if err := json.Unmarshal(data, &captured); err != nil {
		t.Fatal(err)
	}
	if captured["index.html"] != string(oldIndex) {
		t.Fatal("orphan compiler read a later build's index")
	}
	for name := range captured {
		if strings.HasPrefix(name, "assets/") && !strings.Contains(captured["index.html"], "/"+name) {
			t.Fatal("orphan compiler mixed asset revisions")
		}
	}
	if got, err := os.ReadFile(filepath.Join(fixture.Output, "tosub2")); err != nil || string(got) != prior {
		t.Fatal("killed owner published its orphan compiler's output")
	}
	// Only this temporary fixture owns this known inactive staging directory.
	if err := os.RemoveAll(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
}

func TestProgramBuildRealCompilerUsesSnapshotAndIndependentOutput(t *testing.T) {
	fixture := newProgramFixture(t)
	writeBuildFixture(t, fixture.Root, "cmd/tosub2/main.go", "package main\nimport (\"embed\";\"encoding/json\";\"io/fs\";\"os\")\n//go:embed web/dist/*\nvar files embed.FS\nfunc main(){out:=map[string]string{};fs.WalkDir(files,\"web/dist\",func(p string,d fs.DirEntry,e error)error{if e==nil&&!d.IsDir(){b,_:=files.ReadFile(p);out[p]=string(b)};return e});json.NewEncoder(os.Stdout).Encode(out)}\n")
	var expected []byte
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		var err error
		expected, err = os.ReadFile(filepath.Join(fixture.Root, "cmd/tosub2/web/dist/index.html"))
		if err != nil {
			t.Fatal(err)
		}
		// Exercise Go's real embed behavior even if the live tree was
		// removed after snapshot creation, as after an owner's forced exit.
		if err := os.RemoveAll(filepath.Join(fixture.Root, "cmd/tosub2/web/dist")); err != nil {
			t.Fatal(err)
		}
		return exec.CommandContext(ctx, "go", args...)
	}
	if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: fixture.Output, command: command}); err != nil {
		t.Fatal(err)
	}
	data, err := exec.CommandContext(t.Context(), filepath.Join(fixture.Output, "tosub2")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var embedded map[string]string
	if err := json.Unmarshal(data, &embedded); err != nil {
		t.Fatal(err)
	}
	if embedded["web/dist/index.html"] != string(expected) {
		t.Fatal("real compiler did not embed the snapshotted index")
	}
	for name := range embedded {
		if strings.HasPrefix(name, "web/dist/assets/") && !strings.Contains(string(expected), strings.TrimPrefix(name, "web/dist")) {
			t.Fatal("real compiler embedded a mismatching asset")
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.Root, "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("independent output wrote the workspace bin directory")
	}
}

func TestProgramCompileFailurePreservesPreviousOutputs(t *testing.T) {
	fixture := newProgramFixture(t)
	for _, name := range []string{"tosub2", "AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt", "unrelated-program"} {
		if err := os.WriteFile(filepath.Join(fixture.Output, name), []byte("previous "+name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProgramCompilerFailureFixture$")
	}
	if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: fixture.Output, command: command}); err == nil {
		t.Fatal("failed compilation reported success")
	}
	entries, err := os.ReadDir(fixture.Output)
	if err != nil || len(entries) != 3 {
		t.Fatal("failed compilation left extra outputs", entries, err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(fixture.Output, entry.Name()))
		if err != nil || string(data) != "previous "+entry.Name() {
			t.Fatal("failed compilation replaced a prior output", entry.Name(), err)
		}
	}
}

func TestProgramCrossBuildUsesEveryRequestedTargetAndOneSnapshot(t *testing.T) {
	fixture := newProgramFixture(t)
	releaseProgramFixture(t, fixture)
	if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: fixture.Output, cross: true, command: programFixtureCommand}); err != nil {
		t.Fatal(err)
	}
	var source string
	for _, target := range []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "windows/amd64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		name := "tosub2-" + goos + "-" + goarch
		if goos == "windows" {
			name += ".exe"
		}
		if _, err := os.Stat(filepath.Join(fixture.Output, name)); err != nil {
			t.Fatal("missing release target", name, err)
		}
		var environment map[string]string
		if err := json.Unmarshal(waitProgramFile(t, filepath.Join(fixture.Control, name+".environment")), &environment); err != nil {
			t.Fatal(err)
		}
		if environment["CGO_ENABLED"] != "0" || environment["GOOS"] != goos || environment["GOARCH"] != goarch {
			t.Fatal("wrong release environment", environment)
		}
		if source != "" && source != environment["source"] {
			t.Fatal("release targets used different snapshots")
		}
		source = environment["source"]
	}
	data, err := os.ReadFile(filepath.Join(fixture.Output, "AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt"))
	if err != nil || string(data) != "synthetic runtime notices" {
		t.Fatal("missing runtime notices", err)
	}
}

func TestProgramBuildRejectsOutputInsideSnapshottedSources(t *testing.T) {
	fixture := newProgramFixture(t)
	for _, source := range []string{"internal", "cmd/tosub2"} {
		called := false
		command := func(ctx context.Context, args ...string) *exec.Cmd {
			called = true
			return programFixtureCommand(ctx, args...)
		}
		if err := buildWebProgram(t.Context(), fixture.Root, false, programBuild{directory: filepath.Join(source, "bin"), command: command}); err == nil || called {
			t.Fatal("recursive output location reached compilation", source, err)
		}
		entries, err := os.ReadDir(filepath.Join(fixture.Root, source, "bin"))
		if err != nil || len(entries) != 0 {
			t.Fatal("recursive source copy was created", entries, err)
		}
	}
}

func TestProgramStaleFrontendCheckNeverStartsCompiler(t *testing.T) {
	fixture := newProgramFixture(t)
	if err := buildWeb(fixture.Root); err != nil {
		t.Fatal(err)
	}
	writeBuildFixture(t, fixture.Root, "web/src/style.css", "body { color: orange; }")
	called := false
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		called = true
		return programFixtureCommand(ctx, args...)
	}
	releaseProgramFixture(t, fixture)
	if err := buildWebProgram(t.Context(), fixture.Root, true, programBuild{directory: fixture.Output, command: command}); err == nil || called {
		t.Fatal("stale frontend started compiler or reported success", err, called)
	}
}

func TestProgramPublicationFailureRestoresPriorFiles(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "retain-recovery-material"}[rollbackFails], func(t *testing.T) {
			staging, output := t.TempDir(), t.TempDir()
			for _, name := range []string{"tosub2", "notices"} {
				if err := os.WriteFile(filepath.Join(staging, name), []byte("new "+name), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(output, name), []byte("old "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			publishErr, restoreErr := errors.New("publication failed"), errors.New("restoration failed")
			rename := func(from, to string) error {
				if from == filepath.Join(staging, "notices") {
					return publishErr
				}
				if rollbackFails && from == filepath.Join(staging, "previous", "tosub2") {
					return restoreErr
				}
				return os.Rename(from, to)
			}
			remove, err := publishProgramArtifacts(staging, output, []string{"tosub2", "notices"}, rename)
			if !errors.Is(err, publishErr) || errors.Is(err, restoreErr) != rollbackFails || remove == rollbackFails {
				t.Fatal("publication lost errors or recovery directory", remove, err)
			}
			for _, name := range []string{"tosub2", "notices"} {
				path := filepath.Join(output, name)
				if rollbackFails && name == "tosub2" {
					path = filepath.Join(staging, "previous", name)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "old "+name {
					t.Fatal("prior output lost", name, err)
				}
			}
		})
	}
}
