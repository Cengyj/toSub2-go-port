package credentials

import (
	"os"
	"strings"
	"sync"
)

// MemoryStore enables non-persistent workflows where no protected OS service is
// available. No password, TOTP seed or proxy credential is written to disk.
type MemoryStore struct {
	mu     sync.Mutex
	values map[string]Credentials
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{values: map[string]Credentials{}} }
func copyCredentials(c Credentials) Credentials {
	if c.MailHeaders != nil {
		h := make(map[string]string, len(c.MailHeaders))
		for k, v := range c.MailHeaders {
			h[k] = v
		}
		c.MailHeaders = h
	}
	return c
}
func (s *MemoryStore) Save(email string, c Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[strings.ToLower(strings.TrimSpace(email))] = copyCredentials(c)
	return nil
}
func (s *MemoryStore) Load(email string) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.values[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return Credentials{}, os.ErrNotExist
	}
	return copyCredentials(c), nil
}
func (s *MemoryStore) Delete(email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, strings.ToLower(strings.TrimSpace(email)))
	return nil
}
