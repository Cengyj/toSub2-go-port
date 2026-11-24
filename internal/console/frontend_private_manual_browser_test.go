package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

func TestFrontendPrivateInfoAndManualResultInBrowser(t *testing.T) {
	if *uploadBrowser == "" {
		t.Skip("opt-in real browser requires -upload-browser")
	}
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	alias := map[string]string{"react": "react/react.production.js", "react-dom": "react-dom/react-dom.production.js", "react-dom/client": "react-dom/react-dom-client.production.js", "lucide-react": "lucide-react/index.js", "scheduler": "scheduler/scheduler.production.js"}
	for name, path := range alias {
		alias[name] = filepath.Join(web, "vendor", path)
	}
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: privateManualBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal("private/manual browser bundle", built.Errors)
	}
	assets := map[string][]byte{}
	for _, file := range built.OutputFiles {
		assets[filepath.Base(file.Path)] = file.Contents
	}
	if len(assets["app.js"]) == 0 || len(assets["app.css"]) == 0 {
		t.Fatal("missing fixture JS or production styles")
	}
	for _, scenario := range []struct {
		width int
		theme string
	}{{1280, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", scenario.width, scenario.theme), func(t *testing.T) { verifyPrivateManualBrowser(t, assets, scenario.width, scenario.theme) })
	}
}

func verifyPrivateManualBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	capture := newProtectedSettingsBrowserCapture()
	var mu sync.Mutex
	privateValid, probeState, requestID := false, "old", ""
	privateReads, starts, forbidden := 0, 0, 0
	job := map[string]any{"id": "info-one", "email": "info@example.invalid", "status": "completed", "authorizationMethod": "chatgpt", "manualExpiresAt": "", "subscriptionExpiresAt": "", "expiresAt": "", "expiresAtSource": ""}
	old := func() map[string]any {
		return map[string]any{"id": "visual-one", "task": "pelican-svg", "requestId": "old-attempt", "state": "success", "svg": `<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180"><text x="10" y="40">synthetic previous artwork</text></svg>`, "finishedAt": "2026-11-24T00:00:00Z"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'")
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><iframe title="Synthetic private/manual viewport" width="%d" height="900" src="/fixture?width=%d&theme=%s"></iframe>`, width, width, theme)
		case "GET /fixture":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html data-theme="%s"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"><body><main id="root"></main><script src="/app.js"></script></body></html>`, theme)
		case "GET /app.js", "GET /app.css":
			name := strings.TrimPrefix(r.URL.Path, "/")
			if name == "app.js" {
				w.Header().Set("Content-Type", "text/javascript")
			} else {
				w.Header().Set("Content-Type", "text/css")
			}
			_, _ = w.Write(assets[name])
		case "GET /favicon.ico":
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/jobs/info-one":
			write(job)
		case "GET /api/jobs/info-one/account-info":
			mu.Lock()
			privateReads++
			valid := privateValid
			mu.Unlock()
			w.Header().Set("Cache-Control", "no-store")
			if !valid {
				write(map[string]any{})
				return
			}
			token := r.Header.Get("x-console-token")
			if token != "synthetic-A" && token != "synthetic-B" {
				mu.Lock()
				forbidden++
				mu.Unlock()
			}
			write(accountInfoResponse{ID: "info-one", Email: "info@example.invalid", LoginMode: "email", Password: "  " + token + " private password  ", MailRequestHeaders: map[string]string{}, CredentialSource: "secure-store", TOTPStatus: "none"})
		case "GET /api/account-quality/manual":
			mu.Lock()
			state, id := probeState, requestID
			mu.Unlock()
			if r.URL.Query().Get("id") != "visual-one" {
				mu.Lock()
				forbidden++
				mu.Unlock()
			}
			value := old()
			if state != "old" {
				value["requestId"] = id
				value["state"] = state
				if state == "running" {
					delete(value, "svg")
					delete(value, "finishedAt")
				} else {
					value["finishedAt"] = "2026-11-24T00:01:00Z"
					value["svg"] = `<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180"><text x="10" y="40">synthetic current artwork</text></svg>`
				}
			}
			write(value)
		case "POST /api/account-quality/manual":
			var body struct {
				ID        string `json:"id"`
				Task      string `json:"task"`
				RequestID string `json:"requestId"`
				Options   struct {
					Model  string `json:"model"`
					Effort string `json:"reasoningEffort"`
				} `json:"options"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			valid := decoder.Decode(&body) == nil && decoder.Decode(new(any)) == io.EOF && body.ID == "visual-one" && body.Task == "pelican-svg" && body.RequestID != "" && body.RequestID != "old-attempt" && body.Options.Model == "synthetic-model" && body.Options.Effort == "high"
			mu.Lock()
			starts++
			requestID = body.RequestID
			if !valid {
				forbidden++
			}
			mu.Unlock()
			// A syntactically valid old response is not this request's receipt.
			write(old())
		case "POST /control":
			var body struct {
				Mode string `json:"mode"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			switch body.Mode {
			case "private-valid":
				privateValid = true
			case "running", "success":
				probeState = body.Mode
			default:
				forbidden++
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "POST /capture":
			var result protectedSettingsBrowserResult
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&result) == nil && decoder.Decode(new(any)) == io.EOF {
				capture.accept(result)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			mu.Lock()
			forbidden++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
	browser, err := runFrontendBrowserPaced(ctx, frontendBrowserPacedOptions{Executable: *uploadBrowser, Profile: t.TempDir(), URL: server.URL + "/", Terminal: capture.done, Stdout: stdout, Stderr: stderr})
	t.Logf("private/manual real React: granted_ms=%d expired_ms=%d wall_ms=%d", browser.GrantedMS, browser.ExpiredMS, browser.WallMS)
	if err != nil {
		t.Fatal("isolated private/manual browser", err)
	}
	select {
	case <-capture.done:
		if !capture.result.OK {
			t.Fatal("private/manual browser", capture.result.Error)
		}
	default:
		t.Fatal("private/manual browser did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if privateReads != 3 || starts != 1 || forbidden != 0 {
		t.Fatalf("private/manual request ledger: private=%d starts=%d forbidden=%d", privateReads, starts, forbidden)
	}
}

const privateManualBrowserDriver = `
import React from "react";
import {createRoot} from "react-dom/client";
import {AccountInfoDialog} from "./src/components/account-info-dialog.jsx";
import {ManualProbePanel} from "./src/components/manual-probe.jsx";
import {ConfirmationProvider} from "./src/components/ui.jsx";
import {apiFetch} from "./src/lib/console.js";
import "./src/styles.css";
const one=selector=>document.querySelector(selector),button=(label,root=document)=>[...root.querySelectorAll("button")].find(node=>node.textContent.trim()===label),pause=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(condition,message)=>{if(!condition)throw new Error(message)};
async function waitFor(test,message){for(let i=0;i<240;i++){if(test())return;await pause()}throw new Error(message)}
const rawFetch=window.fetch.bind(window);let holdPrivate=false,releasePrivate,clipboard="";
window.fetch=async(url,options)=>{const response=await rawFetch(url,options);if(holdPrivate&&String(url).endsWith("/account-info")){holdPrivate=false;const data=await response.json();Object.defineProperty(response,"json",{value:()=>new Promise(resolve=>releasePrivate=()=>resolve(data))})}return response;};
Object.defineProperty(navigator,"clipboard",{configurable:true,value:{writeText:async value=>{clipboard=value}}});
const job={id:"info-one",email:"info@example.invalid",manualExpiresAt:"",subscriptionExpiresAt:"",expiresAt:"",expiresAtSource:""};
const quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},reload(){},manualAPI:(suffix,options)=>apiFetch("synthetic-A","/api/account-quality/manual"+suffix,options)};
function Fixture(){const [open,setOpen]=React.useState(false),[token,setToken]=React.useState("synthetic-A");React.useEffect(()=>{const change=()=>setToken("synthetic-B");window.addEventListener("synthetic-session",change);return()=>window.removeEventListener("synthetic-session",change)},[]);return <ConfirmationProvider><button type="button" onClick={()=>setOpen(true)}>打开账号资料</button><ManualProbePanel job={{id:"visual-one"}} quality={quality} options={{model:"synthetic-model",reasoningEffort:"high"}}/>{open&&<AccountInfoDialog key={JSON.stringify([token,job.id])} job={job} token={token} onClose={()=>setOpen(false)}/>}</ConfirmationProvider>}
const root=createRoot(one("#root"));root.render(<Fixture/>);
async function control(mode){await fetch("/control",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({mode})})}
function layout(node){const rect=node.getBoundingClientRect();require(rect.width>0&&rect.left>=-1&&rect.right<=innerWidth+1,"dialog overflowed or disappeared")}
const info=()=>one('[aria-labelledby="account-info-title"]');
(async()=>{
 const params=new URLSearchParams(location.search);await waitFor(()=>button("打开账号资料")&&one(".manual-probe-canvas iframe"),"fixture did not mount production components");require(innerWidth===Number(params.get("width"))&&document.documentElement.dataset.theme===params.get("theme"),"wrong viewport or theme");
 const trigger=button("打开账号资料");trigger.focus();trigger.click();await waitFor(()=>info()?.textContent.includes("账号资料响应不完整"),"empty private response was not rejected");layout(info());require(!one(".account-info-field")&&!one('[aria-label="复制账号串"]')&&one('input[type="datetime-local"]'),"invalid private data became empty credentials or disabled independent metadata");
 document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!info(),"Escape did not close private dialog");require(document.activeElement===trigger,"private dialog did not restore focus");
 await control("private-valid");holdPrivate=true;trigger.click();await waitFor(()=>releasePrivate,"old-session private response was not held");window.dispatchEvent(new Event("synthetic-session"));
 await waitFor(()=>one('[aria-label="显示密码"]'),"new session private response did not render");one('[aria-label="显示密码"]').click();await waitFor(()=>info().textContent.includes("synthetic-B private password"),"new session secret did not reveal");releasePrivate();await pause();require(!info().textContent.includes("synthetic-A private password"),"late old-session secret replaced current data");
 one('[aria-label="复制账号串"]').click();await waitFor(()=>clipboard.includes("synthetic-B private password"),"current credentials could not be copied");require(clipboard==="info@example.invalid----  synthetic-B private password  ----","copy changed password whitespace or identity");
 document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!info(),"new session private dialog did not close");require(!document.body.textContent.includes("private password"),"closed private dialog retained plaintext");
 const start=button("开始手动检测");start.click();start.click();await waitFor(()=>document.body.textContent.includes("本次操作结果待核实"),"wrong manual receipt was accepted");
 require(button("开始手动检测").disabled&&one(".manual-probe-canvas.is-pending")&&!one(".manual-probe-canvas iframe")&&one(".manual-probe-previous")?.textContent.includes("上次作品")&&!document.body.textContent.includes("绘制完成"),"new attempt displayed previous artwork as current success");
 const historicalFrame=one(".manual-probe-previous iframe");require(historicalFrame&&historicalFrame.getAttribute("sandbox")==="allow-scripts"&&historicalFrame.contentDocument===null&&historicalFrame.srcdoc.includes("connect-src 'none'"),"historical preview escaped its sandbox or gained network access");
 await control("running");button("重新核对").click();await waitFor(()=>button("正在绘制…")&&button("取消")&&!one(".manual-probe-previous"),"matching request was not adopted");
 await control("success");await waitFor(()=>one(".manual-probe-canvas.is-success iframe")&&document.body.textContent.includes("绘制完成"),"matching terminal artifact did not become current");require(!one(".manual-probe-previous"),"previous and current artwork remained conflated");
 const expand=button("放大");expand.focus();expand.click();await waitFor(()=>one('[aria-labelledby="pelican-preview-title"]'),"current artwork preview did not expand");layout(one('[aria-labelledby="pelican-preview-title"]'));document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!one('[aria-labelledby="pelican-preview-title"]'),"preview did not close");require(document.activeElement===expand,"preview failed to restore focus");
 root.unmount();await fetch("/capture",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({ok:true})});
})().catch(async error=>{root.unmount();await fetch("/capture",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({ok:false,error:error.message})})});
`
