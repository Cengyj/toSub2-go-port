package legacyruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestGoCloudflarePureGoWindowTransferRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var frames, completed, verified atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<script>
var _cf_chl_opt={};var f=document.createElement('iframe');f.src=%q;document.body.appendChild(f);
var parentDetached=false,roundtrip=false,childIdentity=false;
window.addEventListener('message',async e=>{
 if(!e.data)return;
 if(e.data.event==='ready'){
   const bytes=new Uint8Array([0,255,128,65]);const data={event:'execute',bytes,map:new Map([['typed',bytes]])};data.self=data;
   f.contentWindow.postMessage(data,'*',[bytes.buffer]);parentDetached=bytes.byteLength===0;
 }
 if(e.data.event==='complete'){
   const data=e.data;roundtrip=data.self===data&&data.bytes instanceof Uint8Array&&Array.from(data.bytes).join(',')==='0,255,128,65'&&data.map instanceof Map&&data.map.get('typed')===data.bytes;
   childIdentity=e.isTrusted&&e.source===f.contentWindow&&e.origin===%q;
 }
 if(e.data.event==='child-detached')await fetch('/finish',{method:'POST',body:JSON.stringify({parentDetached,roundtrip,childIdentity,childDetached:e.data.detached,parentIdentity:e.data.parentIdentity})});
});
</script>`, server.URL+"/frame/rch/transfer", server.URL)
		case "/frame/rch/transfer":
			frames.Add(1)
			fmt.Fprintf(w, `<script>
window.addEventListener('message',e=>{
 if(!e.data||e.data.event!=='execute')return;
 const data=e.data;const parentIdentity=e.isTrusted&&e.source===window.parent&&e.origin===%q;
 if(data.self!==data||!(data.bytes instanceof Uint8Array)||Array.from(data.bytes).join(',')!=='0,255,128,65'||!(data.map instanceof Map)||data.map.get('typed')!==data.bytes)throw Error('transfer fixture invalid');
 const bytes=data.bytes;const response={event:'complete',bytes,map:new Map([['typed',bytes]])};response.self=response;
 window.parent.postMessage(response,'*',[bytes.buffer]);
 window.parent.postMessage({event:'child-detached',detached:bytes.byteLength===0,parentIdentity},'*');
});
window.parent.postMessage({event:'ready'},'*');
</script>`, server.URL)
		case "/finish":
			completed.Add(1)
			var result struct {
				ParentDetached, Roundtrip, ChildIdentity, ChildDetached, ParentIdentity bool
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&result) != nil || !result.ParentDetached || !result.Roundtrip || !result.ChildIdentity || !result.ChildDetached || !result.ParentIdentity {
				t.Error("parent/child transfer lost bytes, cycles, identity or buffer detachment")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "typed-roundtrip", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, "accepted")
		case "/":
			verified.Add(1)
			if cookie, err := r.Cookie("cf_clearance"); err != nil || cookie.Value != "typed-roundtrip" {
				t.Error("typed roundtrip lost its verified clearance")
			}
			_, _ = io.WriteString(w, "verified")
		default:
			t.Error("typed roundtrip used an unexpected fixture route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := goTestClient(t, "")
	if response, err := client.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"}); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("could not obtain the typed transfer challenge")
	}
	if err := client.Challenge(t.Context(), server.URL+"/challenge"); err != nil {
		t.Fatal("typed parent/child transfer failed", err)
	}
	if frames.Load() != 1 || completed.Load() != 1 || verified.Load() != 1 {
		t.Fatal("typed roundtrip duplicated a frame, completion or verification")
	}
	assertRuntimeInputsRemoved(t, tmp)
}
