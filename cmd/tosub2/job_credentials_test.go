package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestOperationCredentialProtectorRetainsUncommittedMaterial(t *testing.T) {
	for _, clear := range []bool{false, true} {
		vault := credentials.NewMemoryStore()
		old := credentials.Credentials{
			Password: "old-password", TOTPSecret: "old-secret", PendingPassword: "pending-password",
			PendingTOTPSecret: "pending-secret", PendingTOTPURI: "pending-uri",
			TOTPResetPending: true, TOTPResetOperationID: "bound-operation",
		}
		if err := vault.Save("owner@example.test", old); err != nil {
			t.Fatal(err)
		}
		accepted := 0
		protect := operationCredentialProtector(t.Context(), vault, "owner@example.test", true, func(value credentials.Credentials) {
			stored, err := vault.Load("owner@example.test")
			if err != nil || !reflect.DeepEqual(stored, value) {
				t.Error("published credentials before a matching save and readback")
			}
			accepted++
		})
		if err := protect(credentials.Credentials{Password: "verified-password", TOTPSecret: "verified-secret"}, clear, clear); err != nil {
			t.Fatal(err)
		}
		stored, err := vault.Load("owner@example.test")
		if err != nil || accepted != 1 || !stored.TOTPResetPending || stored.TOTPResetOperationID != old.TOTPResetOperationID {
			t.Fatal("credential write lost pending operation identity or publication", err)
		}
		if clear {
			if stored.PendingPassword != "" || stored.PendingTOTPSecret != "" || stored.PendingTOTPURI != "" {
				t.Fatal("explicitly committed material remained pending")
			}
		} else if stored.PendingPassword != old.PendingPassword || stored.PendingTOTPSecret != old.PendingTOTPSecret || stored.PendingTOTPURI != old.PendingTOTPURI {
			t.Fatal("unrelated credential update discarded uncommitted material")
		}
	}
}

type operationCredentialFaultStore struct {
	*credentials.MemoryStore
	saveErr error
	drop    bool
}

func (s *operationCredentialFaultStore) Save(email string, value credentials.Credentials) error {
	if s.saveErr != nil || s.drop {
		return s.saveErr
	}
	return s.MemoryStore.Save(email, value)
}

func TestOperationCredentialProtectorDoesNotPublishFailedSave(t *testing.T) {
	saveErr := errors.New("synthetic protected storage unavailable")
	for _, tc := range []struct {
		name string
		err  error
		drop bool
	}{
		{name: "write-error", err: saveErr},
		{name: "readback-mismatch", drop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := &operationCredentialFaultStore{MemoryStore: credentials.NewMemoryStore(), saveErr: tc.err, drop: tc.drop}
			old := credentials.Credentials{PendingTOTPSecret: "synthetic-pending-key"}
			if err := vault.MemoryStore.Save("owner@example.test", old); err != nil {
				t.Fatal(err)
			}
			published := false
			protect := operationCredentialProtector(t.Context(), vault, "owner@example.test", true, func(credentials.Credentials) { published = true })
			err := protect(credentials.Credentials{TOTPSecret: "synthetic-activated-key"}, true, false)
			if err == nil || published {
				t.Fatal("failed persistence published accepted credentials")
			}
			if tc.err != nil && !errors.Is(err, tc.err) || tc.drop && !strings.Contains(err.Error(), "TOTP_SETUP_PERSISTENCE_REQUIRED") {
				t.Fatalf("persistence failure classification changed: %v", err)
			}
			stored, loadErr := vault.Load("owner@example.test")
			if loadErr != nil || !reflect.DeepEqual(stored, old) {
				t.Fatal("failed save lost recovery material", loadErr)
			}
		})
	}
}
