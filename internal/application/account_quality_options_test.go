package application

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestManualOneOffModelReachesBothTransportsWithoutChangingUploadPolicy(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, controller := newQualityControllerFixture(t, method)
			controller.StartManualProbes(t.Context())
			t.Cleanup(func() { _ = controller.CloseManualProbes(context.Background()) })
			enableQualityFixture(t, controller, func(settings *accountquality.Settings) { settings.CooldownSeconds = 0 })
			before, err := controller.Service.Settings()
			if err != nil {
				t.Fatal(err)
			}
			calls := new(atomic.Int32)
			wire := make(chan map[string]any, 1)
			controller.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					calls.Add(1)
					var body map[string]any
					if json.Unmarshal(request.Body, &body) != nil {
						t.Error("invalid synthetic inference payload")
					}
					wire <- body
					return manualResponse(request, manualDrawing), nil
				}), func() {}, nil
			}
			selected := &accountquality.RunOptions{Model: "synthetic-one-off-model", ReasoningEffort: "low"}
			first, err := controller.StartManualProbeWithOptions(t.Context(), f.job.ID, "single-choice-0001", modelprobe.PelicanTask, selected)
			if err != nil || first.Model != selected.Model || first.ReasoningEffort != selected.ReasoningEffort {
				t.Fatal("manual receipt lost selected options", err)
			}
			done := waitManual(t, controller, f.job.ID)
			if done.State != "success" || done.Model != selected.Model || done.ReasoningEffort != selected.ReasoningEffort {
				t.Fatal("manual sample did not retain its own model selection", done.State)
			}
			body := <-wire
			if body["model"] != selected.Model {
				t.Fatal("selected model did not reach the actual inference transport")
			}
			if method == jobs.AuthorizationMethodBPS {
				if body["reasoning_effort"] != "low" {
					t.Fatal("BPS request lost selected reasoning effort")
				}
			} else if reasoning, ok := body["reasoning"].(map[string]any); !ok || reasoning["effort"] != "low" {
				t.Fatal("Codex request lost selected reasoning effort")
			}
			same, err := controller.StartManualProbeWithOptions(t.Context(), f.job.ID, "single-choice-0001", modelprobe.PelicanTask, selected)
			if err != nil || same.State != "success" || calls.Load() != 1 {
				t.Fatal("same request and options replayed a billed request", err)
			}
			changed := &accountquality.RunOptions{Model: "synthetic-other-model", ReasoningEffort: "low"}
			if _, err = controller.StartManualProbeWithOptions(t.Context(), f.job.ID, "single-choice-0001", modelprobe.PelicanTask, changed); !errors.Is(err, accountquality.ErrConflict) {
				t.Fatal("same request ID silently adopted another model", err)
			}
			if _, err = controller.StartManualProbe(t.Context(), f.job.ID, "single-choice-0001", modelprobe.PelicanTask); !errors.Is(err, accountquality.ErrConflict) {
				t.Fatal("same request ID silently replaced explicit options with defaults", err)
			}
			after, err := controller.Service.Settings()
			if err != nil || after != before || calls.Load() != 1 {
				t.Fatal("single-run selection changed global policy or dispatched an extra request")
			}
		})
	}
}
