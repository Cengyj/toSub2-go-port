package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type curlWireCapture struct {
	ciphers, extensions []uint16
	settings            []http2.Setting
	windows             []uint32
	headers             []hpack.HeaderField
	err                 error
}

type curlWireConn struct {
	net.Conn
	hello []byte
}

func (c *curlWireConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.hello = append(c.hello, b[:n]...)
	return n, err
}

// Capture the actual TLS records instead of inspecting the configured profile.
func parseCurlWireHello(records []byte) ([]uint16, []uint16, error) {
	var handshake []byte
	for len(records) >= 5 {
		n := int(binary.BigEndian.Uint16(records[3:5]))
		if len(records) < 5+n {
			break
		}
		if records[0] == 22 {
			handshake = append(handshake, records[5:5+n]...)
		}
		records = records[5+n:]
		if len(handshake) >= 4 {
			length := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
			if handshake[0] == 1 && len(handshake) >= length+4 {
				handshake = handshake[4 : length+4]
				break
			}
		}
	}
	consume := func(n int) ([]byte, error) {
		if n < 0 || len(handshake) < n {
			return nil, io.ErrUnexpectedEOF
		}
		b := handshake[:n]
		handshake = handshake[n:]
		return b, nil
	}
	if _, err := consume(34); err != nil {
		return nil, nil, err
	}
	length, err := consume(1)
	if err != nil {
		return nil, nil, err
	}
	if _, err = consume(int(length[0])); err != nil {
		return nil, nil, err
	}
	length, err = consume(2)
	if err != nil {
		return nil, nil, err
	}
	cipherBytes, err := consume(int(binary.BigEndian.Uint16(length)))
	if err != nil || len(cipherBytes)%2 != 0 {
		return nil, nil, fmt.Errorf("invalid cipher list: %w", err)
	}
	var ciphers []uint16
	for len(cipherBytes) > 0 {
		v := binary.BigEndian.Uint16(cipherBytes[:2])
		cipherBytes = cipherBytes[2:]
		if v&0x0f0f != 0x0a0a {
			ciphers = append(ciphers, v)
		}
	}
	length, err = consume(1)
	if err != nil {
		return nil, nil, err
	}
	if _, err = consume(int(length[0])); err != nil {
		return nil, nil, err
	}
	length, err = consume(2)
	if err != nil {
		return nil, nil, err
	}
	extensionBytes, err := consume(int(binary.BigEndian.Uint16(length)))
	if err != nil {
		return nil, nil, err
	}
	var extensions []uint16
	for len(extensionBytes) >= 4 {
		id, n := binary.BigEndian.Uint16(extensionBytes[:2]), int(binary.BigEndian.Uint16(extensionBytes[2:4]))
		if len(extensionBytes) < 4+n {
			return nil, nil, io.ErrUnexpectedEOF
		}
		if id&0x0f0f != 0x0a0a {
			extensions = append(extensions, id)
		}
		extensionBytes = extensionBytes[4+n:]
	}
	if len(extensionBytes) != 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return ciphers, extensions, nil
}

func curlWireFixture(t *testing.T) (string, *x509.CertPool, <-chan curlWireCapture) {
	t.Helper()
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificates := seed.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(seed.Certificate())
	seed.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	captures := make(chan curlWireCapture, 16)
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[raw] = true
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer raw.Close()
				defer func() { mu.Lock(); delete(connections, raw); mu.Unlock() }()
				raw.SetDeadline(time.Now().Add(10 * time.Second))
				capture := serveCurlWire(raw, certificates)
				captures <- capture
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		for connection := range connections {
			connection.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return "https://" + listener.Addr().String() + "/synthetic", roots, captures
}

func serveCurlWire(raw net.Conn, certificates []tls.Certificate) (capture curlWireCapture) {
	sniff := &curlWireConn{Conn: raw}
	connection := tls.Server(sniff, &tls.Config{Certificates: certificates, NextProtos: []string{"h2"}})
	if capture.err = connection.Handshake(); capture.err != nil {
		return
	}
	capture.ciphers, capture.extensions, capture.err = parseCurlWireHello(sniff.hello)
	if capture.err != nil {
		return
	}
	preface := make([]byte, len(http2.ClientPreface))
	if _, capture.err = io.ReadFull(connection, preface); capture.err != nil {
		return
	}
	if string(preface) != http2.ClientPreface {
		capture.err = fmt.Errorf("unexpected HTTP/2 preface")
		return
	}
	framer := http2.NewFramer(connection, connection)
	framer.ReadMetaHeaders = hpack.NewDecoder(1<<20, nil)
	if capture.err = framer.WriteSettings(); capture.err != nil {
		return
	}
	var stream uint32
	for stream == 0 {
		frame, err := framer.ReadFrame()
		if err != nil {
			capture.err = err
			return
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			frame.ForeachSetting(func(setting http2.Setting) error {
				capture.settings = append(capture.settings, setting)
				return nil
			})
			if !frame.IsAck() {
				if capture.err = framer.WriteSettingsAck(); capture.err != nil {
					return
				}
			}
		case *http2.WindowUpdateFrame:
			if frame.StreamID == 0 {
				capture.windows = append(capture.windows, frame.Increment)
			}
		case *http2.MetaHeadersFrame:
			stream = frame.StreamID
			capture.headers = frame.Fields
		}
	}
	body := []byte(`{"fixture":"decoded response"}`)
	compressed := strings.Contains(curlWireHeader(capture.headers, "accept-encoding"), "zstd")
	if compressed {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			capture.err = err
			return
		}
		body = encoder.EncodeAll(body, nil)
		encoder.Close()
	}
	var block bytes.Buffer
	hpackEncoder := hpack.NewEncoder(&block)
	hpackEncoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	hpackEncoder.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(body))})
	if compressed {
		hpackEncoder.WriteField(hpack.HeaderField{Name: "content-encoding", Value: "zstd"})
	}
	if capture.err = framer.WriteHeaders(http2.HeadersFrameParam{StreamID: stream, BlockFragment: block.Bytes(), EndHeaders: true}); capture.err == nil {
		capture.err = framer.WriteData(stream, true, body)
	}
	return
}

