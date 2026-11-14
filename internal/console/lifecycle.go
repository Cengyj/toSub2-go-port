package console

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

func (s *Server) Close() {
	timeout := time.Duration(s.cfg.ShutdownTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.CloseContext(ctx)
}

func (s *Server) CloseContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.monitorMu.Lock()
	s.monitorClosed = true
	s.updateAutoRepairPolicy()
	cancel, monitorDone := s.monitorCancel, s.monitorDone
	s.monitorCancel, s.monitorNextCheckAt = nil, nil
	s.monitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Each component closes admission and cancels its workers before waiting.
	// Start them together so one slow cleanup cannot consume everyone else's
	// cancellation budget. Keep every error, including a monitor timeout.
	type component struct {
		name  string
		close func(context.Context) error
	}
	components := []component{
		{"SMS", s.closeSMSContext},
		{"CPA monitor", s.closeCPAMonitorContext},
		{"monitor", func(ctx context.Context) error {
			if monitorDone == nil {
				return nil
			}
			select {
			case <-monitorDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}},
	}
	if s.recycleBin != nil {
		components = append(components, component{"recycle bin", s.recycleBin.Close})
	}
	if s.insights != nil {
		components = append(components, component{"account insights", s.insights.Close})
	}
	if s.quality != nil {
		components = append(components, component{"account quality", func(ctx context.Context) error {
			// A completed manual probe may still commit its history through the
			// quality service. Keep that dependency open until manual cleanup ends.
			manualErr := s.quality.CloseManualProbes(ctx)
			return errors.Join(manualErr, s.quality.Service.Close(ctx))
		}})
	}
	closeErrors := make([]error, len(components))
	var wg sync.WaitGroup
	for i, component := range components {
		wg.Go(func() {
			if err := component.close(ctx); err != nil {
				closeErrors[i] = fmt.Errorf("%s shutdown: %w", component.name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(closeErrors...)
}
