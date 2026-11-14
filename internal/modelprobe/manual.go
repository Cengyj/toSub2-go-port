package modelprobe

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"tosub2-go-port/internal/protocol"
)

const PelicanTask = "pelican-svg"

// PelicanTimeoutSeconds allows the full drawing to complete, independently of
// the shorter timeout configured for the scored candy probe.
const PelicanTimeoutSeconds = 300

// SSE includes reasoning, deltas and repeated terminal output. This budget is
// separate from the bounded document retained for the sandboxed preview.
const pelicanMaxResponseBytes = 16 << 20

// Match sub2api's built-in animation question and standalone delivery contract.
const PelicanPrompt = `创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的 2D 动画，你不需要任何测试

所有账号使用相同交付约定：直接返回独立 HTML，不使用 Markdown 代码块或外部依赖。只输出 HTML，不要解释。`

const PelicanMaxDocumentBytes = 2 << 20

var pelicanDocumentFence = regexp.MustCompile("(?is)^```(?:html|svg|xml)?[ \\t]*\\r?\\n(.*)\\r?\\n```$")

type ManualResult struct {
	Model      string    `json:"model"`
	HTML       string    `json:"html"`
	SVG        string    `json:"svg,omitempty"` // Legacy static SVG clients only.
	CheckedAt  time.Time `json:"checkedAt"`
	DurationMS int64     `json:"durationMs"`
}

// Pelican generates a visual sample for human review. It does not create a
// reasoning score, a saved quality measurement, or an upload-policy decision.
func (c *Client) Pelican(ctx context.Context, creds protocol.InsightCredentials, opts Options) (ManualResult, error) {
	started := time.Now()
	opts, err := normalizeOptions(opts)
	if err != nil {
		return ManualResult{}, err
	}
	opts.TimeoutSeconds = max(opts.TimeoutSeconds, PelicanTimeoutSeconds)
	text, err := c.inferTextWithResponseLimit(ctx, creds, opts, PelicanPrompt, "Return only the complete standalone HTML animation requested by the user. Use no external dependencies.", 0, pelicanMaxResponseBytes)
	if err != nil {
		return ManualResult{}, err
	}
	document, err := PelicanDocument(text)
	if err != nil {
		return ManualResult{}, withHTTPStatus(err, http.StatusOK)
	}
	// Preserve the old field only when the result also satisfies its inert SVG
	// contract. HTML and animated SVG must be displayed in the isolated iframe.
	svg, _ := SafePelicanSVG(document)
	return ManualResult{Model: strings.TrimSpace(opts.Model), HTML: document, SVG: svg, CheckedAt: time.Now().UTC(), DurationMS: time.Since(started).Milliseconds()}, nil
}

// PelicanDocument validates document boundaries and size; it does not sanitize
// active content. Callers must render it only in a sandboxed, network-disabled
// iframe, never by inserting it into the application's document.
func PelicanDocument(text string) (string, error) {
	bad := func() (string, error) { return "", &Error{Code: "invalid_response"} }
	text = strings.TrimSpace(text)
	if match := pelicanDocumentFence.FindStringSubmatch(text); match != nil {
		text = strings.TrimSpace(match[1])
	}
	if len(text) == 0 || len(text) > PelicanMaxDocumentBytes || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return bad()
	}
	if strings.HasPrefix(strings.ToLower(text), "<svg") {
		if completePelicanSVG(text) {
			return text, nil
		}
		return bad()
	}
	z := html.NewTokenizer(strings.NewReader(text))
	opened, closed, doctype := false, false, false
	active := ""
	nodes := 0
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF || !opened || !closed || active != "" {
				return bad()
			}
			return text, nil
		}
		token := z.Token()
		switch kind {
		case html.CommentToken:
			// The tokenizer repairs an EOF inside a comment. A completed response
			// must still include its closing delimiter, even after </html>.
			if raw := string(z.Raw()); strings.HasPrefix(raw, "<!--") && !strings.HasSuffix(raw, "-->") && !strings.HasSuffix(raw, "--!>") {
				return bad()
			}
			continue
		case html.DoctypeToken:
			if opened || closed || doctype || strings.ToLower(token.Data) != "html" || len(token.Attr) != 0 {
				return bad()
			}
			doctype = true
		case html.TextToken:
			if (!opened || closed) && strings.TrimSpace(token.Data) != "" {
				return bad()
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			nodes++
			if closed || nodes > 50000 || !opened && token.Data != "html" {
				return bad()
			}
			if token.Data == "html" {
				if opened || kind == html.SelfClosingTagToken {
					return bad()
				}
				opened = true
			}
			if token.Data == "script" || token.Data == "style" {
				if active != "" || kind == html.SelfClosingTagToken {
					return bad()
				}
				active = token.Data
			}
		case html.EndTagToken:
			if !opened || closed {
				return bad()
			}
			if token.Data == active {
				active = ""
			}
			if token.Data == "html" {
				if active != "" {
					return bad()
				}
				closed = true
			}
		}
	}
}

