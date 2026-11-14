package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func waitCandyIdle(t *testing.T, c *AccountQualityController) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if status := c.Service.Status(); status.Active == 0 && status.Queued == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("synthetic candy check did not finish")
}

func TestCandyBusyQueryReportsAdmissionFailureWithoutInferenceOrLosingHistory(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			enableQualityFixture(t, c, nil)
			var calls, builds atomic.Int32
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				builds.Add(1)
				return qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					calls.Add(1)
					return qualitySyntheticResponse(request, true), nil
				}), func() {}, nil
			}
			submit := func() {
				t.Helper()
				receipt, err := c.Service.Refresh(t.Context(), []string{f.job.ID}, false)
				if err != nil || len(receipt.Accepted) != 1 {
					t.Fatal("synthetic explicit candy submission failed", err)
				}
				waitCandyIdle(t, c)
			}
			submit()
			before, _ := c.Service.Snapshots([]string{f.job.ID})
			previous := before[f.job.ID]
			if previous.LastAttempt.Status != "success" || previous.LastResult == nil || len(previous.ProbeHistory) != 1 || calls.Load() != 1 {
				t.Fatal("missing prior synthetic successful observation")
			}
			// Models and manual drawing both hold this real manager reservation.
			query, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer query.Release()
			submit()
			after, _ := c.Service.Snapshots([]string{f.job.ID})
			blocked := after[f.job.ID]
			if blocked.LastAttempt.Status != "failed" || blocked.LastAttempt.ErrorCode != "account_busy" || blocked.LastAttempt.HTTPStatus != 0 || !strings.Contains(blocked.LastAttempt.Message, "未发起检测") || !blocked.Persisted {
				t.Fatalf("busy query lost its admission diagnostic: %+v", blocked.LastAttempt)
			}
			if !reflect.DeepEqual(previous.LastResult, blocked.LastResult) || !reflect.DeepEqual(previous.ProbeHistory, blocked.ProbeHistory) || !reflect.DeepEqual(previous.HTTPObservation, blocked.HTTPObservation) || calls.Load() != 1 || builds.Load() != 1 {
				t.Fatal("busy admission inferred a low score, replaced history, or contacted upstream")
			}
			restored, err := accountquality.New(f.store.Root, accountquality.Provider{Accounts: c.accounts, Acquire: c.acquire})
			if err != nil {
				t.Fatal(err)
			}
			reloaded, _ := restored.Snapshots([]string{f.job.ID})
			if reloaded[f.job.ID].LastAttempt.ErrorCode != "account_busy" || !reflect.DeepEqual(reloaded[f.job.ID].LastResult, previous.LastResult) {
				t.Fatal("restart lost the local busy diagnostic or prior measurement")
			}
			query.Release()
			// Releasing the unrelated query cannot schedule an automatic replay.
			if status := c.Service.Status(); status.Active != 0 || status.Queued != 0 || calls.Load() != 1 {
				t.Fatal("query release resumed the rejected candy operation")
			}
			submit()
			if calls.Load() != 2 {
				t.Fatal("a new explicit submission failed or repeated inference")
			}
		})
	}
}

func TestQualityGrantAdmissionErrorsKeepStableCauses(t *testing.T) {
	for _, tc := range []struct{ original, expected error }{
		{jobs.ErrAccountQueryBusy, accountquality.ErrBusy},
		{jobs.ErrAccountQuerySuperseded, accountquality.ErrSuperseded},
	} {
		if !errors.Is(qualityGrantError(tc.original), tc.expected) {
			t.Fatal("grant admission error lost its quality classification")
		}
	}
}
