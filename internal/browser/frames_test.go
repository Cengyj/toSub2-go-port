package browser

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChromiumCrossSiteOOPIFDOMAndDetach(t *testing.T) {
	s, ctx := integrationSession(t)
	frame := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><div id="answer">isolated-frame-ready</div><script>window.privateFixture='not-an-isolated-world-global'</script>`)
	}))
	defer frame.Close()
	frameURL := strings.Replace(frame.URL, "127.0.0.1", "localhost", 1) + "/challenge/frame"
	top := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><iframe src=%q></iframe>`, frameURL)
	}))
	defer top.Close()
	if err := s.Navigate(ctx, top.URL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.frameTargetIDs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.frameTargetIDs()) == 0 {
		t.Fatal("fixture did not create a real out-of-process iframe")
	}
	value, err := s.EvaluateFrame(ctx, frameURL, `document.querySelector('#answer').textContent`)
	if err != nil || value != "isolated-frame-ready" {
		t.Fatalf("OOPIF DOM: value=%v err=%v", value, err)
	}
	value, err = s.EvaluateFrame(ctx, frameURL, `typeof window.privateFixture`)
	if err != nil || value != "undefined" {
		t.Fatal("iframe page global leaked into isolated world")
	}
	urls, err := s.FrameURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range urls {
		found = found || u == frameURL
	}
	if !found {
		t.Fatal("OOPIF URL omitted")
	}
	if _, err = s.Evaluate(ctx, `document.querySelector('iframe').remove()`); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for len(s.frameTargetIDs()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.frameTargetIDs()) != 0 {
		t.Fatal("detached iframe session retained")
	}
	if _, err = s.EvaluateFrame(ctx, frameURL, `1`); err == nil {
		t.Fatal("detached frame accepted")
	}
}
