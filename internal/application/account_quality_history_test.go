package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/transport"
)

func controllerProbeHistory(t *testing.T, c *AccountQualityController, id string) accountquality.ProbeHistory {
	t.Helper()
	rows, _ := c.Snapshots([]string{id})
	return rows[id].ProbeHistory
}

func TestManualProbeHistoryPersistsSafeResultsWithoutScoreOrDrawing(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	observationTransport(c, qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		return manualResponse(request, manualDrawing), nil
	}))
	if record := runObservationManual(t, c, f.job.ID, "history-success-01"); record.State != "success" {
		t.Fatal(record.State)
	}
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: http.StatusForbidden}, nil
	}))
	if record := runObservationManual(t, c, f.job.ID, "history-failure-02"); record.State != "failed" {
		t.Fatal(record.State)
	}
	history := controllerProbeHistory(t, c, f.job.ID)
	if len(history) != 2 || history[0].Kind != "pelican" || history[0].Status != "success" || history[1].Status != "failed" || history[1].HTTPStatus != 403 || history[0].ID == history[1].ID {
		t.Fatal("manual history missing", history)
	}
	for _, item := range history {
		if item.Score != nil || item.Total != 0 || item.Verdict != "" {
			t.Fatal("manual result became a reasoning score")
		}
	}
	data, _ := json.Marshal(history)
	for _, private := range []string{"<svg", "<html", "synthetic-private", "HTML", "SVG"} {
		if strings.Contains(string(data), private) {
			t.Fatal("history included a drawing or credential")
		}
	}
	n, err := NewAccountQualityController(config.Config{OutputRoot: f.store.Root}, f.manager)
	if err != nil || !reflect.DeepEqual(history, controllerProbeHistory(t, n, f.job.ID)) {
		t.Fatal("manual history not restored", err)
	}
	rows, _ := c.Service.Snapshots([]string{f.job.ID})
	if rows[f.job.ID].LastResult != nil || rows[f.job.ID].LastAttempt.Status != "never" {
		t.Fatal("manual history changed automatic probe evidence")
	}
}

func TestManualProbeHistoryWriteFailureDoesNotChangeSuccessOrRetry(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	sum := sha256.Sum256([]byte(f.job.ID))
	path := filepath.Join(f.store.Root, "account-quality", hex.EncodeToString(sum[:])+".json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	observationTransport(c, qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		calls.Add(1)
		return manualResponse(request, manualDrawing), nil
	}))
	record := runObservationManual(t, c, f.job.ID, "history-write-failure")
	if record.State != "success" || record.HTML == "" || calls.Load() != 1 {
		t.Fatal("display persistence changed network success or retried", record.State, calls.Load())
	}
	if len(controllerProbeHistory(t, c, f.job.ID)) != 0 {
		t.Fatal("failed persistence published a saved history entry")
	}
}

func TestManualProbeHistoryRejectsReplacedGrant(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	observationTransport(c, qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement-access"
		f.write(t)
		return manualResponse(request, manualDrawing), nil
	}))
	record := runObservationManual(t, c, f.job.ID, "history-replaced-grant")
	if record.ErrorCode != "superseded" || len(controllerProbeHistory(t, c, f.job.ID)) != 0 {
		t.Fatal("old grant outcome entered replacement account history", record.ErrorCode)
	}
}
