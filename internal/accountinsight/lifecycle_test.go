package accountinsight

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInsightCloseWaitsForAdmittedSettingsWrite(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-start", true: "started"}[started], func(t *testing.T) {
			s, err := New(t.TempDir(), (&fixture{}).provider())
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			write := s.persist
			s.persist = func(ctx context.Context, name string, value any) error {
				close(entered)
				<-release
				return write(ctx, name, value)
			}
			if started {
				s.Start(t.Context())
			}
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = s.Close(ctx)
			})
			updateDone := make(chan error, 1)
			requested := s.Settings()
			requested.IntervalMinutes++
			go func() { _, err := s.Update(t.Context(), requested); updateDone <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("settings write did not begin")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = s.Close(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("close reported completion while a settings write still owned storage: %v", err)
			}
			close(release)
			if err := <-updateDone; err != nil {
				t.Fatal("admitted local commit was discarded", err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.Close(ctx); err != nil {
				t.Fatal("close did not observe completed persistence", err)
			}
			if _, err := s.Update(t.Context(), s.Settings()); !errors.Is(err, ErrClosed) {
				t.Fatal("closed service accepted a new settings write", err)
			}
		})
	}
}
