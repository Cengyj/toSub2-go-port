//go:build windows

package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

func TestDPAPICurrentUserRoundTripOwnsReturnedMemory(t *testing.T) {
	plain := []byte("synthetic-dpapi-test-secret")
	protected, e := protectDPAPI(plain)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := append([]byte(nil), protected...)
	// Exercise further Windows allocations before using the first result. A
	// returned view into LocalFree'd storage would not remain stable.
	for i := 0; i < 128; i++ {
		if _, e := protectDPAPI(bytes.Repeat([]byte{byte(i)}, 256)); e != nil {
			t.Fatal(e)
		}
	}
	runtime.GC()
	if !bytes.Equal(protected, snapshot) {
		t.Fatal("DPAPI returned released memory")
	}
	got, e := unprotectDPAPI(protected)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatalf("DPAPI roundtrip failed: %v", e)
	}
	protected[len(protected)-1] ^= 1
	if _, e := unprotectDPAPI(protected); e == nil {
		t.Fatal("DPAPI accepted corrupt ciphertext")
	}
}
func TestDPAPIEmptyInputRejected(t *testing.T) {
	if _, e := protectDPAPI(nil); e == nil {
		t.Fatal("empty protect accepted")
	}
	if _, e := unprotectDPAPI(nil); e == nil {
		t.Fatal("empty unprotect accepted")
	}
}

// Runs DPAPI and DACL APIs on Windows, with synthetic credentials only. Merely
// cross-compiling the package does not execute this security integration test.
func TestDPAPIStoreWritesCurrentUserPrivateFile(t *testing.T) {
	s, err := NewSecureFileStore(filepath.Join(t.TempDir(), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	want := Credentials{Password: "synthetic-windows-password", TOTPSecret: "SYNTHETICTOTP", ProxyURL: "http://fixture:private-password@proxy.example"}
	if err = s.Save("acl@example.test", want); err != nil {
		t.Fatal(err)
	}
	private, err := os.Open(s.path("acl@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if err = privatefile.Verify(private); err != nil {
		t.Fatal(err)
	}
	_ = private.Close()
	b, err := os.ReadFile(s.path("acl@example.test"))
	if err != nil || bytes.Contains(b, []byte(want.Password)) || bytes.Contains(b, []byte(want.TOTPSecret)) || bytes.Contains(b, []byte("private-password")) {
		t.Fatal("plaintext credential material persisted")
	}
	got, err := s.Load("acl@example.test")
	if err != nil || got.Password != want.Password || got.TOTPSecret != want.TOTPSecret || got.ProxyURL != want.ProxyURL {
		t.Fatal("DPAPI store roundtrip failed")
	}
	if err = s.Delete("acl@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.path("acl@example.test")); !os.IsNotExist(err) {
		t.Fatal("private credential file not removed")
	}
}
