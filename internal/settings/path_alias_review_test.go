package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"tosub2-go-port/internal/credentials"
)

// The protected hierarchy rejects a symlink at its root, while normal system
// and user paths may contain an ancestor alias. Both spellings must identify
// the same revision lease without weakening the root's path policy.
func TestProtectedSettingsAncestorAliasCannotBypassHeldRevision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; native alias acceptance is separate")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "data")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	direct := New(root, protector)
	throughAlias := New(filepath.Join(alias, "data"), protector)
	first := json.RawMessage(`{"value":"original","enabled":false,"zero":0}`)
	saved, err := direct.Save(t.Context(), Mail, 0, first)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := throughAlias.Load(t.Context(), Mail)
	if err != nil || observed.Revision != 1 || !equal(observed, saved) {
		t.Fatal("ancestor alias is not a readable spelling of this protected directory", err)
	}
	release, err := direct.HoldRevision(t.Context(), Mail, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := throughAlias.Save(t.Context(), Mail, 1, json.RawMessage(`{"value":"replacement"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("ancestor alias bypassed the held settings revision", err)
	}
	if _, err := throughAlias.Confirm(t.Context(), Mail, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("ancestor alias confirmed a revision while its application lease was held", err)
	}
	if otherRelease, err := throughAlias.HoldRevision(t.Context(), Mail, 1); !errors.Is(err, ErrConflict) {
		if otherRelease != nil {
			_ = otherRelease()
		}
		t.Fatal("ancestor alias acquired a second application lease", err)
	}
	after, err := direct.Load(t.Context(), Mail)
	if err != nil || !equal(after, observed) {
		t.Fatal("rejected alias save changed the held configuration", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := throughAlias.Confirm(t.Context(), Mail, 1); err != nil || !equal(confirmed, observed) {
		t.Fatal("released revision could not be confirmed through its ancestor alias", err)
	}
}

type firstSettingsSeal struct {
	credentials.StateProtector
	before func()
}

func (p firstSettingsSeal) SealContext(ctx context.Context, identity string, plain, aad []byte) ([]byte, error) {
	p.before()
	return p.StateProtector.SealContext(ctx, identity, plain, aad)
}

func TestProtectedSettingsMissingRootAliasesShareFirstWriteLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; native alias acceptance is separate")
	}
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "not-created", "data")
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	throughAlias := New(filepath.Join(alias, "not-created", "data"), protector)
	checked := false
	direct := New(root, firstSettingsSeal{StateProtector: protector, before: func() {
		checked = true
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("fixture no longer reaches protection before first directory creation", err)
		}
		if _, err := throughAlias.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"alias"}`)); !errors.Is(err, ErrConflict) {
			t.Error("missing root ancestor alias acquired a second initial write lease", err)
		}
	}})
	saved, err := direct.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"direct"}`))
	if err != nil || !checked {
		t.Fatal("initial protected write failed", err)
	}
	observed, err := throughAlias.Load(t.Context(), SMS)
	if err != nil || !equal(saved, observed) {
		t.Fatal("initial save did not preserve its revision across alias creation", err)
	}
	if _, err := throughAlias.Save(t.Context(), SMS, 0, json.RawMessage(`{"writer":"stale"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale initial revision replaced the committed file", err)
	}
}

func TestProtectedSettingsRejectsSymlinkAtProtectedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; native alias acceptance is separate")
	}
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	direct, linked := New(root, protector), New(alias, protector)
	if _, err := direct.Save(t.Context(), Proxy, 0, json.RawMessage(`{"value":"original"}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(direct.path(Proxy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linked.Load(t.Context(), Proxy); !errors.Is(err, ErrUnavailable) {
		t.Fatal("symlink protected root became readable", err)
	}
	if _, err := linked.Save(t.Context(), Proxy, 1, json.RawMessage(`{"value":"replacement"}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("symlink protected root became writable", err)
	}
	after, err := os.ReadFile(direct.path(Proxy))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected root alias changed the original ciphertext", err)
	}
}

func TestProtectedSettingsCaseAliasCannotBypassHeldRevision(t *testing.T) {
	parent := t.TempDir()
	root, alias := filepath.Join(parent, "MixedCaseData"), filepath.Join(parent, "mixedcasedata")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	one, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(alias)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("filesystem distinguishes case variants; these names are not directory aliases")
	}
	if err != nil || !os.SameFile(one, two) {
		t.Fatal("case variant did not resolve to the synthetic directory", err)
	}
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	direct, throughAlias := New(root, protector), New(alias, protector)
	saved, err := direct.Save(t.Context(), Mail, 0, json.RawMessage(`{"value":"original"}`))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := throughAlias.Load(t.Context(), Mail)
	if err != nil || !equal(saved, observed) {
		t.Fatal("case alias was not readable under the protected file policy", err)
	}
	release, err := direct.HoldRevision(t.Context(), Mail, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := throughAlias.Save(t.Context(), Mail, saved.Revision, json.RawMessage(`{"value":"replacement"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("case alias bypassed the held settings revision", err)
	}
	if after, err := direct.Load(t.Context(), Mail); err != nil || !equal(after, saved) {
		t.Fatal("case alias changed the held configuration", err)
	}
}
