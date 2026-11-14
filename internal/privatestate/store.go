// Package privatestate provides encrypted, durable JSON operation evidence.
// Registries contain protection capabilities only, never plaintext or Cookies.
package privatestate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
)

const Format = "tosub2-private-state"
const Schema = 1
const MaxPlaintextBytes int64 = 1 << 20

var ErrProtectionRequired = errors.New("private state protector is required; refusing plaintext fallback")
var ErrInvalidState = errors.New("invalid private state")

// Binding is authenticated with the ciphertext. Identity is a SHA-256 digest,
// never an account email. A zero Binding denotes a legacy plaintext JSON file.
type Binding struct {
	Schema    int    `json:"schema"`
	Purpose   string `json:"purpose"`
	Identity  string `json:"identity"`
	Operation string `json:"operation"`
}

type envelope struct {
	Format string `json:"private_state"`
	Binding
	Ciphertext []byte `json:"ciphertext"`
}

type registration struct {
	protector credentials.StateProtector
	required  bool
}

type Registry struct {
	mu    sync.RWMutex
	roots map[string]registration
}

func NewRegistry() *Registry { return &Registry{roots: make(map[string]registration)} }

var defaultRegistry = NewRegistry()

// RootFor returns the configured protection root containing a state file.
// It exposes no protector or plaintext and rejects unsafe descendant paths.
func RootFor(path string) (string, error) {
	_, root, _, err := defaultRegistry.lookup(path)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", ErrProtectionRequired
	}
	return root, nil
}

func Identity(identity string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(identity))))
	return hex.EncodeToString(digest[:])
}

// Require prevents plaintext writes below root even when registration fails.
// Production configures this before restoring jobs or constructing a runner.
func Require(root string) error { return defaultRegistry.Require(root) }
func Register(root string, protector credentials.StateProtector) error {
	return defaultRegistry.Register(root, protector)
}

func (r *Registry) Require(root string) error {
	root, err := rootPath(root)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roots == nil {
		r.roots = make(map[string]registration)
	}
	entry := r.roots[root]
	entry.required = true
	r.roots[root] = entry
	return nil
}

// Register is safe to repeat for a root and does no keychain I/O. A subsequent
// registration replaces that root's capability for future operations. Require
// remains sticky. A nil or typed-nil protector is rejected without panicking.
func (r *Registry) Register(root string, protector credentials.StateProtector) error {
	if protector == nil {
		return ErrProtectionRequired
	}
	value := reflect.ValueOf(protector)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Map || value.Kind() == reflect.Func || value.Kind() == reflect.Interface || value.Kind() == reflect.Slice) && value.IsNil() {
		return ErrProtectionRequired
	}
	root, err := rootPath(root)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roots == nil {
		r.roots = make(map[string]registration)
	}
	entry := r.roots[root]
	entry.protector = protector
	entry.required = true
	r.roots[root] = entry
	return nil
}

func rootPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("private state root is required")
	}
	path, err := filepath.Abs(path)
	if err != nil || filepath.Dir(path) == path {
		return "", errors.New("invalid private state root")
	}
	return filepath.Clean(path), nil
}

func (r *Registry) lookup(path string) (string, string, registration, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", registration{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var root string
	var entry registration
	for candidate, configured := range r.roots {
		rel, err := filepath.Rel(candidate, absolute)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) && len(candidate) > len(root) {
			root, entry = candidate, configured
		}
	}
	if root != "" {
		if err := checkPath(root, absolute); err != nil {
			return "", "", registration{}, err
		}
	}
	return absolute, root, entry, nil
}

// Reject symlinks/reparse points at the root and every existing descendant.
// The file reader additionally verifies that its open handle matches Lstat.
func checkPath(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return fmt.Errorf("%w: a file below the root is required", ErrInvalidState)
	}
	current := root
	parts := append([]string{""}, strings.Split(relative, string(filepath.Separator))...)
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (current != path && !info.IsDir()) {
			return fmt.Errorf("%w: private state path is not a real file hierarchy", ErrInvalidState)
		}
	}
	return nil
}

func authenticatedData(binding Binding, root, path string) []byte {
	relative, _ := filepath.Rel(root, path)
	data, _ := json.Marshal(struct {
		Format string `json:"format"`
		Binding
		Path string `json:"path"`
	}{Format: Format, Binding: binding, Path: filepath.ToSlash(relative)})
	return data
}

func WriteJSON(ctx context.Context, path, purpose, identity, operation string, value any) error {
	return defaultRegistry.WriteJSON(ctx, path, purpose, identity, operation, value)
}

