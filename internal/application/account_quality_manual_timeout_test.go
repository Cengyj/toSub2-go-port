package application

import (
	"context"
	"fmt"
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

func TestManualDrawingBudgetIsIndependentOfCandySettings(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		for _, configured := range []int{90, 300} {
			t.Run(fmt.Sprintf("%s/%d", method, configured), func(t *testing.T) {
				f, c := newQualityControllerFixture(t, method)
				c.StartManualProbes(t.Context())
				enableQualityFixture(t, c, func(s *accountquality.Settings) { s.TimeoutSeconds = configured })
				before, err := c.Service.Settings()
				if err != nil {
					t.Fatal(err)
				}
				type budget struct {
					drawing          bool
					context, request time.Duration
				}
				observed := make(chan budget, 2)
				c.transportFactory = func(string) (protocol.Doer, func(), error) {
					return qualityDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
						deadline, _ := ctx.Deadline()
						drawing := strings.Contains(string(r.Body), "鹈鹕")
						observed <- budget{drawing: drawing, context: time.Until(deadline), request: r.Timeout}
						if drawing {
							return manualResponse(r, manualDrawing), nil
						}
						return qualitySyntheticResponse(r, true), nil
					}), func() {}, nil
				}
				if _, err := c.StartManualProbe(t.Context(), f.job.ID, "drawing-budget-0001", modelprobe.PelicanTask); err != nil {
					t.Fatal(err)
				}
				c.manual.mu.Lock()
				done := c.manual.done
				c.manual.mu.Unlock()
				<-done
				if c.ManualProbe(f.job.ID).State != "success" {
					t.Fatal("manual drawing did not succeed")
				}
				ctx, gate, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
				defer cleanup()
				if err := gate(ctx, f.account); err != nil {
					t.Fatal("candy probe did not succeed", err)
				}
				for _, want := range []budget{{drawing: true, request: 300 * time.Second}, {drawing: false, request: time.Duration(configured) * time.Second}} {
					got := <-observed
					if got.drawing != want.drawing || got.context < want.request-time.Second || got.context > want.request || got.request < want.request-time.Second || got.request > want.request {
						t.Fatalf("incorrect inference budget: got=%+v want=%+v", got, want)
					}
				}
				after, err := c.Service.Settings()
				if err != nil || before != after {
					t.Fatal("drawing changed saved candy settings", err)
				}
			})
		}
	}
}

func TestManualDrawingTimeoutKeepsFailureSpecificAndAllowsAnotherRequest(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, method)
			c.StartManualProbes(t.Context())
			enableQualityFixture(t, c, nil)
			var calls atomic.Int32
			c.transportFactory = func(string) (protocol.Doer, func(), error) {
				return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					if calls.Add(1) == 1 {
						return nil, context.DeadlineExceeded
					}
					return manualResponse(r, manualDrawing), nil
				}), func() {}, nil
			}
			for _, requestID := range []string{"drawing-timeout-0001", "drawing-timeout-0002"} {
				if _, err := c.StartManualProbe(t.Context(), f.job.ID, requestID, modelprobe.PelicanTask); err != nil {
					t.Fatal("new drawing request was blocked", err)
				}
				c.manual.mu.Lock()
				done := c.manual.done
				c.manual.mu.Unlock()
				<-done
				record := c.ManualProbe(f.job.ID)
				if requestID == "drawing-timeout-0001" {
					if record.State != "failed" || record.ErrorCode != "timeout" || !strings.Contains(record.Message, "绘图") || strings.Contains(record.Message, "5 分钟") || c.Status().Active != 0 || calls.Load() != 1 {
						t.Fatalf("early upstream timeout was misreported or retried: %+v", record)
					}
				} else if record.State != "success" || record.SVG != manualDrawing || calls.Load() != 2 {
					t.Fatalf("explicit retry did not complete: %+v", record)
				}
			}
		})
	}
}
