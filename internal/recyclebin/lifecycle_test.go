package recyclebin

import (
	"context"
	"errors"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestRetentionCloseWaitsForAdmittedOperations(t *testing.T) {
	for _, operation := range []string{"update", "update-with-background", "sweep"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			r := &fakeRepository{}
			if operation == "sweep" {
				r.entries = []*jobs.TrashEntry{{ID: "synthetic-purge", State: jobs.TrashStatePurging}}
			}
			s := newService(t, r)
			// Make a failure exit release the owned operation before service cleanup.
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			done := make(chan error, 1)
			if operation == "sweep" {
				r.purge = func(context.Context, string) (int, error) { close(entered); <-release; return 1, nil }
				go func() { done <- s.Sweep(t.Context(), time.Now()) }()
			} else {
				original := s.writeSettings
				s.writeSettings = func(ctx context.Context, next Settings) error { close(entered); <-release; return original(ctx, next) }
				if operation == "update-with-background" {
					s.Start(t.Context())
				}
				go func() {
					_, err := s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 7})
					done <- err
				}()
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("operation did not enter its local side effect")
			}
			deadline, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			if err := s.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close returned before admitted %s released ownership: %v", operation, err)
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("admitted operation did not finish")
			}
			finish, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			if err := s.Close(finish); err != nil {
				t.Fatal("Close did not finish after owned work released", err)
			}
			if _, err := s.Update(finish, Settings{SchemaVersion: 1, Revision: s.Settings().Revision, RetentionDays: 0}); !errors.Is(err, ErrClosed) {
				t.Fatal("closed service accepted a new settings mutation", err)
			}
		})
	}
}

func TestRetentionRejectsIncompleteStoredPolicyBeforeCleanup(t *testing.T) {
	cases := map[string]string{
		"missing-schema-and-revision": `{"retentionDays":1}`,
		"missing-schema":              `{"revision":1,"retentionDays":1}`,
		"missing-revision":            `{"schemaVersion":1,"retentionDays":1}`,
		"null-schema":                 `{"schemaVersion":null,"revision":1,"retentionDays":1}`,
		"null-revision":               `{"schemaVersion":1,"revision":null,"retentionDays":1}`,
		"null-document":               "null",
		"empty-document":              "{}",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			disk, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.WriteBytes(t.Context(), "recycle-bin-settings.json", []byte(raw)); err != nil {
				t.Fatal(err)
			}
			repo := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "synthetic-expired", State: jobs.TrashStateTrashed, DeletedAt: time.Now().Add(-48 * time.Hour)}}}
			if service, err := New(disk.Root, repo); err == nil {
				_ = service.Close(context.Background())
				t.Fatal("incomplete stored policy was accepted using implicit schema/revision defaults")
			}
			if len(repo.calls) != 0 {
				t.Fatal("invalid stored policy started cleanup")
			}
		})
	}
}

func TestRetentionCompleteStoredPolicyPreservesRevisionAndValue(t *testing.T) {
	disk, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expected := Settings{SchemaVersion: 1, Revision: 7, RetentionDays: 90}
	if err := disk.WriteJSON(t.Context(), "recycle-bin-settings.json", expected); err != nil {
		t.Fatal(err)
	}
	service, err := New(disk.Root, &fakeRepository{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if got := service.Settings(); got != expected {
		t.Fatalf("stored policy changed: got=%+v want=%+v", got, expected)
	}
}
