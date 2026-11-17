package transport

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andybalholm/brotli"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/klauspost/compress/zstd"
)

func TestFingerprintCompressedResponseLimitAndReuse(t *testing.T) {
	plain := bytes.Repeat([]byte("synthetic response data\n"), 256)
	for _, encoding := range []string{"br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			if encoding == "br" {
				writer := brotli.NewWriter(&compressed)
				if _, err := writer.Write(plain); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				writer, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(plain); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.ProtoMajor != 2 {
					t.Error("fixture did not exercise HTTP/2 decompression")
				}
				w.Header().Set("Content-Encoding", encoding)
				w.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
				_, _ = w.Write(compressed.Bytes())
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			client, err := NewCurlCompatibleFingerprintClientExact("chrome146", "", nil, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, limit := range []int{len(plain), 32, len(plain)} {
				response, err := client.Do(t.Context(), Request{URL: server.URL, Headers: http.Header{"Accept-Encoding": {encoding}}, MaxResponseBytes: int64(limit), CaptureResponse: true, SingleAttempt: true})
				if response == nil || response.StatusCode != http.StatusOK {
					t.Fatalf("response receipt missing: response=%v error=%v", response, err)
				}
				if limit < len(plain) {
					if !errors.Is(err, ErrResponseTooLarge) || response.BodyComplete || !bytes.Equal(response.Body, plain[:limit]) {
						t.Fatalf("decoded response limit was not enforced: bytes=%d complete=%v error=%v", len(response.Body), response.BodyComplete, err)
					}
				} else if err != nil || !response.BodyComplete || !bytes.Equal(response.Body, plain) {
					t.Fatalf("compressed response did not complete: bytes=%d complete=%v error=%v", len(response.Body), response.BodyComplete, err)
				}
			}
			if calls.Load() != 3 {
				t.Fatalf("compression handling replayed a request: calls=%d", calls.Load())
			}
		})
	}
}
