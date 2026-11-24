package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"tosub2-go-port/internal/credentials"
)

func TestProtectedSettingsDarwinMissingCaseAliasesShareFirstWriteLease(t *testing.T) {
	parent := t.TempDir()
	sensitive, err := unix.Pathconf(parent, 11) // _PC_CASE_SENSITIVE
	if err != nil || sensitive != 0 && sensitive != 1 {
		t.Fatal("cannot inspect native filesystem case semantics", err)
	}
	root, alias := filepath.Join(parent, "New", "Data"), filepath.Join(parent, "new", "data")
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	throughAlias := New(alias, protector)
	checked := false
	direct := New(root, firstSettingsSeal{StateProtector: protector, before: func() {
		checked = true
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("fixture no longer reaches protection before first directory creation", err)
		}
		_, err := throughAlias.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"alias"}`))
		if sensitive == 0 && !errors.Is(err, ErrConflict) {
			t.Error("case alias acquired another first-write lease", err)
		}
		if sensitive == 1 && err != nil {
			t.Error("case-sensitive volume's separate directory was blocked", err)
		}
	}})
	saved, err := direct.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"direct"}`))
	if err != nil || !checked {
		t.Fatal("first protected save failed", err)
	}
	if sensitive == 0 {
		observed, err := throughAlias.Load(t.Context(), SMS)
		if err != nil || !equal(saved, observed) {
			t.Fatal("first save changed its identity after creating the directory", err)
		}
		release, err := throughAlias.HoldRevision(t.Context(), SMS, saved.Revision)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := direct.Save(t.Context(), SMS, saved.Revision, json.RawMessage(`{"writer":"replacement"}`)); !errors.Is(err, ErrConflict) {
			t.Fatal("created case alias bypassed the original revision lease", err)
		}
	}
	t.Logf("native _PC_CASE_SENSITIVE=%d", sensitive)
}

func TestProtectedSettingsDarwinMissingUnicodeAliasesShareFirstWriteLease(t *testing.T) {
	parent := t.TempDir()
	var volume unix.Statfs_t
	if err := unix.Statfs(parent, &volume); err != nil {
		t.Fatal(err)
	}
	if unix.ByteSliceToString(volume.Fstypename[:]) != "apfs" {
		t.Skip("canonical Unicode normalization is verified only for APFS")
	}
	// Establish that these two Unicode forms really alias on this test volume.
	probe := filepath.Join(parent, "probe-Cafe\u0301")
	if err := os.Mkdir(probe, 0700); err != nil {
		t.Fatal(err)
	}
	one, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(parent, "probe-Caf\u00e9"))
	if err != nil || !os.SameFile(one, two) {
		t.Fatal("APFS Unicode forms did not identify the same synthetic directory", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	root, alias := filepath.Join(parent, "Caf\u00e9", "Data"), filepath.Join(parent, "Cafe\u0301", "Data")
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	throughAlias := New(alias, protector)
	checked := false
	direct := New(root, firstSettingsSeal{StateProtector: protector, before: func() {
		checked = true
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("fixture no longer reaches protection before first directory creation", err)
		}
		if _, err := throughAlias.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"alias"}`)); !errors.Is(err, ErrConflict) {
			t.Error("Unicode alias acquired another first-write lease", err)
		}
	}})
	saved, err := direct.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"direct"}`))
	if err != nil || !checked {
		t.Fatal("first protected save failed", err)
	}
	observed, err := throughAlias.Load(t.Context(), SMS)
	if err != nil || !equal(saved, observed) {
		t.Fatal("first save lost its revision across a Unicode directory alias", err)
	}
	release, err := throughAlias.HoldRevision(t.Context(), SMS, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := direct.Confirm(t.Context(), SMS, saved.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("created Unicode alias bypassed the original revision lease", err)
	}
	t.Log("native APFS NFC/NFD alias could not replace a pending first save")
}

func TestProtectedSettingsDarwinFullCaseAliasesShareFirstWriteLease(t *testing.T) {
	parent := t.TempDir()
	var volume unix.Statfs_t
	if err := unix.Statfs(parent, &volume); err != nil {
		t.Fatal(err)
	}
	sensitive, err := unix.Pathconf(parent, 11) // _PC_CASE_SENSITIVE
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
			protector, err := credentials.NewMemoryStateProtector()
			if err != nil {
				t.Fatal(err)
			}
			throughAlias := New(alias, protector)
			checked := false
			direct := New(root, firstSettingsSeal{StateProtector: protector, before: func() {
				checked = true
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatal("fixture missed protection before first directory creation", err)
				}
				if _, err := throughAlias.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"alias"}`)); !errors.Is(err, ErrConflict) {
					t.Error("full case alias acquired a second first-write lease", err)
				}
			}})
			saved, err := direct.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"direct"}`))
			if err != nil || !checked {
				t.Fatal("first protected save failed", err)
			}
			one, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			two, err := os.Stat(alias)
			if err != nil || !os.SameFile(one, two) {
				t.Fatal("native case aliases did not identify the saved directory", err)
			}
			observed, err := throughAlias.Load(t.Context(), SMS)
			if err != nil || !equal(saved, observed) {
				t.Fatal("case alias lost the first committed settings revision", err)
			}
			release, err := throughAlias.HoldRevision(t.Context(), SMS, saved.Revision)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if _, err := direct.Confirm(t.Context(), SMS, saved.Revision); !errors.Is(err, ErrConflict) {
				t.Fatal("created case alias bypassed the held revision", err)
			}
		})
	}
}
