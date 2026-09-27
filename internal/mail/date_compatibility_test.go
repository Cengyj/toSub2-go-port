package mail

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLegacyMailboxDateFormatsRetainRequestTimeFiltering(t *testing.T) {
	// JavaScript Date.parse interprets timezone-less ISO datetimes in the
	// service's local timezone, but date-only ISO values remain UTC.
	previous := time.Local
	time.Local = time.FixedZone("mail-fixture-UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = previous })
	for _, tc := range []struct {
		name, input string
		want        time.Time
	}{
		{"compact-offset", "2026-09-27T10:20:30+0800", time.Date(2026, 9, 27, 2, 20, 30, 0, time.UTC)},
		{"local-fraction", "2026-09-27T10:20:30.123", time.Date(2026, 9, 27, 2, 20, 30, 123000000, time.UTC)},
		{"local-spaces", "2026-09-27 10:20:30", time.Date(2026, 9, 27, 2, 20, 30, 0, time.UTC)},
		{"space-with-offset", "2026-09-27 10:20:30+08:00", time.Date(2026, 9, 27, 2, 20, 30, 0, time.UTC)},
		{"minute-with-zone", "2026-09-27T10:20Z", time.Date(2026, 9, 27, 10, 20, 0, 0, time.UTC)},
		{"rfc-date", "Sun, 27 Sep 2026 02:20:30 GMT", time.Date(2026, 9, 27, 2, 20, 30, 0, time.UTC)},
		{"date-only", "2026-09-27", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"id": "synthetic-message", "body": "OpenAI verification code 654321", "received_at": tc.input})
			got := ExtractCandidates(string(raw))
			if len(got) != 1 || got[0].ReceivedAt != tc.want.UnixMilli() {
				t.Fatalf("mail timestamp %q: got %#v, want %d", tc.input, got, tc.want.UnixMilli())
			}
			if len(FilterByRequestTime(got, tc.want.Add(time.Second).UnixMilli())) != 0 {
				t.Fatal("known old message bypassed the request-time filter")
			}
		})
	}
}
