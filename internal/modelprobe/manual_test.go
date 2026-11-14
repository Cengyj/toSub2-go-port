package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

const safeDrawing = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 480"><defs><linearGradient id="body"><stop offset="0" stop-color="#fff"/></linearGradient></defs><g fill="url(#body)"><circle cx="100" cy="200" r="40"/><path d="M0 0L10 10" style="stroke: #222; fill: none"/></g></svg>`

const animatedDocument = `<!doctype html><html lang="zh"><head><meta charset="utf-8"><style>svg{width:100%}.wheel{animation:spin 2s linear infinite}@keyframes spin{to{transform:rotate(360deg)}}</style></head><body><svg viewBox="0 0 640 480"><circle class="wheel" cx="100" cy="200" r="40"/></svg><script>document.querySelector('svg').dataset.animated = 'yes';requestAnimationFrame(function frame(){requestAnimationFrame(frame)})</script></body></html>`

func TestPelicanDocumentAcceptsCompleteAnimationWithoutPretendingToSanitize(t *testing.T) {
	for _, document := range []string{animatedDocument, strings.ToUpper("<!doctype html>") + strings.TrimPrefix(animatedDocument, "<!doctype html>"), safeDrawing, `<svg xmlns="http://www.w3.org/2000/svg"><circle><animate attributeName="cx" values="10;20;10" dur="1s" repeatCount="indefinite"/></circle></svg>`, "```html\n" + animatedDocument + "\n```", "```HTML\r\n" + animatedDocument + "\r\n```"} {
		if got, err := PelicanDocument(document); err != nil || got == "" {
			t.Fatalf("complete animation rejected: %v", err)
		}
	}
	got, err := PelicanDocument(animatedDocument)
	if err != nil || got != animatedDocument {
		t.Fatal("document validator altered the animation instead of preserving source", err)
	}
	for _, document := range []string{
		"", "Here is the HTML: " + animatedDocument, animatedDocument + " explanation", animatedDocument + animatedDocument,
		`<!doctype html><html><body><svg></svg></body>`, `<html><body><script>const unfinished = '</html>'`,
		`<html><style>body{background:white}</html>`, `<html><body></body></html><script>alert(1)</script>`,
		animatedDocument + "<!--unfinished comment",
		`<html><html></html></html>`, `<!doctype html><!doctype html><html></html>`, `<html/>`,
		`<svg><circle/></svg><svg/>`, `<svg><circle></svg>`, `<svg><script>`, `<svg xmlns="wrong"/>`,
		"```html\n" + animatedDocument, animatedDocument + string([]byte{0xff}), strings.Replace(animatedDocument, "yes", "\x00", 1),
		`<html><body>` + strings.Repeat("x", PelicanMaxDocumentBytes) + `</body></html>`,
	} {
		if got, err := PelicanDocument(document); err == nil || got != "" {
			t.Fatalf("incomplete, oversized or multiple document accepted: length=%d", len(document))
		}
	}
	prefix, suffix := "<!doctype html><html><body>", "</body></html>"
	boundary := prefix + strings.Repeat("x", PelicanMaxDocumentBytes-len(prefix)-len(suffix)) + suffix
	if got, err := PelicanDocument(boundary); err != nil || got != boundary {
		t.Fatal("complete document at the storage boundary was rejected", err)
	}
}

func TestPelicanAnimationQuestionUsesStandaloneHTMLForBothGrants(t *testing.T) {
	if !strings.Contains(PelicanPrompt, "SVG 绘制一个鹈鹕骑自行车的 2D 动画") || !strings.Contains(PelicanPrompt, "直接返回独立 HTML") || strings.Contains(PelicanPrompt, "不使用脚本") {
		t.Fatal("animation question reverted to the static SVG contract")
	}
	for _, method := range []string{"chatgpt", "bps"} {
		t.Run(method, func(t *testing.T) {
			creds := testCredentials()
			creds.AuthorizationMethod = method
			calls := 0
			client := Client{Transport: fixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				calls++
				if !request.SingleAttempt || !strings.Contains(string(request.Body), "2D 动画") {
					t.Fatal("HTML animation did not use the fixed single-attempt prompt")
				}
				response := successfulResponse(animatedDocument)
				if method == "bps" {
					response.URL, _ = url.Parse(bpsResponsesURL)
				}
				return response, nil
			})}
			result, err := client.Pelican(t.Context(), creds, Options{Model: "synthetic-model"})
			if err != nil || result.HTML != animatedDocument || result.SVG != "" || calls != 1 {
				t.Fatal("complete HTML animation was lost or labelled as an inert image", err)
			}
		})
	}
}

