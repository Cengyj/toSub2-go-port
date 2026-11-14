package modelprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// This synthetic fixture preserves only the observed stream shape: metadata,
// two reasoning items, and one completed assistant message at output index 2.
// It contains no captured provider response, account identity or reasoning.
func metadataStreamFixture() (prefix []byte, terminal map[string]any) {
	appendEvent := func(event map[string]any) { prefix = append(prefix, eventBytes(event)...) }
	appendEvent(map[string]any{"type": "response.metadata", "synthetic_private": "must-not-be-persisted"})
	output := []any{}
	for index, id := range []string{"synthetic-reasoning-a", "synthetic-reasoning-b"} {
		item := map[string]any{"id": id, "type": "reasoning", "status": "completed", "summary": []any{}}
		appendEvent(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
		appendEvent(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		output = append(output, item)
	}
	message := messageOutput("21")
	appendEvent(map[string]any{"type": "response.output_item.added", "output_index": 2, "item": message})
	for _, delta := range []string{"2", "1"} {
		appendEvent(map[string]any{"type": "response.output_text.delta", "output_index": 2, "content_index": 0, "delta": delta})
	}
	appendEvent(map[string]any{"type": "response.output_text.done", "output_index": 2, "content_index": 0, "text": "21"})
	appendEvent(map[string]any{"type": "response.output_item.done", "output_index": 2, "item": message})
	output = append(output, message)
	terminal = completedFixture("21")
	response := terminal["response"].(map[string]any)
	response["model"], response["output"] = "gpt-6.1-sol", output
	return prefix, terminal
}

func TestProbeAcceptsObservedMetadataStreamWithoutContentType(t *testing.T) {
	for _, contentType := range []string{"", "text/event-stream; charset=utf-8"} {
		calls := 0
		client := Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
			calls++
			if !request.SingleAttempt || request.RetryBeforeSend || request.RetryReadOnly {
				t.Fatal("metadata compatibility enabled request replay")
			}
			prefix, terminal := metadataStreamFixture()
			response := successfulResponse("21")
			response.Header = http.Header{}
			if contentType != "" {
				response.Header.Set("Content-Type", contentType)
			}
			response.Body = append(prefix, eventBytes(terminal)...)
			response.BodyComplete = true
			return response, nil
		})}
		result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "gpt-6.1-sol", ReasoningEffort: "high"})
		if err != nil || result.Score != 1 || result.Verdict != "normal" || result.Model != "gpt-6.1-sol" || calls != 1 {
			t.Fatal("complete observed stream rejected or replayed", err, calls)
		}
		encoded, err := json.Marshal(result)
		if err != nil || bytes.Contains(encoded, []byte("synthetic_private")) || bytes.Contains(encoded, []byte("must-not-be-persisted")) {
			t.Fatal("metadata escaped the transient parser")
		}
	}
}

func TestMissingContentTypeStillRequiresValidCompleteSSE(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*transport.Response, []byte, map[string]any)
	}{
		{"html", "invalid_stream", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Body = []byte("<html><body>21</body></html>")
		}},
		{"json", "invalid_stream", func(r *transport.Response, _ []byte, terminal map[string]any) { r.Body, _ = json.Marshal(terminal) }},
		{"no-completed-event", "incomplete", func(r *transport.Response, prefix []byte, _ map[string]any) { r.Body = prefix }},
		{"metadata-only", "incomplete", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Body = eventBytes(map[string]any{"type": "response.metadata"})
		}},
		{"unknown-event", "invalid_stream", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Body = append(eventBytes(map[string]any{"type": "response.synthetic_unknown"}), r.Body...)
		}},
		{"failed-event", "upstream_error", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Body = append(eventBytes(map[string]any{"type": "response.failed"}), r.Body...)
		}},
		{"terminal-error", "upstream_error", func(r *transport.Response, prefix []byte, terminal map[string]any) {
			terminal["response"].(map[string]any)["error"] = map[string]any{"message": "synthetic-private failure"}
			r.Body = append(prefix, eventBytes(terminal)...)
		}},
		{"contradictory-delta", "invalid_stream", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Body = bytes.Replace(r.Body, []byte(`"delta":"2"`), []byte(`"delta":"9"`), 1)
		}},
		{"wrong-message-id", "invalid_stream", func(r *transport.Response, prefix []byte, terminal map[string]any) {
			response := terminal["response"].(map[string]any)
			message := messageOutput("21")
			message["id"] = "changed-message"
			response["output"].([]any)[2] = message
			r.Body = append(prefix, eventBytes(terminal)...)
		}},
		{"wrong-model", "model_mismatch", func(r *transport.Response, prefix []byte, terminal map[string]any) {
			terminal["response"].(map[string]any)["model"] = "different-model"
			r.Body = append(prefix, eventBytes(terminal)...)
		}},
		{"explicit-html-type", "unexpected_content_type", func(r *transport.Response, _ []byte, _ map[string]any) { r.Header.Set("Content-Type", "text/html") }},
		{"explicit-json-type", "unexpected_content_type", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Header.Set("Content-Type", "application/json")
		}},
		{"explicit-empty-media-type", "unexpected_content_type", func(r *transport.Response, _ []byte, _ map[string]any) {
			r.Header.Set("Content-Type", "; charset=utf-8")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				prefix, terminal := metadataStreamFixture()
				r := successfulResponse("21")
				r.Header = http.Header{}
				r.Body, r.BodyComplete = append(prefix, eventBytes(terminal)...), true
				tc.change(r, prefix, terminal)
				return r, nil
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "gpt-6.1-sol"})
			failure := requireProbeError(t, result, err, tc.code)
			if calls != 1 || failure.HTTPStatus != http.StatusOK || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("unsafe stream replayed, lost status or leaked upstream data")
			}
		})
	}
}
