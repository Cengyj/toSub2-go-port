package main

import (
	"context"
	"errors"
	"time"

	"tosub2-go-port/internal/challenge"
)

// The callback closes over one browser and proxy identity. A pending/rejected
// human verification or a canceled child operation must not reload the page
// and discard the user's progress. Other failures retain the retry budget.
func retryBrowserChallenge(ctx context.Context, retries int, delay time.Duration, solve func() error, notify func()) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := solve()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, challenge.ErrVerificationPending) || errors.Is(err, challenge.ErrVerificationRejected) {
			return err
		}
		if attempt >= retries {
			return err
		}
		if notify != nil {
			notify()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
