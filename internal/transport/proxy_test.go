package transport

import "testing"

func TestProfileAndProxyRotation(t *testing.T) {
	p := ProfileFor("chrome120")
	if p.Major != 120 || p.UserAgent == "" {
		t.Fatal(p)
	}
	s := NewProxySession("http://u:p@host:1?sid={sid}")
	a := s.Rotate()
	b := s.Rotate()
	if a == b {
		t.Fatal("session did not rotate")
	}
}
