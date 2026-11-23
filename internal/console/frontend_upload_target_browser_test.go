package console

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

var uploadBrowser = flag.String("upload-browser", "", "opt-in: absolute Chromium path for synthetic manual-upload UI tests with a temporary profile")

// This test mounts the production App and drives its DOM. Only the loopback
// console API is synthetic; no fixture callback performs an upload for the App.
func TestFrontendManualUploadInBrowser(t *testing.T) {
	if *uploadBrowser == "" {
		t.Skip("opt-in real browser requires -upload-browser")
	}
	if !filepath.IsAbs(*uploadBrowser) {
		t.Fatal("browser path must be absolute")
	}
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	alias := map[string]string{"react": "react/react.production.js", "react-dom": "react-dom/react-dom.production.js", "react-dom/client": "react-dom/react-dom-client.production.js", "lucide-react": "lucide-react/index.js", "scheduler": "scheduler/scheduler.production.js"}
	for name, path := range alias {
		alias[name] = filepath.Join(web, "vendor", path)
	}
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: manualUploadBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal("could not bundle production upload UI", built.Errors)
	}
	assets := map[string][]byte{}
	for _, output := range built.OutputFiles {
		assets[filepath.Base(output.Path)] = output.Contents
	}
	if len(assets["app.js"]) == 0 || len(assets["app.css"]) == 0 {
		t.Fatal("production UI bundle is incomplete")
	}
	for _, test := range []struct {
		width int
		theme string
	}{{1440, "light"}, {390, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", test.width, test.theme), func(t *testing.T) {
			verifyManualUploadBrowser(t, assets, test.width, test.theme)
		})
	}
}

func verifyManualUploadBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	type uploadRecord struct {
		Target string
		IDs    []string
	}
	var mu sync.Mutex
	var uploads []uploadRecord
	var unexpected []string
	result := make(chan struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}, 1)
	jobs := []map[string]any{}
	for _, id := range []string{"first", "second"} {
		jobs = append(jobs, map[string]any{"id": id, "email": id + "@example.invalid", "status": "completed", "canDownload": true, "authorizationMethod": "chatgpt"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'")
		if r.Method == "GET" {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><iframe title="Synthetic console" style="border:0;width:%dpx;height:844px" src="/app?width=%d&amp;theme=%s"></iframe>`, width, width, theme)
				return
			case "/app":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body><div id="root"></div><script src="/app.js"></script></body></html>`)
				return
			case "/app.js", "/app.css":
				w.Header().Set("Content-Type", map[string]string{"/app.js": "text/javascript", "/app.css": "text/css"}[r.URL.Path])
				w.Write(assets[strings.TrimPrefix(r.URL.Path, "/")])
				return
			case "/favicon.ico":
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET /api/bootstrap":
			write(map[string]any{"token": "synthetic-console-token", "features": map[string]bool{"sub2apiUpload": true, "cpaUpload": true, "bulkActions": true}})
		case "GET /api/jobs":
			write(map[string]any{"jobs": jobs, "selection": jobs})
		case "GET /api/cpa/settings":
			write(map[string]any{"baseUrl": "https://cpa.example.invalid", "configured": true, "hasApiKey": true, "credentialPersistence": "secure", "revision": 1})
		case "GET /api/pool-monitor":
			write(map[string]any{"target": "sub2api", "enabled": false, "configured": true, "running": false, "intervalMinutes": 5, "cooldownMinutes": 5, "revision": 1})
		case "GET /api/account-insights/settings":
			write(map[string]any{"enabled": false, "automatic": false, "intervalMinutes": 5, "revision": 1})
		case "GET /api/account-insights":
			write(map[string]any{"snapshots": map[string]any{}})
		case "GET /api/account-insights/status":
			aggregate := map[string]int{"total": 0, "eligible": 0, "queued": 0, "active": 0, "coolingDown": 0, "unavailable": 0, "retryAfterSeconds": 0}
			write(map[string]any{"enabled": false, "automatic": false, "refresh": aggregate, "selection": aggregate})
		case "POST /api/mail-request-config":
			write(map[string]bool{"ok": true})
		case "POST /api/cpa/upload", "POST /api/sub2api/upload":
			var body struct {
				IDs    []string        `json:"ids"`
				Config json.RawMessage `json:"config"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil || len(body.IDs) == 0 || r.URL.Path == "/api/cpa/upload" && len(body.Config) != 0 {
				mu.Lock()
				unexpected = append(unexpected, "invalid upload body")
				mu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				write(map[string]string{"error": "synthetic invalid upload body"})
				return
			}
			target := strings.Split(r.URL.Path, "/")[2]
			mu.Lock()
			uploads = append(uploads, uploadRecord{target, body.IDs})
			mu.Unlock()
			results := []map[string]string{}
			for _, id := range body.IDs {
				results = append(results, map[string]string{"id": id, "outcome": "succeeded"})
			}
			write(map[string]any{"ok": true, "outcome": "completed", "uploaded": len(body.IDs), "result": map[string]int{"created": len(body.IDs), "updated": 0, "success": len(body.IDs)}, "results": results})
		case "GET /ledger":
			mu.Lock()
			write(map[string]int{"uploads": len(uploads)})
			mu.Unlock()
		case "POST /capture":
			var receipt struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&receipt) == nil {
				select {
				case result <- receipt:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			mu.Lock()
			unexpected = append(unexpected, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			write(map[string]string{"error": "unexpected fixture route"})
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, *uploadBrowser, "--headless", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions", "--disable-background-networking", "--disable-component-update", "--password-store=basic", "--use-mock-keychain", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=127.0.0.1", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost", "--user-data-dir="+t.TempDir(), "--window-size=1600,1000", "--dump-dom", "--virtual-time-budget=12000", server.URL)
	if _, err := cmd.Output(); err != nil {
		t.Fatal("isolated Chromium failed", err)
	}
	select {
	case receipt := <-result:
		if !receipt.OK {
			t.Fatal("production upload UI:", receipt.Error)
		}
	default:
		t.Fatal("browser did not finish upload checks")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unexpected) > 0 {
		t.Fatalf("unexpected fixture requests: %v", unexpected)
	}
	want := []uploadRecord{{"cpa", []string{"first"}}, {"sub2api", []string{"first", "second"}}}
	if !reflect.DeepEqual(uploads, want) {
		t.Fatal("fake console did not receive exactly the selected single CPA and batch Sub2API uploads")
	}
}

const manualUploadBrowserDriver = `
const params=new URLSearchParams(location.search);
localStorage.setItem("chatgpt-onboarding.sub2api-upload-settings-v1",JSON.stringify({baseUrl:"https://sub2.example.invalid",adminApiKey:"synthetic-sub2-key"}));
localStorage.setItem("tosub2.console.theme",params.get("theme"));
location.hash="accounts";
const sleep=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(condition,message)=>{if(!condition)throw new Error(message)};
const one=selector=>document.querySelector(selector);
const button=label=>[...document.querySelectorAll("button")].find(node=>node.textContent.trim()===label);
async function waitFor(test,message){for(let i=0;i<200;i++){const value=test();if(value)return value;await sleep();}throw new Error(message);}
async function ledger(expected){const value=await fetch("/ledger").then(response=>response.json());require(value.uploads===expected,"upload count changed before confirmation or repeated after completion");}
function dialog(){return one('[role="dialog"][aria-labelledby="manual-upload-title"]');}
function checkLayout(){const panel=dialog(),rect=panel.getBoundingClientRect();require(window.innerWidth===Number(params.get("width")),"fixture viewport width differs");require(rect.left>=0&&rect.right<=innerWidth+1&&panel.scrollWidth<=panel.clientWidth+1,"upload dialog overflows viewport");require(document.documentElement.dataset.theme===params.get("theme"),"requested theme was not applied");}
async function openRow(){const trigger=await waitFor(()=>one('button[aria-label="选择上传目标 first@example.invalid"]'),"row upload entry missing");trigger.focus();trigger.click();await waitFor(dialog,"row did not open target dialog");checkLayout();return trigger;}
(async()=>{
 await import("./src/main.jsx");
 const firstTrigger=await openRow();
 await waitFor(()=>dialog().textContent.match(/已配置/g)?.length===2,"both saved targets not visible");
 await ledger(0);
 const close=one('button[aria-label="关闭上传目标选择"]');
 const submit=dialog().querySelector('button[type="submit"]');
 close.focus();document.dispatchEvent(new KeyboardEvent("keydown",{key:"Tab",shiftKey:true,bubbles:true}));require(document.activeElement===submit,"Shift+Tab escaped modal focus");
 submit.focus();document.dispatchEvent(new KeyboardEvent("keydown",{key:"Tab",bubbles:true}));require(document.activeElement===close,"Tab escaped modal focus");
 document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!dialog(),"Escape did not close dialog");require(document.activeElement===firstTrigger,"closing dialog lost trigger focus");await ledger(0);
 await openRow();one('input[name="manual-upload-target"][value="cpa"]').click();await waitFor(()=>button("上传到 CPA"),"CPA selection did not change confirmation");button("取消").click();await waitFor(()=>!dialog(),"cancel did not close dialog");await ledger(0);require(!localStorage.getItem("tosub2.upload-target-v1"),"cancel saved an unconfirmed preference");
 await openRow();one('input[name="manual-upload-target"][value="cpa"]').click();const cpa=await waitFor(()=>button("上传到 CPA"),"CPA confirmation unavailable");await ledger(0);cpa.click();cpa.click();await waitFor(()=>!dialog()&&document.body.textContent.includes("CPA · 同步已完成"),"CPA upload did not complete");await ledger(1);
 one('input[aria-label="选择当前页全部任务"]').click();await sleep();
 const batch=one('button[aria-label="选择上传目标"]');require(batch&&!batch.disabled,"batch upload entry unavailable");batch.click();await waitFor(dialog,"batch did not open target dialog");checkLayout();require(dialog().textContent.includes("本次选择 2 个账号"),"batch account count missing");await ledger(1);
 one('input[name="manual-upload-target"][value="sub2api"]').click();const sub2=await waitFor(()=>button("上传到 Sub2API"),"Sub2 confirmation unavailable");await ledger(1);sub2.click();sub2.click();await waitFor(()=>!dialog()&&document.body.textContent.includes("Sub2API · 同步已完成"),"Sub2 upload did not complete");await ledger(2);
 await fetch("/capture",{method:"POST",body:JSON.stringify({ok:true})});
})().catch(async error=>{await fetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:error.message})});});
`
