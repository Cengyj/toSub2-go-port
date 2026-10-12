package legacyruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestChallengeDeadlineReportsSafeStage(t *testing.T) {
	testChallengeSafeFailureDiagnostics(t, false)
}

func TestChallengeImmediateScriptErrorReportsSafeStage(t *testing.T) {
	testChallengeSafeFailureDiagnostics(t, true)
}

func testChallengeSafeFailureDiagnostics(t *testing.T, exception bool) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var resourceCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			if r.Header.Get("Priority") != "" || r.Header.Get("Upgrade-Insecure-Requests") != "" {
				t.Error("solver defaults escaped into an ordinary protocol request")
			}
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			ending := ""
			if exception {
				ending = `throw new Error("fixture-private-url-token-password");`
			}
			fmt.Fprintf(w, `<script>var _cf_chl_opt={};fetch(%q);%s</script>`, server.URL+"/resource", ending)
		case "/resource":
			resourceCalls.Add(1)
			if r.Header.Get("Priority") != "u=0, i" || r.Header.Get("Sec-Fetch-User") != "?1" || r.Header.Get("Upgrade-Insecure-Requests") != "1" || r.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
				t.Error("dynamic solver lost curl Chrome default headers")
			}
			_, _ = w.Write([]byte("fixture"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := goTestClient(t, "")
	c.opts.SolverTimeout = 800 * time.Millisecond
	if _, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
		t.Fatal(err)
	}
	err := c.Challenge(context.Background(), server.URL+"/challenge")
	if err == nil || errors.Is(err, context.DeadlineExceeded) == exception {
		t.Fatal("challenge script failure or timeout lost its classification", err)
	}
	wants := []string{"stage=parent_execution", "frame=false child=false", "requests=1", "last_http=200"}
	if exception {
		wants = append(wants, "GO_JS_RUNTIME: SCRIPT_EXCEPTION")
	} else {
		wants = append(wants, "solver time budget exhausted")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("safe diagnostics missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), server.URL) || resourceCalls.Load() != 1 {
		t.Fatal("diagnostics exposed remote data or replayed a request", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestChallengeFinalNestedErrorsAreCountedWithoutRemoteText(t *testing.T) {
	c, tmp := bridgeTestClient(t, "nested-errors", "")
	c.challenges["https://fixture.invalid/"] = []byte(`<script>var _cf_chl_opt={};</script>`)
	err := c.Challenge(context.Background(), "https://fixture.invalid/")
	if err == nil || !strings.Contains(err.Error(), "parent_errors=2") || strings.Contains(err.Error(), "fixture-private") {
		t.Fatal("final errors were lost or remote text was disclosed", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("completed runtime incorrectly waited for its deadline")
	}
	assertRuntimeInputsRemoved(t, tmp)
}
