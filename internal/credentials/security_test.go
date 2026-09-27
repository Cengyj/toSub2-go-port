package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
)

type memoryKeychain struct {
	values      map[string]string
	unavailable bool
	writes      int
	mu          sync.Mutex
}

func (m *memoryKeychain) run(ctx context.Context, args []string, input string) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return nil, ErrUnavailable
	}
	var svc string
	for i, x := range args {
		if x == "-s" && i+1 < len(args) {
			svc = args[i+1]
		}
	}
	switch args[0] {
	case "find-generic-password":
		if v, ok := m.values[svc]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	case "add-generic-password":
		m.writes++
		if len(args) == 0 || args[len(args)-1] != "-w" {
			return nil, errors.New("key must be read from stdin")
		}
		m.values[svc] = strings.Split(input, "\n")[0]
		return nil, nil
	case "delete-generic-password":
		delete(m.values, svc)
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}
func testMacStore(t *testing.T, m *memoryKeychain) *SecureFileStore {
	t.Helper()
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", filepath.Join(t.TempDir(), "legacy"))
	return &SecureFileStore{Root: t.TempDir(), platform: "darwin", run: m.run}
}
func writeEncrypted(t *testing.T, path string, key []byte) {
	t.Helper()
	enc, e := encryptCredentialEnvelope(key, []byte(`{"password":"old-secret","totpSecret":"old-totp"}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte(enc), 0600); e != nil {
		t.Fatal(e)
	}
}

func TestKeychainLockedOrMissingNeverReplacesKeyOrCiphertext(t *testing.T) {
	for _, locked := range []bool{true, false} {
		t.Run(map[bool]string{true: "locked", false: "missing"}[locked], func(t *testing.T) {
			m := &memoryKeychain{values: map[string]string{}, unavailable: locked}
			s := testMacStore(t, m)
			writeEncrypted(t, s.path("a@example.com"), bytes.Repeat([]byte{3}, 32))
			before, _ := os.ReadFile(s.path("a@example.com"))
			if e := s.Save("a@example.com", Credentials{Password: "new-secret"}); e == nil {
				t.Fatal("save should fail")
			}
			after, _ := os.ReadFile(s.path("a@example.com"))
			if !bytes.Equal(before, after) || m.writes != 0 {
				t.Fatal("existing key or ciphertext replaced")
			}
			if _, e := s.Load("a@example.com"); e == nil || errors.Is(e, os.ErrNotExist) {
				t.Fatalf("must report unavailable key, not absent account: %v", e)
			}
		})
	}
}
func TestKeychainInvalidKeyAndWrongKeyPreserveData(t *testing.T) {
	for _, value := range []string{"damaged", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))} {
		m := &memoryKeychain{values: map[string]string{macKeychainService: value}}
		s := testMacStore(t, m)
		writeEncrypted(t, s.path("a@example.com"), bytes.Repeat([]byte{3}, 32))
		before, _ := os.ReadFile(s.path("a@example.com"))
		if e := s.Save("a@example.com", Credentials{}); e == nil {
			t.Fatal("save should fail")
		}
		after, _ := os.ReadFile(s.path("a@example.com"))
		if !bytes.Equal(before, after) || m.writes != 0 {
			t.Fatal("key/ciphertext replaced")
		}
	}
}
func TestKeychainLegacyServiceAndEnvelopePath(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	m := &memoryKeychain{values: map[string]string{macLegacyKeyService: base64.StdEncoding.EncodeToString(key)}}
	s := testMacStore(t, m)
	writeEncrypted(t, s.legacyPath("Test@Example.com"), key)
	c, e := s.Load("test@example.com")
	if e != nil || c.Password != "old-secret" {
		t.Fatalf("legacy load: %#v %v", c, e)
	}
	if e = s.Save("test@example.com", Credentials{Password: "updated"}); e != nil {
		t.Fatal(e)
	}
	if m.writes != 0 {
		t.Fatal("legacy key unexpectedly replaced")
	}
	c, e = s.Load("test@example.com")
	if e != nil || c.Password != "updated" {
		t.Fatal("updated credential not restored")
	}
}
func TestKeychainDirectJSONMigrationAndNoPlaintext(t *testing.T) {
	m := &memoryKeychain{values: map[string]string{macKeychainService: `{"password":"keep-secret","totpSecret":"keep-totp"}`}}
	s := testMacStore(t, m)
	value, e := s.Load("a@example.com")
	if e != nil {
		t.Fatal(e)
	}
	value.ProxyURL = "socks5://user:proxy-secret@host:1080"
	if e := s.Save("a@example.com", value); e != nil {
		t.Fatal(e)
	}
	c, e := s.Load("a@example.com")
	if e != nil || c.Password != "keep-secret" || c.TOTPSecret != "keep-totp" {
		t.Fatalf("migration: %#v %v", c, e)
	}
	b, _ := os.ReadFile(s.path("a@example.com"))
	for _, secret := range []string{"keep-secret", "keep-totp", "proxy-secret"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("plaintext persisted")
		}
	}
	private, err := os.Open(s.path("a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if err = privatefile.Verify(private); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyKeychainMigrationHonorsExplicitClears(t *testing.T) {
	m := &memoryKeychain{values: map[string]string{macKeychainService: `{"password":"clear-this","totpSecret":"keep-this","proxyUrl":"http://clear:secret@proxy.example"}`}}
	s := testMacStore(t, m)
	value, err := s.Load("clear@example.com")
	if err != nil {
		t.Fatal(err)
	}
	value.Password, value.ProxyURL = "", ""
	if err := s.Save("clear@example.com", value); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("clear@example.com")
	if err != nil || got.Password != "" || got.ProxyURL != "" || got.TOTPSecret != "keep-this" {
		t.Fatal("legacy migration restored explicitly cleared credentials")
	}
}
func TestKeychainConcurrentCreationOneKey(t *testing.T) {
	m := &memoryKeychain{values: map[string]string{}}
	s := testMacStore(t, m)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Save("a@example.com", Credentials{Password: "secret"}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if m.writes != 1 {
		t.Fatalf("created %d keys", m.writes)
	}
}
func TestCredentialContextCancelsWhileWaitingAndRunning(t *testing.T) {
	s := &SecureFileStore{Root: t.TempDir(), platform: "darwin", run: func(ctx context.Context, _ []string, _ string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := s.LoadContext(ctx, "a@example.com"); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("got %v", e)
	}
	if e := lockCredentialStore(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer unlockCredentialStore()
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if e := s.SaveContext(ctx2, "a@example.com", Credentials{}); !errors.Is(e, context.Canceled) {
		t.Fatalf("got %v", e)
	}
}
func TestLegacyWindowsDPAPIPaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	s := &SecureFileStore{Root: t.TempDir(), platform: "windows", unprotect: func(b []byte) ([]byte, error) {
		if string(b) != "protected" {
			return nil, errors.New("bad payload")
		}
		return []byte(`{"password":"restored"}`), nil
	}}
	p := filepath.Join(root, "toSub2", "credentials", credentialID("A@example.com")+".dpapi")
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString([]byte("protected"))), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := s.Load("a@example.com")
	if e != nil || c.Password != "restored" {
		t.Fatalf("legacy DPAPI: %#v %v", c, e)
	}
}
func TestEnvelopeRejectsInvalidLengthsAndVersion(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	for _, env := range []credentialEnvelope{{Version: 2, IV: "AA==", Tag: "AA=="}, {Version: 1, IV: "AA==", Tag: base64.StdEncoding.EncodeToString(make([]byte, 16))}, {Version: 1, IV: base64.StdEncoding.EncodeToString(make([]byte, 12)), Tag: "AA=="}} {
		b, _ := json.Marshal(env)
		if _, e := decryptCredentialEnvelope(key, string(b)); e == nil {
			t.Fatalf("accepted %s", b)
		}
	}
}
func TestLinuxRefusesPersistence(t *testing.T) {
	s := &SecureFileStore{Root: t.TempDir(), platform: "linux"}
	if e := s.Save("a@example.com", Credentials{Password: "secret"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	files, _ := os.ReadDir(s.Root)
	if len(files) != 0 {
		t.Fatal("files persisted on unsupported system")
	}
}
