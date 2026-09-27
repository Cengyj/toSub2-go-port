package mail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLegacyMailboxTimestampRankingAndPOSTRequest(t *testing.T) {
	raw := `{"data":[{"id":"8662a68c-8d5e-4b94-9508-e6c3f676715c","subject":"New sign-in to your OpenAI account","body_text":"New sign-in details for your OpenAI account.","received_at":"2026-08-21T07:03:20.48048Z"},{"id":"233381cc-38cd-49c0-bac5-490580bb9125","subject":"Your temporary ChatGPT login code","body_text":"Enter this temporary verification code to continue: 866483","received_at":"2026-08-21T07:03:13.197752Z"},{"id":"4b38e61d-7c5f-4988-8e9b-80a1f1ebb7b8","subject":"Your temporary OpenAI verification code","body_text":"Enter this temporary verification code to continue: 606195","received_at":"2026-08-20T23:56:22.341347Z"}],"page":1,"size":20,"total":3}`
	got := ExtractCandidates(raw)
	if len(got) != 2 || got[0].Code != "866483" || got[1].Code != "606195" || got[0].ReceivedAt <= got[1].ReceivedAt {
		t.Fatalf("legacy mailbox ranking or identifier filtering changed: %#v", got)
	}
	before, _ := time.Parse(time.RFC3339, "2026-08-21T07:03:00Z")
	after, _ := time.Parse(time.RFC3339, "2026-08-21T07:04:00Z")
	if len(FilterByRequestTime(got, before.UnixMilli())) != 1 || len(FilterByRequestTime(got, after.UnixMilli())) != 0 {
		t.Fatal("request cutoff filtering changed")
	}
	body := "mailbox_id%3Daccount-specific-id%26page%3D1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(b) != body || r.Header.Get("Authorization") != "Bearer test-mail-token" || r.Header.Get("Referer") != "https://mail.example/private/inbox" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("mailbox request configuration changed")
		}
		io.WriteString(w, `{"data":[{"id":"new-message","body_text":"Your ChatGPT verification code is 752941","received_at":"2026-08-21T08:00:00Z"}]}`)
	}))
	defer server.Close()
	got, err := FetchCandidates(context.Background(), server.URL, RequestConfig{Method: "POST", Body: body, Headers: map[string]string{"authorization": "Bearer test-mail-token", "referer": "https://mail.example/private/inbox", "content-type": "application/json"}}, server.Client())
	if err != nil || len(got) != 1 || got[0].Code != "752941" {
		t.Fatal("configured mailbox request failed", err)
	}
}
