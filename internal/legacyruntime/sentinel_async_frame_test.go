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
	"time"
)

// The SDK builds its iframe after an awaited Go HTTP response, rather than in
// the initial token() stack. Both methods must complete through the real child
// frame SDK, and reusing the frame must never replay its load event.
func TestGoSentinelPureGoLoadsAsynchronouslyCreatedSDKFrameOnce(t *testing.T) {
	testSentinelLoadsAsyncSDKFrameOnce(t)
}

func testSentinelLoadsAsyncSDKFrameOnce(t *testing.T) {
	for _, firstFrameMethod := range []string{"token", "sessionObserverToken"} {
		t.Run(firstFrameMethod, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var preparations, frameCalls atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/loader.js":
					fmt.Fprintf(w, `const script=document.createElement("script");script.src=%q;document.head.appendChild(script);`, server.URL+"/sentinel/fixture/sdk.js")
				case "/sentinel/fixture/sdk.js":
					fmt.Fprintf(w, asyncFrameSDKFixture, server.URL+"/fixture-prepare", firstFrameMethod, server.URL+"/backend-api/sentinel/frame.html", server.URL+"/fixture-frame-token")
				case "/fixture-prepare":
					preparations.Add(1)
					// Force iframe insertion into a later event-loop turn, after the
					// former callSdk-only zero-delay load dispatch has already run.
					time.Sleep(20 * time.Millisecond)
					_, _ = io.WriteString(w, "prepared")
				case "/fixture-frame-token":
					frameCalls.Add(1)
					var body struct {
						Method, Flow string
						Loads        int
						Framed       bool
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Framed || body.Loads != 1 || body.Flow != "password_verify" {
						t.Error("child SDK did not receive exactly one frame load before token execution")
					}
					if body.Method != "token" && body.Method != "sessionObserverToken" {
						t.Error("unexpected child SDK method")
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"proof": "local-iframe-proof"})
				default:
					t.Errorf("unexpected local fixture request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := goTestClient(t, server.URL+"/loader.js")
			client.opts.SolverTimeout = 3 * time.Second
			headers, err := client.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/log-in/password")
			if err != nil {
				t.Fatal("asynchronous iframe SDK did not complete token and observer", err)
			}
			for _, header := range []string{"Openai-Sentinel-Token", "Openai-Sentinel-So-Token"} {
				var token map[string]any
				if err := json.Unmarshal([]byte(headers.Get(header)), &token); err != nil || token["id"] != "fixture-device" || token["flow"] != "password_verify" {
					t.Fatal("iframe SDK token lost its device or flow", err)
				}
				if header == "Openai-Sentinel-Token" && firstFrameMethod == "sessionObserverToken" {
					if token["p"] != "direct-fixture-proof" {
						t.Fatal("fixture did not defer first frame creation until observer")
					}
					continue
				}
				if token["frameLoads"] != float64(1) || token["childLoads"] != float64(1) {
					t.Fatal("frame load was lost or dispatched more than once")
				}
			}
			wantFrameCalls := int32(2)
			if firstFrameMethod == "sessionObserverToken" {
				wantFrameCalls = 1
			}
			if preparations.Load() != 2 || frameCalls.Load() != wantFrameCalls {
				t.Fatal("SDK did not finish both awaited operations in the same runtime")
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}

const asyncFrameSDKFixture = `
(() => {
  const prepareURL = %q, firstFrameMethod = %q, frameURL = %q, tokenURL = %q;
  if (window.__sentinelFrame) {
    let loads = 0;
    window.addEventListener("load", () => { loads++; });
    window.addEventListener("message", async event => {
      if (event.data?.type !== "fixture-call" || event.source !== window.parent) return;
      const {method, flow} = event.data;
      const response = await fetch(tokenURL, {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({method, flow, loads, framed:window.top !== window})});
      const data = await response.json();
      const token = method === "token"
        ? {p:data.proof,t:null,c:"fixture-challenge",id:"fixture-device",flow}
        : {so:data.proof,c:"fixture-challenge",id:"fixture-device",flow};
      window.parent.postMessage({type:"fixture-result",method,flow,token,childLoads:loads}, "*");
    });
    return;
  }
  let frame, ready, frameLoads = 0;
  function ensureFrame() {
    if (ready) return ready;
    ready = new Promise(resolve => {
      frame = document.createElement("iframe");
      frame.src = frameURL;
      frame.addEventListener("load", () => { frameLoads++; resolve(frame); });
      document.body.appendChild(frame);
    });
    return ready;
  }
  async function call(method, flow) {
    await fetch(prepareURL+"?method="+method);
    if (method === "token" && firstFrameMethod === "sessionObserverToken") {
      return JSON.stringify({p:"direct-fixture-proof",t:null,c:"fixture-challenge",id:"fixture-device",flow});
    }
    await ensureFrame();
    return await new Promise(resolve => {
      const receive = event => {
        if (event.data?.type !== "fixture-result" || event.data.method !== method || event.data.flow !== flow) return;
        window.removeEventListener("message", receive);
        resolve(JSON.stringify({...event.data.token,frameLoads,childLoads:event.data.childLoads}));
      };
      window.addEventListener("message", receive);
      frame.contentWindow.postMessage({type:"fixture-call",method,flow}, "*");
    });
  }
  window.SentinelSDK = {token:flow=>call("token",flow),sessionObserverToken:flow=>call("sessionObserverToken",flow)};
})();`
