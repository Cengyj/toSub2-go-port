package console

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

type accountSplit struct{ segments, separators []string }

func (s accountSplit) join(start, end int) string {
	v := s.segments[start]
	for i := start; i < end; i++ {
		v += s.separators[i] + s.segments[i+1]
	}
	return v
}
func splitMatches(line string, positions [][2]int) accountSplit {
	s := accountSplit{}
	cursor := 0
	for _, p := range positions {
		s.segments = append(s.segments, line[cursor:p[0]])
		s.separators = append(s.separators, line[p[0]:p[1]])
		cursor = p[1]
	}
	s.segments = append(s.segments, line[cursor:])
	return s
}
func splitPattern(line, pattern string) accountSplit {
	matches := regexp.MustCompile(pattern).FindAllStringIndex(line, -1)
	positions := make([][2]int, len(matches))
	for i, m := range matches {
		positions[i] = [2]int{m[0], m[1]}
	}
	return splitMatches(line, positions)
}
func splitHyphens(line string, width int) accountSplit {
	var positions [][2]int
	for _, m := range regexp.MustCompile(`-+`).FindAllStringIndex(line, -1) {
		n := m[1] - m[0]
		if n == width || (width == 0 && (n == 3 || n == 4)) {
			positions = append(positions, [2]int{m[0], m[1]})
		} else if (width > 0 && n == width*2) || (width == 0 && n >= 6 && n <= 8) {
			a := width
			if a == 0 {
				a = 4
				if n == 6 {
					a = 3
				}
			}
			positions = append(positions, [2]int{m[0], m[0] + a}, [2]int{m[0] + a, m[1]})
		}
	}
	return splitMatches(line, positions)
}

type parseCandidate struct {
	entry             createRequest
	score, recognized int
	ambiguity         bool
}

