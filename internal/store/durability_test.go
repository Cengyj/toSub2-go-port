package store

import (
	"errors"
	"os"
	"testing"
)

func TestDirectorySyncFailureReportsAlreadyReplacedFile(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, errors.New("synthetic directory sync failure")} {
		s := newTestStore(t)
		if err := s.WriteBytes(t.Context(), "state.json", []byte(`{"version":1}`)); err != nil {
			t.Fatal(err)
		}
		called := false
		err := s.writeBytes(t.Context(), "state.json", []byte(`{"version":2}`), func(root string) error {
			called = true
			if root != s.Root {
				t.Fatal("directory sync used a different store")
			}
			data, err := os.ReadFile(s.Path("state.json"))
			if err != nil || string(data) != `{"version":2}` {
				t.Fatal("fault was injected before the real file replacement")
			}
			return cause
		})
		if !called || !errors.Is(err, ErrDurabilityUncertain) || !errors.Is(err, cause) {
			t.Fatalf("successful rename was classified as an ordinary write failure: %v", err)
		}
		data, readErr := os.ReadFile(s.Path("state.json"))
		if readErr != nil || string(data) != `{"version":2}` {
			t.Fatal("uncertain durability hid or removed the visible replacement")
		}
		assertNoTemporaryFiles(t, s)
	}
}

func TestFailedRenameIsNotDurabilityUncertainty(t *testing.T) {
	s := newTestStore(t)
	if err := os.Mkdir(s.Path("directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := s.writeBytes(t.Context(), "directory.json", []byte(`{"version":2}`), func(string) error {
		called = true
		return nil
	})
	if err == nil || errors.Is(err, ErrDurabilityUncertain) || called {
		t.Fatal("failed rename was classified as an already committed replacement")
	}
	assertNoTemporaryFiles(t, s)
}
