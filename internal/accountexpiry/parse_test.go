package accountexpiry

import (
	"strings"
	"testing"
	"time"
)

func TestParseManualExpiryUsesImportDateAndExplicitTimezone(t *testing.T) {
	now := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ input, zone, want string }{
		{"16.30", "Asia/Shanghai", "2026-10-18T08:30:00Z"},
		{"16:30", "America/Los_Angeles", "2026-10-17T23:30:00Z"},
		{"08.00", "Asia/Shanghai", "2026-10-18T00:00:00Z"},
		{"7-18 08:00", "Asia/Shanghai", "2026-07-18T00:00:00Z"},
		{"7-18 8:00", "Asia/Shanghai", "2026-07-18T00:00:00Z"},
		{"07-18 8:00", "America/Los_Angeles", "2026-07-18T15:00:00Z"},
		{"7-18 8:00:15", "Asia/Shanghai", "2026-07-18T00:00:15Z"},
		{"2026-10-20 16:30", "Asia/Shanghai", "2026-10-20T08:30:00Z"},
		{"2026-10-20T16:30", "Asia/Shanghai", "2026-10-20T08:30:00Z"},
		{"2026-10-20T16:30:00+08:00", "UTC", "2026-10-20T08:30:00Z"},
		{" 2026-10-20T08:30:00Z ", "Asia/Shanghai", "2026-10-20T08:30:00Z"},
	} {
		t.Run(tc.input+tc.zone, func(t *testing.T) {
			got, err := ParseManual(tc.input, tc.zone, now)
			if err != nil || got.UTC().Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestParseMonthDayExpiryUsesBrowserImportYearWithoutRollover(t *testing.T) {
	for _, tc := range []struct {
		input, zone, now, want string
	}{
		{"7-18 08:00", "Asia/Shanghai", "2026-01-01T00:30:00Z", "2026-07-18T00:00:00Z"},
		{"7-18 8:00", "America/Los_Angeles", "2026-01-01T00:30:00Z", "2025-07-18T15:00:00Z"},
		{"1-1 8:00", "Asia/Shanghai", "2025-12-31T23:30:00Z", "2026-01-01T00:00:00Z"},
		{"2-29 8:00", "Asia/Shanghai", "2024-10-18T00:00:00Z", "2024-02-29T00:00:00Z"},
	} {
		t.Run(tc.input+tc.zone+tc.now, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseManual(tc.input, tc.zone, now)
			if err != nil || got.Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestParseMonthDayExpiryRejectsInvalidCalendarAndDST(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ input, zone string }{
		{"0-18 8:00", "UTC"}, {"13-18 8:00", "UTC"},
		{"7-0 8:00", "UTC"}, {"7-32 8:00", "UTC"},
		{"2-30 8:00", "UTC"}, {"2-29 8:00", "UTC"},
		{"7-18 24:00", "UTC"}, {"7-18 8:60", "UTC"},
		{"7-18 8:00:60", "UTC"}, {"7-18 8:0", "UTC"},
		{"7-18", "UTC"}, {"7/18 8:00", "UTC"},
		{"3-8 2:30", "America/New_York"},
		{"11-1 1:30", "America/New_York"},
	} {
		t.Run(tc.input+tc.zone, func(t *testing.T) {
			if _, err := ParseManual(tc.input, tc.zone, now); err == nil {
				t.Fatal("invalid or ambiguous month/day date was accepted")
			}
		})
	}
}

func TestParseManualExpiryRejectsInvalidAndAmbiguousDates(t *testing.T) {
	for _, tc := range []struct{ input, zone string }{
		{"", "UTC"}, {"24.00", "UTC"}, {"16.60", "UTC"},
		{"16.3", "UTC"}, {"2026-02-30 16:30", "UTC"},
		{"0000-01-01T00:00:00Z", "UTC"}, {"9999-12-31T23:30:00-08:00", "UTC"},
		{"2026-10-20", "UTC"}, {"private-canary", "UTC"},
		{"16.30", "not-a-zone"}, {"16.30", "../private-canary"},
		{"2026-03-08 02:30", "America/New_York"},
		{"2026-11-01 01:30", "America/New_York"},
	} {
		if _, err := ParseManual(tc.input, tc.zone, time.Now()); err == nil || strings.Contains(err.Error(), "private-canary") {
			t.Fatalf("invalid input was accepted or echoed: %v", err)
		}
	}
}

func FuzzParseManual(f *testing.F) {
	for _, seed := range []string{"16.30", "7-18 08:00", "7-18 8:00", "2-30 8:00", "11-1 1:30", "2030-10-20T16:30", "2026-11-01 01:30", "2026-03-08 02:30", "invalid-private-input", "2030-01-01T16:30:00+08:00"} {
		f.Add(seed)
	}
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, value string) {
		got, err := ParseManual(value, "America/New_York", now)
		if err != nil {
			return
		}
		if got.IsZero() || got.Year() < 1 || got.Year() > 9999 {
			t.Fatal("parser returned invalid persistence timestamp")
		}
		roundTrip, err := ParseManual(got.Format(time.RFC3339Nano), "UTC", now)
		if err != nil || !roundTrip.Equal(got) {
			t.Fatal("successful parse did not round trip")
		}
	})
}