func curlWireHeader(headers []hpack.HeaderField, name string) string {
	for _, header := range headers {
		if header.Name == name {
			return header.Value
		}
	}
	return ""
}

func assertCurlWireChrome146(t *testing.T, capture curlWireCapture) {
	t.Helper()
	if capture.err != nil {
		t.Fatal(capture.err)
	}
	wantCiphers := []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49171, 49172, 156, 157, 47, 53}
	if !slices.Equal(capture.ciphers, wantCiphers) {
		t.Fatalf("Chrome146 cipher order changed: %v", capture.ciphers)
	}
	wantSettings := []http2.Setting{{ID: 1, Val: 65536}, {ID: 2, Val: 0}, {ID: 4, Val: 6291456}, {ID: 6, Val: 262144}}
	if !slices.Equal(capture.settings, wantSettings) || !slices.Equal(capture.windows, []uint32{15663105}) {
		t.Fatalf("Chrome146 HTTP/2 settings/window changed: %v %v", capture.settings, capture.windows)
	}
	if len(capture.headers) < 4 {
		t.Fatalf("missing pseudo headers: %v", capture.headers)
	}
	for i, name := range []string{":method", ":authority", ":scheme", ":path"} {
		if capture.headers[i].Name != name {
			t.Fatalf("pseudo-header order changed: %v", capture.headers[:4])
		}
	}
}

