package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/mail"
)

const mailboxFetchTimeout = 60 * time.Second

var errMailPollingTimeout = errors.New("MAIL_OTP_TIMEOUT: automatic mailbox polling paused for manual input")

type mailStateEvent func(status, safeError string)

type mailInputState struct {
	seen              map[string]bool
	observations      map[string]int
	baselineAvailable bool
	paused            bool
	fetch             func(context.Context, string, mail.RequestConfig) ([]mail.Candidate, error)
}

func newMailInputState() *mailInputState {
	return &mailInputState{seen: map[string]bool{}, observations: map[string]int{}}
}

func (s *mailInputState) fetchCandidates(ctx context.Context, url string, request mail.RequestConfig) ([]mail.Candidate, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, mailboxFetchTimeout)
	defer cancel()
	if s.fetch != nil {
		return s.fetch(fetchCtx, url, request)
	}
	return mail.FetchCandidates(fetchCtx, url, request, http.DefaultClient)
}

// baseline snapshots the mailbox before sending an OTP. A failed snapshot does
// not prevent manual login, but undated messages cannot safely be auto-submitted.
func (s *mailInputState) baseline(ctx context.Context, url string, request mail.RequestConfig, event mailStateEvent) error {
	candidates, err := s.fetchCandidates(ctx, url, request)
	s.observations = map[string]int{}
	if err != nil {
		s.baselineAvailable = false
		emitMailState(event, "error", safeMailboxError(err))
		return ctx.Err()
	}
	for _, c := range candidates {
		if c.Key != "" {
			s.seen[c.Key] = true
		}
	}
	s.baselineAvailable = true
	emitMailState(event, "ready", "")
	return ctx.Err()
}

func (s *mailInputState) requested() {
	s.paused = false
	s.observations = map[string]int{}
}

func emitMailState(event mailStateEvent, status, message string) {
	if event != nil {
		event(status, message)
	}
}

func safeMailboxError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "收码接口请求超时，请检查接口或手动输入验证码"
	}
	return "收码接口请求失败，请检查地址、鉴权和网络连接，或手动输入验证码"
}

func (s *mailInputState) candidate(candidates []mail.Candidate, cutoff int64) string {
	for _, c := range mail.FilterByRequestTime(candidates, cutoff) {
		if c.Code == "" || c.Key == "" || s.seen[c.Key] {
			continue
		}
		if c.ReceivedAt == 0 && !s.baselineAvailable {
			continue
		}
		s.observations[c.Key]++
		if c.Score < 12 && s.observations[c.Key] < 2 {
			continue
		}
		s.seen[c.Key] = true
		return c.Code
	}
	return ""
}

// pollMailInput bounds the poll and each fetch, permits manual override while a
// request is pending, and deduplicates message identity instead of six digits.
func pollMailInput(ctx context.Context, cfg config.Config, url string, request mail.RequestConfig, cutoff int64, state *mailInputState, manual func() (string, bool), event mailStateEvent) (string, error) {
	if state.paused {
		return "", errMailPollingTimeout
	}
	timeout := time.Duration(cfg.MailPollTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	interval := time.Duration(cfg.MailPollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 2500 * time.Millisecond
	}
	poll := time.NewTimer(0)
	defer poll.Stop()
	manualTicker := time.NewTicker(100 * time.Millisecond)
	defer manualTicker.Stop()
	type result struct {
		candidates []mail.Candidate
		err        error
	}
	responses := make(chan result, 1)
	emitMailState(event, "polling", "")
	for {
		if value, ok := manual(); ok {
			emitMailState(event, "ready", "")
			return value, nil
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			state.paused = true
			emitMailState(event, "timeout", "自动收码等待超时，请手动输入或重新发送")
			return "", errMailPollingTimeout
		case <-manualTicker.C:
		case <-poll.C:
			go func() {
				c, e := state.fetchCandidates(pollCtx, url, request)
				select {
				case responses <- result{c, e}:
				case <-pollCtx.Done():
				}
			}()
		case response := <-responses:
			if value, ok := manual(); ok {
				emitMailState(event, "ready", "")
				return value, nil
			}
			if response.err != nil {
				emitMailState(event, "error", safeMailboxError(response.err))
			} else if code := state.candidate(response.candidates, cutoff); code != "" {
				emitMailState(event, "found", "")
				return code, nil
			} else {
				emitMailState(event, "polling", "")
			}
			poll.Reset(interval)
		}
	}
}
