package credentials

import (
	"runtime"
	"testing"
)

func TestSecureStoreRejectsUnsupportedPlatform(t *testing.T) {
	s, err := NewSecureFileStore(t.TempDir())
	if runtime.GOOS == "linux" {
		if err == nil || s != nil {
			t.Fatalf("expected secure store refusal on Linux without system service: %#v %v", s, err)
		}
	}
}
func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc, err := encrypt(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decrypt(key, enc)
	if err != nil || string(got) != "secret" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := decrypt(key, enc+"x"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
