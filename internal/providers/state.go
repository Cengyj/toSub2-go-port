package providers

import (
	"errors"
	"slices"
	"strings"

	"tosub2-go-port/internal/mail"
)

// CustomPoolState contains API URLs and observed OTP identifiers. It is private
// credential material and must only be stored in an OS-protected credential
// store, never in job metadata or a public API response.
type CustomPoolState struct {
	Entries []string                    `json:"entries"`
	Next    int                         `json:"next"`
	Leases  map[string]CustomLeaseState `json:"leases"`
}
type CustomLeaseState struct {
	URL       string          `json:"url"`
	Seen      map[string]bool `json:"seen"`
	Requested int64           `json:"requested"`
}

func (p *CustomPool) PrivateState() CustomPoolState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := CustomPoolState{Entries: append([]string(nil), p.entries...), Next: p.next, Leases: map[string]CustomLeaseState{}}
	for id, l := range p.leases {
		seen := map[string]bool{}
		for k, v := range l.seen {
			seen[k] = v
		}
		s.Leases[id] = CustomLeaseState{URL: l.url, Seen: seen, Requested: l.requested}
	}
	return s
}
func (p *CustomPool) RestorePrivateState(s CustomPoolState) error {
	if p.err != nil || !slices.Equal(s.Entries, p.entries) || s.Next < 0 || s.Next > len(p.entries) || len(s.Leases) > s.Next {
		return errors.New("invalid custom SMS state")
	}
	allocatedURLs := map[string]bool{}
	for _, entry := range p.entries[:s.Next] {
		allocatedURLs[strings.SplitN(entry, "----", 2)[1]] = true
	}
	for id, l := range s.Leases {
		if !strings.HasPrefix(id, "custom-") || len(id) > 80 || !mail.ValidateURL(l.URL) || !allocatedURLs[l.URL] || len(l.Seen) > 10000 || l.Requested < 0 {
			return errors.New("invalid custom SMS lease state")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.Next > p.next {
		p.next = s.Next
	}
	for id, l := range s.Leases {
		seen := map[string]bool{}
		for k, v := range l.Seen {
			seen[k] = v
		}
		p.leases[id] = &customLeaseState{url: l.URL, seen: seen, requested: l.Requested}
	}
	return nil
}
