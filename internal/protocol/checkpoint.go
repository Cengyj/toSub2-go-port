package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/transport"
)

// Checkpoint mirrors the v1 login-checkpoint.json shape. Extra fields are
// retained so newer/older versions can resume without dropping state.
type Checkpoint struct {
	Version     int                `json:"version"`
	Stage       string             `json:"stage"`
	UpdatedAt   string             `json:"updated_at"`
	Email       string             `json:"email"`
	ChatGPTBase string             `json:"chatgpt_base,omitempty"`
	AuthBase    string             `json:"auth_base,omitempty"`
	Warning     string             `json:"warning,omitempty"`
	Cookies     []transport.Cookie `json:"cookies"`
	Web         map[string]any     `json:"web,omitempty"`
	OAuth       map[string]any     `json:"oauth,omitempty"`
	Extra       map[string]any     `json:"-"`
}

func (c Checkpoint) MarshalJSON() ([]byte, error) {
	type alias Checkpoint
	b, e := json.Marshal(alias(c))
	if e != nil {
		return nil, e
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b, nil
	}
	for k, v := range c.Extra {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return json.Marshal(m)
}
func (c *Checkpoint) UnmarshalJSON(b []byte) error {
	type alias Checkpoint
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*c = Checkpoint(a)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	known := map[string]bool{"version": true, "stage": true, "updated_at": true, "email": true, "chatgpt_base": true, "auth_base": true, "warning": true, "cookies": true, "web": true, "oauth": true}
	c.Extra = map[string]any{}
	for k, v := range m {
		if !known[k] {
			var x any
			if json.Unmarshal(v, &x) == nil {
				c.Extra[k] = x
			}
		}
	}
	return nil
}

func (c Checkpoint) Validate() error {
	if c.Version != 1 || c.Stage == "" || c.Email == "" || c.Cookies == nil {
		return errors.New("CHECKPOINT_INVALID: malformed login checkpoint")
	}
	return nil
}
func ReadCheckpoint(file string) (Checkpoint, error) {
	b, e := os.ReadFile(file)
	if e != nil {
		return Checkpoint{}, e
	}
	var c Checkpoint
	if e = json.Unmarshal(b, &c); e != nil {
		return Checkpoint{}, fmt.Errorf("CHECKPOINT_INVALID: %w", e)
	}
	if e = c.Validate(); e != nil {
		return Checkpoint{}, e
	}
	return c, nil
}
func WriteCheckpoint(file string, c Checkpoint) error {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.UpdatedAt == "" {
		c.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if c.Warning == "" {
		c.Warning = "This file contains login cookies and OAuth state. Do not share it."
	}
	if c.Cookies == nil {
		c.Cookies = []transport.Cookie{}
	}
	if e := c.Validate(); e != nil {
		return e
	}
	return WritePrivateJSON(file, c)
}
func WritePrivateJSON(file string, v any) error { return writeAtomic(file, v, 0o600) }
func WriteJSONAtomic(file string, v any) error  { return writeAtomic(file, v, 0o644) }
func writeAtomic(file string, v any, mode os.FileMode) error {
	data, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	data = append(data, '\n')
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
func RemoveCheckpoint(file string) error {
	if e := os.Remove(file); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}
