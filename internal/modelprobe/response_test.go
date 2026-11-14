package modelprobe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func requireParseError(t *testing.T, body []byte, code string) {
	t.Helper()
	text, err := parseStream(body, "synthetic-model")
	var failure *Error
	if text != "" || !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("parse text=%q err=%v; want %s", text, err, code)
	}
}

func TestStreamAcceptsCompleteConsistentResponses(t *testing.T) {
	for _, mode := range []string{"terminal", "deltas", "dated-model", "completed-items", "reasoning", "multiline"} {
		t.Run(mode, func(t *testing.T) {
			event := completedFixture(testAnswers())
			response := event["response"].(map[string]any)
			var prefix []byte
			switch mode {
			case "deltas":
				prefix = append(prefix, eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": testAnswers()[:1]})...)
				prefix = append(prefix, eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": testAnswers()[1:]})...)
				prefix = append(prefix, eventBytes(map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": testAnswers()})...)
			case "dated-model":
				response["model"] = "synthetic-model-2026-11-03"
			case "completed-items":
				response["output"] = []any{}
				prefix = eventBytes(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageOutput(testAnswers())})
			case "reasoning":
				response["output"] = []any{map[string]any{"id": "reasoning", "type": "reasoning", "summary": []any{}}, messageOutput(testAnswers())}
				prefix = eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "delta": testAnswers()})
			case "multiline":
				pretty, _ := json.MarshalIndent(event, "", " ")
				body := ": keepalive\r\nevent: response.completed\r\n"
				for _, line := range strings.Split(string(pretty), "\n") {
					body += "data: " + line + "\r\n"
				}
				body += "\r\ndata: [DONE]\r\n\r\n"
				text, err := parseStream([]byte(body), "synthetic-model")
				if err != nil || text != testAnswers() {
					t.Fatalf("valid multiline stream rejected: %v", err)
				}
				return
			}
			body := append(prefix, eventBytes(event)...)
			body = append(body, []byte("data: [DONE]\n\n")...)
			text, err := parseStream(body, "synthetic-model")
			if err != nil || text != testAnswers() {
				t.Fatalf("complete stream rejected: text=%q err=%v", text, err)
			}
		})
	}
}

func TestStreamRejectsInvalidTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(map[string]any, map[string]any)
	}{
		{"missing-id", "invalid_stream", func(_ map[string]any, r map[string]any) { delete(r, "id") }},
		{"missing-object", "invalid_stream", func(_ map[string]any, r map[string]any) { delete(r, "object") }},
		{"missing-model", "invalid_stream", func(_ map[string]any, r map[string]any) { delete(r, "model") }},
		{"wrong-model", "model_mismatch", func(_ map[string]any, r map[string]any) { r["model"] = "different-model" }},
		{"misleading-model-prefix", "model_mismatch", func(_ map[string]any, r map[string]any) { r["model"] = "synthetic-model-mini" }},
		{"invalid-snapshot", "model_mismatch", func(_ map[string]any, r map[string]any) { r["model"] = "synthetic-model-2026-02-31" }},
		{"missing-status", "invalid_stream", func(_ map[string]any, r map[string]any) { delete(r, "status") }},
		{"incomplete-status", "incomplete", func(_ map[string]any, r map[string]any) { r["status"] = "incomplete" }},
		{"failed-status", "upstream_error", func(_ map[string]any, r map[string]any) { r["status"] = "failed" }},
		{"incomplete-detail", "incomplete", func(_ map[string]any, r map[string]any) {
			r["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}},
		{"upstream-error", "upstream_error", func(_ map[string]any, r map[string]any) { r["error"] = map[string]any{"message": "synthetic-secret"} }},
		{"no-output", "invalid_stream", func(_ map[string]any, r map[string]any) { r["output"] = []any{} }},
		{"null-output", "invalid_stream", func(_ map[string]any, r map[string]any) { r["output"] = nil }},
		{"two-messages", "invalid_stream", func(_ map[string]any, r map[string]any) {
			r["output"] = []any{messageOutput(testAnswers()), messageOutput("extra text")}
		}},
		{"tool-call", "invalid_stream", func(_ map[string]any, r map[string]any) {
			r["output"] = []any{map[string]any{"type": "function_call", "name": "shell"}, messageOutput(testAnswers())}
		}},
		{"refusal", "refused", func(_ map[string]any, r map[string]any) {
			m := messageOutput("")
			m["content"] = []any{map[string]any{"type": "refusal", "refusal": "declined"}}
			r["output"] = []any{m}
		}},
		{"not-assistant", "invalid_stream", func(_ map[string]any, r map[string]any) {
			m := messageOutput(testAnswers())
			m["role"] = "user"
			r["output"] = []any{m}
		}},
		{"message-in-progress", "invalid_stream", func(_ map[string]any, r map[string]any) {
			m := messageOutput(testAnswers())
			m["status"] = "in_progress"
			r["output"] = []any{m}
		}},
		{"multiple-content-parts", "invalid_stream", func(_ map[string]any, r map[string]any) {
			m := messageOutput(testAnswers())
			m["content"] = append(m["content"].([]any), map[string]any{"type": "output_text", "text": "extra"})
			r["output"] = []any{m}
		}},
		{"explicit-failed-event", "upstream_error", func(e map[string]any, _ map[string]any) { e["type"] = "response.failed" }},
		{"explicit-incomplete-event", "incomplete", func(e map[string]any, _ map[string]any) { e["type"] = "response.incomplete" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := completedFixture(testAnswers())
			tc.change(event, event["response"].(map[string]any))
			requireParseError(t, eventBytes(event), tc.code)
		})
	}
}

func TestStreamRejectsAmbiguityAndTruncation(t *testing.T) {
	good := string(eventBytes(completedFixture(testAnswers())))
	for _, tc := range []struct{ name, body, code string }{
		{"empty", "", "incomplete"},
		{"only-done", "data: [DONE]\n\n", "incomplete"},
		{"missing-delimiter", strings.TrimRight(good, "\n"), "incomplete"},
		{"missing-terminal", string(eventBytes(map[string]any{"type": "response.output_text.delta", "delta": testAnswers(), "output_index": 0, "content_index": 0})), "incomplete"},
		{"duplicate-terminal", good + good, "invalid_stream"},
		{"duplicate-type", strings.Replace(good, `"type":"response.completed"`, `"type":"response.completed","type":"response.completed"`, 1), "invalid_stream"},
		{"duplicate-nested-status", strings.Replace(good, `"status":"completed"`, `"status":"completed","status":"completed"`, 1), "invalid_stream"},
		{"event-type-mismatch", "event: response.failed\n" + good, "invalid_stream"},
		{"invalid-utf8", good + string([]byte{0xff}), "invalid_stream"},
		{"late-delta", good + string(eventBytes(map[string]any{"type": "response.output_text.delta", "delta": "extra", "output_index": 0, "content_index": 0})), "invalid_stream"},
		{"wrong-delta", string(eventBytes(map[string]any{"type": "response.output_text.delta", "delta": "different answer", "output_index": 0, "content_index": 0})) + good, "invalid_stream"},
		{"wrong-output-done-index", string(eventBytes(map[string]any{"type": "response.output_text.done", "text": testAnswers(), "output_index": 1, "content_index": 0})) + good, "invalid_stream"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireParseError(t, []byte(tc.body), tc.code) })
	}
}

func TestStreamRejectsExtraOrChangedOutputItems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event map[string]any
	}{
		{"extra-completed-message", map[string]any{"type": "response.output_item.done", "output_index": 1, "item": messageOutput("extra output")}},
		{"extra-announced-message", map[string]any{"type": "response.output_item.added", "output_index": 1, "item": messageOutput("extra output")}},
		{"changed-completed-message", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageOutput("changed output")}},
		{"extra-content-part", map[string]any{"type": "response.content_part.done", "output_index": 0, "content_index": 1, "part": map[string]any{"type": "output_text", "text": "extra output"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append(eventBytes(tc.event), eventBytes(completedFixture(testAnswers()))...)
			requireParseError(t, body, "invalid_stream")
		})
	}
}

