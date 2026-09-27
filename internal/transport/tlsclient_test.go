package transport

import "testing"

func TestFingerprintProfileAliasAndFallback(t *testing.T) {
	if got := ProfileCandidates("chrome133"); len(got) < 2 || got[0] != "chrome_133" {
		t.Fatalf("alias candidates: %#v", got)
	}
	got := ProfileCandidates("chrome999")
	if len(got) == 0 || got[0] != "chrome_133" {
		t.Fatalf("fallback candidates: %#v", got)
	}
	c, err := NewFingerprintClient("chrome146", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile() != "chrome_133" {
		t.Fatalf("profile=%s", c.Profile())
	}
}
