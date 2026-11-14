package legacyruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestClearanceValuesUseMatchedJarEntries(t *testing.T) {
	jar := transport.NewCookieJar()
	jar.SetFromResponse("https://challenge.example.test/challenge", http.Header{"Set-Cookie": {
		`cf_clearance="root quoted"; Path=/`,
		"cf_clearance=path-scoped; Path=/challenge",
		"cf_clearance=wrong-path; Path=/elsewhere",
		"cf_clearance=secure-only; Path=/challenge/nested; Secure",
		"unrelated=ignored; Path=/",
	}})
	values := clearanceValues(jar, "http://challenge.example.test/challenge/nested")
	if len(values) != 2 || !values["root quoted"] || !values["path-scoped"] {
		t.Fatal("clearance matching or raw cookie values diverged from the owning jar")
	}
}

func TestGoWorkerUsesOnlyAuthoritativeCookieJar(t *testing.T) {
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loader.js":
			fmt.Fprintf(w, `var script=document.createElement('script');script.src=%q;document.head.appendChild(script);`, server.URL+"/sentinel/fixture/sdk.js")
		case "/sentinel/fixture/sdk.js":
			fmt.Fprintf(w, `window.SentinelSDK={
				token:async function(flow){
					if (__config.cookies !== undefined) throw new Error('duplicate cookie snapshot');
					document.cookie='public=script; Path=/';
					const reply=await __host.request({kind:'fetch',method:'GET',url:%q,headers:{cookie:'forged=stale'}});
					if (Object.keys(reply.headers).some(name=>/^set-cookie2?$/i.test(name))) throw new Error('raw cookie header crossed bridge');
					if (reply.headers['x-visible'] !== 'preserved') throw new Error('ordinary header lost');
					const visible=document.cookie;
					if (!visible.includes('public=server') || visible.includes('protected=') || visible.includes('server-private=')) throw new Error('document cookie diverged');
					return JSON.stringify({p:'shared-jar-proof',t:null,c:'fixture',id:'authority-device',flow});
				},
				sessionObserverToken:async flow=>JSON.stringify({so:'observer',c:'fixture',id:'authority-device',flow})
			};`, server.URL+"/resource")
		case "/resource":
			requests.Add(1)
			if header := r.Header.Get("Cookie"); !strings.Contains(header, "protected=server") || !strings.Contains(header, "public=script") || strings.Contains(header, "forged=") {
				t.Error("script request did not use the authoritative cookie jar")
			}
			w.Header().Add("Set-Cookie", "public=server; Path=/")
			w.Header().Add("Set-Cookie", "server-private=hidden; Path=/; HttpOnly")
			w.Header().Set("Set-Cookie2", "obsolete-private=hidden")
			w.Header().Set("X-Visible", "preserved")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := goTestClient(t, server.URL+"/loader.js")
	jar := transport.NewCookieJar()
	jar.SetFromResponse(server.URL, http.Header{"Set-Cookie": {"protected=server; Path=/; HttpOnly"}})
	client.SetJar(jar)
	headers, err := client.Sentinel(context.Background(), server.URL, "authority-device", "password_verify", server.URL+"/log-in")
	if err != nil || headers.Get("Openai-Sentinel-Token") == "" || requests.Load() != 1 {
		t.Fatal("worker did not complete through the authoritative jar", err)
	}
	if jar.Value("public", server.URL) != "server" || jar.Value("server-private", server.URL) != "hidden" {
		t.Fatal("response cookies were not retained by the owning jar")
	}
}