func TestCandyScoringRejectsAmbiguousOrIncompleteAnswers(t *testing.T) {
	for _, answer := range []string{
		"", "null", "[]", "prefix 21", "21 trailing", "21 21", "21 or 29", "21/29", "21.0", "021", "-21", "+21",
		"```\n21\n```", `"21"`, `{"answer":"21"}`, `{"q1":"4","q2":"no","q3":"red,blue","q4":"DABC","q5":"3","q6":"6"}`,
		"The answer is 21.", "答案为21", "２１", "2 1",
	} {
		score, err := scoreAnswers(answer)
		if err == nil || score != 0 {
			t.Fatalf("invalid candy answer was scored: %q => %d, %v", answer, score, err)
		}
	}
}

func FuzzParseProbeStream(f *testing.F) {
	f.Add(eventBytes(completedFixture(testAnswers())))
	f.Add([]byte("data: [DONE]\n\n"))
	f.Add([]byte("data: {\"type\":\"error\"}\n\n"))
	f.Fuzz(func(t *testing.T, body []byte) {
		answer, err := parseStream(body, "synthetic-model")
		if err != nil && answer != "" {
			t.Fatal("invalid stream retained an apparently scoreable answer")
		}
	})
}

func FuzzProbeAnswerJSON(f *testing.F) {
	f.Add(testAnswers())
	f.Add("21 or 29")
	f.Fuzz(func(t *testing.T, answer string) {
		if len(answer) > maxResponseBytes {
			t.Skip()
		}
		score, err := scoreAnswers(answer)
		if score < 0 || score > TotalQuestions || err != nil && score != 0 {
			t.Fatal("invalid answer score escaped bounds")
		}
	})
}

