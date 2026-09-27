package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"tosub2-go-port/internal/logging"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

const diagnosticCanary = "private-diagnostic-canary-7693"

func assertSafeDiagnostic(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected classified error")
	}
	jsonError, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	envelope, marshalErr := json.Marshal(struct {
		Error error `json:"error"`
	}{err})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	outputs := []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%q", err), string(jsonError), string(envelope)}
	for _, output := range outputs {
		for _, secret := range append(forbidden, diagnosticCanary) {
			if secret != "" && strings.Contains(output, secret) {
				t.Fatal("diagnostic exposed raw private error data")
			}
		}
	}
	assertDiagnosticSurvivesRedaction(t, err)
}

// Errors cross several independently sanitized protocol/job/API boundaries.
// Keep closed diagnostic text readable through all of them, including the
// outer wording used by callers, without relaxing the global secret rules.
func assertDiagnosticSurvivesRedaction(t *testing.T, err error) {
	t.Helper()
	for _, candidate := range []error{err, fmt.Errorf("TLS fingerprint transport unavailable for %q: %w", "auto", err)} {
		want := candidate.Error()
		if len([]rune(want)) > 512 {
			t.Fatal("diagnostic fixture exceeds SafeError truncation bound")
		}
		text := want
		for round := 0; round < 5; round++ {
			text = logging.SafeError(errors.New(logging.RedactText(text)))
			if text != want {
				t.Fatalf("safe diagnostic changed after redaction round %d: got %q want %q", round+1, text, want)
			}
			if strings.Contains(text, diagnosticCanary) {
				t.Fatal("redacted diagnostic exposed secret canary")
			}
		}
	}
}

func TestEveryTransportDiagnosticSurvivesRepeatedRedaction(t *testing.T) {
	for kind := failureRequest; kind <= failureBody; kind++ {
		for phase := phaseRequest; phase <= phaseProxyTunnel; phase++ {
			for _, loopback := range []bool{false, true} {
				for _, status := range []int{0, http.StatusBadGateway} {
					d := &TransportError{kind: kind, phase: phase, status: status, loopbackProxy: loopback}
					assertDiagnosticSurvivesRedaction(t, d)
					assertDiagnosticSurvivesRedaction(t, &ProfileProbeError{failures: []*TransportError{d, d, d, d}})
				}
			}
		}
	}
}

func diagnosticProbe(t *testing.T, proxyURL string) error {
	t.Helper()
	client, err := ProbeFingerprintClientWithOptions(context.Background(), "auto", proxyURL, "https://192.0.2.11/?token="+diagnosticCanary, nil, FingerprintProbeOptions{CandidateTimeout: time.Second, TotalTimeout: 3 * time.Second})
	if client != nil {
		client.Close()
		t.Fatal("failing local proxy unexpectedly produced a client")
	}
	assertSafeDiagnostic(t, err, proxyURL, "192.0.2.11", "token=")
	var probe *ProfileProbeError
	if !errors.As(err, &probe) {
		t.Fatalf("lost candidate diagnostics: %v", err)
	}
	if len(probe.failures) != len(ProfileCandidates("auto")) {
		t.Fatalf("diagnostic candidate count=%d", len(probe.failures))
	}
	if strings.Contains(err.Error(), "no compatible") {
		t.Fatal("network failure mislabeled as profile incompatibility")
	}
	return err
}

func TestTLSProbeClosedProxyDiagnostic(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			u := &url.URL{Scheme: scheme, Host: endpoint, User: url.UserPassword(diagnosticCanary+"-user", diagnosticCanary+"-password")}
			err := diagnosticProbe(t, u.String())
			if !strings.Contains(err.Error(), "proxy connection refused") || !strings.Contains(err.Error(), loopbackProxyHint) {
				t.Fatalf("missing actionable local proxy diagnostic: %v", err)
			}
			assertSafeDiagnostic(t, err, endpoint)
		})
	}
}

func TestTLSProbeHTTPProxyStatusDiagnostic(t *testing.T) {
	for _, status := range []int{http.StatusProxyAuthRequired, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Proxy-Authenticate", "Basic realm="+diagnosticCanary)
				w.Header().Set("Set-Cookie", "session="+diagnosticCanary)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, diagnosticCanary)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			u.User = url.UserPassword(diagnosticCanary+"-user", diagnosticCanary+"-password")
			err := diagnosticProbe(t, u.String())
			want := fmt.Sprintf("proxy CONNECT rejected (HTTP %d)", status)
			if status == http.StatusProxyAuthRequired {
				want = "proxy authentication rejected (HTTP 407)"
			}
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), loopbackProxyHint) {
				t.Fatalf("proxy status diagnostic: %v", err)
			}
		})
	}
}

func TestTLSProbeUntrustedHTTPSProxyDiagnostic(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP reached untrusted HTTPS proxy")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	u, _ := url.Parse(server.URL)
	u.User = url.UserPassword(diagnosticCanary+"-user", diagnosticCanary+"-password")
	err := diagnosticProbe(t, u.String())
	if !strings.Contains(err.Error(), "HTTPS proxy certificate validation failed") || strings.Contains(err.Error(), loopbackProxyHint) {
		t.Fatalf("certificate diagnosis lost: %v", err)
	}
}

type diagnosticFailureClient struct {
	tlsclient.HttpClient
	err error
}

func (c *diagnosticFailureClient) Do(*fhttp.Request) (*fhttp.Response, error) {
	return nil, c.err
}

