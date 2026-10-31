package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type limitedTestBody struct {
	io.Reader
	closed bool
	read   int
}

func (b *limitedTestBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.read += n
	return n, e
}
func (b *limitedTestBody) Close() error { b.closed = true; return nil }

type limitedFingerprintClient struct {
	tlsclient.HttpClient
	body   *limitedTestBody
	status int
}

func (c *limitedFingerprintClient) Do(r *fhttp.Request) (*fhttp.Response, error) {
	return &fhttp.Response{StatusCode: c.status, Header: fhttp.Header{}, Body: c.body, Request: r}, nil
}

func TestTransportResponseLimitClosesBodyAndPreservesStatus(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		for _, status := range []int{200, 403} {
			for _, size := range []int{4, 5, 10000} {
				t.Run(kind+"/"+http.StatusText(status)+"/"+strings.Repeat("x", min(size, 6)), func(t *testing.T) {
					b := &limitedTestBody{Reader: strings.NewReader(strings.Repeat("x", size))}
					var c lifetimeDoer
					if kind == "native" {
						x := NewClient(nil)
						x.HTTP.Transport = completionRoundTripper(func(r *http.Request) (*http.Response, error) {
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: b, Request: r}, nil
						})
						c = x
					} else {
						x, e := NewFingerprintClient("chrome_133", "", nil)
						if e != nil {
							t.Fatal(e)
						}
						x.client = &limitedFingerprintClient{HttpClient: x.client, body: b, status: status}
						c = x
					}
					defer c.Close()
					resp, e := c.Do(context.Background(), Request{Method: http.MethodGet, URL: "https://fixture.invalid/info", MaxResponseBytes: 4})
					if !b.closed || b.read > 5 || resp == nil || resp.StatusCode != status {
						t.Fatalf("closed=%v read=%d response=%v", b.closed, b.read, resp)
					}
					if size > 4 {
						if !errors.Is(e, ErrResponseTooLarge) || len(resp.Body) != 0 || IsRetryableReadError(e) {
							t.Fatalf("oversize: %v %v", resp, e)
						}
					} else if e != nil || string(resp.Body) != "xxxx" {
						t.Fatalf("exact limit: %v %v", resp, e)
					}
				})
			}
		}
	}
}

func TestResponseLimitZeroPreservesUnboundedCaller(t *testing.T) {
	data, e := readResponseBody(strings.NewReader("synthetic"), 0)
	if e != nil || string(data) != "synthetic" {
		t.Fatal(string(data), e)
	}
}
