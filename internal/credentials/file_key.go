package credentials

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/privatefile"
)

const fileKeyMarker = ".file-key.json"
const fileKeyLock = ".file-key.lock"
const fileKeyCredentialType = "file-key-credentials+aes-256-gcm"
const fileKeyStateType = "file-key-recovery+aes-256-gcm"

type fileKeyReference struct {
	Version int    `json:"version"`
	SHA256  string `json:"key_sha256"`
}

func fileKeyError(message string) error {
	return fmt.Errorf("TOSUB2_CREDENTIAL_KEY_FILE: %s: %w", message, ErrUnavailable)
}

func newFileKeyStore(root, keyPath string) (*SecureFileStore, error) {
	if strings.TrimSpace(keyPath) == "" {
		// A configured data root must never silently become an ephemeral vault
		// when its environment or key mount disappears after a deployment.
		empty, err := fileKeyRootEmpty(root)
		if err != nil || !empty {
			return nil, fileKeyError("key file is required for the existing credential directory")
		}
		return nil, ErrUnavailable
	}
	keyPath, err := filepath.Abs(keyPath)
	if err != nil {
		return nil, fileKeyError("key file path is invalid")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fileKeyError("credential directory is invalid")
	}
	if within, err := filepath.Rel(root, keyPath); err == nil && within != ".." && !strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return nil, fileKeyError("key file must be outside the credential directory")
	}
	s := &SecureFileStore{Root: root, platform: "linux", fileKeyPath: keyPath}
	ctx, cancel := boundedContext(context.Background())
	defer cancel()
	err = withFileKeyRootLock(ctx, root, func() error {
		key, err := readFileKey(keyPath)
		if err != nil {
			return err
		}
		defer clear(key)
		s.fileKeyDigest = sha256.Sum256(key)
		return s.bindFileKey(ctx)
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// fileKeyRootEmpty conservatively treats unknown files as protected data.
// The lock alone is safe to retain after an interrupted first initialization.
func fileKeyRootEmpty(root string) (bool, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("credential directory must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != fileKeyLock {
			return false, nil
		}
	}
	return true, nil
}

func (s *SecureFileStore) bindFileKey(ctx context.Context) error {
	marker := filepath.Join(s.Root, fileKeyMarker)
	_, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		empty, rootErr := fileKeyRootEmpty(s.Root)
		if rootErr != nil || !empty {
			return fileKeyError("key reference is missing; existing credential files are preserved")
		}
		data, _ := json.Marshal(fileKeyReference{Version: 1, SHA256: hex.EncodeToString(s.fileKeyDigest[:])})
		if err = writePrivate(ctx, marker, append(data, '\n')); err != nil {
			return fmt.Errorf("could not durably bind the credential key: %w", err)
		}
	} else if err != nil {
		return fileKeyError("key reference could not be inspected")
	}
	if err := s.verifyFileKeyReference(); err != nil {
		return err
	}
	// A previous attempt may have published the reference but failed its
	// directory sync. Reading the right bytes alone does not confirm durability.
	if err := privatefile.SyncDirectory(s.Root); err != nil {
		return fmt.Errorf("%w: credential key reference directory sync failed", privatefile.ErrDurabilityUncertain)
	}
	return nil
}

func (s *SecureFileStore) verifyFileKeyReference() error {
	raw, err := readFileKeyPrivate(filepath.Join(s.Root, fileKeyMarker), 4096)
	if err != nil {
		return fileKeyError("key reference is unavailable; existing data preserved")
	}
	var ref fileKeyReference
	if json.Unmarshal(raw, &ref) != nil || ref.Version != 1 || ref.SHA256 != hex.EncodeToString(s.fileKeyDigest[:]) {
		return fileKeyError("key does not match the credential directory; restore the original key")
	}
	return nil
}

