package logging

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const Redacted = "[REDACTED]"

// SensitiveKey treats camelCase, snake_case, dashes and case consistently.
// Callers must never pass unknown, unlabelled secrets in free-form messages.
func SensitiveKey(key string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	k := b.String()
	switch k {
	case "password", "newpassword", "oldpassword", "passwd", "totp", "totpsecret", "totpsetupsecret", "totpsetupuri", "2fakey", "secret", "apikey", "adminapikey", "sub2apiadminapikey", "authorization", "proxyauthorization", "cookie", "cookies", "setcookie", "access_token", "code", "otp", "emailotp", "phoneotp", "smsotp", "mfaotp", "codeverifier", "codechallenge", "csrf", "nonce", "state", "credentials", "headers", "mailheaders", "mailrequestbody", "mailbody", "mailapiurl", "proxyurl", "passwordaddresult":
		return true
	}
	for _, suffix := range []string{"password", "secret", "apikey", "token", "cookie"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

var urlPattern = regexp.MustCompile(`(?i)(?:https?|socks5h?|otpauth)://[^\s<>"'\x60]+`)
var authPattern = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9_~+/=.-]+`)
var jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{3,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
var secretAssignment = regexp.MustCompile(`(?i)(["']?(?:[a-z0-9_-]*(?:password|secret|api[_-]?key|token|cookie)|credentials|authorization|proxy[_-]?authorization|set[_-]?cookie|headers|code[_-]?verifier|code[_-]?challenge|totp|2fa[_-]?key|otp|email[_-]?otp|phone[_-]?otp|sms[_-]?otp|mfa[_-]?otp|code|csrf|nonce|state)["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;&}\]]+)`)
var headerLines = regexp.MustCompile(`(?im)((?:^|\n)\s*(?:set-cookie|cookie|authorization|proxy-authorization|x-api-key)\s*:\s*)[^\r\n]+`)
var sixDigits = regexp.MustCompile(`[0-9]{6}`)
var contextualCode = regexp.MustCompile(`(?i)((?:verification|security|email|phone|sms|mfa|2fa|totp|one-time|one time|验证码|校验码)[ _-]*(?:code|otp)?\s+(?:is\s+)?)[A-Za-z0-9]{4,10}\b`)

// RedactText handles quoted JSON fields, key=value, credential headers,
// bearer/basic tokens, URLs, JWTs, and six-digit one-time codes. It must run
// before truncation so a truncated closing quote cannot defeat redaction.
func RedactText(text string) string {
	text = headerLines.ReplaceAllString(text, "${1}"+Redacted)
	text = urlPattern.ReplaceAllString(text, "[URL REDACTED]")
	text = authPattern.ReplaceAllString(text, "[AUTH REDACTED]")
	text = jwtPattern.ReplaceAllString(text, Redacted)
	text = secretAssignment.ReplaceAllString(text, "${1}"+Redacted)
	text = contextualCode.ReplaceAllString(text, "${1}"+Redacted)
	indices := sixDigits.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, idx := range indices {
		if idx[0] > 0 && text[idx[0]-1] >= '0' && text[idx[0]-1] <= '9' {
			continue
		}
		if idx[1] < len(text) && text[idx[1]] >= '0' && text[idx[1]] <= '9' {
			continue
		}
		b.WriteString(text[last:idx[0]])
		b.WriteString(Redacted)
		last = idx[1]
	}
	b.WriteString(text[last:])
	return b.String()
}
func RedactKnown(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, Redacted)
		}
	}
	return RedactText(text)
}
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	msg := RedactText(err.Error())
	runes := []rune(msg)
	if len(runes) > 512 {
		return string(runes[:512])
	}
	return msg
}

// RedactValue returns a separate JSON-safe value; it never mutates source data.
// Depth and collection limits avoid recursive/cyclic values and unbounded logs.
func RedactValue(value any) any { return redactReflect(reflect.ValueOf(value), 0) }
func redactReflect(v reflect.Value, depth int) any {
	if !v.IsValid() {
		return nil
	}
	if depth > 12 {
		return Redacted
	}
	if v.CanInterface() {
		switch x := v.Interface().(type) {
		case error:
			return SafeError(x)
		case time.Time:
			return x
		case []byte:
			return Redacted
		}
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return redactReflect(v.Elem(), depth+1)
	case reflect.String:
		return RedactText(v.String())
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return v.Float()
	case reflect.Map:
		out := map[string]any{}
		it := v.MapRange()
		count := 0
		for it.Next() {
			count++
			if count > 1000 {
				out["_truncated"] = true
				break
			}
			k := fmt.Sprint(it.Key().Interface())
			if SensitiveKey(k) {
				out[k] = Redacted
			} else {
				out[k] = redactReflect(it.Value(), depth+1)
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		limit := v.Len()
		if limit > 1000 {
			limit = 1000
		}
		out := make([]any, limit)
		for i := 0; i < limit; i++ {
			out[i] = redactReflect(v.Index(i), depth+1)
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if SensitiveKey(name) {
				out[name] = Redacted
			} else {
				out[name] = redactReflect(v.Field(i), depth+1)
			}
		}
		return out
	default:
		return Redacted
	}
}
