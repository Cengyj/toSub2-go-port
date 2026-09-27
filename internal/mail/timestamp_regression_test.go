package mail

import (
	"reflect"
	"testing"
	"time"
)

func TestSMSCodeTimestampNeverUsesExpiryOrFractionAsOTP(t *testing.T) {
	raw := `{"expired_date":"2099-01-01T00:00:00Z","code_time":"2026-09-25T10:00:00.123456Z","code":"654321"}`
	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-09-25T10:00:00.123456Z")
	var previous []Candidate
	for i := 0; i < 100; i++ {
		got := ExtractCandidates(raw)
		if len(got) != 1 || got[0].Code != "654321" || got[0].ReceivedAt != wantTime.UnixMilli() {
			t.Fatalf("iteration %d extracted metadata or selected expiry: %#v", i, got)
		}
		if i > 0 && !reflect.DeepEqual(got, previous) {
			t.Fatal("candidate order or identity depends on map iteration")
		}
		if len(FilterByRequestTime(got, wantTime.Add(time.Second).UnixMilli())) != 0 {
			t.Fatal("stale code survived request-time cutoff")
		}
		previous = got
	}
}

func TestMessageMetadataOverridesEnvelopeAndDeduplicatesRepresentations(t *testing.T) {
	raw := `{"id":"envelope","created_at":"2026-09-25T12:00:00Z","messages":[{"id":"message","created_at":"2026-09-25T10:00:00Z","received_at":"2026-09-25T10:01:00Z","body":"ChatGPT verification code: 654321","html":"<b>Your code: 654321</b>"}]}`
	wantTime, _ := time.Parse(time.RFC3339, "2026-09-25T10:01:00Z")
	got := ExtractCandidates(raw)
	if len(got) != 1 || got[0].ReceivedAt != wantTime.UnixMilli() {
		t.Fatalf("message metadata should replace envelope and identify one email: %#v", got)
	}
	wantKey := fingerprint("654321|id:message|time:1790330460000")
	if got[0].Key != wantKey {
		t.Fatalf("legacy candidate identity changed: got %s want %s", got[0].Key, wantKey)
	}
}

func TestRawCodesAdjacentCodesAndInlineTimestamps(t *testing.T) {
	if got := ExtractCandidates("654321"); len(got) != 1 || got[0].Code != "654321" {
		t.Fatalf("plain-code endpoint unsupported: %#v", got)
	}
	if got := ExtractCandidates(`{"body":"OTP 654321 123456 9999999"}`); len(got) != 2 {
		t.Fatalf("adjacent codes or longer numbers parsed incorrectly: %#v", got)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-09-25T10:01:00Z")
	if got := ExtractCandidates("2026-09-25T10:01:00Z OpenAI verification code 654321"); len(got) != 1 || got[0].ReceivedAt != wantTime.UnixMilli() {
		t.Fatalf("inline timestamp not parsed: %#v", got)
	}
	if got := ExtractCandidates("2026-09-25T10:01:00.123456Z OpenAI verification code 654321"); len(got) != 1 || got[0].Code != "654321" || got[0].ReceivedAt != wantTime.UnixMilli()+123 {
		t.Fatalf("inline timestamp fraction became an OTP: %#v", got)
	}
	if got := ExtractCandidates(`{"body":"<script>otp 123456</script><p title='654321'>OpenAI</p>"}`); len(got) != 0 {
		t.Fatalf("HTML attributes or script became OTP text: %#v", got)
	}
}
