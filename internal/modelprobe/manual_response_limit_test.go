package modelprobe

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func drawingStreamResponse(body []byte) *transport.Response {
	response := successfulResponse(safeDrawing)
	response.URL, response.Body = nil, body
	return response
}

func drawingReasoningEvent(size int) []byte {
	return eventBytes(map[string]any{"type": "response.reasoning_text.delta", "delta": strings.Repeat("r", size)})
}

func TestPelicanResponseBudgetAcceptsLargeCompleteStreamsForBothGrants(t *testing.T) {
	longLine := drawingReasoningEvent(maxResponseBytes + 1)
	severalEvents := bytes.Repeat(drawingReasoningEvent(80<<10), 16)
	for _, method := range []string{protocol.AuthorizationMethodChatGPT, protocol.AuthorizationMethodBPS} {
		for name, prefix := range map[string][]byte{"large-line": longLine, "many-events": severalEvents} {
			t.Run(method+"/"+name, func(t *testing.T) {
				body := append(append([]byte(nil), prefix...), eventBytes(completedFixture(safeDrawing))...)
				if len(body) <= maxResponseBytes || len(body) >= pelicanMaxResponseBytes {
					t.Fatal("fixture does not exercise the drawing-only response budget")
				}
				creds := testCredentials()
				creds.AuthorizationMethod = method
				calls := 0
				client := Client{Transport: fixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
					calls++
					deadline, ok := ctx.Deadline()
					if r.MaxResponseBytes != pelicanMaxResponseBytes || !r.SingleAttempt || r.RetryReadOnly || r.RetryBeforeSend || !ok || time.Until(deadline) < 299*time.Second || r.Timeout < 299*time.Second {
						t.Fatal("drawing lost its independent size, time or single-attempt contract")
					}
					return drawingStreamResponse(body), nil
				})}
				result, err := client.Pelican(t.Context(), creds, Options{Model: "synthetic-model"})
				if err != nil || result.SVG != safeDrawing || calls != 1 {
					t.Fatal("large complete drawing stream rejected", err)
				}
				client.Transport = fixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					if r.MaxResponseBytes != maxResponseBytes {
						t.Fatal("drawing enlarged the candy response budget")
					}
					return drawingStreamResponse(body), nil
				})
				candy, err := client.Probe(t.Context(), creds, Options{Model: "synthetic-model"})
				requireProbeError(t, candy, err, "response_too_large")
			})
		}
	}
}

func TestPelicanRejectsOversizeTransportAndBufferedStreamsWithoutPartialResult(t *testing.T) {
	oversize := make([]byte, pelicanMaxResponseBytes+1)
	copy(oversize, eventBytes(completedFixture(safeDrawing)))
	for _, transportFailure := range []bool{false, true} {
		name := "buffered-response"
		if transportFailure {
			name = "transport-limit"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := Client{Transport: fixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.MaxResponseBytes != pelicanMaxResponseBytes || !r.SingleAttempt {
					t.Fatal("transport did not receive the drawing response budget")
				}
				if transportFailure {
					return drawingStreamResponse(eventBytes(completedFixture(safeDrawing))), transport.ErrResponseTooLarge
				}
				return drawingStreamResponse(oversize), nil
			})}
			result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			diagnostic := DiagnoseFailure(err)
			if result != (ManualResult{}) || diagnostic.Code != "response_too_large" || diagnostic.HTTPStatus != 200 || calls != 1 {
				t.Fatalf("oversize stream returned partial output, lost status or retried: %+v %+v calls=%d", result, diagnostic, calls)
			}
		})
	}
}

func TestDrawingStreamLimitAcceptsBoundaryAndPreservesCompletionChecks(t *testing.T) {
	terminal := eventBytes(completedFixture(safeDrawing))
	// A single ignored SSE comment exercises the scanner's selected limit as
	// well as the body limit, without enlarging the final SVG.
	boundary := append([]byte(":"+strings.Repeat("r", pelicanMaxResponseBytes-len(terminal)-3)+"\n\n"), terminal...)
	if len(boundary) != pelicanMaxResponseBytes {
		t.Fatal("incorrect boundary fixture")
	}
	text, err := parseStreamWithPolicyAndLimit(boundary, "synthetic-model", false, pelicanMaxResponseBytes)
	if err != nil || text != safeDrawing {
		t.Fatal("complete stream at the drawing limit rejected", err)
	}
	text, err = parseStreamWithPolicyAndLimit(append(boundary, '\n'), "synthetic-model", false, pelicanMaxResponseBytes)
	if text != "" || DiagnoseFailure(err).Code != "response_too_large" {
		t.Fatal("stream exceeding the drawing limit was accepted", err)
	}
	if text, err := parseStream(boundary, "synthetic-model"); text != "" || DiagnoseFailure(err).Code != "response_too_large" {
		t.Fatal("default parser inherited the drawing response budget", err)
	}
	prefix := drawingReasoningEvent(maxResponseBytes + 1)
	wrongDelta := eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "contradictory drawing"})
	for _, tc := range []struct {
		name, code string
		suffix     []byte
	}{
		{"missing-completion", "incomplete", nil},
		{"unterminated-completion", "incomplete", terminal[:len(terminal)-2]},
		{"contradictory-delta", "invalid_stream", append(wrongDelta, terminal...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append(append([]byte(nil), prefix...), tc.suffix...)
			for _, nativeBPS := range []bool{false, true} {
				text, err := parseStreamWithPolicyAndLimit(body, "synthetic-model", nativeBPS, pelicanMaxResponseBytes)
				if text != "" || DiagnoseFailure(err).Code != tc.code {
					t.Fatalf("large unsafe stream returned output: text length=%d err=%v", len(text), err)
				}
			}
		})
	}
}

func TestPelicanLargerStreamBudgetDoesNotEnlargeDocumentLimit(t *testing.T) {
	oversizeSVG := `<svg xmlns="http://www.w3.org/2000/svg"><text>` + strings.Repeat("x", PelicanMaxDocumentBytes) + `</text></svg>`
	client := Client{Transport: fixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return drawingStreamResponse(eventBytes(completedFixture(oversizeSVG))), nil
	})}
	result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
	if result != (ManualResult{}) || DiagnoseFailure(err).Code != "invalid_response" {
		t.Fatal("larger SSE budget bypassed the final document size limit", err)
	}
}

func TestPelicanDocumentBudgetAllowsAnimationLargerThanLegacyStaticImage(t *testing.T) {
	largeDocument := strings.Replace(animatedDocument, "</body>", "<!--"+strings.Repeat("x", 256<<10)+"--></body>", 1)
	client := Client{Transport: fixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return drawingStreamResponse(eventBytes(completedFixture(largeDocument))), nil
	})}
	result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
	if err != nil || result.HTML != largeDocument || result.SVG != "" {
		t.Fatal("HTML animation retained the old static image budget", err)
	}
}
