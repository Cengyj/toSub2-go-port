package modelprobe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestStreamAcceptsSSEByteOrderMarkAndLineEndings(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, bom := range []bool{false, true} {
			body := append([]byte(": keepalive\nevent: response.completed\n"), eventBytes(completedFixture("21"))...)
			body = append(body, []byte("data: [DONE]\n\n")...)
			body = bytes.ReplaceAll(body, []byte("\n"), []byte(ending))
			if bom {
				body = append([]byte{0xef, 0xbb, 0xbf}, body...)
			}
			for _, nativeBPS := range []bool{false, true} {
				answer, err := parseStreamWithPolicy(body, "synthetic-model", nativeBPS)
				if err != nil || answer != "21" {
					t.Fatalf("valid SSE encoding rejected: ending=%q bom=%t bps=%t err=%v", ending, bom, nativeBPS, err)
				}
			}
		}
	}
}

func TestSSECarriageReturnAtScannerBoundaryAndMixedEndings(t *testing.T) {
	// The CR is the last byte of the scanner's initial 4096-byte buffer.
	prefix := ":" + strings.Repeat("k", 4094) + "\r\n\r\n"
	body := append([]byte(prefix), eventBytes(completedFixture("21"))...)
	body = append(body, []byte("data: [DONE]\r\r")...)
	answer, err := parseStream(body, "synthetic-model")
	if err != nil || answer != "21" {
		t.Fatal("mixed SSE line endings split a CRLF pair", err)
	}
}

func TestSSECompatibilityDoesNotPromoteIncompleteOrContradictoryStreams(t *testing.T) {
	good := eventBytes(completedFixture("21"))
	for _, tc := range []struct {
		name, code string
		body       []byte
	}{
		{"only done", "incomplete", []byte("data: [DONE]\n\n")},
		{"unterminated terminal", "incomplete", bytes.TrimRight(good, "\n")},
		{"terminal missing blank line", "incomplete", good[:len(good)-1]},
		{"duplicate terminal", "invalid_stream", append(append([]byte(nil), good...), good...)},
		{"changed delta", "invalid_stream", append(eventBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "other"}), good...)},
		{"interior BOM", "invalid_stream", append([]byte(": comment\n\xef\xbb\xbf"), good...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append([]byte{0xef, 0xbb, 0xbf}, bytes.ReplaceAll(tc.body, []byte("\n"), []byte("\r"))...)
			answer, err := parseStream(body, "synthetic-model")
			if answer != "" || DiagnoseFailure(err).Code != tc.code {
				t.Fatalf("unsafe stream accepted or misclassified: %v", err)
			}
		})
	}
}

func TestProbeCompleteResponseSurvivesLateConnectionErrorWithoutReplay(t *testing.T) {
	for _, requestErr := range []error{io.EOF, errors.New("synthetic-private late connection failure")} {
		calls := 0
		client := Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
			calls++
			if !request.SingleAttempt || !request.CaptureResponse || request.RetryBeforeSend || request.RetryReadOnly {
				t.Fatal("probe lost its single request and bounded response contract")
			}
			response := successfulResponse("21")
			response.BodyComplete = true
			return response, requestErr
		})}
		result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
		if err != nil || result.Score != 1 || result.Verdict != "normal" || calls != 1 {
			t.Fatal("complete response lost or request replayed", err, calls)
		}
	}
}

func TestProbeLateErrorStillRequiresCompleteConsistentBoundResponse(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*transport.Response)
		err        error
	}{
		{"partial response", "network_error", func(r *transport.Response) { r.BodyComplete = false }, io.EOF},
		{"only done", "incomplete", func(r *transport.Response) { r.Body = []byte("data: [DONE]\n\n") }, io.EOF},
		{"unknown stream", "invalid_stream", func(r *transport.Response) { r.Body = []byte("data: {\"type\":\"synthetic-private\"}\n\n") }, io.EOF},
		{"wrong content type", "unexpected_content_type", func(r *transport.Response) { r.Header.Set("Content-Type", "text/html") }, io.EOF},
		{"wrong model", "model_mismatch", func(r *transport.Response) {
			r.Body = bytes.ReplaceAll(r.Body, []byte("synthetic-model"), []byte("other-model"))
		}, io.EOF},
		{"oversize error", "response_too_large", func(*transport.Response) {}, transport.ErrResponseTooLarge},
		{"explicit cancellation", "canceled", func(*transport.Response) {}, context.Canceled},
		{"deadline", "timeout", func(*transport.Response) {}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				r := successfulResponse("21")
				r.BodyComplete = true
				tc.change(r)
				return r, tc.err
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			failure := requireProbeError(t, result, err, tc.code)
			if calls != 1 || failure.HTTPStatus != http.StatusOK || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("unsafe response leaked data, lost status or replayed")
			}
		})
	}
}

func TestProbeDiagnosticDistinguishesResponseStreamAndAnswerFormats(t *testing.T) {
	for _, tc := range []struct {
		code   string
		change func(*transport.Response)
	}{
		{"unexpected_content_type", func(r *transport.Response) { r.Header.Set("Content-Type", "text/plain") }},
		{"invalid_stream", func(r *transport.Response) { r.Body = []byte("data: {\"type\":\"synthetic-private\"}\n\n") }},
		{"invalid_answer_format", func(r *transport.Response) { r.Body = eventBytes(completedFixture("答案为21 synthetic-private")) }},
	} {
		client := Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			r := successfulResponse("21")
			tc.change(r)
			return r, nil
		})}
		result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
		requireProbeError(t, result, err, tc.code)
		d := DiagnoseFailure(err)
		if d.Code != tc.code || d.HTTPStatus != 200 || d.Message == failureMessage("invalid_response") || strings.Contains(d.Message, "synthetic-private") {
			t.Fatal("diagnostic lost its safe stage-specific explanation", d)
		}
	}
}
