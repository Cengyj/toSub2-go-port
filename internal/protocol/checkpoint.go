package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"tosub2-go-port/internal/privatefile"
)

var ErrLoginPersistenceRemoved = errors.New("LOGIN_PERSISTENCE_REMOVED: saved login sessions and session exports are no longer supported; start a new operation")

// Checkpoint is the current call's progress. It cannot be encoded or decoded;
// the old file entry points below only reject unsupported input.
type Checkpoint struct {
	Stage      string
	Web, OAuth map[string]any
}

func (Checkpoint) MarshalJSON() ([]byte, error) { return nil, ErrLoginPersistenceRemoved }
func (*Checkpoint) UnmarshalJSON([]byte) error  { return ErrLoginPersistenceRemoved }
func (Checkpoint) Validate() error              { return ErrLoginPersistenceRemoved }
func ReadCheckpoint(string) (Checkpoint, error) { return Checkpoint{}, ErrLoginPersistenceRemoved }
func WriteCheckpoint(string, any) error         { return ErrLoginPersistenceRemoved }
func WritePrivateJSON(file string, v any) error { return writeAtomic(file, v, 0o600) }
func WriteJSONAtomic(file string, v any) error  { return writeAtomic(file, v, 0o644) }
func writeAtomic(file string, v any, mode os.FileMode) error {
	data, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	data = append(data, '\n')
	if mode == 0o600 {
		return privatefile.Write(context.Background(), file, data)
	}
	if e = privatefile.EnsureParentDir(filepath.Dir(file)); e != nil {
		return e
	}
	f, e := privatefile.CreateTemp(filepath.Dir(file), ".tmp-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(tmp, file)
}
func RemovePrivateResult(file string) error {
	return privatefile.Remove(context.Background(), file)
}