func TestTransportDiagnosticFormattingDiscardsRawErrors(t *testing.T) {
	secretURL := "https://" + diagnosticCanary + ":password@" + diagnosticCanary + ".invalid/?token=" + diagnosticCanary
	certificate := &x509.Certificate{Subject: pkix.Name{CommonName: diagnosticCanary}, DNSNames: []string{diagnosticCanary + ".invalid"}}
	cases := []struct {
		name string
		raw  error
		kind failureKind
	}{
		{"dns", &net.DNSError{Err: diagnosticCanary, Name: diagnosticCanary, Server: diagnosticCanary}, failureDNS},
		{"refused", &net.OpError{Op: diagnosticCanary, Net: diagnosticCanary, Err: fmt.Errorf("%s: %w", diagnosticCanary, syscall.ECONNREFUSED)}, failureRefused},
		{"unreachable", fmt.Errorf("%s: %w", diagnosticCanary, syscall.ENETUNREACH), failureUnreachable},
		{"certificate", x509.CertificateInvalidError{Cert: certificate, Reason: x509.Expired, Detail: diagnosticCanary}, failureCertificate},
		{"hostname", x509.HostnameError{Certificate: certificate, Host: diagnosticCanary}, failureCertificate},
		{"untrusted", x509.UnknownAuthorityError{Cert: certificate}, failureCertificate},
		{"tls-record", tls.RecordHeaderError{Msg: diagnosticCanary}, failureTLS},
		{"closed", fmt.Errorf("%s: %w", diagnosticCanary, io.ErrUnexpectedEOF), failureClosed},
		{"cancel", fmt.Errorf("%s: %w", diagnosticCanary, context.Canceled), failureCanceled},
		{"deadline", fmt.Errorf("%s: %w", diagnosticCanary, context.DeadlineExceeded), failureTimeout},
		{"unknown", errors.New(secretURL), failureRequest},
		{"safe-wrapped", fmt.Errorf("%s: %w", diagnosticCanary, &TransportError{kind: failureProxyAuth}), failureProxyAuth},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := &url.Error{Op: diagnosticCanary, URL: secretURL, Err: test.raw}
			factory := func(name string) (*FingerprintClient, error) {
				c, err := newFingerprintClientExact(name, "", nil)
				if err != nil {
					return nil, err
				}
				c.client = &diagnosticFailureClient{HttpClient: c.client, err: raw}
				return c, nil
			}
			c, err := factory("chrome_133")
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Do(context.Background(), Request{URL: secretURL})
			c.Close()
			assertSafeDiagnostic(t, err, secretURL)
			var classified *TransportError
			if !errors.As(err, &classified) || classified.kind != test.kind {
				t.Fatalf("classification failed: %v", err)
			}
			assertSafeDiagnostic(t, *classified, secretURL)
			_, aggregate := probeFingerprintWithOptions(context.Background(), "auto", secretURL, FingerprintProbeOptions{CandidateTimeout: time.Second, TotalTimeout: 3 * time.Second}, factory)
			assertSafeDiagnostic(t, aggregate, secretURL)
			var probe *ProfileProbeError
			if !errors.As(aggregate, &probe) {
				t.Fatalf("missing aggregate: %v", aggregate)
			}
			assertSafeDiagnostic(t, *probe, secretURL)
			for _, safe := range probe.Unwrap() {
				assertSafeDiagnostic(t, safe, secretURL)
			}
			for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
				if errors.Is(raw, sentinel) != errors.Is(aggregate, sentinel) || errors.Is(raw, sentinel) != errors.Is(err, sentinel) {
					t.Fatal("context sentinel contract changed")
				}
			}
		})
	}
}

func TestTLSProbeCandidateDeadlineAggregate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	factory, snapshot := trackedProbeFactory()
	client, err := probeFingerprintWithOptions(context.Background(), "auto", server.URL, FingerprintProbeOptions{CandidateTimeout: 80 * time.Millisecond, TotalTimeout: 3 * time.Second}, factory)
	var aggregate *ProfileProbeError
	if client != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &aggregate) {
		t.Fatalf("candidate deadline aggregate contract: %v", err)
	}
	if len(aggregate.failures) != len(ProfileCandidates("auto")) {
		t.Fatalf("candidate deadline count=%d", len(aggregate.failures))
	}
	for name, client := range snapshot() {
		if client.closed.Load() != 1 {
			t.Fatalf("timed-out candidate %s was not closed", name)
		}
	}
}

func TestTransportLoopbackHintScope(t *testing.T) {
	for _, address := range []string{"http://localhost:1", "https://LOCALHOST.:1", "http://127.0.0.2:1", "socks5h://[::1]:1"} {
		u, _ := url.Parse(address)
		if !isLoopbackProxy(u) {
			t.Fatalf("loopback proxy missed: %s", address)
		}
	}
	for _, address := range []string{"http://host.docker.internal:1", "http://192.0.2.10:1", "http://localhost.example:1"} {
		u, _ := url.Parse(address)
		if isLoopbackProxy(u) {
			t.Fatalf("non-loopback proxy received container hint: %s", address)
		}
	}
	for _, phase := range []failurePhase{phaseRequest, phaseProxyConnect, phaseProxyTLS, phaseProxyTunnel} {
		for _, kind := range []failureKind{failureRequest, failureRefused, failureUnreachable, failureTimeout, failureDNS, failureCertificate, failureProxyAuth, failureTLS} {
			d := &TransportError{kind: kind, phase: phase, loopbackProxy: true}
			want := phase == phaseProxyConnect && (kind == failureRequest || kind == failureRefused || kind == failureUnreachable || kind == failureTimeout)
			if strings.Contains(d.Error(), loopbackProxyHint) != want {
				t.Fatalf("incorrect container hint phase=%d kind=%d", phase, kind)
			}
		}
	}
}