func curlWireRequest(t *testing.T, compatible bool, rawURL string, roots *x509.CertPool, captures <-chan curlWireCapture, headers http.Header) (*Response, curlWireCapture) {
	t.Helper()
	constructor := NewFingerprintClientExact
	if compatible {
		constructor = NewCurlCompatibleFingerprintClientExact
	}
	client, err := constructor("chrome146", "", nil, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Profile() != "chrome_146" {
		t.Fatalf("selected another profile: %s", client.Profile())
	}
	response, err := client.Do(context.Background(), Request{URL: rawURL, Headers: headers, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case capture := <-captures:
		assertCurlWireChrome146(t, capture)
		return response, capture
	case <-time.After(5 * time.Second):
		t.Fatal("wire capture did not complete")
		return nil, curlWireCapture{}
	}
}

func TestCurlCompatibleChrome146WireIdentity(t *testing.T) {
	rawURL, roots, captures := curlWireFixture(t)
	orders := map[string]bool{}
	for i := 0; i < 5; i++ {
		_, capture := curlWireRequest(t, true, rawURL, roots, captures, nil)
		if curlWireHeader(capture.headers, "accept-encoding") != "gzip, deflate, br" {
			t.Fatal("non-solver request inherited curl navigation compression defaults")
		}
		orders[fmt.Sprint(capture.extensions)] = true
		extensions := slices.Clone(capture.extensions)
		slices.Sort(extensions)
		// Loopback uses an IP URL, so neither client sends the SNI extension.
		want := []uint16{5, 10, 11, 13, 16, 18, 23, 27, 35, 43, 45, 51, 17613, 65037, 65281}
		if !slices.Equal(extensions, want) {
			t.Fatalf("curl_cffi 0.15.0 Chrome146 extension set differs: %v", extensions)
		}
	}
	if len(orders) < 2 {
		t.Fatal("independent Chrome146 handshakes kept a fixed extension order")
	}
	var previous []uint16
	for i := 0; i < 2; i++ {
		_, capture := curlWireRequest(t, false, rawURL, roots, captures, nil)
		if !slices.Contains(capture.extensions, uint16(0xca34)) {
			t.Fatal("compatibility mutated the ordinary bundled Chrome146 profile")
		}
		if i > 0 && !slices.Equal(previous, capture.extensions) {
			t.Fatal("ordinary profile unexpectedly randomized its extension order")
		}
		previous = capture.extensions
		if curlWireHeader(capture.headers, "accept-encoding") != "gzip, deflate, br" {
			t.Fatal("ordinary compression defaults changed")
		}
	}
}

func TestCurlCompatibleHTTP2HeaderOrderAndZstd(t *testing.T) {
	rawURL, roots, captures := curlWireFixture(t)
	headers := http.Header{"Accept": {"*/*"}, "Accept-Encoding": {"gzip, deflate, br, zstd"}, "Referer": {rawURL}, "Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}, "Priority": {"u=0, i"}, "X-Fixture": {"synthetic"}}
	response, capture := curlWireRequest(t, true, rawURL, roots, captures, headers)
	var names []string
	for _, field := range capture.headers {
		names = append(names, field.Name)
	}
	want := []string{":method", ":authority", ":scheme", ":path", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "accept-encoding", "accept-language", "priority", "referer", "x-fixture"}
	if !slices.Equal(names, want) {
		t.Fatalf("curl Chrome header order differs: %v", names)
	}
	if curlWireHeader(capture.headers, "accept-encoding") != "gzip, deflate, br, zstd" {
		t.Fatal("curl Chrome compression capabilities missing")
	}
	if string(response.Body) != `{"fixture":"decoded response"}` {
		t.Fatalf("zstd response was not decoded: encoding=%q body=%q", response.Header.Get("Content-Encoding"), response.Body)
	}
	if slices.Contains(names, "sec-fetch-user") || slices.Contains(names, "upgrade-insecure-requests") {
		t.Fatal("transport injected navigation headers into a non-navigation request")
	}
	_, capture = curlWireRequest(t, true, rawURL, roots, captures, http.Header{"Accept-Encoding": {"identity"}})
	if curlWireHeader(capture.headers, "accept-encoding") != "identity" || curlWireHeader(capture.headers, "priority") != "" {
		t.Fatal("transport replaced explicit compression or injected priority")
	}
	headers.Del("Accept-Encoding")
	_, capture = curlWireRequest(t, false, rawURL, roots, captures, headers)
	if capture.headers[4].Name != "accept" || curlWireHeader(capture.headers, "accept-encoding") != "gzip, deflate, br" {
		t.Fatal("ordinary header ordering/defaults changed")
	}
}

// Inspect the raw HPACK fields: Go's HTTP/2 server normalizes equal duplicate
// Content-Length fields, so a handler's Header.Get would conceal this defect.
func TestFingerprintHTTP2ContentLengthIsEmittedOnce(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		mode := "ordinary"
		if compatible {
			mode = "curl_compatible"
		}
		for _, headerName := range []string{"Content-Length", "content-length"} {
			for _, body := range []struct {
				name string
				data []byte
			}{{"nil_body", nil}, {"empty_body", []byte{}}, {"nonempty_body", []byte("fixture payload")}} {
				t.Run(mode+"/"+headerName+"/"+body.name, func(t *testing.T) {
					rawURL, roots, captures := curlWireFixture(t)
					constructor := NewFingerprintClientExact
					if compatible {
						constructor = NewCurlCompatibleFingerprintClientExact
					}
					client, err := constructor("chrome146", "", nil, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
					if err != nil {
						t.Fatal(err)
					}
					defer client.Close()
					want := strconv.Itoa(len(body.data))
					headers := http.Header{headerName: {want}, "Accept": {"*/*"}}
					_, err = client.Do(context.Background(), Request{Method: http.MethodPost, URL: rawURL, Headers: headers, Body: body.data, Timeout: 5 * time.Second, SingleAttempt: true})
					if err != nil {
						t.Fatal(err)
					}
					var capture curlWireCapture
					select {
					case capture = <-captures:
					case <-time.After(5 * time.Second):
						t.Fatal("wire capture did not complete")
					}
					if capture.err != nil {
						t.Fatal(capture.err)
					}
					var lengths []string
					for _, field := range capture.headers {
						if field.Name == "content-length" {
							lengths = append(lengths, field.Value)
						}
					}
					if !slices.Equal(lengths, []string{want}) {
						t.Fatalf("expected one content-length=%s, received %v", want, lengths)
					}
					if curlWireHeader(capture.headers, "content-type") != "" {
						t.Fatal("an untyped POST gained Content-Type")
					}
					if values := headers[headerName]; !slices.Equal(values, []string{want}) {
						t.Fatal("transport mutated caller headers")
					}
				})
			}
		}
	}
}