func (s *SecureFileStore) fileKey(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.fileKeyPath == "" {
		return nil, ErrUnavailable
	}
	key, err := readFileKey(s.fileKeyPath)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(key) != s.fileKeyDigest {
		clear(key)
		return nil, fileKeyError("key changed while the service was running; existing data preserved")
	}
	if err = s.verifyFileKeyReference(); err != nil {
		clear(key)
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

// HMAC-SHA256 derives independent keys for credentials and recovery evidence,
// and for each normalized identity. The master key is never an encryption key.
func deriveFileKey(master []byte, purpose, identity string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("tosub2-file-key-v1\x00" + purpose + "\x00" + normalizedEmail(identity)))
	return mac.Sum(nil)
}

func (s *SecureFileStore) fileKeyPurposeKey(ctx context.Context, purpose, identity string) ([]byte, error) {
	if normalizedEmail(identity) == "" {
		return nil, errors.New("credential identity is required")
	}
	master, err := s.fileKey(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(master)
	return deriveFileKey(master, purpose, identity), nil
}

func (s *SecureFileStore) fileKeySaveContext(ctx context.Context, email string, value Credentials) error {
	key, err := s.fileKeyPurposeKey(ctx, "credentials", email)
	if err != nil {
		return err
	}
	defer clear(key)
	file, err := s.existingFile(email)
	if err != nil {
		return err
	}
	if file != "" {
		if _, err = s.readFileKeyCredentials(file, email, key); err != nil {
			return fmt.Errorf("existing credentials cannot be read; original file preserved: %w", err)
		}
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return errors.New("invalid credentials")
	}
	defer clear(plain)
	encrypted, err := sealState(key, email, plain, []byte("tosub2-credentials-v1"), stateCiphertext{Type: fileKeyCredentialType})
	if err != nil {
		return errors.New("credential encryption failed")
	}
	return writePrivate(ctx, s.path(email), append(encrypted, '\n'))
}

func (s *SecureFileStore) readFileKeyCredentials(path, email string, key []byte) (Credentials, error) {
	raw, err := readFileKeyPrivate(path, 16<<20)
	if err != nil {
		return Credentials{}, errors.New("could not read private encrypted credentials")
	}
	var envelope stateCiphertext
	if json.Unmarshal(raw, &envelope) != nil || envelope.Version != 1 || envelope.Type != fileKeyCredentialType || len(envelope.ProtectedKey) != 0 {
		return Credentials{}, errors.New("invalid file-key credential envelope")
	}
	plain, err := openState(key, email, envelope, []byte("tosub2-credentials-v1"))
	if err != nil {
		return Credentials{}, errors.New("credential authentication failed; original file preserved")
	}
	defer clear(plain)
	return decodeCredentials(plain)
}

func (s *SecureFileStore) fileKeyLoadContext(ctx context.Context, email string) (Credentials, error) {
	key, err := s.fileKeyPurposeKey(ctx, "credentials", email)
	if err != nil {
		return Credentials{}, err
	}
	defer clear(key)
	file, err := s.existingFile(email)
	if err != nil {
		return Credentials{}, err
	}
	if file == "" {
		return Credentials{}, os.ErrNotExist
	}
	return s.readFileKeyCredentials(file, email, key)
}

func readFileKey(path string) ([]byte, error) {
	key, err := readFileKeyPrivate(path, 32)
	if err != nil {
		return nil, fileKeyError("cannot read key; require a private regular file owned by the service user, mode 0600 or 0400")
	}
	if len(key) != 32 {
		clear(key)
		return nil, fileKeyError("key file must contain exactly 32 raw bytes")
	}
	return key, nil
}

// InitializeFileKey provisions a random master key exactly once. It never
// replaces a key and refuses to create one for existing credential data.
// The console itself only reads keys; deployments mount the key directory read-only.
func InitializeFileKey(keyPath, credentialRoot string) error {
	if strings.TrimSpace(keyPath) == "" || strings.TrimSpace(credentialRoot) == "" {
		return fileKeyError("key file and credential directory are required")
	}
	keyPath, err := filepath.Abs(keyPath)
	if err != nil {
		return fileKeyError("invalid key file path")
	}
	credentialRoot, err = filepath.Abs(credentialRoot)
	if err != nil {
		return fileKeyError("invalid credential directory")
	}
	if within, err := filepath.Rel(credentialRoot, keyPath); err == nil && within != ".." && !strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return fileKeyError("key file must be outside the credential directory")
	}
	ctx, cancel := boundedContext(context.Background())
	defer cancel()
	return withFileKeyRootLock(ctx, credentialRoot, func() error {
		if _, err := os.Lstat(keyPath); errors.Is(err, os.ErrNotExist) {
			empty, err := fileKeyRootEmpty(credentialRoot)
			if err != nil || !empty {
				return fileKeyError("key is missing for existing credentials; restore the original key instead of generating a replacement")
			}
			if err := createFileKey(keyPath); err != nil {
				return err
			}
		} else if err != nil {
			return fileKeyError("could not inspect key file")
		}
		key, err := readFileKey(keyPath)
		if err != nil {
			return err
		}
		defer clear(key)
		if err := privatefile.SyncDirectory(filepath.Dir(keyPath)); err != nil {
			return fmt.Errorf("%w: credential master key directory sync failed", privatefile.ErrDurabilityUncertain)
		}
		s := &SecureFileStore{Root: credentialRoot, platform: "linux", fileKeyPath: keyPath, fileKeyDigest: sha256.Sum256(key)}
		if err := s.bindFileKey(ctx); err != nil {
			return err
		}
		// Re-read after binding so initialization cannot report success for a
		// key replaced by an external actor during the durable marker write.
		readback, err := readFileKey(keyPath)
		defer clear(readback)
		if err != nil || !bytes.Equal(readback, key) {
			return fileKeyError("key changed during initialization")
		}
		return nil
	})
}
