package console

import (
	"context"
	"embed"
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
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

// Real React runs the production collection, polling and command hooks together.
// Every list starts with the actual Go compact projection of 21 save-only
// accounts. Local fixture responses control only read ordering and refusals;
// no authorization, upload or credential-changing endpoint is invoked.
func TestFrontendCompactAccountPollingInBrowser(t *testing.T) {
	if *uploadBrowser == "" {
		t.Skip("opt-in real browser requires -upload-browser")
	}
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	root, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	manager := jobs.NewManager(context.Background(), 1, root, nil)
	manager.SetCredentialStore(vault)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error("synthetic account manager did not shut down:", err)
		}
	})
	for index := range 21 {
		id := fmt.Sprintf("compact-%02d", index)
		if _, err := manager.Submit(jobs.Spec{ID: id, Email: id + "@example.invalid", ImportOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	apiServer := NewServer(config.Config{OutputRoot: root.Root, StartupToken: "fixture"}, manager, embed.FS{}, nil, vault)
	t.Cleanup(apiServer.Close)
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	alias := map[string]string{"react": "react/react.production.js", "react-dom": "react-dom/react-dom.production.js", "react-dom/client": "react-dom/react-dom-client.production.js", "lucide-react": "lucide-react/index.js", "scheduler": "scheduler/scheduler.production.js"}
	for name, path := range alias {
		alias[name] = filepath.Join(web, "vendor", path)
	}
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: compactAccountPollingBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": "\"production\""}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatal("could not bundle production account hook fixture")
	}
	capture := newProtectedSettingsBrowserCapture()
	var mu sync.Mutex
	reads, commands := 0, 0
	var disabled string
	var unexpected []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; connect-src 'self'")
		switch r.Method + " " + r.URL.Path {
		case "GET /":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<!doctype html><meta charset="utf-8"><div id="root"></div><script src="/app.js"></script>`)
		case "GET /app.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(built.OutputFiles[0].Contents)
		case "GET /favicon.ico":
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/jobs", "POST /api/jobs/query":
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
			mu.Lock()
			reads++
			blocked := disabled
			if err != nil || r.URL.Query().Get("selection") != "compact" || r.Header.Get("X-Console-Token") != "A" && r.Header.Get("X-Console-Token") != "B" {
				unexpected = append(unexpected, "invalid compact read identity or negotiation")
			}
			mu.Unlock()
			var requestBody any
			if len(body) != 0 {
				requestBody = json.RawMessage(body)
			}
			response := frontendRequest(apiServer, r.Method, r.URL.RequestURI(), requestBody, "fixture")
			if response.Code != http.StatusOK {
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
				return
			}
			var data map[string]any
			if json.Unmarshal(response.Body.Bytes(), &data) != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if blocked != "" {
				for _, key := range []string{"jobs", "selection"} {
					for _, item := range data[key].([]any) {
						row := item.(map[string]any)
						if row["id"] == blocked {
							row["canForceRelogin"] = false
						}
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(data)
		case "POST /fixture/disable":
			var data struct{ ID string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&data) != nil || data.ID != "" && !strings.HasPrefix(data.ID, "compact-") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			disabled = data.ID
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/jobs/relogin-batch":
			mu.Lock()
			commands++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"outcome":"unknown"}`)
		case "POST /capture":
			var receipt protectedSettingsBrowserResult
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&receipt) == nil && decoder.Decode(new(any)) == io.EOF {
				capture.accept(receipt)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			mu.Lock()
			unexpected = append(unexpected, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
	browser, err := runFrontendBrowserPaced(ctx, frontendBrowserPacedOptions{Executable: *uploadBrowser, Profile: t.TempDir(), URL: server.URL + "/", Terminal: capture.done, Stdout: stdout, Stderr: stderr})
	t.Logf("compact real React: granted_ms=%d expired_ms=%d wall_ms=%d", browser.GrantedMS, browser.ExpiredMS, browser.WallMS)
	if err != nil {
		t.Fatal("isolated compact browser host:", err)
	}
	select {
	case <-capture.done:
		if !capture.result.OK {
			t.Fatal("compact browser fixture:", capture.result.Error)
		}
	default:
		t.Fatal("compact browser checks did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if reads < 8 || commands != 1 || len(unexpected) != 0 {
		t.Fatalf("compact fixture dispatches: reads=%d commands=%d unexpected=%v", reads, commands, unexpected)
	}
	t.Logf("verified compact reads=%d, explicit unknown command=%d; no forbidden request", reads, commands)
}

const compactAccountPollingBrowserDriver = `
import React,{useLayoutEffect,useRef,useState} from "react";
import {createRoot} from "react-dom/client";
import {useAccountCollection} from "./src/hooks/accountCollection.js";
import {useAccountPolling} from "./src/hooks/serverState.js";
import {useAccountCommands} from "./src/hooks/accountCommands.js";
const rawFetch=window.fetch.bind(window),reads=[],held=new Map();let holdNext="",filterTarget="",failure="";
window.fetch=async(url,options={})=>{
 const response=await rawFetch(url,options);
 if(url.startsWith("/api/jobs?selection=")||url.startsWith("/api/jobs/query?selection=")){
  const data=await response.json(),entry={url,token:options.headers?.["x-console-token"],signal:options.signal};reads.push(entry);
  if(data.selectionMode!=="compact"||!data.selection.every(row=>Object.keys(row).length===22))throw new Error("actual Go compact response missing its contract");
  if(holdNext){const key=holdNext;holdNext="";Object.defineProperty(response,"json",{value:()=>new Promise(resolve=>held.set(key,()=>resolve(data)))})}
  else Object.defineProperty(response,"json",{value:async()=>data});
 }
 return response;
};
const pause=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(value,message)=>{if(!value)throw new Error(message)};
async function waitFor(test,message){for(let i=0;i<240;i++){if(test())return;await pause()}throw new Error(message)}
const one=selector=>document.querySelector(selector),click=id=>one("#"+id).click();
let current;
function Fixture(){
 const accounts=useAccountCollection(),[token,setToken]=useState("A"),[emails,setEmails]=useState([]),[connected,setConnected]=useState(false),[error,setError]=useState(""),[commandError,setCommandError]=useState(""),headingRef=useRef(null);
 const refresh=useAccountPolling({token,page:accounts.page,emailFilter:emails,...accounts,setConnected,setConnectionError:setError});
 const commands=useAccountCommands({token,features:{forceRelogin:true},accounts:{...accounts,refresh},settings:{proxy:{ready:true,value:""},uploadTarget:"sub2api",uploadTargets:{sub2api:{available:false,configured:false}}},feedback:{setError:setCommandError,showNotice(){}},navigation:{},trash:{actionBusy:false,verificationRequired:false},confirm:async()=>true,headingRef});
 useLayoutEffect(()=>{current={accounts,token,emails,connected,error,commands,refresh};});
 return <main><h1 ref={headingRef}>Synthetic compact accounts</h1><output id="state">{JSON.stringify({token,page:accounts.page,rows:accounts.jobs.map(row=>row.id),selected:[...accounts.selectedJobIds],filter:emails,connected,error})}</output>
 <button id="refresh" onClick={refresh}>Read</button><button id="page-one" onClick={()=>accounts.setPage(1)}>Page 1</button><button id="page-two" onClick={()=>accounts.setPage(2)}>Page 2</button>
 <button id="filter" onClick={()=>{setEmails([filterTarget]);accounts.setPage(1)}}>Filter</button><button id="clear-filter" onClick={()=>setEmails([])}>Clear</button>
 <button id="token-a" onClick={()=>setToken("A")}>Token A</button><button id="token-b" onClick={()=>setToken("B")}>Token B</button>
 <button id="relogin" onClick={commands.forceReloginSelected}>Authorize selected</button>
 {accounts.jobs.map(job=><label key={job.id}><input type="checkbox" data-id={job.id} checked={accounts.selectedJobIds.has(job.id)} onChange={()=>accounts.toggleJobSelection(job.id)}/>{job.email}</label>)}
 {commands.reloginAction&&<div role="dialog"><button id="submit" onClick={()=>commands.submitReloginAction({authorizationMethod:"chatgpt"}).catch(error=>setCommandError(error.message))}>Submit</button><button id="close" onClick={()=>{commands.setReloginAction(null);setCommandError("")}}>Close</button><span role="alert">{commandError}</span></div>}
 </main>;
}
createRoot(one("#root")).render(<Fixture/>);
const state=()=>JSON.parse(one("#state").textContent),select=id=>one('input[data-id="'+id+'"]').click();
async function hold(label){holdNext=label;click("refresh");await waitFor(()=>held.has(label),"list response did not enter controlled hold");}
async function release(label){held.get(label)();held.delete(label);await pause();await pause();}
(async()=>{
 await waitFor(()=>current?.accounts.jobs.length===20&&current.connected,"initial compact page did not load");
 const first=current.accounts.jobs[0].id,firstEmail=current.accounts.jobs[0].email;
 select(first);await pause();await hold("previous-page");click("page-two");await waitFor(()=>current.accounts.jobs.length===1&&current.accounts.page===2,"second compact page did not load");const second=current.accounts.jobs[0].id;select(second);await pause();
 require(current.accounts.selectedJobIds.size===2&&current.accounts.forceReloginSelectedCount===2,"compact cross-page selection lost its command capabilities");
 await release("previous-page");require(current.accounts.jobs.length===1&&current.accounts.jobs[0].id===second&&state().page===2,"late previous-page response replaced current rows");
 await hold("previous-filter");filterTarget=firstEmail;click("filter");await waitFor(()=>current.accounts.jobs.length===1&&current.accounts.jobs[0].id===first&&current.accounts.selectedJobIds.size===1,"filtered compact index did not reconcile selection");
 await release("previous-filter");require(current.accounts.jobs[0].id===first&&current.accounts.selectedJobIds.has(first)&&!current.accounts.selectedJobIds.has(second),"late previous-filter response restored removed selection");
 await hold("previous-token");const beforeTokenRows=current.accounts.jobs;click("token-b");await waitFor(()=>current.token==="B"&&reads.at(-1)?.token==="B"&&current.accounts.jobs!==beforeTokenRows,"token B read missing");const tokenBRows=current.accounts.jobs;click("token-a");await waitFor(()=>current.token==="A"&&reads.at(-1)?.token==="A"&&current.accounts.jobs!==tokenBRows&&!current.error,"returned token read missing");
 const freshRows=current.accounts.jobs;await release("previous-token");require(current.accounts.jobs===freshRows,"old token read replaced the returned token's current rows");
 // Browser availability properties are controlled only in this synthetic page.
 // Production listeners, fetch abortion and React state transitions remain real.
 let hidden=false,online=true;Object.defineProperty(document,"hidden",{configurable:true,get:()=>hidden});Object.defineProperty(navigator,"onLine",{configurable:true,get:()=>online});
 await hold("hidden-read");hidden=true;document.dispatchEvent(new Event("visibilitychange"));require(reads.at(-1).signal.aborted,"hidden page did not abort its in-flight list read");const hiddenCount=reads.length;
 click("refresh");await release("hidden-read");await pause();require(reads.length===hiddenCount&&current.accounts.jobs[0].id===first,"hidden page dispatched or applied an obsolete read");
 online=false;window.dispatchEvent(new Event("offline"));hidden=false;document.dispatchEvent(new Event("visibilitychange"));await pause();require(reads.length===hiddenCount&&!current.connected&&current.error.includes("离线"),"offline recovery dispatched or concealed disconnection");
 online=true;window.dispatchEvent(new Event("online"));await waitFor(()=>reads.length===hiddenCount+1&&current.connected&&!current.error,"online recovery was not one current-scope read");
 click("relogin");await waitFor(()=>one('[role="dialog"]'),"cross-page command confirmation missing");await rawFetch("/fixture/disable",{method:"POST",body:JSON.stringify({ID:first})});click("refresh");await waitFor(()=>current.accounts.forceReloginSelectedCount===0,"new compact capability did not arrive");click("submit");await waitFor(()=>one('[role="alert"]').textContent.includes("重新选择"),"confirmation failed to recheck compact capabilities");
 click("close");await rawFetch("/fixture/disable",{method:"POST",body:JSON.stringify({ID:""})});click("refresh");await waitFor(()=>current.accounts.forceReloginSelectedCount===1,"restored compact capability did not arrive");click("relogin");await waitFor(()=>one("#submit"),"second command confirmation missing");click("submit");click("submit");await waitFor(()=>one('[role="alert"]')?.textContent.includes("操作回执不完整"),"unknown command receipt was accepted");
 require(one('[role="dialog"]')&&current.accounts.selectedJobIds.has(first),"unknown command cleared confirmation or selection");
 await rawFetch("/capture",{method:"POST",body:JSON.stringify({ok:true})});
})().catch(async error=>{failure=error.message;await rawFetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:failure})});});
`
