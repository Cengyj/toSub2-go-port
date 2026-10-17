package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"tosub2-go-port/internal/privatefile"
)

var ErrNotFound = os.ErrNotExist

// ErrDurabilityUncertain marks a successful file replacement whose directory
// synchronization failed. The new file is visible, but crash durability is
// unknown; callers must not treat this as a write that never happened.
var ErrDurabilityUncertain = errors.New("file was replaced but durable storage could not be confirmed")

// Store provides atomic persistence for the current metadata and recovery
// files. It never writes credentials into job metadata.
type Store struct{ Root string }

func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("store root is empty")
	}
	if err := privatefile.EnsureDir(root); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}
func (s *Store) Path(name string) string { return filepath.Join(s.Root, filepath.Base(name)) }

func (s *Store) ReadJSON(ctx context.Context, name string, dst any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(s.Path(name))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 32<<20 {
		return errors.New("JSON file exceeds 32 MiB limit")
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: io.LimitReader(file, (32<<20)+1)})
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values in file")
	}
	return ctx.Err()
}
func (s *Store) WriteJSON(ctx context.Context, name string, value any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.WriteBytes(ctx, name, data)
}
func (s *Store) WriteBytes(ctx context.Context, name string, data []byte) error {
	return s.writeBytes(ctx, name, data, syncDirectory)
}

// Keep the final durability boundary local to this call so it can be tested
// after a real atomic rename without process-wide filesystem hooks.
func (s *Store) writeBytes(ctx context.Context, name string, data []byte, syncDir func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := s.Path(name)
	tmp, err := privatefile.CreateTemp(s.Root, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, target); err != nil {
		return err
	}
	if err := syncDir(s.Root); err != nil {
		return fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	return nil
}

func syncDirectory(root string) error {
	// Syncing the containing directory makes the rename durable on Unix.
	// Windows does not expose directory fsync through os.File.Sync.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(root)
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := dir.Sync(); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func (s *Store) Exists(name string) bool { _, err := os.Stat(s.Path(name)); return err == nil }
func IsNotFound(err error) bool          { return errors.Is(err, os.ErrNotExist) }
func CloneJSON(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
