package jshost

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/grafana/sobek"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

//go:embed webplatform.js
var webPlatformSource string

// installWebPlatform only uses the realm's owner goroutine. All HTTP, scheduling
// and process ownership remain with the host; these bridges never use a network.
func installWebPlatform(vm *sobek.Runtime) error {
	bridge := vm.NewObject()
	set := func(name string, value any) {
		if err := bridge.Set(name, value); err != nil {
			panic(err)
		}
	}
	fail := func(message string) { panic(vm.NewTypeError(message)) }
	buffer := func(bytes []byte) sobek.Value { return vm.ToValue(vm.NewArrayBuffer(bytes)) }
	bytes := func(value sobek.Value) []byte {
		if array, ok := value.Export().(sobek.ArrayBuffer); ok {
			return array.Bytes()
		}
		fail("Expected ArrayBuffer")
		return nil
	}
	set("resolveURL", func(call sobek.FunctionCall) sobek.Value {
		var parsed *whatwg.Url
		var err error
		if sobek.IsUndefined(call.Argument(1)) {
			parsed, err = whatwg.Parse(call.Argument(0).String())
		} else {
			var base *whatwg.Url
			base, err = whatwg.Parse(call.Argument(1).String())
			if err == nil {
				parsed, err = base.Parse(call.Argument(0).String())
			}
		}
		if err != nil {
			fail("Invalid URL")
		}
		return vm.ToValue(webURLParts(parsed))
	})
	set("setURL", func(call sobek.FunctionCall) sobek.Value {
		parsed, err := whatwg.Parse(call.Argument(0).String())
		if err != nil {
			fail("Invalid URL")
		}
		value := call.Argument(2).String()
		switch call.Argument(1).String() {
		case "protocol":
			parsed.SetProtocol(value)
		case "username":
			parsed.SetUsername(value)
		case "password":
			parsed.SetPassword(value)
		case "host":
			parsed.SetHost(value)
		case "hostname":
			parsed.SetHostname(value)
		case "port":
			parsed.SetPort(value)
		case "pathname":
			parsed.SetPathname(value)
		case "search":
			parsed.SetSearch(value)
		case "hash":
			parsed.SetHash(value)
		default:
			fail("Invalid URL component")
		}
		return vm.ToValue(webURLParts(parsed))
	})
	set("formEncode", func(value string) string {
		return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(value), "%2A", "*"), "~", "%7E")
	})
	set("formDecode", func(value string) string {
		value = strings.ReplaceAll(value, "+", " ")
		var out []byte
		for i := 0; i < len(value); i++ {
			if value[i] == '%' && i+2 < len(value) {
				if b, err := hex.DecodeString(value[i+1 : i+3]); err == nil {
					out = append(out, b[0])
					i += 2
					continue
				}
			}
			out = append(out, value[i])
		}
		decoded, _, _ := decodeWebText(out, "utf-8", false, false)
		return decoded
	})
	set("encodeText", func(call sobek.FunctionCall) sobek.Value { return buffer([]byte(call.Argument(0).String())) })
	set("encoding", func(label string) string {
		encoding, err := htmlindex.Get(label)
		if err != nil {
			fail("Unknown encoding")
		}
		name, err := htmlindex.Name(encoding)
		if err != nil {
			fail("Unknown encoding")
		}
		return name
	})
	set("decodeText", func(call sobek.FunctionCall) sobek.Value {
		data := bytes(call.Argument(0))
		label := call.Argument(1).String()
		fatal, stream := call.Argument(2).ToBoolean(), call.Argument(3).ToBoolean()
		decoded, remaining, err := decodeWebText(data, label, fatal, stream)
		if err != nil {
			fail(err.Error())
		}
		return vm.ToValue(map[string]any{"text": decoded, "remaining": vm.NewArrayBuffer(remaining)})
	})
	set("encodeBase64", func(call sobek.FunctionCall) sobek.Value {
		return vm.ToValue(base64.StdEncoding.EncodeToString(bytes(call.Argument(0))))
	})
	set("decodeBase64", func(call sobek.FunctionCall) sobek.Value {
		value := call.Argument(0).String()
		if strings.ContainsAny(value, " \t\r\n\f") {
			value = strings.Map(func(r rune) rune {
				if strings.ContainsRune(" \t\r\n\f", r) {
					return -1
				}
				return r
			}, value)
		}
		unpadded := strings.TrimRight(value, "=")
		if len(value)-len(unpadded) > 2 || strings.Contains(unpadded, "=") || len(unpadded)%4 == 1 || len(value) != len(unpadded) && len(value)%4 != 0 {
			fail("Invalid base64")
		}
		decoded, err := base64.RawStdEncoding.DecodeString(unpadded)
		if err != nil {
			fail("Invalid base64")
		}
		return buffer(decoded)
	})
	set("randomBytes", func(call sobek.FunctionCall) sobek.Value {
		length := call.Argument(0).ToInteger()
		if length < 0 || length > 65536 {
			fail("Random byte quota exceeded")
		}
		data := make([]byte, int(length))
		if _, err := rand.Read(data); err != nil {
			fail("Random source unavailable")
		}
		return buffer(data)
	})
	set("randomUUID", func() string {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			fail("Random source unavailable")
		}
		b[6] = b[6]&15 | 64
		b[8] = b[8]&63 | 128
		encoded := hex.EncodeToString(b[:])
		return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	})
	set("digest", func(call sobek.FunctionCall) sobek.Value {
		data := bytes(call.Argument(1))
		var hash []byte
		switch strings.ToUpper(call.Argument(0).String()) {
		case "SHA-1":
			sum := sha1.Sum(data)
			hash = sum[:]
		case "SHA-256":
			sum := sha256.Sum256(data)
			hash = sum[:]
		case "SHA-384":
			sum := sha512.Sum384(data)
			hash = sum[:]
		case "SHA-512":
			sum := sha512.Sum512(data)
			hash = sum[:]
		default:
			fail("Unsupported digest algorithm")
		}
		return buffer(hash)
	})
	newline := "\n"
	if runtime.GOOS == "windows" {
		newline = "\r\n"
	}
	set("newline", newline)
	if err := vm.Set("__webPlatformBridge", bridge); err != nil {
		return err
	}
	_, err := vm.RunScript("webplatform.js", webPlatformSource)
	_ = vm.GlobalObject().Delete("__webPlatformBridge")
	return err
}

