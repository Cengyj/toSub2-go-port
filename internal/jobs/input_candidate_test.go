package jobs

import (
	"context"
	"sync"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestDiscardInputIfEqualPreservesCredentialsAndNewValues(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	const original = " \tSynthetic-Candidate9! "
	j, err := m.Submit(Spec{Email: "candidate@example.test", Input: map[string]any{"password": original, "value": "unrelated-input"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.DiscardInputIfEqual("missing", "password", original) || m.DiscardInputIfEqual(j.ID, "password", "Synthetic-Candidate9!") {
		t.Fatal("missing or byte-different candidate was discarded")
	}
	if !m.DiscardInputIfEqual(j.ID, "password", original) || m.DiscardInputIfEqual(j.ID, "password", original) {
		t.Fatal("matching candidate was not consumed exactly once")
	}
	input, _ := m.InputCopy(j.ID)
	if _, present := input["password"]; present || input["value"] != "unrelated-input" {
		t.Fatal("candidate removal affected unrelated input")
	}
	stored, err := vault.Load(j.Email)
	current, _ := m.Get(j.ID)
	if err != nil || stored.Password != original || !current.HasPassword || current.LoginMode != "password" {
		t.Fatal("candidate consumption changed persistent credentials or flags", err)
	}
	const replacement = "Synthetic-New-Password9!"
	if !m.Input(j.ID, map[string]any{"password": replacement}) {
		t.Fatal("new password input rejected")
	}
	if m.DiscardInputIfEqual(j.ID, "password", original) {
		t.Fatal("a different new password was discarded")
	}
	if value, ok := m.ConsumeInput(j.ID, "password"); !ok || value != replacement {
		t.Fatal("new password was not preserved for the prompt")
	}
}

func TestDiscardInputIfEqualRacingManualReplacement(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, err := m.Submit(Spec{Email: "candidate-race@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	const original = "Synthetic-Old9!"
	const replacement = "Synthetic-New9!"
	for i := 0; i < 100; i++ {
		if !m.Input(j.ID, map[string]any{"password": original}) {
			t.Fatal("initial candidate input rejected")
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			m.DiscardInputIfEqual(j.ID, "password", original)
		}()
		go func() {
			defer wg.Done()
			<-start
			m.Input(j.ID, map[string]any{"password": replacement})
		}()
		close(start)
		wg.Wait()
		if value, ok := m.ConsumeInput(j.ID, "password"); !ok || value != replacement {
			t.Fatal("concurrent candidate cleanup swallowed a new manual password")
		}
	}
}