func (r *Registry) WriteJSON(ctx context.Context, path, purpose, identity, operation string, value any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, root, entry, err := r.lookup(path)
	if err != nil {
		return err
	}
	if entry.protector == nil && entry.required {
		return ErrProtectionRequired
	}
	plain, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("private state JSON encoding failed")
	}
	if int64(len(plain)) > MaxPlaintextBytes {
		return fmt.Errorf("%w: state exceeds maximum size", ErrInvalidState)
	}
	if entry.protector == nil {
		// Compatibility helper for preexisting tests/nonproduction callers.
		// Production roots must call Require before they can reach this path.
		return privatefile.Write(ctx, path, append(plain, '\n'))
	}
	if strings.TrimSpace(purpose) == "" || strings.TrimSpace(identity) == "" || strings.TrimSpace(operation) == "" {
		return fmt.Errorf("%w: purpose, identity and operation are required", ErrInvalidState)
	}
	binding := Binding{Schema: Schema, Purpose: purpose, Identity: Identity(identity), Operation: operation}
	// Never overwrite unreadable sealed evidence (including a missing key).
	// Legacy plaintext is validated and can migrate on the next normal write.
	_, old, err := r.ReadJSONWithBinding(ctx, path, purpose, MaxPlaintextBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && old.Schema != 0 && old.Identity != binding.Identity {
		return fmt.Errorf("%w: existing state identity differs", ErrInvalidState)
	}
	ciphertext, err := entry.protector.SealContext(ctx, binding.Identity, plain, authenticatedData(binding, root, path))
	if err != nil {
		return err
	}
	sealed, err := json.MarshalIndent(envelope{Format: Format, Binding: binding, Ciphertext: ciphertext}, "", "  ")
	if err != nil {
		return errors.New("private state envelope encoding failed")
	}
	if err := checkPath(root, path); err != nil {
		return err
	}
	return privatefile.Write(ctx, path, append(sealed, '\n'))
}

func ReadJSON(ctx context.Context, path, purpose string, maxBytes int64) ([]byte, error) {
	return defaultRegistry.ReadJSON(ctx, path, purpose, maxBytes)
}

func (r *Registry) ReadJSON(ctx context.Context, path, purpose string, maxBytes int64) ([]byte, error) {
	data, _, err := r.ReadJSONWithBinding(ctx, path, purpose, maxBytes)
	return data, err
}

func ReadJSONWithBinding(ctx context.Context, path, purpose string, maxBytes int64) ([]byte, Binding, error) {
	return defaultRegistry.ReadJSONWithBinding(ctx, path, purpose, maxBytes)
}

func (r *Registry) ReadJSONWithBinding(ctx context.Context, path, purpose string, maxBytes int64) ([]byte, Binding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, Binding{}, err
	}
	path, root, entry, err := r.lookup(path)
	if err != nil {
		return nil, Binding{}, err
	}
	if maxBytes <= 0 || maxBytes > MaxPlaintextBytes {
		maxBytes = MaxPlaintextBytes
	}
	// GCM and two base64 layers need more room than the decoded JSON limit.
	data, err := readPrivate(path, maxBytes*2+16384)
	if err != nil {
		return nil, Binding{}, err
	}
	var marker map[string]json.RawMessage
	if json.Unmarshal(data, &marker) != nil || marker == nil {
		return nil, Binding{}, fmt.Errorf("%w: malformed JSON object", ErrInvalidState)
	}
	_, sealed := marker["private_state"]
	if !sealed {
		// Broken envelope metadata must never downgrade to a legacy payload.
		if _, suspicious := marker["ciphertext"]; suspicious {
			return nil, Binding{}, fmt.Errorf("%w: missing envelope marker", ErrInvalidState)
		}
		if int64(len(data)) > maxBytes {
			return nil, Binding{}, fmt.Errorf("%w: legacy state exceeds maximum size", ErrInvalidState)
		}
		return data, Binding{}, nil
	}
	var decoded envelope
	if json.Unmarshal(data, &decoded) != nil || decoded.Format != Format || decoded.Schema != Schema || decoded.Purpose != purpose || len(decoded.Identity) != 64 || decoded.Operation == "" || len(decoded.Ciphertext) == 0 {
		return nil, Binding{}, fmt.Errorf("%w: invalid envelope binding", ErrInvalidState)
	}
	if _, err := hex.DecodeString(decoded.Identity); err != nil {
		return nil, Binding{}, fmt.Errorf("%w: invalid identity digest", ErrInvalidState)
	}
	if entry.protector == nil {
		return nil, Binding{}, ErrProtectionRequired
	}
	plain, err := entry.protector.OpenContext(ctx, decoded.Identity, decoded.Ciphertext, authenticatedData(decoded.Binding, root, path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Binding{}, fmt.Errorf("%w: key unavailable for existing state", ErrProtectionRequired)
		}
		return nil, Binding{}, err
	}
	if int64(len(plain)) > maxBytes || !json.Valid(plain) {
		return nil, Binding{}, fmt.Errorf("%w: invalid decrypted JSON", ErrInvalidState)
	}
	return plain, decoded.Binding, nil
}

func readPrivate(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: state must be a regular private file", ErrInvalidState)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: state changed during read", ErrInvalidState)
	}
	if err := privatefile.Verify(file); err != nil {
		return nil, err
	}
	if opened.Size() > maxBytes {
		return nil, fmt.Errorf("%w: state exceeds maximum size", ErrInvalidState)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: state exceeds maximum size", ErrInvalidState)
	}
	return data, nil
}
