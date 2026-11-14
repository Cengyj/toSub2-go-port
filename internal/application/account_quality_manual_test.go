package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

const manualDrawing = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 480"><circle cx="100" cy="200" r="40"/></svg>`
const manualAnimation = `<!doctype html><html><head><style>.wheel{animation:rotate 1s infinite}@keyframes rotate{to{transform:rotate(360deg)}}</style></head><body><svg viewBox="0 0 640 480"><circle class="wheel" cx="100" cy="200" r="40"/></svg><script>document.body.dataset.animation='running';</script></body></html>`

func manualResponse(request transport.Request, svg string) *transport.Response {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(request.Body, &payload)
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "synthetic-manual-response", "object": "response", "status": "completed", "model": payload.Model, "output": []any{map[string]any{"id": "synthetic-message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": svg}}}}}})
	return &transport.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("data: " + string(event) + "\n\n")}
}
func waitManual(t *testing.T, c *AccountQualityController, id string) ManualProbeRecord {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		record := c.ManualProbe(id)
		if record.State != "running" {
			return record
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("manual probe did not finish")
			return ManualProbeRecord{}
		}
	}
}

func TestManualHTMLAnimationIsRetainedWithoutScoringAndReleasedOnReplacement(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, nil)
			calls := new(atomic.Int32)
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					calls.Add(1)
					return manualResponse(request, manualAnimation), nil
				}), func() {}, nil
			}
			for _, requestID := range []string{"animation-request-0001", "animation-request-0002"} {
				if _, err := c.StartManualProbe(t.Context(), f.job.ID, requestID, modelprobe.PelicanTask); err != nil {
					t.Fatal("animation admission failed", err)
				}
				c.manual.mu.Lock()
				done := c.manual.done
				c.manual.mu.Unlock()
				<-done
				record := c.ManualProbe(f.job.ID)
				if record.State != "success" || record.HTML != manualAnimation || record.SVG != "" {
					t.Fatal("animation source was lost or exposed as an inert SVG field")
				}
			}
			old, err := c.StartManualProbe(t.Context(), f.job.ID, "animation-request-0001", modelprobe.PelicanTask)
			if err != nil || old.HTML != "" || old.SVG != "" || old.ErrorCode != "result_replaced" || calls.Load() != 2 {
				t.Fatal("replaced HTML retained memory or replayed a billable request", err)
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			if rows[f.job.ID].LastResult != nil || rows[f.job.ID].LastAttempt.Status != "never" {
				t.Fatal("HTML animation changed automatic score or upload evidence")
			}
		})
	}
}
func TestManualProbeDeduplicatesHoldsGrantAndKeepsAutomaticScoreSeparate(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, func(s *accountquality.Settings) { s.CooldownSeconds = 60 })
			entered, release := make(chan struct{}), make(chan struct{})
			calls := new(atomic.Int32)
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
					calls.Add(1)
					close(entered)
					select {
					case <-release:
						return manualResponse(r, manualDrawing), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}), func() {}, nil
			}
			first, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-0001", modelprobe.PelicanTask)
			if err != nil || first.State != "running" {
				t.Fatal("start failed", err)
			}
			<-entered
			if c.Status().Active != 1 || c.Service.Status().Active != 0 {
				t.Fatal("manual activity missing or added to automatic runner")
			}
			same, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-0001", modelprobe.PelicanTask)
			if err != nil || same.RequestID != first.RequestID {
				t.Fatal("duplicate request not adopted", err)
			}
			if _, err = c.StartManualProbe(t.Context(), f.job.ID, "manual-0002", modelprobe.PelicanTask); !errors.Is(err, accountquality.ErrBusy) {
				t.Fatal("overlapping request accepted", err)
			}
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if lease != nil {
				lease.Release()
			}
			if !errors.Is(err, jobs.ErrAccountQueryBusy) {
				t.Fatal("manual request released its account before result", err)
			}
			close(release)
			done := waitManual(t, c, f.job.ID)
			if done.State != "success" || done.SVG != manualDrawing || done.NextAllowedAt == nil || calls.Load() != 1 || c.Status().Active != 0 {
				t.Fatal("incorrect manual terminal record")
			}
			same, err = c.StartManualProbe(t.Context(), f.job.ID, "manual-0001", modelprobe.PelicanTask)
			if err != nil || same.State != "success" || calls.Load() != 1 {
				t.Fatal("completed duplicate replayed model request", err)
			}
			if _, err = c.StartManualProbe(t.Context(), f.job.ID, "manual-0002", modelprobe.PelicanTask); err == nil {
				t.Fatal("manual cooldown bypassed")
			}
			rows, _ := c.Service.Snapshots([]string{f.job.ID})
			if rows[f.job.ID].LastResult != nil || rows[f.job.ID].LastAttempt.Status != "never" {
				t.Fatal("manual drawing overwrote automatic quality evidence")
			}
		})
	}
}
func TestManualProbeCancellationShutdownAndSupersededResults(t *testing.T) {
	for _, action := range []string{"cancel", "disabled", "shutdown", "superseded"} {
		t.Run(action, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
					close(entered)
					select {
					case <-release:
						return manualResponse(r, manualDrawing), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}), func() {}, nil
			}
			if _, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-action-01", modelprobe.PelicanTask); err != nil {
				t.Fatal(err)
			}
			<-entered
			switch action {
			case "cancel":
				if _, err := c.CancelManualProbe(t.Context(), f.job.ID, "manual-action-01"); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				c.CancelActiveManualProbe()
			case "shutdown":
				if err := c.CloseManualProbes(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "superseded":
				f.account["credentials"].(map[string]any)["access_token"] = "synthetic-replacement"
				f.write(t)
				close(release)
			}
			record := waitManual(t, c, f.job.ID)
			if record.SVG != "" || record.State == "success" || c.Status().Active != 0 {
				t.Fatal("cancelled or superseded request published drawing")
			}
			if action == "superseded" && record.ErrorCode != "superseded" {
				t.Fatalf("replacement identity not reported: %s", record.ErrorCode)
			}
			data, _ := json.Marshal(record)
			if strings.Contains(string(data), "synthetic-private") {
				t.Fatal("manual error leaked private authorization")
			}
		})
	}
}

