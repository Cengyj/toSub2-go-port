package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

// Actual App/React lifecycles and DOM interactions, with loopback-only API
// fixtures. The exported connection hook is wrapped solely to deliver synthetic
// token generations; no account-command state or handlers are injected.
func TestFrontendAccountCommandsInBrowser(t *testing.T) {
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
	connection := filepath.Join(web, "src/hooks/consoleConnection.js")
	plugin := api.Plugin{Name: "synthetic-connection-generation", Setup: func(build api.PluginBuild) {
		build.OnResolve(api.OnResolveOptions{Filter: `/hooks/consoleConnection\.js$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			if args.Namespace == "synthetic-connection" {
				return api.OnResolveResult{Path: connection}, nil
			}
			return api.OnResolveResult{Path: connection, Namespace: "synthetic-connection"}, nil
		})
		build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "synthetic-connection"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			source := fmt.Sprintf(`import {useEffect} from "react";import {useConsoleConnection as actual} from %q;
export function useConsoleConnection(){const state=actual();useEffect(()=>{const change=event=>state.setToken(event.detail);window.addEventListener("synthetic-console-token",change);return()=>window.removeEventListener("synthetic-console-token",change)},[state.setToken]);useEffect(()=>{document.documentElement.dataset.connectionGeneration=state.token},[state.token]);return state}`, connection)
			return api.OnLoadResult{Contents: &source, Loader: api.LoaderJS, ResolveDir: web}, nil
		})
	}}
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: accountCommandBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Plugins: []api.Plugin{plugin}, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal("could not bundle production account commands", built.Errors)
	}
	assets := map[string][]byte{}
	for _, output := range built.OutputFiles {
		assets[filepath.Base(output.Path)] = output.Contents
	}
	for _, test := range []struct {
		width int
		theme string
	}{{1440, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", test.width, test.theme), func(t *testing.T) {
			verifyAccountCommandsBrowser(t, assets, test.width, test.theme)
		})
	}
}

func verifyAccountCommandsBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	var unexpected []string
	result := make(chan struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}, 1)
	jobs := []map[string]any{}
	for _, id := range []string{"first", "second"} {
		jobs = append(jobs, map[string]any{"id": id, "email": id + "@example.invalid", "status": "completed", "canForceRelogin": true, "canResetTotp": true, "canDownload": true, "authorizationMethod": "chatgpt"})
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
			write(map[string]any{"token": "A", "features": map[string]bool{"sub2apiUpload": true, "bulkActions": true, "forceRelogin": true, "totpReset": true, "cancelAll": true}})
		case "GET /api/jobs":
			// A synthetic queued history entry outside the visible account rows
			// enables the independent global cancellation command.
			write(map[string]any{"jobs": jobs, "selection": jobs, "stats": map[string]int{"active": 0, "queued": 1, "completed": 2}})
		case "GET /api/account-insights/settings":
			write(map[string]any{"enabled": false, "automatic": false, "intervalMinutes": 5, "revision": 1})
		case "GET /api/account-insights":
			write(map[string]any{"snapshots": map[string]any{}})
		case "GET /api/account-insights/status":
			aggregate := map[string]int{"total": 0, "eligible": 0, "queued": 0, "active": 0, "coolingDown": 0, "unavailable": 0, "retryAfterSeconds": 0}
			write(map[string]any{"enabled": false, "automatic": false, "refresh": aggregate, "selection": aggregate})
		case "POST /api/mail-request-config":
			write(map[string]bool{"ok": true})
		case "POST /api/jobs/first/setup-2fa":
			var body struct {
				Reset  bool   `json:"resetTotp"`
				Intent string `json:"totpIntentId"`
			}
			mu.Lock()
			counts["totp"]++
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || !body.Reset || body.Intent == "" || r.Header.Get("x-console-token") != "A" {
				unexpected = append(unexpected, "invalid single TOTP request")
			}
			mu.Unlock()
			write(map[string]any{"job": jobs[0], "outcome": "unknown"})
		case "POST /api/jobs/cancel-all":
			mu.Lock()
			counts["cancel"]++
			mu.Unlock()
			write(map[string]any{"canceled": 1, "outcome": "unknown"})
		case "POST /api/jobs/relogin-batch", "POST /api/sub2api/upload", "POST /api/jobs/batch":
			kind := map[string]string{"/api/jobs/relogin-batch": "relogin", "/api/sub2api/upload": "upload", "/api/jobs/batch": "import"}[r.URL.Path]
			var body struct {
				IDs  []string `json:"ids"`
				Text string   `json:"text"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || r.Header.Get("x-console-token") != "A" || kind != "import" && strings.Join(body.IDs, ",") != "first,second" && strings.Join(body.IDs, ",") != "second,first" {
				mu.Lock()
				unexpected = append(unexpected, "wrong request scope/token: "+kind)
				mu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				write(map[string]string{"error": "invalid synthetic request"})
				return
			}
			mu.Lock()
			counts[kind]++
			n := counts[kind]
			mu.Unlock()
			switch kind {
			case "relogin":
				if n == 1 {
					write(map[string]any{"ok": false, "outcome": "partial", "selected": 2, "queued": 1, "started": 1, "skipped": 0, "failed": 0, "unknown": 1, "uncertain": true, "jobs": jobs[:1], "failures": []map[string]any{{"id": "second", "uncertain": true, "error": "合成受理结果未知"}}})
				} else if n == 2 {
					write(map[string]any{"ok": true, "outcome": "queued", "selected": 2, "queued": 2, "started": 2, "skipped": 0, "jobs": jobs})
				} else {
					write(map[string]any{"ok": true, "outcome": "queued", "selected": 2, "queued": 2, "started": 2, "skipped": 0, "jobs": []map[string]any{{"id": "foreign"}, {"id": "second"}}})
				}
			case "upload":
				if n == 1 {
					write(map[string]any{"ok": true, "selected": 2, "uploaded": 2, "skipped": 0, "result": map[string]int{"created": 0, "updated": 2, "success": 2}})
				} else {
					write(map[string]any{"ok": false, "outcome": "partial", "uncertain": true, "selected": 2, "uploaded": 2, "skipped": 0, "result": map[string]int{"created": 1, "updated": 0, "success": 1, "unknown": 1}})
				}
			case "import":
				if body.Text != "new@example.invalid----  private password  \nnext@example.invalid" {
					mu.Lock()
					unexpected = append(unexpected, "import trimmed password whitespace")
					mu.Unlock()
				}
				w.WriteHeader(http.StatusConflict)
				write(map[string]any{"ok": false, "outcome": "partial", "selected": 2, "completed": 1, "created": 1, "updated": 0, "failed": 0, "unknown": 1, "unprocessed": 0, "uncertain": true, "jobs": []map[string]any{{"id": "new", "email": "new@example.invalid", "status": "imported"}}, "error": "已保存 1 条；另 1 条保存结果待核实"})
			}
		case "GET /ledger":
			mu.Lock()
			write(counts)
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
	cmd := exec.CommandContext(ctx, *uploadBrowser, "--headless", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions", "--disable-background-networking", "--disable-component-update", "--password-store=basic", "--use-mock-keychain", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=127.0.0.1", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost", "--user-data-dir="+t.TempDir(), "--window-size=1600,1000", "--dump-dom", "--virtual-time-budget=15000", server.URL)
	if err := runFrontendBrowserCommand(cmd); err != nil {
		t.Fatal("isolated Chromium failed", err)
	}
	select {
	case receipt := <-result:
		if !receipt.OK {
			t.Fatal("production account commands:", receipt.Error)
		}
	default:
		t.Fatal("browser did not finish account command checks")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unexpected) > 0 || counts["relogin"] != 3 || counts["upload"] != 2 || counts["import"] != 1 || counts["totp"] != 1 || counts["cancel"] != 1 {
		t.Fatalf("unexpected requests or mutation replay: counts=%v, unexpected=%v", counts, unexpected)
	}
}

const accountCommandBrowserDriver = `
const params=new URLSearchParams(location.search);
localStorage.setItem("chatgpt-onboarding.sub2api-upload-settings-v1",JSON.stringify({baseUrl:"https://sub2.example.invalid",adminApiKey:"synthetic-sub2-key"}));
localStorage.setItem("tosub2.console.theme",params.get("theme"));location.hash="accounts";
// Delay receipt delivery after draining the real loopback response. Holding an
// open network request stalls Chromium's virtual clock, preventing UI timers.
const rawFetch=window.fetch.bind(window),responseCounts={},held=new Map();
window.fetch=async (url,options)=>{
 const response=await rawFetch(url,options),kind=url==="/api/jobs/relogin-batch"?"relogin":url==="/api/sub2api/upload"?"upload":"";
 if(kind){const number=responseCounts[kind]=(responseCounts[kind]||0)+1;if(kind==="relogin"&&number===2||kind==="upload"&&number===1){const body=await response.json();Object.defineProperty(response,"json",{value:()=>new Promise(resolve=>held.set(kind,()=>resolve(body)))})}}
 return response;
};
async function release(kind){await waitFor(()=>held.get(kind),"delayed receipt missing: "+kind);held.get(kind)();held.delete(kind)}
const sleep=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(condition,message)=>{if(!condition)throw new Error(message)};
const one=selector=>document.querySelector(selector),button=label=>[...document.querySelectorAll("button")].find(node=>node.textContent.trim()===label);
const selected=id=>one('input[aria-label="选择 '+id+'@example.invalid"]');
const dialog=()=>one('[role="dialog"][aria-labelledby="relogin-action-title"]');
async function waitFor(test,message){for(let i=0;i<200;i++){const value=await test();if(value)return value;await sleep();}throw new Error(message)}
async function ledger(kind,count){await waitFor(async()=>{const value=await fetch("/ledger").then(response=>response.json());return value[kind]===count},"mutation count differs: "+kind)}
async function openRelogin(){const trigger=button("重登授权");trigger.focus();trigger.click();await waitFor(dialog,"relogin dialog missing");const rect=dialog().getBoundingClientRect();require(rect.left>=0&&rect.right<=innerWidth+1,"relogin dialog overflows viewport");require(document.documentElement.dataset.theme===params.get("theme"),"theme lost");return trigger}
async function submitRelogin(){const submit=dialog().querySelector('button[type="submit"]');submit.click();submit.click();await sleep()}
async function setToken(token){window.dispatchEvent(new CustomEvent("synthetic-console-token",{detail:token}));await waitFor(()=>document.documentElement.dataset.connectionGeneration===token,"token generation did not render")}
(async()=>{
 await import("./src/main.jsx");await waitFor(()=>selected("first"),"accounts missing");require(innerWidth===Number(params.get("width")),"wrong viewport");
 one('input[aria-label="选择当前页全部任务"]').click();await sleep();await openRelogin();await submitRelogin();
 await waitFor(()=>!dialog()&&document.body.textContent.includes("结果待核实"),"partial batch result not presented");require(!selected("first").checked&&selected("second").checked,"partial receipt lost an unconfirmed selection");await ledger("relogin",1);
 selected("first").click();await sleep();one('button[aria-label="上传 Sub2API"]').click();await ledger("upload",1);
 selected("first").click();await sleep();selected("first").click();await sleep();
 await release("upload");await waitFor(()=>document.body.textContent.includes("Sub2API · 同步已完成"),"upload response did not finish");require(selected("first").checked&&selected("second").checked,"upload receipt cleared new selection of the same IDs");
 await openRelogin();await submitRelogin();await ledger("relogin",2);
 await setToken("B");await waitFor(()=>!dialog(),"old generation dialog survived");await setToken("A");await openRelogin();const fresh=dialog();
 await release("relogin");await sleep();await sleep();
 require(dialog()===fresh&&selected("first").checked&&selected("second").checked,"A-B-A response closed new dialog or cleared selection");require(!fresh.querySelector('[role="alert"]'),"old response injected a new dialog error");
 document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!dialog(),"Escape did not dismiss current dialog");require(document.activeElement===button("重登授权"),"dialog close did not restore focus");
 await openRelogin();await submitRelogin();await waitFor(()=>dialog()?.textContent.includes("操作回执不完整"),"foreign-ID receipt was accepted");require(selected("first").checked&&selected("second").checked,"malformed receipt cleared account selection");await ledger("relogin",3);
 dialog().querySelector('button[aria-label="关闭重登授权窗口"]').click();await waitFor(()=>!dialog(),"malformed result dialog could not close");
 one('button[aria-label="上传 Sub2API"]').click();await waitFor(()=>document.body.textContent.includes("Sub2API · 部分同步 · 结果待核实"),"HTTP200 unknown upload not visible");require(selected("first").checked&&selected("second").checked,"aggregate partial upload invented success IDs");await ledger("upload",2);
 const totpDialog=()=>one('[role="dialog"][aria-labelledby="totp-action-title"]');one('button[aria-label="重置 2FA first@example.invalid"]').click();await waitFor(totpDialog,"single TOTP dialog missing");const intent=sessionStorage.getItem("tosub2.totp-intent.first");require(intent,"TOTP intent missing");const totpSubmit=totpDialog().querySelector('button[type="submit"]');totpSubmit.click();totpSubmit.click();
 await waitFor(()=>totpDialog()?.textContent.includes("操作回执不完整"),"explicit unknown single TOTP receipt was acknowledged");require(sessionStorage.getItem("tosub2.totp-intent.first")===intent&&selected("first").checked&&selected("second").checked,"unknown single TOTP result cleared intent or selection");await ledger("totp",1);totpDialog().querySelector('button[aria-label="关闭窗口"]').click();await waitFor(()=>!totpDialog(),"TOTP error dialog did not close");
 one('button[aria-label="打开其他批量操作菜单"]').click();const stop=await waitFor(()=>button("停止全部"),"global cancellation entry missing");stop.click();await waitFor(()=>one('[aria-labelledby="confirmation-title"]'),"cancel confirmation missing");button("确认执行").click();
 await waitFor(()=>one('.global-error')?.textContent.includes("操作回执不完整"),"explicit unknown cancellation receipt was accepted");require(selected("first").checked&&selected("second").checked,"unknown cancellation cleared original selection");await ledger("cancel",1);
 button("批量导入").click();await waitFor(()=>one("#batch-input"),"import dialog missing");const input=one("#batch-input"),draft="new@example.invalid----  private password  \nnext@example.invalid";Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,"value").set.call(input,draft);input.dispatchEvent(new Event("input",{bubbles:true}));await sleep();const submit=input.closest("form").querySelector('button[type="submit"]');submit.click();submit.click();
 await waitFor(()=>one('[aria-labelledby="batch-title"]')?.textContent.includes("避免重复导入"),"partial import did not retain recovery instructions");require(one("#batch-input").value===draft,"partial import lost private whitespace or draft");await ledger("import",1);
 await fetch("/capture",{method:"POST",body:JSON.stringify({ok:true})});
})().catch(async error=>{await fetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:error.message})});});
`