func TestPelicanSVGAllowsInertDrawingAndRejectsActiveOrExternalContent(t *testing.T) {
	for _, text := range []string{safeDrawing, "```svg\n" + safeDrawing + "\n```"} {
		if _, err := SafePelicanSVG(text); err != nil {
			t.Fatal("inert drawing rejected", err)
		}
	}
	for _, text := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><p>html</p></foreignObject></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://example.test/a"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><use href="https://example.test/a#x"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><use href="&#106;avascript:alert(1)"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><path fill="url(https://example.test/a)"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><path style="fill: u\72l(https://example.test/a)"/></svg>`,
		`<!DOCTYPE svg [<!ENTITY x "value">]><svg xmlns="http://www.w3.org/2000/svg"><text>&x;</text></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"/><svg xmlns="http://www.w3.org/2000/svg"/>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><animate attributeName="href" values="https://example.test"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><g xmlns="http://www.w3.org/1999/xhtml"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><text>` + strings.Repeat("x", 256<<10) + `</text></svg>`,
	} {
		if svg, err := SafePelicanSVG(text); err == nil || svg != "" {
			t.Fatal("unsafe drawing accepted")
		}
	}
}
func TestPelicanUsesSingleInferenceWithoutAutomaticScoring(t *testing.T) {
	calls := 0
	client := Client{Transport: fixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		calls++
		if !r.SingleAttempt || !strings.Contains(string(r.Body), "鹈鹕") || strings.Contains(string(r.Body), testCredentials().AccessToken) {
			t.Fatal("manual inference violated fixed prompt or privacy contract")
		}
		var body map[string]any
		if json.Unmarshal(r.Body, &body) != nil {
			t.Fatal("bad body")
		}
		if _, ok := body["max_output_tokens"]; ok {
			t.Fatal("manual inference added unsupported Codex token limit")
		}
		return successfulResponse(safeDrawing), nil
	})}
	result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
	if err != nil || result.SVG != safeDrawing || calls != 1 || result.CheckedAt.IsZero() {
		t.Fatal("manual drawing failed", err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "score") || strings.Contains(string(data), "verdict") {
		t.Fatal("manual result became an automatic quality score")
	}
}

func TestPelicanUsesIndependentDrawingTimeout(t *testing.T) {
	for name, configured := range map[string]int{"default": 0, "short": 1, "candy-default": 90, "maximum": 300} {
		t.Run(name, func(t *testing.T) {
			client := Client{Transport: fixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < 299*time.Second || remaining > 300*time.Second || r.Timeout < 299*time.Second || r.Timeout > 300*time.Second {
					t.Fatalf("drawing lost its five-minute budget: context=%s request=%s", remaining, r.Timeout)
				}
				return successfulResponse(safeDrawing), nil
			})}
			if result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model", TimeoutSeconds: configured}); err != nil || result.SVG != safeDrawing {
				t.Fatal("drawing failed", err)
			}
		})
	}
}

func TestPelicanTimeoutStillValidatesOptionsAndHonorsCallerCancellation(t *testing.T) {
	calls := 0
	client := Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return successfulResponse(safeDrawing), nil
	})}
	for _, configured := range []int{-1, MaxTimeoutSeconds + 1} {
		result, err := client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model", TimeoutSeconds: configured})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_options" || result != (ManualResult{}) {
			t.Fatal("invalid timeout was accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Pelican(ctx, testCredentials(), Options{Model: "synthetic-model"}); DiagnoseFailure(err).Code != "canceled" {
		t.Fatal("caller cancellation was ignored", err)
	}
	if calls != 0 {
		t.Fatal("invalid or canceled drawing dispatched a request")
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client.Transport = fixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || r.Timeout <= 0 || r.Timeout > time.Second {
			t.Fatal("drawing overrode the caller's earlier deadline")
		}
		return nil, context.DeadlineExceeded
	})
	if _, err := client.Pelican(ctx, testCredentials(), Options{Model: "synthetic-model"}); DiagnoseFailure(err).Code != "timeout" {
		t.Fatal("deadline failure was not preserved", err)
	}
}
