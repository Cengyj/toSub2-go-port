package protocol

import (
	"crypto/hmac"
	"crypto/sha1"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

func normalizeTOTPSecret(secret string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '=' {
			return -1
		}
		return r
	}, secret))
}

func DecodeBase32Secret(secret string) ([]byte, error) {
	s := normalizeTOTPSecret(secret)
	if len(s) < 16 || len(s) > 128 {
		return nil, fmt.Errorf("TOTP secret must contain 16-128 Base32 characters")
	}
	// Legacy secrets permit every length in this range and discard incomplete
	// trailing bytes. Keep the same bit decoding for imported credentials.
	b := make([]byte, 0, len(s)*5/8)
	var pending uint16
	bits := 0
	for _, r := range s {
		value := strings.IndexRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", r)
		if value < 0 {
			return nil, fmt.Errorf("TOTP secret contains invalid Base32 character")
		}
		pending = pending<<5 | uint16(value)
		bits += 5
		if bits >= 8 {
			bits -= 8
			b = append(b, byte(pending>>bits))
		}
	}
	return b, nil
}

func GenerateTOTP(secret string, at time.Time) (string, error) {
	key, e := DecodeBase32Secret(secret)
	if e != nil {
		return "", e
	}
	counter := uint64(at.Unix() / 30)
	msg := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		msg[i] = byte(counter)
		counter >>= 8
	}
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", n%1000000), nil
}
func CurrentTOTP(secret string) (string, error) { return GenerateTOTP(secret, time.Now()) }
func BuildTOTPURI(email, secret string) (string, error) {
	if _, e := DecodeBase32Secret(secret); e != nil {
		return "", e
	}
	q := url.Values{}
	q.Set("secret", normalizeTOTPSecret(secret))
	q.Set("issuer", "OpenAI")
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("OpenAI:"+email) + "?" + q.Encode(), nil
}
func TOTPAtUnix(secret string, unix int64) (string, error) {
	return GenerateTOTP(secret, time.Unix(unix, 0))
}
func ParseTOTPCode(code string) bool { return digits6.MatchString(code) }
