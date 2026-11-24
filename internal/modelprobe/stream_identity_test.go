package modelprobe

import (
	"context"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestProbeRejectsContradictoryProgressIdentityWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix map[string]any
	}{
		{"response changed", map[string]any{"type": "response.created", "response": map[string]any{"id": "different-response"}}},
		{"response id malformed", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": 123}}},
		{"delta response changed", map[string]any{"type": "response.output_text.delta", "response_id": "different-response", "output_index": 0, "content_index": 0, "delta": "21"}},
		{"added item changed", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "id": "different-item"}}},
		{"delta item changed", map[string]any{"type": "response.output_text.delta", "item_id": "different-item", "output_index": 0, "content_index": 0, "delta": "21"}},
		{"reasoning item absent in completion", map[string]any{"type": "response.reasoning_text.delta", "item_id": "missing-item", "output_index": 1, "content_index": 0, "delta": "private reasoning"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				r := successfulResponse("21")
				r.Body = append(eventBytes(tc.prefix), r.Body...)
				return r, nil
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			if err == nil || result.Verdict != "" || calls != 1 || strings.Contains(err.Error(), "different-") {
				t.Fatalf("mixed identity accepted/replayed/leaked: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestStreamKeepsConsistentOptionalProgressIdentityCompatible(t *testing.T) {
	terminal := completedFixture("21")
	response := terminal["response"].(map[string]any)
	message := response["output"].([]any)[0].(map[string]any)
	body := eventBytes(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "model": response["model"], "status": "in_progress"}})
	body = append(body, eventBytes(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "id": message["id"]}})...)
	body = append(body, eventBytes(terminal)...)
	if text, err := parseStream(body, "synthetic-model"); err != nil || text != "21" {
		t.Fatal("consistent identity rejected", err)
	}
}
