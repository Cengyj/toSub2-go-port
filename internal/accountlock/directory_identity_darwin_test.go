package accountlock

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirectoryIdentityDarwinMissingCaseAliasesStayStable(t *testing.T) {
	parent := t.TempDir()
	sensitive, err := unix.Pathconf(parent, directoryCaseSensitive)
	if err != nil || sensitive != 0 && sensitive != 1 {
		t.Fatal("cannot inspect native filesystem case semantics", err)
	}
	root, alias := filepath.Join(parent, "New", "Data"), filepath.Join(parent, "new", "data")
	before, err := DirectoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := DirectoryIdentity(alias)
	if err != nil || (before == other) != (sensitive == 0) {
		t.Fatal("missing paths did not follow the actual volume's case semantics", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("identity lookup created a directory", err)
	}
	// A peer can create the directory using the other accepted spelling.
	if err := os.MkdirAll(alias, 0700); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{root: before, alias: other} {
		if got, err := DirectoryIdentity(path); err != nil || got != expected {
			t.Fatal("first creation changed an already-held directory identity", err)
		}
	}
	t.Logf("native _PC_CASE_SENSITIVE=%d", sensitive)
}

func TestDirectoryIdentityDarwinUnavailableSemanticsFailClosed(t *testing.T) {
	if _, err := directoryIdentityComponent(filepath.Join(t.TempDir(), "missing"), "child"); err == nil {
		t.Fatal("unavailable filesystem semantics were guessed")
	}
}

func TestDirectoryIdentityDarwinUnicodeAliasesStayStable(t *testing.T) {
	parent := t.TempDir()
	var volume unix.Statfs_t
	if err := unix.Statfs(parent, &volume); err != nil {
		t.Fatal(err)
	}
	if unix.ByteSliceToString(volume.Fstypename[:]) != "apfs" {
		t.Skip("canonical Unicode normalization is verified only for APFS")
	}
	composed := filepath.Join(parent, "Caf\u00e9", "Data")
	decomposed := filepath.Join(parent, "Cafe\u0301", "Data")
	before, err := DirectoryIdentity(composed)
	if err != nil {
		t.Fatal(err)
	}
	other, err := DirectoryIdentity(decomposed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(decomposed, 0700); err != nil {
		t.Fatal(err)
	}
	one, err := os.Stat(composed)
	if os.IsNotExist(err) {
		t.Skip("native filesystem distinguishes these Unicode names; they are not directory aliases")
	}
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(decomposed)
	if err != nil || !os.SameFile(one, two) {
		t.Fatal("native Unicode alias does not identify the synthetic directory", err)
	}
	if before != other {
		t.Error("equivalent missing Unicode paths acquired different lock identities")
	}
	for _, path := range []string{composed, decomposed} {
		if got, err := DirectoryIdentity(path); err != nil || got != before {
			t.Error("Unicode directory creation changed the already-held identity", err)
		}
	}
	t.Log("native APFS NFC/NFD aliases retain one identity before and after creation")
}

func TestDirectoryIdentityDarwinFullCaseAliasesStayStable(t *testing.T) {
	parent := t.TempDir()
	var volume unix.Statfs_t
	if err := unix.Statfs(parent, &volume); err != nil {
		t.Fatal(err)
	}
	sensitive, err := unix.Pathconf(parent, directoryCaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	if unix.ByteSliceToString(volume.Fstypename[:]) != "apfs" || sensitive != 0 {
		t.Skip("full Unicode case aliases require case-insensitive APFS")
	}
	for _, pair := range []struct{ name, one, two string }{
		{"sigma", "Σ", "ς"},
		{"long_s", "S", "ſ"},
		{"sharp_s", "SS", "ß"},
		{"ligature", "FF", "ﬀ"},
		{"dotted_i", "İ", "i\u0307"},
	} {
		t.Run(pair.name, func(t *testing.T) {
			base := t.TempDir()
			root, alias := filepath.Join(base, pair.one, "Data"), filepath.Join(base, pair.two, "Data")
			before, err := DirectoryIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			other, err := DirectoryIdentity(alias)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(alias, 0700); err != nil {
				t.Fatal(err)
			}
			one, err := os.Stat(root)
			if err != nil {
				t.Fatal("native APFS case alias was not readable", err)
			}
			two, err := os.Stat(alias)
			if err != nil || !os.SameFile(one, two) {
				t.Fatal("native APFS case aliases identify different directories", err)
			}
			if before != other {
				t.Error("equivalent missing case aliases acquired different identities")
			}
			for _, path := range []string{root, alias} {
				if got, err := DirectoryIdentity(path); err != nil || got != before {
					t.Error("case alias creation changed the already-held identity", err)
				}
			}
		})
	}
}
