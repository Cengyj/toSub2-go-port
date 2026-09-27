package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/mail"
)

func mailboxCandidates(t *testing.T, id, code string, stamp int64) []mail.Candidate {
	t.Helper()
	raw := map[string]any{"messageId": id, "message": "Your OpenAI verification code is " + code}
	if stamp != 0 {
		raw["receivedAt"] = stamp
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := mail.ExtractCandidates(string(b))
	if len(c) != 1 {
		t.Fatalf("unexpected fixture candidate count %d", len(c))
	}
	return c
}

func TestMailBaselineAndMessageIdentity(t *testing.T) {
	state := newMailInputState()
	old := mailboxCandidates(t, "old-message", "123456", 0)
	freshSameCode := mailboxCandidates(t, "new-message", "123456", 0)
	state.fetch = func(context.Context, string, mail.RequestConfig) ([]mail.Candidate, error) { return old, nil }
	if err := state.baseline(context.Background(), "https://mail.example", mail.RequestConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	state.requested()
	if got := state.candidate(old, time.Now().UnixMilli()); got != "" {
		t.Fatal("undated baseline code was submitted")
	}
	if got := state.candidate(freshSameCode, time.Now().UnixMilli()); got != "123456" {
		t.Fatal("new message with repeated digits was lost")
	}
	if got := state.candidate(freshSameCode, 0); got != "" {
		t.Fatal("message identity was submitted twice")
	}
	another := mailboxCandidates(t, "third-message", "123456", 0)
	if got := state.candidate(another, 0); got != "123456" {
		t.Fatal("rejected digits prevented a new message from being used")
	}
}

func TestMailMissingBaselineRejectsUnknownAge(t *testing.T) {
	for _, failed := range []bool{false, true} {
		state := newMailInputState()
		if failed {
			state.fetch = func(context.Context, string, mail.RequestConfig) ([]mail.Candidate, error) {
				return nil, errors.New("private api-key=sensitive")
			}
			if err := state.baseline(context.Background(), "https://mail.example/private", mail.RequestConfig{}, func(status, message string) {
				if status != "error" || strings.Contains(message, "sensitive") {
					t.Error("mail error leaked details")
				}
			}); err != nil {
				t.Fatal("API failure prevented manual login", err)
			}
		}
		now := time.Now().UnixMilli()
		unknown := mailboxCandidates(t, "unknown-age", "123456", 0)
		if got := state.candidate(unknown, now); got != "" {
			t.Fatal("unknown age code accepted without baseline")
		}
		old := mailboxCandidates(t, "old-date", "234567", now-60000)
		if got := state.candidate(old, now); got != "" {
			t.Fatal("old dated code accepted")
		}
		fresh := mailboxCandidates(t, "fresh-date", "345678", now+100)
		if got := state.candidate(fresh, now); got != "345678" {
			t.Fatal("known recent code rejected without baseline")
		}
	}
}

func TestMailFetchDeadlineAndCancellation(t *testing.T) {
	state := newMailInputState()
	state.fetch = func(ctx context.Context, _ string, _ mail.RequestConfig) ([]mail.Candidate, error) {
		deadline, ok := ctx.Deadline()
		remain := time.Until(deadline)
		if !ok || remain > 60*time.Second || remain < 59*time.Second {
			t.Error("mail fetch does not preserve the 60 second cap")
		}
		return nil, context.DeadlineExceeded
	}
	if err := state.baseline(context.Background(), "https://mail.example", mail.RequestConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.fetch = func(ctx context.Context, _ string, _ mail.RequestConfig) ([]mail.Candidate, error) {
		return nil, ctx.Err()
	}
	if err := state.baseline(ctx, "https://mail.example", mail.RequestConfig{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("baseline swallowed job cancellation")
	}
}

func TestMailTimeoutPausesUntilResendAndPreservesManualOverride(t *testing.T) {
	state := newMailInputState()
	calls := 0
	fetchStopped := make(chan struct{})
	state.fetch = func(ctx context.Context, _ string, _ mail.RequestConfig) ([]mail.Candidate, error) {
		calls++
		<-ctx.Done()
		close(fetchStopped)
		return nil, ctx.Err()
	}
	var statuses []string
	event := func(status, message string) { statuses = append(statuses, status) }
	_, err := pollMailInput(context.Background(), config.Config{MailPollTimeoutMS: 25, MailPollIntervalMS: 5}, "https://mail.example", mail.RequestConfig{}, 0, state, func() (string, bool) { return "", false }, event)
	if !errors.Is(err, errMailPollingTimeout) || !state.paused {
		t.Fatal("overall timeout did not pause automatic mode")
	}
	<-fetchStopped
	prior := calls
	_, err = pollMailInput(context.Background(), config.Config{}, "https://mail.example", mail.RequestConfig{}, 0, state, func() (string, bool) { return "", false }, event)
	if !errors.Is(err, errMailPollingTimeout) || calls != prior {
		t.Fatal("rejected manual input restarted the timed-out poll")
	}
	state.requested()
	got, err := pollMailInput(context.Background(), config.Config{}, "https://mail.example", mail.RequestConfig{}, 0, state, func() (string, bool) { return "123456", true }, event)
	if got != "123456" || err != nil || state.paused {
		t.Fatal("manual override/resend reset failed")
	}
	if len(statuses) < 3 || statuses[0] != "polling" || statuses[len(statuses)-1] != "ready" {
		t.Fatal("mail events missing", statuses)
	}
}

func TestMailFetchErrorsRetryAndNeverExposeRequestDetails(t *testing.T) {
	state := newMailInputState()
	state.baselineAvailable = true
	fresh := mailboxCandidates(t, "fresh", "345678", 0)
	attempts := 0
	state.fetch = func(context.Context, string, mail.RequestConfig) ([]mail.Candidate, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("secret-token cookie password https://mail.example?api_key=private")
		}
		return fresh, nil
	}
	statuses := []string{}
	got, err := pollMailInput(context.Background(), config.Config{MailPollTimeoutMS: 1000, MailPollIntervalMS: 1}, "https://mail.example", mail.RequestConfig{}, 0, state, func() (string, bool) { return "", false }, func(status, message string) {
		statuses = append(statuses, status)
		for _, secret := range []string{"secret-token", "private", "cookie", "password"} {
			if strings.Contains(message, secret) {
				t.Error("mail event exposed raw request error")
			}
		}
	})
	if got != "345678" || err != nil || attempts != 2 || strings.Join(statuses, ",") != "polling,error,found" {
		t.Fatal("transient fetch did not recover", statuses, err)
	}
}

func TestMailLowConfidenceRequiresTwoSeparatePolls(t *testing.T) {
	state := newMailInputState()
	state.baselineAvailable = true
	candidate := []mail.Candidate{{Code: "123456", Key: "weak", Score: 5}}
	if state.candidate(candidate, 0) != "" || state.candidate(candidate, 0) != "123456" {
		t.Fatal("weak candidate confirmation changed")
	}
}
