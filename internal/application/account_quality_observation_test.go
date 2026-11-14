package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func runObservationManual(t *testing.T, c *AccountQualityController, id, requestID string) ManualProbeRecord {
	t.Helper()
	if _, err := c.StartManualProbe(t.Context(), id, requestID, modelprobe.PelicanTask); err != nil {
		t.Fatal(err)
	}
	record := waitManual(t, c, id)
	c.manual.mu.Lock()
	done := c.manual.done
	c.manual.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("manual request cleanup did not finish")
	}
	return record
}

func observedHTTP(t *testing.T, c *AccountQualityController, id string) *accountquality.HTTPObservation {
	t.Helper()
	rows, _ := c.Snapshots([]string{id})
	return rows[id].HTTPObservation
}

func observationTransport(c *AccountQualityController, run qualityDoer) {
	c.transportFactory = func(string) (protocol.Doer, func(), error) { return run, func() {}, nil }
}

func TestManualHTTPObservationProjectsStatusesWithoutChangingAutomaticEvidence(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodBPS)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, nil)
			var calls atomic.Int32
			observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls.Add(1)
				return &transport.Response{StatusCode: status, Body: []byte("synthetic-private upstream error")}, nil
			}))
			r := runObservationManual(t, c, f.job.ID, "observation-status-01")
			o := observedHTTP(t, c, f.job.ID)
			if r.State != "failed" || o == nil || o.Status != "failed" || o.HTTPStatus != status || o.Source != "pelican" || o.FinishedAt.IsZero() || o.ErrorCode == "" || o.Message == "" {
				t.Fatalf("manual status not projected: %+v", o)
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			if rows[f.job.ID].LastResult != nil || rows[f.job.ID].LastAttempt.Status != "never" || rows[f.job.ID].HTTPObservation != nil {
				t.Fatal("manual observation overwrote automatic evidence")
			}
			o.Message = "caller mutation"
			if observedHTTP(t, c, f.job.ID).Message == "caller mutation" || calls.Load() != 1 {
				t.Fatal("projection exposed shared state or queried upstream")
			}
			encoded, _ := json.Marshal(observedHTTP(t, c, f.job.ID))
			if strings.Contains(string(encoded), "synthetic-private") {
				t.Fatal("projection leaked raw provider text")
			}
		})
	}
}

func TestHTTPObservationRetainsUnknownAndSelectsNewerSuccessAcrossSources(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: 403}, nil
	}))
	runObservationManual(t, c, f.job.ID, "observation-failed-01")
	failed := observedHTTP(t, c, f.job.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		close(entered)
		<-release
		return nil, errors.New("synthetic-private disconnect")
	}))
	if _, err := c.StartManualProbe(t.Context(), f.job.ID, "observation-unknown-02", modelprobe.PelicanTask); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("manual request did not start")
	}
	if o := observedHTTP(t, c, f.job.ID); o == nil || !reflect.DeepEqual(o, failed) {
		t.Fatal("running request erased known HTTP failure")
	}
	close(release)
	waitManual(t, c, f.job.ID)
	c.manual.mu.Lock()
	done := c.manual.done
	c.manual.mu.Unlock()
	<-done
	if o := observedHTTP(t, c, f.job.ID); o == nil || !reflect.DeepEqual(o, failed) {
		t.Fatal("unknown request erased known HTTP failure")
	}
	observationTransport(c, qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return manualResponse(r, `<html><body><script>incomplete`), nil
	}))
	runObservationManual(t, c, f.job.ID, "observation-malformed-02")
	if o := observedHTTP(t, c, f.job.ID); o == nil || !reflect.DeepEqual(o, failed) {
		t.Fatal("malformed HTTP 200 erased known HTTP failure")
	}
	observationTransport(c, qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return qualitySyntheticResponse(r, true), nil
	}))
	if _, err := c.Service.Refresh(t.Context(), []string{f.job.ID}, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := c.Service.Status()
		if s.Active == 0 && s.Queued == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if o := observedHTTP(t, c, f.job.ID); o == nil || o.Status != "success" || o.Source != "candy" || !o.FinishedAt.After(failed.FinishedAt) {
		t.Fatalf("new automatic success did not clear manual failure: %+v", o)
	}
	automatic, _ := c.Service.Snapshots([]string{f.job.ID})
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: 429}, nil
	}))
	runObservationManual(t, c, f.job.ID, "observation-limited-03")
	if o := observedHTTP(t, c, f.job.ID); o == nil || o.HTTPStatus != 429 || o.Source != "pelican" {
		t.Fatal("new manual failure did not replace older automatic success")
	}
	observationTransport(c, qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return manualResponse(r, manualDrawing), nil
	}))
	runObservationManual(t, c, f.job.ID, "observation-success-04")
	if o := observedHTTP(t, c, f.job.ID); o == nil || o.Status != "success" || o.HTTPStatus != 200 || o.Source != "pelican" {
		t.Fatal("new manual success retained old error")
	}
	after, _ := c.Service.Snapshots([]string{f.job.ID})
	if !reflect.DeepEqual(automatic[f.job.ID].LastResult, after[f.job.ID].LastResult) || automatic[f.job.ID].LastAttempt.CheckID != after[f.job.ID].LastAttempt.CheckID {
		t.Fatal("manual observations changed automatic score or attempt")
	}
}

func TestHTTPObservationHidesChangedAuthorizationAndRecycledAccounts(t *testing.T) {
	for _, change := range []string{"grant", "proxy", "recycle"} {
		t.Run(change, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, nil)
			observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return &transport.Response{StatusCode: 401}, nil
			}))
			runObservationManual(t, c, f.job.ID, "observation-identity-01")
			if observedHTTP(t, c, f.job.ID) == nil {
				t.Fatal("initial observation missing")
			}
			switch change {
			case "grant":
				f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement-access"
				f.write(t)
			case "proxy":
				if _, err := f.manager.Update(f.job.ID, map[string]any{"proxyUrl": "http://synthetic-proxy.example.test:8080"}); err != nil {
					t.Fatal(err)
				}
			case "recycle":
				if n, err := f.manager.TrashAccounts(t.Context(), []string{f.job.ID}); err != nil || n != 1 {
					t.Fatal("recycle failed", err)
				}
			}
			if observedHTTP(t, c, f.job.ID) != nil {
				t.Fatal("old observation claimed current authority")
			}
			if change == "recycle" {
				entries := f.manager.ListTrash()
				if len(entries) != 1 {
					t.Fatal("missing recycle entry")
				}
				if n, err := f.manager.RestoreTrash(t.Context(), []string{entries[0].ID}); err != nil || n != 1 {
					t.Fatal("restore failed", err)
				}
				if o := observedHTTP(t, c, f.job.ID); o == nil || o.FinishedAt.IsZero() {
					t.Fatal("same-authority restore lost dated saved observation")
				}
			}
		})
	}
}

func TestHTTPObservationReadsDoNotWritePrivateDataOrUseNetwork(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: 403}, nil
	}))
	runObservationManual(t, c, f.job.ID, "observation-readonly-01")
	manifest := func() map[string]string {
		files := map[string]string{}
		err := filepath.WalkDir(f.store.Root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = string(data)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := manifest()
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		t.Fatal("snapshot constructed network transport")
		return nil, nil, nil
	}
	for i := 0; i < 3; i++ {
		if observedHTTP(t, c, f.job.ID) == nil {
			t.Fatal("read lost observation")
		}
	}
	if !reflect.DeepEqual(before, manifest()) {
		t.Fatal("read-only projection wrote private data")
	}
}
