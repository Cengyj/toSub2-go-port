package legacyruntime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestChallengeStrictTimerCanCreateFrameAfterRequests(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var mu sync.Mutex
	var paths []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		wantMethod := http.MethodGet
		if r.URL.Path == "/finish" {
			wantMethod = http.MethodPost
		}
		if r.Method != wantMethod {
			t.Errorf("fixture request %q used %s, want %s", r.URL.Path, r.Method, wantMethod)
		}
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<script>
var _cf_chl_opt={};
window.addEventListener("message", function(event) {
  if (event.data && event.data.event === "complete") fetch(%q, {method:"POST"});
});
fetch(%q).then(function() {
  fetch(%q);
  void document.cookie;
  setTimeout(function(frameURL) {
    "use strict";
    if (this !== globalThis) throw new Error("synthetic timer global mismatch");
    var frame=this.document.createElement("iframe");
    frame.src=frameURL;
    this.document.body.appendChild(frame);
    frame.contentWindow.postMessage({event:"execute"}, "*");
  }, 0, %q);
});
</script>`, server.URL+"/finish", server.URL+"/first", server.URL+"/second", server.URL+"/frame/rch/timer")
		case "/first", "/second":
			_, _ = w.Write([]byte("fixture resource"))
		case "/frame/rch/timer":
			_, _ = w.Write([]byte(`<script>
window.addEventListener("message", function(event) {
  if (!event.data || event.data.event !== "execute") return;
  setTimeout(function() {
    "use strict";
    if (this !== globalThis) throw new Error("synthetic child timer global mismatch");
    this.parent.postMessage({event:"complete"}, "*");
  }, 0);
});
</script>`))
		case "/finish":
			w.Header().Set("Set-Cookie", "cf_clearance=timer-fixture; Path=/; HttpOnly")
			_, _ = w.Write([]byte("accepted"))
		case "/":
			if !strings.Contains(r.Header.Get("Cookie"), "cf_clearance=timer-fixture") {
				t.Error("verification lost the new clearance")
			}
			_, _ = w.Write([]byte("verified page"))
		default:
			t.Errorf("unexpected local fixture request %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := goTestClient(t, "")
	c.opts.SolverTimeout = 3 * time.Second
	response, err := c.Do(t.Context(), transport.Request{URL: server.URL + "/challenge"})
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("failed to load the fixture challenge", err)
	}
	if err := c.Challenge(t.Context(), server.URL+"/challenge"); err != nil {
		t.Fatal(err)
	}
	if err := c.Challenge(t.Context(), server.URL+"/challenge"); err == nil || !strings.Contains(err.Error(), "no matching challenge response") {
		t.Fatal("completed challenge could be replayed")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/challenge", "/first", "/second", "/frame/rch/timer", "/finish", "/"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("challenge request sequence changed: got %v, want %v", paths, want)
	}
	assertRuntimeInputsRemoved(t, tmp)
}
