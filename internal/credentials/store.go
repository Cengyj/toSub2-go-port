// Package credentials persists secrets only through an OS-protected key.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tosub2-go-port/internal/privatefile"
)

const macKeychainService = "com.local.chatgpt-onboarding.credentials"
const macLegacyKeyService = "toSub2-go-key"
const macLegacyRootName = "Library/Application Support/toSub2/credentials"
const macIsolatedServicePrefix = macKeychainService + ".local."
const macIsolatedServiceSuffixLimit = 64

var ErrUnavailable = errors.New("secure credential service unavailable; refusing plaintext fallback")
var credentialAccess = make(chan struct{}, 1)

func lockCredentialStore(ctx context.Context) error {
	select {
	case credentialAccess <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func unlockCredentialStore() { <-credentialAccess }

type Credentials struct {
	PendingPassword   string            `json:"pendingPassword,omitempty"`
	PendingTOTPSecret string            `json:"pendingTotpSecret,omitempty"`
	PendingTOTPURI    string            `json:"pendingTotpUri,omitempty"`
	Password          string            `json:"password,omitempty"`
	TOTPSecret        string            `json:"totpSecret,omitempty"`
	ProxyURL          string            `json:"proxyUrl,omitempty"`
	MailAPIURL        string            `json:"mailApiUrl,omitempty"`
	MailMethod        string            `json:"mailMethod,omitempty"`
	MailBody          string            `json:"mailBody,omitempty"`
	MailHeaders       map[string]string `json:"mailHeaders,omitempty"`
}
type Store interface {
	Save(string, Credentials) error
	Load(string) (Credentials, error)
	Delete(string) error
}
type ContextStore interface {
	SaveContext(context.Context, string, Credentials) error
	LoadContext(context.Context, string) (Credentials, error)
	DeleteContext(context.Context, string) error
}
type securityRunner func(context.Context, []string, string) ([]byte, error)
type SecureFileStore struct {
	Root         string
	fallbackRoot string
	platform     string
	// A nonempty service is frozen at construction and denotes a private
	// runtime namespace. Zero-value test stores retain legacy compatibility.
	keychainService string
	run             securityRunner
	protect         func([]byte) ([]byte, error)
	unprotect       func([]byte) ([]byte, error)
}

func NewSecureFileStore(root string) (*SecureFileStore, error) {
	return newSecureFileStore(root, runtime.GOOS)
}

func newSecureFileStore(root, platform string) (*SecureFileStore, error) {
	if root == "" {
		return nil, errors.New("credential root is required")
	}
	if platform != "darwin" && platform != "windows" {
		return nil, ErrUnavailable
	}
	fallbackRoot := ""
	keychainService := ""
	if platform == "darwin" {
		keychainService = os.Getenv("TOSUB2_MAC_KEYCHAIN_SERVICE")
		if err := validateMacKeychainService(keychainService); err != nil {
			return nil, err
		}
		if configured := strings.TrimSpace(os.Getenv("TOSUB2_MAC_CREDENTIAL_ROOT")); configured != "" && filepath.Clean(configured) != filepath.Clean(root) {
			// Preserve the legacy environment variable for both reads and writes.
			// Earlier Go builds wrote below OutputRoot; retain that read fallback.
			fallbackRoot, root = root, configured
		}
		if keychainService != "" {
			fallbackRoot = ""
		}
	}
	return &SecureFileStore{Root: root, fallbackRoot: fallbackRoot, platform: platform, keychainService: keychainService, run: runSecurity, protect: protectDPAPI, unprotect: unprotectDPAPI}, nil
}

func validateMacKeychainService(service string) error {
	if service == "" {
		return nil
	}
	suffix, ok := strings.CutPrefix(service, macIsolatedServicePrefix)
	if !ok || len(suffix) == 0 || len(suffix) > macIsolatedServiceSuffixLimit {
		return errors.New("TOSUB2_MAC_KEYCHAIN_SERVICE must use the local runtime prefix and a 1-64 character ASCII alphanumeric, dot, underscore or hyphen suffix")
	}
	for _, c := range suffix {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			continue
		}
		return errors.New("TOSUB2_MAC_KEYCHAIN_SERVICE must use the local runtime prefix and a 1-64 character ASCII alphanumeric, dot, underscore or hyphen suffix")
	}
	return nil
}

func (s *SecureFileStore) keychainServices() []string {
	if s.keychainService != "" {
		return []string{s.keychainService}
	}
	return []string{macKeychainService, macLegacyKeyService}
}
func runSecurity(ctx context.Context, args []string, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "security", args...)
	cmd.Stdin = strings.NewReader(input)
	b, e := cmd.Output()
	if e == nil {
		return b, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(e, &ee) && ee.ExitCode() == 44 {
		return nil, os.ErrNotExist
	}
	return nil, ErrUnavailable
}
func normalizedEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
func credentialID(email string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(normalizedEmail(email))))
}
func (s *SecureFileStore) path(email string) string {
	return filepath.Join(s.Root, credentialID(email)+".cred")
}
func (s *SecureFileStore) legacyPath(email string) string {
	home, e := os.UserHomeDir()
	if e != nil {
		return ""
	}
	root := os.Getenv("TOSUB2_MAC_CREDENTIAL_ROOT")
	if root == "" {
		root = filepath.Join(home, macLegacyRootName)
	}
	return filepath.Join(root, credentialID(email)+".enc")
}
func (s *SecureFileStore) paths(email string) []string {
	out := []string{s.path(email)}
	if s.platform == "darwin" && s.keychainService != "" {
		// An isolated runtime never reads or deletes legacy files, including
		// an .enc file or another output root using the same account email.
		return out
	}
	if s.platform == "darwin" {
		if s.fallbackRoot != "" {
			out = append(out, filepath.Join(s.fallbackRoot, credentialID(email)+".cred"), filepath.Join(s.fallbackRoot, credentialID(email)+".enc"))
		}
		out = append(out, filepath.Join(s.Root, credentialID(email)+".enc"), s.legacyPath(email))
	}
	if s.platform == "windows" {
		root := os.Getenv("LOCALAPPDATA")
		if root == "" {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, "AppData", "Local")
		}
		out = append(out, filepath.Join(s.Root, credentialID(email)+".dpapi"), filepath.Join(root, "toSub2", "credentials", credentialID(email)+".dpapi"))
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range out {
		if p != "" && !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	return unique
}
func (s *SecureFileStore) Save(email string, c Credentials) error {
	return s.SaveContext(context.Background(), email, c)
}
func (s *SecureFileStore) Load(email string) (Credentials, error) {
	return s.LoadContext(context.Background(), email)
}
func (s *SecureFileStore) Delete(email string) error {
	return s.DeleteContext(context.Background(), email)
}
func boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, 15*time.Second)
}
func (s *SecureFileStore) SaveContext(ctx context.Context, email string, c Credentials) error {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if e := lockCredentialStore(ctx); e != nil {
		return e
	}
	defer unlockCredentialStore()
	if e := ctx.Err(); e != nil {
		return e
	}
	if normalizedEmail(email) == "" {
		return errors.New("credential email is required")
	}
	switch s.platform {
	case "darwin":
		return s.keychainSaveContext(ctx, email, c)
	case "windows":
		return s.dpapiSaveContext(ctx, email, c)
	default:
		return ErrUnavailable
	}
}
func (s *SecureFileStore) LoadContext(ctx context.Context, email string) (Credentials, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if e := lockCredentialStore(ctx); e != nil {
		return Credentials{}, e
	}
	defer unlockCredentialStore()
	if e := ctx.Err(); e != nil {
		return Credentials{}, e
	}
	switch s.platform {
	case "darwin":
		return s.keychainLoadContext(ctx, email)
	case "windows":
		return s.dpapiLoadContext(ctx, email)
	default:
		return Credentials{}, ErrUnavailable
	}
}
func (s *SecureFileStore) DeleteContext(ctx context.Context, email string) error {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if e := lockCredentialStore(ctx); e != nil {
		return e
	}
	defer unlockCredentialStore()
	if e := ctx.Err(); e != nil {
		return e
	}
	if s.platform != "darwin" && s.platform != "windows" {
		return ErrUnavailable
	}
	if s.platform == "darwin" {
		for _, svc := range s.keychainServices() {
			_, e := s.runner()(ctx, []string{"delete-generic-password", "-a", normalizedEmail(email), "-s", svc}, "")
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return fmt.Errorf("delete macOS credential key: %w", e)
			}
		}
	}
	for _, p := range s.paths(email) {
		if e := os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
			return errors.New("could not delete encrypted credential file")
		}
	}
	return nil
}
func (s *SecureFileStore) runner() securityRunner {
	if s.run != nil {
		return s.run
	}
	return runSecurity
}

