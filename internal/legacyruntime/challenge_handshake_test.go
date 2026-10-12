package legacyruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestGoCloudflareDeferredExecuteHandshake(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var frameCalls, finishCalls, verifyCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<html><body><script>
var _cf_chl_opt={};
var frame=document.createElement("iframe");
frame.src=%q;
document.body.appendChild(frame);
window.addEventListener("message",function(e){
  if(!e.data)return;
  if(e.data.event==="init")frame.contentWindow.postMessage({event:"init"},"*");
  if(e.data.event==="requestExtraParams")frame.contentWindow.postMessage({event:"extraParams"},"*");
  if(e.data.event==="ready"){
    frame.contentWindow.postMessage({event:"configuration"},"*");
    frame.contentWindow.postMessage({event:"execute"},"*");
  }
  if(e.data.event==="complete")fetch(%q,{method:"POST",body:JSON.stringify(e.data)});
});
</script></body></html>`, server.URL+"/frame/rch/fixture", server.URL+"/finish")
		case "/frame/rch/fixture":
			frameCalls.Add(1)
			if r.Header.Get("Sec-Fetch-Dest") != "iframe" {
				t.Error("frame request lost iframe navigation headers")
			}
			fmt.Fprintf(w, `<html><body><script>
var seen=[];var parentIdentity=true;
window.addEventListener("message",function(e){
  if(!e.data)return;
  seen.push(e.data.event);
  parentIdentity=parentIdentity&&e.isTrusted&&e.source===window.parent&&e.origin===%q;
  if(e.data.event==="extraParams")window.parent.postMessage({event:"ready"},"*");
  if(e.data.event==="execute")window.parent.postMessage({event:"complete",seen:seen,parentIdentity:parentIdentity},"*");
});
</script></body></html>`, server.URL)
		case "/finish":
			finishCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var result struct {
				Event          string   `json:"event"`
				Seen           []string `json:"seen"`
				ParentIdentity bool     `json:"parentIdentity"`
			}
			if r.Method != http.MethodPost || json.Unmarshal(body, &result) != nil || result.Event != "complete" || !result.ParentIdentity || !reflect.DeepEqual(result.Seen, []string{"init", "extraParams", "configuration", "execute"}) {
				t.Errorf("handshake lost message order, identity or dynamic event: %s", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Set-Cookie", "cf_clearance=deferred-handshake; Path=/; HttpOnly")
			_, _ = w.Write([]byte("accepted"))
		case "/":
			verifyCalls.Add(1)
			if !strings.Contains(r.Header.Get("Cookie"), "cf_clearance=deferred-handshake") {
				t.Error("verification lost the newly issued clearance")
			}
			_, _ = w.Write([]byte("verified page"))
		default:
			t.Errorf("unexpected handshake path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := goTestClient(t, "")
	response, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"})
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("failed to load the fixture challenge", err)
	}
	if err := c.Challenge(context.Background(), server.URL+"/challenge"); err != nil {
		t.Fatal(err)
	}
	if frameCalls.Load() != 1 || finishCalls.Load() != 1 || verifyCalls.Load() != 1 {
		t.Fatalf("frame=%d finish=%d verify=%d", frameCalls.Load(), finishCalls.Load(), verifyCalls.Load())
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestTurnstileRuntimeOrdersInitialAndLiveParentMessages(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c := goTestClient(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make(chan runtimeEvent, 8)
	r, err := c.startRuntime(ctx, "child", map[string]any{
		"url":       "https://fixture.invalid/rch/widget",
		"parentUrl": "https://parent.fixture.invalid/",
		"userAgent": c.UserAgent(),
		"waitMs":    2500,
		"html": `<script>
var seen=[];var parentIdentity=true;
window.addEventListener("message",function(e){
  seen.push(e.data.event);
  parentIdentity=parentIdentity&&e.isTrusted&&e.source===window.parent&&e.origin==="https://parent.fixture.invalid";
  if(seen.length===4)window.parent.postMessage({seen:seen,parentIdentity:parentIdentity},"*");
});
</script>`,
		"parentMessages": []map[string]string{{"event": "init"}, {"event": "extraParams"}},
	}, events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
		assertRuntimeInputsRemoved(t, tmp)
	}()
	for _, kind := range []string{"configuration", "execute"} {
		if err := r.send(ctx, map[string]any{"type": "parent-message", "data": map[string]string{"event": kind}}); err != nil {
			t.Fatal(err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal("initial and live messages did not complete", ctx.Err())
		case event := <-events:
			if event.err != nil {
				t.Fatal(event.err)
			}
			if event.message.Type == "final" {
				t.Fatal("child runtime completed without delivering all parent messages")
			}
			if event.message.Type != "parent-message" {
				continue
			}
			var result struct {
				Seen           []string `json:"seen"`
				ParentIdentity bool     `json:"parentIdentity"`
			}
			if json.Unmarshal(event.message.Message.Data, &result) != nil || !result.ParentIdentity || !reflect.DeepEqual(result.Seen, []string{"init", "extraParams", "configuration", "execute"}) {
				t.Fatalf("initial/live parent message order or identity changed: %#v", result)
			}
			return
		}
	}
}
