package mail

import "testing"

func TestExtractCandidatesJSONHTMLAndFilter(t *testing.T) {
	raw := `{"messages":[{"id":"a","received_at":"2026-09-25T10:00:00Z","subject":"OpenAI verification code","body":"Your code is 123456"},{"id":"b","received_at":"2026-09-25T11:00:00Z","body":"code 654321"}]}`
	got := ExtractCandidates(raw)
	if len(got) < 2 || got[0].Code != "654321" {
		t.Fatalf("%#v", got)
	}
	if len(FilterByRequestTime(got, 1790331000000)) != 1 {
		t.Fatalf("filter=%#v", got)
	}
}

func TestExtractCandidatesFindsMultipleCodesInOneMessage(t *testing.T) {
	got := ExtractCandidates(`{"body":"OpenAI code 123456; previous code 654321"}`)
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
}