type keychainValue struct {
	key    []byte
	direct *Credentials
}

func (s *SecureFileStore) readKeychain(ctx context.Context, email string) (keychainValue, error) {
	var direct *Credentials
	var failure error
	for _, svc := range s.keychainServices() {
		raw, e := s.runner()(ctx, []string{"find-generic-password", "-a", normalizedEmail(email), "-s", svc, "-w"}, "")
		if e != nil {
			if !errors.Is(e, os.ErrNotExist) {
				failure = e
			}
			continue
		}
		v := strings.TrimSpace(string(raw))
		if key, e := base64.StdEncoding.DecodeString(v); e == nil && len(key) == 32 {
			return keychainValue{key: key}, nil
		}
		if key, e := hex.DecodeString(v); e == nil && len(key) == 32 {
			return keychainValue{key: key}, nil
		}
		var c Credentials
		if strings.HasPrefix(v, "{") && json.Unmarshal([]byte(v), &c) == nil {
			direct = &c
		} else if recovered, ok := recoverTruncatedLegacyCredentials(v); ok {
			direct = &recovered
		} else {
			failure = errors.New("invalid macOS credential key")
		}
	}
	if direct != nil {
		return keychainValue{direct: direct}, nil
	}
	if failure != nil {
		return keychainValue{}, failure
	}
	return keychainValue{}, os.ErrNotExist
}
func (s *SecureFileStore) existingFile(email string) (string, error) {
	for _, p := range s.paths(email) {
		_, e := os.Stat(p)
		if e == nil {
			return p, nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", errors.New("could not inspect encrypted credential file")
		}
	}
	return "", nil
}
func (s *SecureFileStore) keychainSaveContext(ctx context.Context, email string, c Credentials) error {
	file, e := s.existingFile(email)
	if e != nil {
		return e
	}
	kv, e := s.readKeychain(ctx, email)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("cannot read macOS credential key; existing data preserved: %w", e)
	}
	if len(kv.key) == 0 && file != "" {
		return errors.New("macOS credential key unavailable; existing encrypted file preserved")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return errors.New("could not read existing encrypted credentials; original file preserved")
		}
		if _, err = decryptCredentialEnvelope(kv.key, strings.TrimSpace(string(b))); err != nil {
			return errors.New("existing credentials cannot be decrypted; original key and file preserved")
		}
	}
	if len(kv.key) == 0 {
		// Save always replaces the full value, including explicit empty fields.
		// Callers load and merge omitted inputs before saving; merging again here
		// would resurrect a password or proxy that the user deliberately cleared
		// when an old direct-Keychain record is migrated to AES-GCM.
		kv.key = make([]byte, 32)
		if _, e = rand.Read(kv.key); e != nil {
			return errors.New("credential key generation failed")
		}
		encoded := base64.StdEncoding.EncodeToString(kv.key)
		// Supply key on stdin, never in the process argument list.
		args := []string{"add-generic-password", "-a", normalizedEmail(email), "-s", s.keychainServices()[0]}
		if kv.direct != nil {
			args = append(args, "-U")
		}
		args = append(args, "-w")
		_, e = s.runner()(ctx, args, encoded+"\n"+encoded+"\n")
		if e != nil {
			return fmt.Errorf("cannot save macOS credential key: %w", e)
		}
	}
	plain, e := json.Marshal(c)
	if e != nil {
		return errors.New("invalid credentials")
	}
	enc, e := encryptCredentialEnvelope(kv.key, plain)
	if e != nil {
		return errors.New("credential encryption failed")
	}
	return writePrivate(ctx, s.path(email), []byte(enc+"\n"))
}
func (s *SecureFileStore) keychainLoadContext(ctx context.Context, email string) (Credentials, error) {
	file, e := s.existingFile(email)
	if e != nil {
		return Credentials{}, e
	}
	kv, e := s.readKeychain(ctx, email)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) && file != "" {
			return Credentials{}, errors.New("macOS credential key missing for encrypted file")
		}
		return Credentials{}, e
	}
	if file == "" {
		if kv.direct != nil {
			return *kv.direct, nil
		}
		return Credentials{}, os.ErrNotExist
	}
	if len(kv.key) != 32 {
		return Credentials{}, errors.New("macOS encrypted credential key unavailable")
	}
	b, e := os.ReadFile(file)
	if e != nil {
		return Credentials{}, errors.New("could not read encrypted credentials")
	}
	plain, e := decryptCredentialEnvelope(kv.key, strings.TrimSpace(string(b)))
	if e != nil {
		return Credentials{}, errors.New("credential decryption failed; original file preserved")
	}
	return decodeCredentials(plain)
}
func (s *SecureFileStore) dpapiSaveContext(ctx context.Context, email string, c Credentials) error {
	b, e := json.Marshal(c)
	if e != nil {
		return errors.New("invalid credentials")
	}
	protect := s.protect
	if protect == nil {
		protect = protectDPAPI
	}
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		return errors.New("credential encryption key generation failed")
	}
	defer clear(key)
	encrypted, e := encryptCredentialEnvelope(key, b)
	if e != nil {
		return errors.New("credential AES-256-GCM encryption failed")
	}
	protected, e := protect(key)
	if e != nil {
		return errors.New("Windows DPAPI CurrentUser protection failed")
	}
	if len(protected) == 0 {
		return errors.New("Windows DPAPI returned an empty protected key")
	}
	envelope, e := json.Marshal(dpapiCredentialEnvelope{Version: 1, Type: "dpapi-current-user+aes-256-gcm", ProtectedKey: base64.StdEncoding.EncodeToString(protected), Payload: json.RawMessage(encrypted)})
	if e != nil {
		return errors.New("invalid protected credential envelope")
	}
	return writePrivate(ctx, s.path(email), append(envelope, '\n'))
}

