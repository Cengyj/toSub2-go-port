// Package mail extracts one-time passwords from mailbox API responses.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

var otpRE = regexp.MustCompile(`[0-9]{6}`)
var timestampRE = regexp.MustCompile(`\b(20[0-9]{2}[-/.][0-9]{1,2}[-/.][0-9]{1,2}[T\s][0-9]{1,2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]{1,9})?)?(?:\s?(?:Z|[+-][0-9]{2}:?[0-9]{2}))?)\b`)
var dateTimeSpaceRE = regexp.MustCompile(`^([0-9]{4}[-/.][0-9]{1,2}[-/.][0-9]{1,2})\s+`)
var codeFieldRE = regexp.MustCompile(`(?i)code|otp|verification|verify|验证码|校验码`)
var contentFieldRE = regexp.MustCompile(`(?i)body|content|text|html|message|subject|snippet|preview|payload|data|mail`)
var codeContextRE = regexp.MustCompile(`(?i)openai|chatgpt|verification|verify|security\s*code|one[- ]time|\botp\b|验证码|校验码|一次性`)
var messageIDFieldRE = regexp.MustCompile(`(?i)^(id|uid|uuid|message_?id|mail_?id|email_?id)$`)
var messageTimeFieldRE = regexp.MustCompile(`(?i)(created|received|delivered|sent|arrival|timestamp|date|time|updated)(_?at|_?on|_?time)?$`)
var htmlIgnoredRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>|<style\b[^>]*>.*?</style>`)
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

type Candidate struct {
	Code       string  `json:"code"`
	Score      float64 `json:"score"`
	Key        string  `json:"key"`
	ReceivedAt int64   `json:"receivedAt,omitempty"`
}
type RequestConfig struct {
	Method  string
	Headers map[string]string
	Body    string
	OnRetry func(transport.ReadRetryEvent) `json:"-"`
}

func ValidateURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// FetchCandidates fetches at most 2 MiB and honours context cancellation.
func FetchCandidates(ctx context.Context, rawURL string, cfg RequestConfig, client *http.Client) ([]Candidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if !ValidateURL(rawURL) {
		return nil, permanentRequestError("mail API URL must be HTTP or HTTPS")
	}
	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return nil, permanentRequestError("unsupported mail method")
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, permanentRequestError("invalid mail API request")
	}
	req.Header.Set("Accept", "application/json, text/plain, text/html, */*")
	req.Header.Set("User-Agent", "Mozilla/5.0 ChatGPT-Onboarding-Console/1.0")
	for k, v := range cfg.Headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		req.Header.Set(k, v)
	}
	if method == http.MethodPost && cfg.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = http.DefaultClient
	}
	requestBody := []byte(nil)
	if method == http.MethodPost {
		requestBody = []byte(cfg.Body)
	}
	request := transport.Request{Method: method, URL: rawURL, Headers: req.Header, Body: requestBody, Timeout: 12 * time.Second, RetryReadOnly: true}
	// Mail APIs are explicit queries even when configured as POST. Follow
	// their redirects ourselves so each connection has one retry budget and
	// no HTTP library can silently replay a failed request.
	for redirects := 0; ; redirects++ {
		resp, err := transport.DoHTTPWithRetry(ctx, client, request, 2<<20, transport.DefaultNetworkRetries, transport.DefaultNetworkRetryDelay, cfg.OnRetry)
		if err != nil {
			return nil, &requestError{cause: err, terminal: !transport.IsRetryableReadError(err)}
		}
		if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect {
			if redirects >= 9 {
				return nil, permanentRequestError("mail API redirect limit exceeded")
			}
			current, err := url.Parse(request.URL)
			if err != nil {
				return nil, permanentRequestError("invalid mail API redirect")
			}
			next, err := current.Parse(resp.Header.Get("Location"))
			if err != nil || !ValidateURL(next.String()) || resp.Header.Get("Location") == "" {
				return nil, permanentRequestError("invalid mail API redirect")
			}
			if !strings.EqualFold(current.Host, next.Host) || !strings.EqualFold(current.Scheme, next.Scheme) {
				request.Headers = request.Headers.Clone()
				for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key"} {
					request.Headers.Del(name)
				}
			}
			if resp.StatusCode == http.StatusSeeOther || (resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && request.Method == http.MethodPost {
				request.Method, request.Body = http.MethodGet, nil
				request.Headers.Del("Content-Type")
			}
			request.URL = next.String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, permanentRequestError(fmt.Sprintf("mail API returned HTTP %d", resp.StatusCode))
		}
		return ExtractCandidates(string(resp.Body)), nil
	}
}

type requestError struct {
	cause    error
	terminal bool
	message  string
}

func (e *requestError) Error() string {
	if e.message != "" {
		return e.message
	}
	if e.terminal && e.cause != nil {
		return "mail API request failed: " + e.cause.Error()
	}
	return "mail API request failed: " + transport.ReadFailureMessage(e.cause)
}
func (e *requestError) Unwrap() error  { return e.cause }
func (e *requestError) Terminal() bool { return e.terminal }

func permanentRequestError(message string) error {
	return &requestError{message: message, terminal: true}
}

// IsTerminal tells polling callers when repeating the same configuration or
// permanent server rejection cannot recover without user input.
func IsTerminal(err error) bool {
	var terminal interface{ Terminal() bool }
	return errors.As(err, &terminal) && terminal.Terminal()
}

func FilterByRequestTime(in []Candidate, cutoff int64) []Candidate {
	if cutoff <= 0 {
		return in
	}
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if c.ReceivedAt == 0 || c.ReceivedAt >= cutoff {
			out = append(out, c)
		}
	}
	return out
}

func ExtractCandidates(raw string) []Candidate {
	var root any
	sources := []source{}
	if json.Unmarshal([]byte(raw), &root) == nil {
		// Many mailbox APIs return the code as a plain numeric response,
		// which is also valid JSON. Preserve it as text at the root.
		if _, numeric := root.(float64); numeric {
			sources = append(sources, source{Path: "$raw", Text: raw})
		} else {
			collect(root, "$", nil, 0, &sources, map[any]bool{})
		}
	} else {
		sources = append(sources, source{Path: "$raw", Text: raw})
	}
	seen := map[string]Candidate{}
	for i, s := range sources {
		for _, variant := range variants(s.Text) {
			text := normalize(variant)
			matches := otpRE.FindAllStringIndex(text, -1)
			timestamps := timestampRE.FindAllStringIndex(text, -1)
			for _, m := range matches {
				// RE2 has no lookahead. Check boundaries without consuming the
				// separator, so adjacent six-digit codes are both considered.
				if (m[0] > 0 && text[m[0]-1] >= '0' && text[m[0]-1] <= '9') || (m[1] < len(text) && text[m[1]] >= '0' && text[m[1]] <= '9') {
					continue
				}
				insideTimestamp := false
				for _, timestamp := range timestamps {
					if m[0] >= timestamp[0] && m[1] <= timestamp[1] {
						insideTimestamp = true
						break
					}
				}
				if insideTimestamp {
					continue
				}
				code := text[m[0]:m[1]]
				contextStart := m[0] - 120
				if contextStart < 0 {
					contextStart = 0
				}
				contextEnd := m[1] + 120
				if contextEnd > len(text) {
					contextEnd = len(text)
				}
				ctx := text[contextStart:contextEnd]
				score := 0.0
				if codeContextRE.MatchString(ctx) {
					score += 40
				}
				field := s.Path
				if p := strings.LastIndexAny(field, ".["); p >= 0 {
					field = strings.Trim(field[p+1:], "]")
				}
				if codeFieldRE.MatchString(field) {
					score += 40
				}
				if contentFieldRE.MatchString(field) {
					score += 12
				}
				if strings.TrimSpace(text) == code {
					score += 30
				}
				// Traversing message metadata is necessary for inherited IDs and
				// timestamps, but UUID/date fields themselves are not OTP text.
				if score <= 0 {
					continue
				}
				if len(strings.TrimSpace(text)) <= 500 {
					score += 4
				}
				score -= float64(min(i, 20)) * .01
				received := s.Time
				if received == 0 {
					received = nearestTime(text, m[0])
				}
				ident := "context:" + fingerprint(ctx)
				if received != 0 {
					ident = fmt.Sprintf("time:%d", received)
				}
				if s.ID != "" {
					stamp := "unknown"
					if received != 0 {
						stamp = strconv.FormatInt(received, 10)
					}
					ident = "id:" + s.ID + "|time:" + stamp
				}
				sum := sha256.Sum256([]byte(code + "|" + ident))
				key := base64.RawURLEncoding.EncodeToString(sum[:])[:24]
				c := Candidate{Code: code, Score: score, Key: key, ReceivedAt: received}
				if old, ok := seen[key]; !ok || c.Score > old.Score {
					seen[key] = c
				}
			}
		}
	}
	out := make([]Candidate, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ReceivedAt != 0 && out[j].ReceivedAt != 0 && out[i].ReceivedAt != out[j].ReceivedAt {
			return out[i].ReceivedAt > out[j].ReceivedAt
		}
		if (out[i].ReceivedAt != 0) != (out[j].ReceivedAt != 0) {
			return out[i].ReceivedAt != 0
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Key < out[j].Key
	})
	return out
}

type source struct {
	Path, Text, ID string
	Time           int64
}

func collect(v any, path string, meta *source, depth int, out *[]source, seen map[any]bool) {
	if depth > 30 {
		return
	}
	switch x := v.(type) {
	case string:
		s := source{Path: path, Text: x}
		if meta != nil {
			s.ID, s.Time = meta.ID, meta.Time
		}
		*out = append(*out, s)
	case float64:
		if x >= 100000 && x <= 999999 && x == float64(int64(x)) && codeFieldRE.MatchString(path) {
			s := source{Path: path, Text: strconv.Itoa(int(x))}
			if meta != nil {
				s.ID, s.Time = meta.ID, meta.Time
			}
			*out = append(*out, s)
		}
	case []any:
		for i, e := range x {
			collect(e, fmt.Sprintf("%s[%d]", path, i), meta, depth+1, out, seen)
		}
	case map[string]any:
		s := source{}
		if meta != nil {
			s = *meta
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// A message's own metadata overrides a containing API envelope.
		// Stable priorities prevent expiry/updated timestamps from making
		// an old message look new when Go randomizes map iteration.
		localID := ""
		localTime, timePriority := int64(0), 100
		for _, k := range keys {
			e := x[k]
			if messageIDFieldRE.MatchString(k) && localID == "" {
				switch e.(type) {
				case string, float64:
					localID = strings.TrimSpace(fmt.Sprint(e))
				}
			}
			if priority := timestampPriority(k); priority < timePriority {
				if stamp := parseTime(e); stamp != 0 {
					localTime, timePriority = stamp, priority
				}
			}
		}
		if localID != "" {
			s.ID = localID
		}
		if localTime != 0 {
			s.Time = localTime
		}
		for _, k := range keys {
			e := x[k]
			// IDs, time values and expiry dates are metadata, even when
			// their names contain code or their fractions have six digits.
			if messageIDFieldRE.MatchString(k) || messageTimeFieldRE.MatchString(k) {
				continue
			}
			collect(e, path+"."+k, &s, depth+1, out, seen)
		}
	}
}

func timestampPriority(key string) int {
	key = strings.ToLower(key)
	if !messageTimeFieldRE.MatchString(key) || strings.Contains(key, "expir") {
		return 100
	}
	for i, preferred := range []string{"received", "delivered", "arrival", "code_time", "sent", "created", "timestamp", "date", "time", "updated"} {
		if strings.Contains(key, preferred) {
			return i
		}
	}
	return 100
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:24]
}
func variants(s string) []string {
	out := []string{s}
	// Mail APIs commonly return MIME Base64 with folded lines or indentation.
	// Validate the compact representation, as the legacy extractor does.
	t := strings.Join(strings.Fields(s), "")
	if len(t) >= 24 && len(t)%4 == 0 {
		if b, e := base64.StdEncoding.DecodeString(t); e == nil && len(b) > 0 {
			printable := 0
			for _, r := range string(b) {
				if r == '\n' || r == '\r' || r == '\t' || (r >= 32 && r < 127) || (r >= 0x4e00 && r <= 0x9fff) {
					printable++
				}
			}
			if float64(printable)/float64(len([]rune(string(b)))) > .85 {
				out = append(out, string(b))
			}
		}
	}
	return out
}
func normalize(s string) string {
	s = htmlIgnoredRE.ReplaceAllString(html.UnescapeString(s), " ")
	s = htmlTagRE.ReplaceAllString(s, "\n")
	return strings.Map(func(r rune) rune {
		if r == '\u00a0' || (r >= '\u2000' && r <= '\u200b') || r == '\u2028' || r == '\u2029' || r == '\u3000' {
			return ' '
		}
		return r
	}, s)
}
func nearestTime(s string, at int) int64 {
	ms := timestampRE.FindAllStringSubmatchIndex(s, -1)
	var best int64
	dist := int(^uint(0) >> 1)
	for _, m := range ms {
		t := parseTime(s[m[2]:m[3]])
		if t == 0 {
			continue
		}
		d := m[0] - at
		if d < 0 {
			d = -d
		}
		if d < dist {
			dist = d
			best = t
		}
	}
	return best
}
func parseTime(v any) int64 {
	var t time.Time
	switch x := v.(type) {
	case float64:
		n := int64(x)
		if n < 1e10 {
			n *= 1000
		}
		t = time.UnixMilli(n)
	case string:
		s := strings.TrimSpace(x)
		if n, e := strconv.ParseInt(s, 10, 64); e == nil {
			if n < 1e10 {
				n *= 1000
			}
			t = time.UnixMilli(n)
		} else {
			t = parseDateText(s)
		}
	}
	if t.IsZero() || t.Year() < 2000 || t.Year() > 2100 {
		return 0
	}
	return t.UnixMilli()
}

func parseDateText(s string) time.Time {
	normalized := dateTimeSpaceRE.ReplaceAllString(s, "${1}T")
	// Preserve Date.parse's explicit-zone formats, including offsets without
	// a colon and ISO timestamps that omit seconds. Date-only ISO stays UTC.
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700", "2006-01-02"} {
		if t, err := time.Parse(layout, normalized); err == nil {
			return t
		}
	}
	if t, err := time.Parse(time.RFC1123Z, s); err == nil {
		return t
	}
	if t, err := http.ParseTime(s); err == nil {
		return t
	}
	// The legacy runtime interprets timezone-less datetimes in its local
	// timezone. Assuming UTC makes old mail appear new outside UTC regions.
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006/01/02T15:04:05", "2006.01.02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, normalized, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}
