// Package providers implements SMS leases without persisting provider secrets.
package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/mail"
)

type NumberLease struct {
	ID       string            `json:"id"`
	Number   string            `json:"number"`
	Provider string            `json:"provider"`
	Metadata map[string]string `json:"metadata,omitempty"`
}
type SmsProvider interface {
	GetNumber(context.Context) (NumberLease, error)
	GetCode(context.Context, NumberLease) (string, error)
	Release(context.Context, NumberLease) error
	Complete(context.Context, NumberLease) error
}
type ReadyProvider interface {
	MarkReady(context.Context, NumberLease) error
}

var ErrWaiting = errors.New("verification code is not ready")
var codeRE = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)
var phoneRE = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
var countryRE = regexp.MustCompile(`^[0-9]{1,5}$`)
var serviceRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
var lubanServiceRE = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,80}$`)
var maxPriceRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,6})?$`)
var apiKeyRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{8,256}$`)

// ProviderError carries retry semantics without retaining provider response
// bodies, URLs, API keys or one-time codes.
type ProviderError struct {
	message  string
	terminal bool
}

func (e *ProviderError) Error() string  { return e.message }
func (e *ProviderError) Terminal() bool { return e.terminal }
func providerError(message string, terminal bool) error {
	return &ProviderError{message: message, terminal: terminal}
}

// IsTerminal matches the original providers' terminal flag. Polling callers
// stop automatic retries but can still offer manual input or a new number.
func IsTerminal(err error) bool {
	var e interface{ Terminal() bool }
	return errors.As(err, &e) && e.Terminal()
}

func safeRequest(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, providerError("SMS API URL must use HTTP or HTTPS", true)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return nil, providerError("invalid SMS request", true)
	}
	req.Header.Set("Accept", "application/json,text/plain,text/html")
	c := &http.Client{Timeout: 12 * time.Second}
	if client != nil {
		*c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := c.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, providerError("SMS provider request failed", false)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, providerError(fmt.Sprintf("SMS provider returned HTTP %d", resp.StatusCode), resp.StatusCode < 500)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if e != nil {
		return nil, providerError("SMS response could not be read", false)
	}
	if len(b) > 2<<20 {
		return nil, providerError("SMS response exceeds 2 MiB", true)
	}
	return b, nil
}
func normalizeNumber(raw string) (string, error) {
	p := strings.Map(func(r rune) rune {
		if strings.ContainsRune(" ()-\t\r\n", r) {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	} else if !strings.HasPrefix(p, "+") {
		p = "+" + p
	}
	if !phoneRE.MatchString(p) {
		return "", providerError("SMS provider returned invalid E.164 number", true)
	}
	return p, nil
}
func asFloat(v any) (float64, bool) {
	var f float64
	var e error
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case json.Number:
		f, e = x.Float64()
	case string:
		f, e = strconv.ParseFloat(strings.TrimSpace(x), 64)
	default:
		return 0, false
	}
	return f, e == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}
func valueString(v any) string {
	if v == nil {
		return ""
	}
	if number, ok := v.(float64); ok {
		// JSON phone numbers and activation IDs can be numeric. fmt.Sprint
		// switches these to exponent notation, corrupting the next API call.
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// HTTPProvider preserves the LubanSMS v2 JSON API.
type HTTPProvider struct {
	ID, APIKey, Base, Service, Country string
	Client                             *http.Client
}

func (p *HTTPProvider) call(ctx context.Context, endpoint string, q url.Values) (map[string]any, error) {
	key := strings.TrimSpace(p.APIKey)
	if !apiKeyRE.MatchString(key) {
		return nil, providerError("invalid LubanSMS API key", true)
	}
	base := p.Base
	if base == "" {
		base = "https://lubansms.com/v2/api/"
	}
	u, e := url.Parse(base)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, providerError("invalid SMS provider URL", true)
	}
	u = u.ResolveReference(&url.URL{Path: endpoint})
	q.Set("apikey", key)
	u.RawQuery = q.Encode()
	b, e := safeRequest(ctx, p.Client, u.String())
	if e != nil {
		return nil, e
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, providerError("LubanSMS returned invalid JSON", false)
	}
	code, ok := asFloat(m["code"])
	if !ok || code != 0 {
		return nil, providerError(fmt.Sprintf("LubanSMS %s failed (provider code %s)", endpoint, safeCode(m["code"])), code == 400 || code == 401)
	}
	return m, nil
}
func safeCode(v any) string {
	// Arbitrary provider strings can echo the API key or an OTP. Only small
	// numeric API status codes belong in user-visible errors.
	if code, ok := asFloat(v); ok && code >= 0 && code <= 9999 && code == float64(int(code)) {
		return strconv.Itoa(int(code))
	}
	return "unknown"
}
func (p *HTTPProvider) GetNumber(ctx context.Context) (NumberLease, error) {
	if !lubanServiceRE.MatchString(strings.TrimSpace(p.Service)) {
		return NumberLease{}, providerError("invalid LubanSMS service ID", true)
	}
	requested := time.Now().Add(-5 * time.Second).UnixMilli()
	m, e := p.call(ctx, "getNumber", url.Values{"service_id": {strings.TrimSpace(p.Service)}})
	if e != nil {
		return NumberLease{}, e
	}
	id := valueString(m["request_id"])
	n, e := normalizeNumber(valueString(m["number"]))
	if e != nil || id == "" {
		return NumberLease{}, providerError("LubanSMS returned no valid number lease", true)
	}
	return NumberLease{ID: id, Number: n, Provider: "luban", Metadata: map[string]string{"requested_at": strconv.FormatInt(requested, 10)}}, nil
}
func (p *HTTPProvider) GetCode(ctx context.Context, l NumberLease) (string, error) {
	m, e := p.call(ctx, "getSms", url.Values{"request_id": {l.ID}})
	if e != nil {
		return "", e
	}
	if strings.EqualFold(valueString(m["msg"]), "wait") {
		return "", ErrWaiting
	}
	cutoff, _ := strconv.ParseInt(l.Metadata["requested_at"], 10, 64)
	code, stale := freshLubanCode(m, cutoff)
	if code != "" {
		return code, nil
	}
	if stale {
		return "", ErrWaiting
	}
	terminal := m["sms_code"] != nil || strings.EqualFold(valueString(m["msg"]), "success")
	return "", providerError("LubanSMS response does not contain a six digit code", terminal)
}
func (p *HTTPProvider) Release(ctx context.Context, l NumberLease) error {
	_, e := p.call(ctx, "setStatus", url.Values{"request_id": {l.ID}, "status": {"reject"}})
	return e
}

// LubanSMS exposes release only; success does not invent a setStatus=complete.
func (p *HTTPProvider) Complete(context.Context, NumberLease) error { return nil }

// SMSBowerProvider implements the documented handler_api.php protocol.
type SMSBowerProvider struct {
	APIKey, Base, Service, Country, MaxPrice string
	Client                                   *http.Client
}

func NewSMSBowerProvider(key, base, service, country string) *SMSBowerProvider {
	service = strings.ToLower(strings.TrimSpace(service))
	country = strings.TrimSpace(country)
	if base == "" {
		base = "https://smsbower.page/stubs/handler_api.php"
	}
	if service == "" {
		service = "dr"
	}
	return &SMSBowerProvider{APIKey: key, Base: base, Service: service, Country: country}
}
func (p *SMSBowerProvider) call(ctx context.Context, action string, q url.Values) (string, error) {
	key := strings.TrimSpace(p.APIKey)
	if !apiKeyRE.MatchString(key) {
		return "", providerError("invalid SMSBower API key", true)
	}
	u, e := url.Parse(p.Base)
	if e != nil {
		return "", providerError("invalid SMSBower API URL", true)
	}
	params := u.Query()
	for name, values := range q {
		params.Del(name)
		for _, value := range values {
			params.Add(name, value)
		}
	}
	params.Set("api_key", key)
	params.Set("action", action)
	u.RawQuery = params.Encode()
	b, e := safeRequest(ctx, p.Client, u.String())
	if e != nil {
		return "", e
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", providerError("SMSBower returned empty response", false)
	}
	return s, nil
}
func (p *SMSBowerProvider) GetNumber(ctx context.Context) (NumberLease, error) {
	service, country, maxPrice := strings.ToLower(strings.TrimSpace(p.Service)), strings.TrimSpace(p.Country), strings.TrimSpace(p.MaxPrice)
	if !serviceRE.MatchString(service) || !countryRE.MatchString(country) {
		return NumberLease{}, providerError("invalid SMSBower service or country ID", true)
	}
	q := url.Values{"service": {service}, "country": {country}}
	if maxPrice != "" {
		price, valid := asFloat(maxPrice)
		if !maxPriceRE.MatchString(maxPrice) || !valid || price <= 0 {
			return NumberLease{}, providerError("invalid SMSBower maximum price", true)
		}
		q.Set("maxPrice", maxPrice)
	}
	s, e := p.call(ctx, "getNumber", q)
	if e != nil {
		return NumberLease{}, e
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 || parts[0] != "ACCESS_NUMBER" || parts[1] == "" {
		return NumberLease{}, bowerError("getNumber", s)
	}
	n, e := normalizeNumber(parts[2])
	if e != nil {
		return NumberLease{}, e
	}
	return NumberLease{ID: parts[1], Number: n, Provider: "smsbower"}, nil
}
func (p *SMSBowerProvider) GetCode(ctx context.Context, l NumberLease) (string, error) {
	s, e := p.call(ctx, "getStatus", url.Values{"id": {l.ID}})
	if e != nil {
		return "", e
	}
	if s == "STATUS_WAIT_CODE" || strings.HasPrefix(s, "STATUS_WAIT_RETRY") {
		return "", ErrWaiting
	}
	if strings.HasPrefix(s, "STATUS_OK:") {
		if c := ExtractCode(strings.TrimPrefix(s, "STATUS_OK:")); c != "" {
			return c, nil
		}
		return "", providerError("SMSBower returned no six digit code", true)
	}
	return "", bowerError("getStatus", s)
}
func bowerError(action, value string) error {
	known := map[string]string{"BAD_KEY": "invalid API key", "BAD_ACTION": "invalid action", "BAD_SERVICE": "invalid service", "NO_NUMBERS": "no numbers available", "NO_BALANCE": "insufficient balance", "NO_ACTIVATION": "lease does not exist", "BAD_STATUS": "invalid lease status", "EARLY_CANCEL_DENIED": "lease cannot be canceled yet", "STATUS_CANCEL": "lease was canceled"}
	desc := known[value]
	terminal := false
	for _, prefix := range []string{"BAD_KEY", "BAD_ACTION", "BAD_SERVICE", "NO_BALANCE", "NO_ACTIVATION", "BAD_STATUS", "BANNED", "STATUS_CANCEL"} {
		if strings.HasPrefix(value, prefix) {
			terminal = true
		}
	}
	if desc == "" {
		desc = "unexpected provider response"
	}
	return providerError(fmt.Sprintf("SMSBower %s: %s", action, desc), terminal)
}
func (p *SMSBowerProvider) status(ctx context.Context, l NumberLease, status string) error {
	s, e := p.call(ctx, "setStatus", url.Values{"id": {l.ID}, "status": {status}})
	if e != nil {
		return e
	}
	switch s {
	case "ACCESS_READY", "ACCESS_RETRY_GET", "ACCESS_ACTIVATION", "ACCESS_CANCEL":
		return nil
	}
	return bowerError("setStatus", s)
}
func (p *SMSBowerProvider) MarkReady(ctx context.Context, l NumberLease) error {
	return p.status(ctx, l, "1")
}
func (p *SMSBowerProvider) Release(ctx context.Context, l NumberLease) error {
	return p.status(ctx, l, "8")
}
func (p *SMSBowerProvider) Complete(ctx context.Context, l NumberLease) error {
	return p.status(ctx, l, "6")
}
func (p *SMSBowerProvider) ListNumberOptions(ctx context.Context, service string) ([]map[string]any, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" {
		service = "dr"
	}
	if !serviceRE.MatchString(service) {
		return nil, providerError("invalid SMSBower service", true)
	}
	a, e := p.call(ctx, "getPrices", url.Values{"service": {service}})
	if e != nil {
		return nil, e
	}
	b, e := p.call(ctx, "getCountries", url.Values{})
	if e != nil {
		return nil, e
	}
	var prices map[string]any
	var countries any
	var rawPrices any
	if json.Unmarshal([]byte(a), &rawPrices) != nil {
		return nil, bowerError("getPrices", a)
	}
	var ok bool
	if prices, ok = rawPrices.(map[string]any); !ok {
		return nil, providerError("SMSBower returned invalid price data", true)
	}
	if json.Unmarshal([]byte(b), &countries) != nil {
		return nil, bowerError("getCountries", b)
	}
	index := map[string]map[string]any{}
	var visit func(any, string, int)
	visit = func(v any, alias string, depth int) {
		if depth > 6 {
			return
		}
		switch x := v.(type) {
		case []any:
			for i, e := range x {
				visit(e, strconv.Itoa(i), depth+1)
			}
		case map[string]any:
			if x["id"] != nil && (x["chn"] != nil || x["eng"] != nil || x["title"] != nil || x["iso"] != nil) {
				for _, k := range []string{alias, valueString(x["id"]), valueString(x["activate_org_code"]), valueString(x["slug"]), valueString(x["iso"]), valueString(x["title"]), valueString(x["eng"]), valueString(x["chn"])} {
					if k != "" {
						index[strings.ToLower(k)] = x
					}
				}
				return
			}
			for k, e := range x {
				visit(e, k, depth+1)
			}
		}
	}
	visit(countries, "", 0)
	unique := map[string]map[string]any{}
	for k, v := range prices {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if sm, ok := m[service].(map[string]any); ok {
			m = sm
		}
		cost, ok1 := asFloat(m["cost"])
		count, ok2 := asFloat(m["count"])
		if !ok1 || !ok2 || cost < 0 || count <= 0 || count > float64(1<<53-1) {
			continue
		}
		info := index[strings.ToLower(k)]
		country := k
		if s := valueString(info["activate_org_code"]); s != "" {
			country = s
		}
		if !countryRE.MatchString(country) {
			continue
		}
		title := map[string]string{"7": "马来西亚", "12": "美国（虚拟号码）", "16": "英国", "187": "美国", "1001": "日本"}[country]
		if title == "" {
			title = "国家 " + country
		}
		for _, key := range []string{"chn", "title", "eng"} {
			if s := valueString(info[key]); s != "" {
				title = strings.Join(strings.Fields(s), " ")
				break
			}
		}
		iso := strings.ToUpper(valueString(info["iso"]))
		if !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(iso) {
			iso = ""
		}
		prefix := regexp.MustCompile(`[^0-9]`).ReplaceAllString(valueString(info["prefix"]), "")
		if len(prefix) > 4 {
			prefix = prefix[:4]
		}
		record := map[string]any{"country": country, "title": title, "iso": iso, "prefix": prefix, "price": cost, "count": int(count)}
		if old := unique[country]; old == nil || cost < old["price"].(float64) {
			unique[country] = record
		}
	}
	out := []map[string]any{}
	for _, v := range unique {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["price"] != out[j]["price"] {
			return out[i]["price"].(float64) < out[j]["price"].(float64)
		}
		if out[i]["count"] != out[j]["count"] {
			return out[i]["count"].(int) > out[j]["count"].(int)
		}
		return out[i]["title"].(string) < out[j]["title"].(string)
	})
	if len(out) == 0 {
		return nil, providerError("SMSBower has no available numbers", true)
	}
	return out, nil
}

func ExtractCode(v any) string {
	if m, ok := v.(map[string]any); ok {
		for _, key := range []string{"sms_code", "otp", "verification_code", "code", "sms_msg", "message", "text"} {
			if c := ExtractCode(m[key]); c != "" {
				return c
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			low := strings.ToLower(k)
			if strings.Contains(low, "id") || low == "number" || low == "phone" || low == "code" {
				continue
			}
			if c := ExtractCode(m[k]); c != "" {
				return c
			}
		}
		return ""
	}
	switch x := v.(type) {
	case string:
		if m := codeRE.FindStringSubmatch(x); len(m) > 1 {
			return m[1]
		}
	case float64:
		if x >= 100000 && x <= 999999 && x == float64(int(x)) {
			return fmt.Sprintf("%06d", int(x))
		}
	case []any:
		for _, e := range x {
			if c := ExtractCode(e); c != "" {
				return c
			}
		}
	}
	return ""
}
func ExtractCodes(v any) []string {
	b, _ := json.Marshal(v)
	candidates := mail.ExtractCandidates(string(b))
	out := []string{}
	seen := map[string]bool{}
	for _, c := range candidates {
		if !seen[c.Code] {
			out = append(out, c.Code)
			seen[c.Code] = true
		}
	}
	return out
}

type customLeaseState struct {
	url       string
	seen      map[string]bool
	requested int64
}
type CustomPool struct {
	entries []string
	next    int
	mu      sync.Mutex
	Client  *http.Client
	leases  map[string]*customLeaseState
	err     error
}

func NewCustomPool(entries []string) *CustomPool {
	p := &CustomPool{leases: map[string]*customLeaseState{}}
	seen := map[string]int{}
	count := 0
	for _, line := range entries {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if count > 500 {
			p.err = errors.New("custom SMS pool exceeds 500 entries")
			break
		}
		parts := strings.SplitN(line, "----", 2)
		if len(parts) != 2 || !phoneRE.MatchString(strings.TrimSpace(parts[0])) || !mail.ValidateURL(strings.TrimSpace(parts[1])) {
			p.err = errors.New("custom SMS entry requires E.164 phone----HTTP(S) URL")
			break
		}
		phone := strings.TrimSpace(parts[0])
		line = phone + "----" + strings.TrimSpace(parts[1])
		if i, ok := seen[phone]; ok {
			p.entries[i] = line
		} else {
			seen[phone] = len(p.entries)
			p.entries = append(p.entries, line)
		}
	}
	if len(p.entries) == 0 && p.err == nil {
		p.err = errors.New("custom SMS pool is empty")
	}
	return p
}
func NewCustomPoolValue(value any) *CustomPool {
	var entries []string
	switch v := value.(type) {
	case string:
		entries = strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n")
	case []string:
		entries = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				entries = append(entries, s)
			}
		}
	}
	return NewCustomPool(entries)
}
func (p *CustomPool) Validate() error { return p.err }
func (p *CustomPool) GetNumber(ctx context.Context) (NumberLease, error) {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return NumberLease{}, p.err
	}
	if p.next >= len(p.entries) {
		p.mu.Unlock()
		return NumberLease{}, providerError("custom SMS pool has no unallocated numbers", true)
	}
	line := p.entries[p.next]
	p.next++
	p.mu.Unlock()
	parts := strings.SplitN(line, "----", 2)
	candidates, e := p.fetchCandidates(ctx, parts[1])
	if e != nil {
		return NumberLease{}, e
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Key] = true
	}
	idBytes := make([]byte, 16)
	if _, e = rand.Read(idBytes); e != nil {
		return NumberLease{}, e
	}
	id := "custom-" + hex.EncodeToString(idBytes)
	p.mu.Lock()
	p.leases[id] = &customLeaseState{url: parts[1], seen: seen, requested: time.Now().UnixMilli()}
	p.mu.Unlock()
	return NumberLease{ID: id, Number: parts[0], Provider: "custom"}, nil
}
func (p *CustomPool) GetCode(ctx context.Context, l NumberLease) (string, error) {
	p.mu.Lock()
	st, ok := p.leases[l.ID]
	if !ok {
		p.mu.Unlock()
		return "", providerError("custom SMS lease not found", true)
	}
	raw := st.url
	p.mu.Unlock()
	candidates, e := p.fetchCandidates(ctx, raw)
	if e != nil {
		return "", e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok = p.leases[l.ID]
	if !ok {
		return "", providerError("custom SMS lease released", true)
	}
	var code string
	for _, c := range mail.FilterByRequestTime(candidates, st.requested) {
		if !st.seen[c.Key] && code == "" {
			code = c.Code
		}
	}
	for _, c := range candidates {
		st.seen[c.Key] = true
	}
	if code == "" {
		return "", ErrWaiting
	}
	return code, nil
}

// Custom APIs use the mailbox contract: redirects are followed and requests
// have a 60-second ceiling. The paid providers deliberately reject redirects
// and use a shorter timeout; that policy must not leak into custom mailboxes.
func (p *CustomPool) fetchCandidates(ctx context.Context, raw string) ([]mail.Candidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	candidates, err := mail.FetchCandidates(ctx, raw, mail.RequestConfig{}, p.Client)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A private mailbox URL may carry its API credential. Keep arbitrary
		// URL, transport and response details out of provider/UI error text.
		return nil, providerError("custom SMS mailbox request failed", false)
	}
	return candidates, nil
}

func (p *CustomPool) Release(_ context.Context, l NumberLease) error {
	p.mu.Lock()
	delete(p.leases, l.ID)
	p.mu.Unlock()
	return nil
}
func (p *CustomPool) Complete(ctx context.Context, l NumberLease) error { return p.Release(ctx, l) }
func PollCode(ctx context.Context, p SmsProvider, l NumberLease, interval, limit time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if limit <= 0 {
		limit = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		code, e := p.GetCode(ctx, l)
		if e == nil && code != "" {
			return code, nil
		}
		if e != nil && !errors.Is(e, ErrWaiting) {
			var failure *ProviderError
			if !errors.As(e, &failure) || failure.terminal {
				return "", e
			}
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
}