type dpapiCredentialEnvelope struct {
	Version      int             `json:"version"`
	Type         string          `json:"type"`
	ProtectedKey string          `json:"protected_key"`
	Payload      json.RawMessage `json:"payload"`
}

func (s *SecureFileStore) dpapiLoadContext(ctx context.Context, email string) (Credentials, error) {
	file, e := s.existingFile(email)
	if e != nil {
		return Credentials{}, e
	}
	if file == "" {
		return Credentials{}, os.ErrNotExist
	}
	b, e := os.ReadFile(file)
	if e != nil {
		return Credentials{}, errors.New("could not read DPAPI credentials")
	}
	if e = ctx.Err(); e != nil {
		return Credentials{}, e
	}
	unprotect := s.unprotect
	if unprotect == nil {
		unprotect = unprotectDPAPI
	}
	encoded := strings.TrimSpace(string(b))
	if strings.HasPrefix(encoded, "{") {
		var envelope dpapiCredentialEnvelope
		if json.Unmarshal(b, &envelope) != nil || envelope.Version != 1 || envelope.Type != "dpapi-current-user+aes-256-gcm" || len(envelope.Payload) == 0 {
			return Credentials{}, errors.New("invalid DPAPI AES-256-GCM envelope")
		}
		wrapped, err := base64.StdEncoding.DecodeString(envelope.ProtectedKey)
		if err != nil || len(wrapped) == 0 {
			return Credentials{}, errors.New("invalid DPAPI protected key")
		}
		key, err := unprotect(wrapped)
		defer clear(key)
		// CryptUnprotectData is synchronous. If cancellation happens inside
		// the OS call, discard its result as soon as control returns.
		if ctx.Err() != nil {
			return Credentials{}, ctx.Err()
		}
		if err != nil || len(key) != 32 {
			return Credentials{}, errors.New("Windows DPAPI CurrentUser key decryption failed")
		}
		plain, err := decryptCredentialEnvelope(key, string(envelope.Payload))
		defer clear(plain)
		if ctx.Err() != nil {
			return Credentials{}, ctx.Err()
		}
		if err != nil {
			return Credentials{}, errors.New("credential AES-256-GCM decryption failed; original file preserved")
		}
		return decodeCredentials(plain)
	}
	// Existing Node/early-Go files are base64 DPAPI blobs containing the JSON
	// credentials directly. Read them without rewriting until the next save.
	ciphertext, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil {
		return Credentials{}, errors.New("invalid DPAPI credential envelope")
	}
	plain, e := unprotect(ciphertext)
	defer clear(plain)
	if ctx.Err() != nil {
		return Credentials{}, ctx.Err()
	}
	if e != nil {
		return Credentials{}, errors.New("Windows DPAPI CurrentUser decryption failed")
	}
	return decodeCredentials(plain)
}
func decodeCredentials(plain []byte) (Credentials, error) {
	var c Credentials
	if e := json.Unmarshal(plain, &c); e != nil {
		return Credentials{}, errors.New("invalid decrypted credential data")
	}
	return c, nil
}
func writePrivate(ctx context.Context, file string, data []byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := privatefile.EnsureDir(filepath.Dir(file)); e != nil {
		return fmt.Errorf("could not establish private credential directory: %w", e)
	}
	f, e := privatefile.CreateTemp(filepath.Dir(file), ".credential-")
	if e != nil {
		return fmt.Errorf("could not establish private credential file: %w", e)
	}
	name := f.Name()
	defer os.Remove(name)
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e == nil {
		e = ctx.Err()
	}
	if e != nil {
		return errors.New("could not write encrypted credentials")
	}
	if e = os.Rename(name, file); e != nil {
		return errors.New("could not replace encrypted credential file")
	}
	return nil
}