func webURLParts(parsed *whatwg.Url) map[string]string {
	origin := "null"
	switch parsed.Scheme() {
	case "http", "https", "ws", "wss", "ftp":
		origin = parsed.Protocol() + "//" + parsed.Host()
	case "blob":
		if inner, err := whatwg.Parse(parsed.Pathname()); err == nil && (inner.Scheme() == "https" || inner.Scheme() == "http") {
			origin = inner.Protocol() + "//" + inner.Host()
		}
	}
	return map[string]string{"href": parsed.Href(false), "origin": origin, "protocol": parsed.Protocol(), "username": parsed.Username(), "password": parsed.Password(), "host": parsed.Host(), "hostname": parsed.Hostname(), "port": parsed.Port(), "pathname": parsed.Pathname(), "search": parsed.Search(), "hash": parsed.Hash()}
}

func decodeWebText(data []byte, label string, fatal, stream bool) (string, []byte, error) {
	var remaining []byte
	if label == "utf-8" {
		var out strings.Builder
		for len(data) > 0 {
			if !utf8.FullRune(data) {
				if stream {
					remaining = append([]byte(nil), data...)
					break
				}
				if fatal {
					return "", nil, fmt.Errorf("Invalid encoded data")
				}
				out.WriteRune(utf8.RuneError)
				break
			}
			r, size := utf8.DecodeRune(data)
			if r == utf8.RuneError && size == 1 && fatal {
				return "", nil, fmt.Errorf("Invalid encoded data")
			}
			out.WriteRune(r)
			data = data[size:]
		}
		return out.String(), remaining, nil
	}
	if label == "utf-16le" || label == "utf-16be" {
		var order binary.ByteOrder = binary.LittleEndian
		if label == "utf-16be" {
			order = binary.BigEndian
		}
		var out strings.Builder
		for len(data) > 0 {
			if len(data) < 2 {
				if stream {
					remaining = append([]byte(nil), data...)
					break
				}
				if fatal {
					return "", nil, fmt.Errorf("Invalid encoded data")
				}
				out.WriteRune(utf8.RuneError)
				break
			}
			u := order.Uint16(data[:2])
			data = data[2:]
			if u >= 0xd800 && u <= 0xdbff {
				if len(data) < 2 && stream {
					remaining = append([]byte{byte(u), byte(u >> 8)}, data...)
					if label == "utf-16be" {
						remaining[0], remaining[1] = remaining[1], remaining[0]
					}
					break
				}
				if len(data) >= 2 {
					v := order.Uint16(data[:2])
					if v >= 0xdc00 && v <= 0xdfff {
						out.WriteRune(utf16.DecodeRune(rune(u), rune(v)))
						data = data[2:]
						continue
					}
				}
				if fatal {
					return "", nil, fmt.Errorf("Invalid encoded data")
				}
				out.WriteRune(utf8.RuneError)
				continue
			}
			if u >= 0xdc00 && u <= 0xdfff {
				if fatal {
					return "", nil, fmt.Errorf("Invalid encoded data")
				}
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(rune(u))
			}
		}
		return out.String(), remaining, nil
	}
	// The common single-byte WHATWG encodings have no incomplete byte sequence.
	// Stateful multibyte streaming requires an owned incremental decoder.
	if stream && label != "windows-1252" && !strings.HasPrefix(label, "iso-8859-") {
		return "", nil, fmt.Errorf("Unsupported streaming encoding")
	}
	encoding, err := htmlindex.Get(label)
	if err != nil {
		return "", nil, fmt.Errorf("Unknown encoding")
	}
	decoded, _, err := transform.Bytes(encoding.NewDecoder(), data)
	if err != nil {
		return "", nil, fmt.Errorf("Invalid encoded data")
	}
	if fatal && strings.ContainsRune(string(decoded), utf8.RuneError) {
		return "", nil, fmt.Errorf("Invalid encoded data")
	}
	return string(decoded), nil, nil
}