func TestNativeBPSOptionalFieldsMatchUpstreamContractWithoutWeakeningCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(map[string]any, map[string]any)
		truncate   bool
		delta      bool
	}{
		{name: "native-omits-object-and-message-status"},
		{name: "explicit-wrong-object", code: "invalid_stream", change: func(r, _ map[string]any) { r["object"] = "chat.completion" }},
		{name: "explicit-null-object", code: "invalid_stream", change: func(r, _ map[string]any) { r["object"] = nil }},
		{name: "explicit-empty-object", code: "invalid_stream", change: func(r, _ map[string]any) { r["object"] = "" }},
		{name: "explicit-unfinished-message", code: "invalid_stream", change: func(_, m map[string]any) { m["status"] = "in_progress" }},
		{name: "explicit-failed-message", code: "invalid_stream", change: func(_, m map[string]any) { m["status"] = "failed" }},
		{name: "explicit-null-message-status", code: "invalid_stream", change: func(_, m map[string]any) { m["status"] = nil }},
		{name: "missing-terminal-status", code: "invalid_stream", change: func(r, _ map[string]any) { delete(r, "status") }},
		{name: "incomplete-terminal-status", code: "incomplete", change: func(r, _ map[string]any) { r["status"] = "incomplete" }},
		{name: "missing-terminal-id", code: "invalid_stream", change: func(r, _ map[string]any) { delete(r, "id") }},
		{name: "missing-terminal-model", code: "invalid_stream", change: func(r, _ map[string]any) { delete(r, "model") }},
		{name: "other-terminal-model", code: "model_mismatch", change: func(r, _ map[string]any) { r["model"] = "other-model" }},
		{name: "native-tool-call", code: "invalid_stream", change: func(_, m map[string]any) { m["type"], m["name"] = "function_call", "run_officejs" }},
		{name: "truncated-terminal-frame", code: "incomplete", truncate: true},
		{name: "contradictory-stream-text", code: "invalid_stream", delta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := completedFixture(testAnswers())
			response := event["response"].(map[string]any)
			message := response["output"].([]any)[0].(map[string]any)
			delete(response, "object")
			delete(message, "status")
			if tc.change != nil {
				tc.change(response, message)
			}
			body := eventBytes(event)
			if tc.truncate {
				body = body[:len(body)-2]
			}
			if tc.delta {
				delta := eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "contradictory text"})
				body = append(delta, body...)
			}
			answer, err := parseStreamWithPolicy(body, "synthetic-model", true)
			if tc.code == "" {
				if err != nil || answer != testAnswers() {
					t.Fatal("native BPS completion was rejected", err)
				}
				if _, codexErr := parseStream(body, "synthetic-model"); codexErr == nil {
					t.Fatal("BPS optional-field policy leaked to the Codex parser")
				}
				return
			}
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != tc.code || answer != "" {
				t.Fatalf("unsafe native BPS completion returned text=%q failure=%v; want %s", answer, err, tc.code)
			}
		})
	}
}