func encrypt(key, plain []byte) (string, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, g.NonceSize())
	if _, e = io.ReadFull(rand.Reader, nonce); e != nil {
		return "", e
	}
	out := append(nonce, g.Seal(nil, nonce, plain, nil)...)
	return base64.RawStdEncoding.EncodeToString(out), nil
}
func decrypt(key []byte, s string) ([]byte, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	b, e := base64.RawStdEncoding.DecodeString(s)
	if e != nil || len(b) < g.NonceSize() {
		return nil, errors.New("invalid encrypted credentials")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
}

type credentialEnvelope struct {
	Version int    `json:"version"`
	IV      string `json:"iv"`
	Tag     string `json:"tag"`
	Data    string `json:"data"`
}

func encryptCredentialEnvelope(key, plain []byte) (string, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	iv := make([]byte, g.NonceSize())
	if _, e = io.ReadFull(rand.Reader, iv); e != nil {
		return "", e
	}
	ciphertext := g.Seal(nil, iv, plain, nil)
	b, e := json.Marshal(credentialEnvelope{Version: 1, IV: base64.StdEncoding.EncodeToString(iv), Tag: base64.StdEncoding.EncodeToString(ciphertext[len(ciphertext)-g.Overhead():]), Data: base64.StdEncoding.EncodeToString(ciphertext[:len(ciphertext)-g.Overhead()])})
	return string(b), e
}
func decryptCredentialEnvelope(key []byte, raw string) ([]byte, error) {
	var env credentialEnvelope
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		if json.Unmarshal([]byte(raw), &env) != nil || env.Version != 1 {
			return nil, errors.New("invalid encrypted credential envelope")
		}
		block, e := aes.NewCipher(key)
		if e != nil {
			return nil, e
		}
		g, e := cipher.NewGCM(block)
		if e != nil {
			return nil, e
		}
		iv, e := base64.StdEncoding.DecodeString(env.IV)
		if e != nil {
			return nil, e
		}
		tag, e := base64.StdEncoding.DecodeString(env.Tag)
		if e != nil {
			return nil, e
		}
		data, e := base64.StdEncoding.DecodeString(env.Data)
		if e != nil {
			return nil, e
		}
		if len(iv) != g.NonceSize() || len(tag) != g.Overhead() {
			return nil, errors.New("invalid encrypted credential nonce or tag length")
		}
		return g.Open(nil, iv, append(data, tag...), nil)
	}
	return decrypt(key, raw)
}
