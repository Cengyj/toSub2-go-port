//go:build windows

package protocol

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
	"tosub2-go-port/internal/privatefile"
)

// Runs actual Windows APIs; successful cross-compilation is not this test passing.
func TestPrivateExportAndCheckpointPreserveExistingParentDACL(t *testing.T) {
	parent := t.TempDir()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	readParent := func() string {
		sd, err := windows.GetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		return sd.String()
	}
	before := readParent()
	paths := []string{filepath.Join(parent, "export.json"), filepath.Join(parent, "checkpoint.json")}
	if err := WritePrivateJSON(paths[0], map[string]any{"fixture": "private"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpoint(paths[1], Checkpoint{Version: 1, Stage: "email_verified", Email: "fixture@example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = privatefile.Verify(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if after := readParent(); after != before {
		t.Fatal("explicit export changed its existing parent's owner or DACL")
	}
}
