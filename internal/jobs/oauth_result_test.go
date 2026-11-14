package jobs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func TestIndependentOAuthOperationCannotAdoptOlderIssuedResult(t *testing.T) {
	for _, action := range []string{"queue", "requeue", "input"} {
		t.Run(action, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			job, err := m.Submit(Spec{ID: "issued-result", Email: "owner@example.test", Operation: "relogin", Input: map[string]any{"authorizationMethod": "bps"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Cancel(job.ID)
			oldPath := OAuthGrantPath(st.Root, job)
			grant := issuedGrantFixture(job.Email, "bps")
			if err := protocol.WriteSavedOAuthGrant(oldPath, grant); err != nil {
				t.Fatal(err)
			}
			legacyPath := filepath.Join(st.Root, job.ID, "oauth-grant.json")
			if err := protocol.WriteSavedOAuthGrant(legacyPath, grant); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(oldPath)
			var fresh *Job
			switch action {
			case "queue":
				fresh, err = m.QueueOperation(job.ID, "relogin", map[string]any{"authorizationMethod": "chatgpt"})
			case "requeue":
				fresh, err = m.Requeue(job.ID, "relogin")
			case "input":
				if !m.Input(job.ID, map[string]any{"password": "explicit-new-password"}) {
					t.Fatal("explicit new operation input rejected")
				}
				fresh, _ = m.Get(job.ID)
			}
			if err != nil || fresh.OAuthResultID == "" || fresh.OAuthResultID == job.OAuthResultID || OAuthGrantPath(st.Root, fresh) == oldPath {
				t.Fatal("independent login reused the old issued-result path", err)
			}
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Restart between admission and network dispatch cannot convert the
			// older provider's grant into this new operation's completion.
			restored := NewManager(t.Context(), 1, st, nil)
			defer restored.Shutdown(context.Background())
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, _ := restored.Get(job.ID)
			if current.OAuthResultID != fresh.OAuthResultID || current.Operation != "relogin" || current.Status == "resume_available" {
				t.Fatal("restart adopted an older issued result")
			}
			restored.Cancel(job.ID)
			retried, err := restored.Retry(job.ID)
			if err != nil || retried.Operation == "resume" || retried.OAuthResultID == fresh.OAuthResultID {
				t.Fatal("failed independent operation retried an older issued result", err)
			}
			for _, path := range []string{oldPath, legacyPath} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("older issued tokens were deleted or overwritten")
				}
			}
		})
	}
}

func TestCurrentIssuedOAuthResultKeepsItsPathAcrossLocalCompletion(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(t.Context(), 1, st, nil)
	job, err := m.Submit(Spec{ID: "current-issued", Email: "owner@example.test", Operation: "relogin"})
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(job.ID)
	path := OAuthGrantPath(st.Root, job)
	if err := protocol.WriteSavedOAuthGrant(path, issuedGrantFixture(job.Email, "chatgpt")); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, st, nil)
	defer restored.Shutdown(context.Background())
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(job.ID)
	if current.Status != "resume_available" || OAuthGrantPath(st.Root, current) != path {
		t.Fatal("current issued tokens lost the local completion path")
	}
	retried, err := restored.Retry(job.ID)
	if err != nil || retried.Operation != "resume" || OAuthGrantPath(st.Root, retried) != path {
		t.Fatal("local completion created a new result or login", err)
	}
}