func candidateSignature(c parseCandidate) string {
	b, _ := json.Marshal(struct {
		Email string
		Input map[string]any
	}{strings.ToLower(c.entry.Email), c.entry.Input})
	return string(b)
}
func smartAccount(line string) (createRequest, error) {
	if isEmail(line) {
		return createRequest{Email: line, Input: map[string]any{}}, nil
	}
	candidates := []parseCandidate{}
	consider := func(s accountSplit, name string, priority int) error {
		c := parseAccountSplit(s, name, priority)
		if c.ambiguity {
			return fmt.Errorf("账号字段存在歧义，请使用 ---- 分隔")
		}
		if c.score > 0 {
			candidates = append(candidates, c)
		}
		return nil
	}
	for _, d := range []struct{ width, priority int }{{4, 110}, {3, 105}, {0, 100}} {
		if err := consider(splitHyphens(line, d.width), "hyphen", d.priority); err != nil {
			return createRequest{}, err
		}
	}
	for _, d := range []struct {
		name, pattern string
		priority      int
	}{{"tab", `\t+`, 80}, {"pipe", `\|+`, 70}, {"colon", `:{2,}`, 65}, {"semicolon", `[;；]+`, 60}, {"comma", `[,，]+`, 55}, {"spaces", ` {2,}`, 40}, {"repeat", `#{2,}`, 37}, {"repeat", `~{2,}`, 37}, {"repeat", `\^{2,}`, 37}, {"repeat", `\*{2,}`, 37}} {
		if err := consider(splitPattern(line, d.pattern), d.name, d.priority); err != nil {
			return createRequest{}, err
		}
	}
	if len(candidates) == 0 {
		return createRequest{}, fmt.Errorf("无法识别独立邮箱和账号字段")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.recognized == best.recognized && candidateSignature(c) != candidateSignature(best) {
			return createRequest{}, fmt.Errorf("字段分隔方式存在歧义，请使用 ---- 分隔")
		}
	}
	if password, _ := best.entry.Input["password"].(string); password != "" {
		for _, pattern := range []string{`-{3,4}`, `\|+`, `:{2,}`, `[;；]+`, `[,，]+`, ` {2,}`} {
			s := splitPattern(password, pattern)
			if len(s.separators) > 0 {
				for _, part := range s.segments {
					if isHTTPURL(strings.TrimSpace(part)) || isTOTPSecret(part) {
						return createRequest{}, fmt.Errorf("混用了多种分隔符，请统一使用 ----")
					}
				}
			}
		}
	}
	return best.entry, nil
}
func parseAccountSplit(s accountSplit, name string, priority int) parseCandidate {
	if len(s.separators) == 0 || len(s.segments) > 64 {
		return parseCandidate{}
	}
	values := make([]string, len(s.segments))
	types := make([]string, len(values))
	emailCount := 0
	for i, v := range s.segments {
		values[i] = strings.TrimSpace(v)
		if isEmail(values[i]) {
			emailCount++
		}
	}
	if emailCount > 1 {
		return parseCandidate{ambiguity: true}
	}
	type span struct {
		start, end int
		value      string
	}
	ranges := []span{}
	for i := range values {
		for j := i; j < len(values); j++ {
			v := strings.TrimSpace(s.join(i, j))
			if strings.Count(v, "@") > 1 {
				break
			}
			if isEmail(v) {
				ranges = append(ranges, span{i, j, v})
			}
		}
	}
	eligible := []span{}
	for _, e := range ranges {
		if e.start == e.end {
			eligible = append(eligible, e)
		}
	}
	if len(eligible) == 0 {
		for _, e := range ranges {
			if e.start == 0 {
				eligible = append(eligible, e)
			}
		}
	}
	if len(eligible) == 0 {
		return parseCandidate{}
	}
	sort.SliceStable(eligible, func(i, j int) bool { return len(eligible[i].value) > len(eligible[j].value) })
	email := eligible[0]
	for i := email.start; i <= email.end; i++ {
		types[i] = "email"
	}
	urls := []int{}
	for i, v := range values {
		if types[i] == "" && isHTTPURL(v) {
			urls = append(urls, i)
		}
	}
	if len(urls) > 1 {
		return parseCandidate{}
	}
	if name == "comma" || name == "semicolon" {
		for _, i := range urls {
			for _, j := range []int{i - 1, i + 1} {
				if j >= 0 && j < len(values) && types[j] == "" && values[j] != "" {
					a, b := i, j
					if a > b {
						a, b = b, a
					}
					if isHTTPURL(s.join(a, b)) {
						return parseCandidate{ambiguity: true}
					}
				}
			}
		}
	}
	for _, i := range urls {
		types[i] = "url"
	}
	remaining, totps := []int{}, []int{}
	empty := false
	for i, v := range values {
		if v == "" {
			empty = true
		}
		if types[i] == "" && v != "" {
			remaining = append(remaining, i)
			if isTOTPSecret(v) {
				totps = append(totps, i)
			}
		}
	}
	totp := ""
	if len(totps) == 1 {
		candidate := totps[0]
		followsURL := len(urls) > 0 && urls[0] < candidate
		if len(remaining) > 1 || followsURL || empty {
			types[candidate] = "totp"
			totp = values[candidate]
		}
	} else if len(totps) > 1 {
		if name != "hyphen" || len(s.separators) != 2 || len(values) != 3 || email.start != 0 || email.end != 0 || len(urls) != 0 || len(totps) != 2 || totps[0] != 1 || totps[1] != 2 {
			return parseCandidate{}
		}
		types[2] = "totp"
		totp = values[2]
	}
	if totp == "" && name == "spaces" {
		groups := []span{}
		part := regexp.MustCompile(`(?i)^[A-Z2-7=]{4,8}$`)
		for i := range values {
			if types[i] != "" || !part.MatchString(values[i]) {
				continue
			}
			for j := i + 1; j < len(values); j++ {
				if types[j] != "" || !part.MatchString(values[j]) {
					break
				}
				v := s.join(i, j)
				if isTOTPSecret(v) {
					groups = append(groups, span{i, j, v})
				}
			}
		}
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].end-groups[i].start > groups[j].end-groups[j].start })
		if len(groups) > 0 {
			g := groups[0]
			outside := false
			for _, i := range remaining {
				if i < g.start || i > g.end {
					outside = true
				}
			}
			ambiguous := len(groups) > 1 && groups[1].end-groups[1].start == g.end-g.start
			if !ambiguous && (outside || len(urls) > 0 || empty) {
				totp = g.value
				for i := g.start; i <= g.end; i++ {
					types[i] = "totp"
				}
			}
		}
	}
	passwordFields := []int{}
	for i, v := range values {
		if types[i] == "" && v != "" {
			passwordFields = append(passwordFields, i)
		}
	}
	for i := 1; i < len(passwordFields); i++ {
		if passwordFields[i] != passwordFields[i-1]+1 {
			return parseCandidate{}
		}
	}
	password := ""
	if len(passwordFields) > 0 {
		password = strings.TrimSpace(s.join(passwordFields[0], passwordFields[len(passwordFields)-1]))
	}
	if password == "" && len(urls) == 0 && totp == "" {
		return parseCandidate{}
	}
	// Structured legacy imports replace the account's login credentials. Empty
	// values must remain explicit so an OTP-only row clears an older password,
	// TOTP seed, or mailbox body instead of silently preserving it. Bare-email
	// imports return an empty map in smartAccount and keep their old credentials.
	in := map[string]any{"password": "", "totpSecret": "", "mailApiUrl": "", "mailRequestBody": ""}
	recognized := 1 + len(urls)
	score := priority + recognized*1000
	if password != "" {
		in["password"] = password
		score += 100
	}
	if len(urls) > 0 {
		in["mailApiUrl"] = values[urls[0]]
	}
	if totp != "" {
		in["totpSecret"] = strings.ToUpper(strings.Join(strings.Fields(strings.ReplaceAll(totp, "=", "")), ""))
		recognized++
		score += 1000
	}
	if len(s.separators) > 3 {
		score -= len(s.separators) - 3
	}
	return parseCandidate{entry: createRequest{Email: email.value, Input: in}, score: score, recognized: recognized}
}
func encodedMailBody(v string) bool {
	s := strings.TrimSpace(v)
	if (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) && json.Valid([]byte(s)) {
		return true
	}
	form := regexp.MustCompile(`(?:^|&)[^=&\s]+=[^&]*(?:&|$)`)
	if form.MatchString(s) || regexp.MustCompile(`%[0-9a-fA-F]{2}`).MatchString(s) {
		return true
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := encoding.DecodeString(s); err == nil {
			v := strings.TrimSpace(string(b))
			if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") || form.MatchString(v) {
				return true
			}
		}
	}
	return false
}
func postMailAccount(line string) (createRequest, error) {
	fields := strings.Split(line, "----")
	email := ""
	remaining := []string{}
	for _, v := range fields {
		v = strings.TrimSpace(v)
		if isEmail(v) {
			if email != "" {
				return createRequest{}, fmt.Errorf("必须包含一个独立邮箱")
			}
			email = v
		} else if v != "" {
			remaining = append(remaining, v)
		}
	}
	if email == "" || len(remaining) == 0 {
		return createRequest{}, fmt.Errorf("缺少邮箱或 POST 请求体")
	}
	in := map[string]any{"password": "", "totpSecret": ""}
	if len(remaining) >= 2 {
		matches := []int{}
		for i, v := range remaining {
			if isTOTPSecret(v) {
				matches = append(matches, i)
			}
		}
		if len(matches) == 1 {
			i := matches[0]
			in["totpSecret"] = strings.ToUpper(strings.Join(strings.Fields(strings.ReplaceAll(remaining[i], "=", "")), ""))
			remaining = append(remaining[:i], remaining[i+1:]...)
		}
	}
	if len(remaining) > 2 {
		return createRequest{}, fmt.Errorf("无法区分密码和 POST 请求体")
	}
	bodyIndex := len(remaining) - 1
	matches := []int{}
	for i, v := range remaining {
		if encodedMailBody(v) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		bodyIndex = matches[0]
	}
	body := remaining[bodyIndex]
	if len(body) > 65536 {
		return createRequest{}, fmt.Errorf("POST 请求体不能超过64KB")
	}
	in["mailRequestBody"] = body
	for i, v := range remaining {
		if i != bodyIndex {
			in["password"] = v
		}
	}
	return createRequest{Email: email, Input: in}, nil
}
func parseBatchEntries(raw, operation string, common map[string]any, cfg mailRequestConfig) ([]createRequest, error) {
	out := []createRequest{}
	seen := map[string]int{}
	count := 0
	for index, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		count++
		if count > 500 {
			return nil, fmt.Errorf("一次最多添加500条任务")
		}
		var entry createRequest
		var err error
		if cfg.Method == http.MethodPost && !isEmail(line) {
			entry, err = postMailAccount(line)
			if err == nil {
				entry.Input["mailApiUrl"] = cfg.URL
			}
		} else {
			entry, err = smartAccount(line)
		}
		if err != nil {
			return nil, fmt.Errorf("第%d行%s", index+1, err.Error())
		}
		entry.Operation = operation
		merged := map[string]any{}
		for k, v := range common {
			merged[k] = v
		}
		for k, v := range entry.Input {
			merged[k] = v
		}
		entry.Input = merged
		key := strings.ToLower(entry.Email)
		if i, exists := seen[key]; exists {
			out[i] = entry
		} else {
			seen[key] = len(out)
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("请至少输入一行账号信息")
	}
	return out, nil
}