func completePelicanSVG(text string) bool {
	dec := xml.NewDecoder(strings.NewReader(text))
	depth, roots, nodes := 0, 0, 0
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			depth++
			nodes++
			if depth > 128 || nodes > 50000 {
				return false
			}
			if depth == 1 {
				roots++
				if roots != 1 || token.Name.Local != "svg" || token.Name.Space != "" && token.Name.Space != "http://www.w3.org/2000/svg" {
					return false
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive, xml.ProcInst:
			return false
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return false
			}
		}
	}
}

var localSVGURL = regexp.MustCompile(`(?i)url\(\s*#[a-z0-9_.:-]+\s*\)`)
var svgElements = stringSet("svg g defs path circle ellipse line polyline polygon rect text tspan title desc linearGradient radialGradient stop clipPath mask pattern use marker")
var svgAttributes = stringSet("id xmlns viewBox width height x y x1 y1 x2 y2 cx cy r rx ry d points fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity transform font-family font-size font-weight font-style text-anchor dominant-baseline letter-spacing dx dy rotate textLength lengthAdjust gradientUnits gradientTransform spreadMethod offset stop-color stop-opacity clip-path clip-rule mask maskUnits maskContentUnits patternUnits patternContentUnits patternTransform preserveAspectRatio href marker-start marker-mid marker-end markerWidth markerHeight refX refY orient style role aria-label")
var svgStyles = stringSet("fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity font-family font-size font-weight font-style text-anchor dominant-baseline letter-spacing stop-color stop-opacity clip-path mask")

func stringSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Fields(s) {
		out[v] = true
	}
	return out
}

// SafePelicanSVG preserves compatibility with the original static result field.
// Animated documents use PelicanDocument and require a sandboxed preview.
func SafePelicanSVG(text string) (string, error) {
	bad := func() (string, error) { return "", &Error{Code: "invalid_response"} }
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```svg\n") && strings.HasSuffix(text, "\n```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```svg\n"), "\n```"))
	}
	if len(text) == 0 || len(text) > 256<<10 {
		return bad()
	}
	dec := xml.NewDecoder(strings.NewReader(text))
	depth, nodes, roots := 0, 0, 0
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bad()
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			nodes++
			if depth > 64 || nodes > 10000 || !svgElements[t.Name.Local] || t.Name.Space != "http://www.w3.org/2000/svg" {
				return bad()
			}
			if depth == 1 {
				roots++
				if roots != 1 || t.Name.Local != "svg" {
					return bad()
				}
			}
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" {
					if a.Name.Local != "xlink" || a.Value != "http://www.w3.org/1999/xlink" {
						return bad()
					}
					continue
				}
				if a.Name.Space != "" && !(a.Name.Space == "http://www.w3.org/1999/xlink" && a.Name.Local == "href") {
					return bad()
				}
				if a.Name.Local == "xmlns" {
					if a.Value != "http://www.w3.org/2000/svg" {
						return bad()
					}
					continue
				}
				if !svgAttributes[a.Name.Local] || !safeSVGValue(a.Value) {
					return bad()
				}
				if a.Name.Local == "href" && (!strings.HasPrefix(a.Value, "#") || len(a.Value) < 2 || strings.ContainsAny(a.Value, " \t\r\n")) {
					return bad()
				}
				if a.Name.Local == "style" {
					for _, decl := range strings.Split(a.Value, ";") {
						if strings.TrimSpace(decl) == "" {
							continue
						}
						parts := strings.SplitN(decl, ":", 2)
						if len(parts) != 2 || !svgStyles[strings.TrimSpace(strings.ToLower(parts[0]))] {
							return bad()
						}
					}
				}
			}
		case xml.EndElement:
			depth--
			if depth < 0 {
				return bad()
			}
		case xml.Directive, xml.ProcInst:
			return bad()
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return bad()
			}
		}
	}
	if roots != 1 || depth != 0 {
		return bad()
	}
	return text, nil
}
func safeSVGValue(value string) bool {
	lower := strings.ToLower(value)
	if strings.ContainsAny(lower, "\\<>@`\x00") || strings.Contains(lower, "javascript:") || strings.Contains(lower, "data:") || strings.Contains(lower, "http:") || strings.Contains(lower, "https:") || strings.Contains(lower, "//") || strings.Contains(lower, "expression(") {
		return false
	}
	// The namespace itself is the only URL attribute accepted separately.
	remaining := localSVGURL.ReplaceAllString(lower, "")
	return !strings.Contains(remaining, "url")
}