func TestManualProbeContinuesBeyond128RequestsAndReleasesOlderDrawings(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	calls := new(atomic.Int32)
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			calls.Add(1)
			return manualResponse(r, manualDrawing), nil
		}), func() {}, nil
	}
	for i := 0; i < 129; i++ {
		_, err := c.StartManualProbe(t.Context(), f.job.ID, fmt.Sprintf("volume-request-%03d", i), modelprobe.PelicanTask)
		if err != nil {
			t.Fatalf("manual request %d blocked: %v", i, err)
		}
		c.manual.mu.Lock()
		done := c.manual.done
		c.manual.mu.Unlock()
		<-done
	}
	if calls.Load() != 129 || c.ManualProbe(f.job.ID).State != "success" {
		t.Fatal("long-running instance stopped admitting manual probes")
	}
	old, err := c.StartManualProbe(t.Context(), f.job.ID, "volume-request-000", modelprobe.PelicanTask)
	if err != nil || old.ErrorCode != "result_replaced" || old.SVG != "" || calls.Load() != 129 {
		t.Fatal("old request replayed inference or retained all drawings")
	}
	retained := 0
	c.manual.mu.Lock()
	for _, attempt := range c.manual.seen {
		if attempt.record.SVG != "" {
			retained++
		}
	}
	c.manual.mu.Unlock()
	if retained != 1 {
		t.Fatalf("retained %d drawings for one account", retained)
	}
}

func TestMovingAccountToTrashCancelsManualProbeAndHidesItsOutput(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	entered := make(chan struct{})
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(ctx context.Context, _ transport.Request) (*transport.Response, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}), func() {}, nil
	}
	if _, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-trash-01", modelprobe.PelicanTask); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if count, err := f.manager.TrashAccounts(ctx, []string{f.job.ID}); err != nil || count != 1 {
		t.Fatal("trash did not drain manual account lease", err)
	}
	if c.Status().Active != 0 || c.ManualProbe(f.job.ID).State != "idle" {
		t.Fatal("trashed account exposed a live or retained visual result")
	}
	c.manual.mu.Lock()
	record := c.manual.seen[f.job.ID+"|manual-trash-01"].record
	c.manual.mu.Unlock()
	if record.SVG != "" || record.State != "cancelled" {
		t.Fatal("removed account published manual drawing")
	}
}

func TestManualProbePanicFinalizesAndReleasesAccount(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	var calls, closed atomic.Int32
	c.transportFactory = func(string) (protocol.Doer, func(), error) {
		return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			if calls.Add(1) == 1 {
				panic("private synthetic credential must not appear")
			}
			return manualResponse(r, manualDrawing), nil
		}), func() { closed.Add(1) }, nil
	}
	for i, requestID := range []string{"panic-probe-0001", "panic-probe-0002"} {
		if _, err := c.StartManualProbe(t.Context(), f.job.ID, requestID, modelprobe.PelicanTask); err != nil {
			t.Fatal("account reservation not released", err)
		}
		c.manual.mu.Lock()
		done := c.manual.done
		c.manual.mu.Unlock()
		<-done
		record := c.ManualProbe(f.job.ID)
		if c.Status().Active != 0 || closed.Load() != int32(i+1) {
			t.Fatal("panic left live resources")
		}
		encoded, _ := json.Marshal(record)
		if strings.Contains(string(encoded), "private synthetic") {
			t.Fatal("panic content leaked")
		}
		if i == 0 && (record.State != "failed" || record.HTML != "" || record.SVG != "") {
			t.Fatal("panic reported a completed drawing")
		}
		if i == 1 && record.State != "success" {
			t.Fatal("later independent manual probe failed")
		}
	}
	rows, _ := c.Service.Snapshots([]string{f.job.ID})
	if rows[f.job.ID].LastResult != nil {
		t.Fatal("manual panic/success created candy score")
	}
}
