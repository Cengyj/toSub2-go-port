package accountplan

import "testing"

func TestAutomaticRepairExcludedUsesKnownSourcesAndExplicitUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, local      string
		remote, excluded []string
		want             bool
	}{
		{"local-free-over-stale-paid", "free", []string{"plus"}, []string{"free"}, true},
		{"remote-free-over-stale-paid", "plus", []string{"free"}, []string{"free"}, true},
		{"known-local-over-unknown", "plus", []string{"unknown"}, []string{"unknown"}, false},
		{"known-remote-over-unknown", "unknown", []string{"plus"}, []string{"unknown"}, false},
		{"both-unknown-explicit", "unknown", []string{"unknown"}, []string{"unknown"}, true},
		{"unknown-is-not-free", "unknown", []string{"unknown"}, []string{"free"}, false},
		{"local-only", "free", nil, []string{"free"}, true},
		{"duplicate-record-veto", "plus", []string{"plus", "free"}, []string{"free"}, true},
		{"business-alias", "business", []string{"pro"}, []string{"team"}, true},
		{"invalid-selection-not-unknown", "unknown", nil, []string{"bogus"}, false},
		{"empty-selection", "free", []string{"free"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutomaticRepairExcluded(tc.local, tc.remote, tc.excluded); got != tc.want {
				t.Fatalf("AutomaticRepairExcluded()=%v, want %v", got, tc.want)
			}
		})
	}
}
