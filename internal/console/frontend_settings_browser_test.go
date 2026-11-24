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

type protectedSettingsBrowserResult struct {
	OK          bool            `json:"ok"`
	Error       string          `json:"error"`
	Diagnostics json.RawMessage `json:"diagnostics"`
}

type protectedSettingsBrowserCapture struct {
	once   sync.Once
	done   chan struct{}
	result protectedSettingsBrowserResult
}

func newProtectedSettingsBrowserCapture() *protectedSettingsBrowserCapture {
	return &protectedSettingsBrowserCapture{done: make(chan struct{})}
}

func (capture *protectedSettingsBrowserCapture) accept(result protectedSettingsBrowserResult) {
	// A later success, including one following a failed terminal receipt, cannot
	// replace the first result. Readers wait for done before reading result.
	capture.once.Do(func() { capture.result = result; close(capture.done) })
}

// Uses real React, browser lifecycle, focus, Blob and opaque iframe execution.
// Every provider/API is a loopback fixture; download dispatch is captured before
// it can write outside the temporary profile.
func TestFrontendProtectedSettingsInBrowser(t *testing.T) {
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
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: protectedSettingsBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": "\"production\""}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal(built.Errors)
	}
	assets := map[string][]byte{}
	for _, output := range built.OutputFiles {
		assets[filepath.Base(output.Path)] = output.Contents
	}
	for _, test := range []struct {
		width int
		theme string
	}{{1440, "light"}, {390, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", test.width, test.theme), func(t *testing.T) { verifyProtectedSettingsBrowser(t, assets, test.width, test.theme) })
	}
}

func verifyProtectedSettingsBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	capture := newProtectedSettingsBrowserCapture()
	var mu sync.Mutex
	calls := map[string]int{}
	var unexpected []string
	mail := json.RawMessage(`{"method":"GET","url":"","headersText":"{\"Server\":\"old\"}","unknown":{"false":false,"zero":0,"empty":[]}}`)
	revision := 1
	maintenanceRevision := 0
	maintenanceEnabled := false
	trashEntries := []map[string]any{}
	for i := 0; i < 23; i++ {
		id := fmt.Sprintf("trash-%02d", i)
		trashEntries = append(trashEntries, map[string]any{"id": id, "email": id + "@example.invalid", "state": "trashed", "deletedAt": fmt.Sprintf("2026-11-%02dT10:00:00Z", i+1), "recordCount": 1, "canRestore": true, "jobs": []map[string]any{{"id": id + "-job", "email": id + "@example.invalid", "status": "interrupted", "createdAt": "2026-10-01T00:00:00Z"}}})
	}
	trashSettings := map[string]any{"schemaVersion": 1, "revision": 1, "retentionDays": 0}
	monitor := map[string]any{"target": "sub2api", "enabled": false, "configured": true, "running": false, "intervalMinutes": 5, "cooldownMinutes": 5, "excludedPlanTypes": []string{}, "revision": 1, "targets": map[string]any{"sub2api": map[string]bool{"configured": true}, "cpa": map[string]bool{"configured": true}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; connect-src 'self'; style-src 'self' 'unsafe-inline'")
		if r.Method == "GET" {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><script type="application/json" id="%s">{}</script><iframe title="Synthetic settings" style="border:0;width:%dpx;height:844px" src="/app?width=%d&amp;theme=%s"></iframe>`, protectedBrowserMarkerID, width, width, theme)
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
				w.WriteHeader(204)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.Path
		calls[key]++
		snapshot := func(scope string, config json.RawMessage, rev int, verified bool) {
			write(map[string]any{"version": 1, "scope": scope, "revision": rev, "config": config, "credentialPersistence": "secure", "verified": verified, "applied": false})
		}
		switch key {
		case "GET /api/bootstrap":
			write(map[string]any{"token": "synthetic-console-token", "features": map[string]bool{"protectedIntegrationSettings": true, "maintenance": true, "accountTrash": true, "sub2apiMonitor": true, "cpaUpload": true}})
		case "GET /api/jobs":
			write(map[string]any{"jobs": []any{}, "selection": []any{}})
		case "GET /api/cpa/settings":
			write(map[string]any{"baseUrl": "https://cpa.synthetic.invalid", "configured": true, "hasApiKey": true, "revision": 1, "credentialPersistence": "secure"})
		case "GET /api/sub2api/monitor":
			write(map[string]any{"configured": true, "enabled": false, "baseUrl": "https://sub2.synthetic.invalid", "groupIds": []string{}, "excludedPlanTypes": []string{}, "openaiRequestTimezone": "UTC"})
		case "GET /api/account-trash/settings":
			write(map[string]any{"settings": trashSettings})
		case "GET /api/account-trash":
			write(map[string]any{"entries": trashEntries, "settings": trashSettings, "status": map[string]bool{"running": false}})
		case "POST /api/account-trash/restore", "POST /api/account-trash/purge":
			var body struct {
				IDs     []string `json:"ids"`
				Confirm bool     `json:"confirmPermanent"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.IDs) == 0 || strings.HasSuffix(r.URL.Path, "purge") && !body.Confirm {
				unexpected = append(unexpected, "invalid trash mutation")
			}
			remaining := []map[string]any{}
			for _, entry := range trashEntries {
				remove := false
				for _, id := range body.IDs {
					if entry["id"] == id {
						remove = true
					}
				}
				if !remove {
					remaining = append(remaining, entry)
				}
			}
			trashEntries = remaining
			countKey := "restored"
			if strings.HasSuffix(r.URL.Path, "purge") {
				countKey = "purged"
			}
			write(map[string]any{"outcome": "completed", countKey: len(body.IDs), "affectedIds": body.IDs})
		case "GET /api/pool-monitor":
			write(monitor)
		case "POST /fixture/monitor-advance":
			monitor["revision"] = 2
			monitor["intervalMinutes"] = 7
			write(map[string]bool{"ok": true})
		case "POST /api/pool-monitor":
			var body struct {
				Revision int      `json:"revision"`
				Target   string   `json:"target"`
				Enabled  bool     `json:"enabled"`
				Interval int      `json:"intervalMinutes"`
				Cooldown int      `json:"cooldownMinutes"`
				Excluded []string `json:"excludedPlanTypes"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Revision != monitor["revision"] || body.Target != "cpa" || body.Cooldown != 0 || !body.Enabled {
				unexpected = append(unexpected, "invalid monitor policy")
			}
			monitor["revision"] = body.Revision + 1
			monitor["target"] = body.Target
			monitor["enabled"] = body.Enabled
			monitor["intervalMinutes"] = body.Interval
			monitor["cooldownMinutes"] = body.Cooldown
			monitor["excludedPlanTypes"] = body.Excluded
			write(monitor)
		case "POST /api/pool-monitor/check":
			monitor["lastCheckAt"] = "2026-11-23T10:00:00Z"
			monitor["lastResult"] = map[string]int{"checked": 3, "started": 1, "excludedPlan": 2}
			write(monitor)
		case "GET /api/account-insights/settings":
			write(map[string]any{"enabled": false, "automatic": false, "intervalMinutes": 5, "revision": 1})
		case "GET /api/account-insights":
			write(map[string]any{"snapshots": map[string]any{}})
		case "GET /api/account-insights/status":
			write(map[string]any{"enabled": false, "automatic": false, "refresh": map[string]int{"total": 0, "eligible": 0, "queued": 0, "active": 0, "coolingDown": 0, "unavailable": 0, "retryAfterSeconds": 0}})
		case "GET /api/integration-settings/mail":
			snapshot("mail", mail, revision, false)
		case "GET /api/integration-settings/sms", "GET /api/integration-settings/proxy", "GET /api/integration-settings/sub2api":
			snapshot(strings.TrimPrefix(r.URL.Path, "/api/integration-settings/"), nil, 0, false)
		case "PUT /api/integration-settings/mail":
			var body struct {
				Revision int             `json:"revision"`
				Config   json.RawMessage `json:"config"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Revision != revision {
				unexpected = append(unexpected, "invalid mail revision")
				w.WriteHeader(409)
				write(map[string]string{"error": "conflict"})
				return
			}
			mail = body.Config
			revision++
			w.WriteHeader(503)
			write(map[string]any{"error": "synthetic durable outcome unknown", "uncertain": true, "outcome": "unknown"})
		case "POST /api/integration-settings/mail/confirm":
			snapshot("mail", mail, revision, true)
		case "POST /fixture/mail-advance":
			mail = json.RawMessage(`{"method":"GET","url":"","headersText":"{\"Other\":\"saved\"}"}`)
			revision++
			write(map[string]bool{"ok": true})
		case "POST /api/mail-request-config":
			var body struct {
				ProtectedRevision int `json:"protectedRevision"`
				Config            struct {
					Headers map[string]string `json:"headers"`
				} `json:"config"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				unexpected = append(unexpected, "invalid mail application body")
			}
			if body.ProtectedRevision != revision {
				w.WriteHeader(409)
				write(map[string]string{"code": "SETTINGS_CONFLICT", "error": "synthetic other browser changed config"})
				return
			}
			if body.Config.Headers["Other"] != "saved" {
				unexpected = append(unexpected, "applied later unsaved mail draft")
			}
			write(map[string]bool{"ok": true})
		case "GET /api/maintenance", "POST /api/maintenance":
			if r.Method == "POST" {
				var body struct {
					Enabled  bool `json:"enabled"`
					Revision int  `json:"revision"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Revision != maintenanceRevision {
					unexpected = append(unexpected, "invalid maintenance revision")
				}
				maintenanceRevision++
				maintenanceEnabled = body.Enabled
			}
			write(map[string]any{"schema": 1, "revision": maintenanceRevision, "enabled": maintenanceEnabled, "uncertain": false, "activeRequests": 0, "jobs": map[string]any{"queued": 0, "running": 0, "queries": 0, "accountChanges": 0, "drained": true}, "insights": 0, "probes": 0, "sms": 0, "recycleRunning": false, "drained": maintenanceEnabled})
		case "GET /ledger":
			write(calls)
		case "POST /capture":
			var receipt protectedSettingsBrowserResult
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&receipt) == nil && decoder.Decode(new(any)) == io.EOF {
				capture.accept(receipt)
			}
			w.WriteHeader(204)
		default:
			if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/account-trash/") && strings.HasSuffix(r.URL.Path, "/logs") {
				write(map[string]any{"logs": "", "entries": []map[string]any{{"at": "2026-11-23T09:00:00Z", "stage": "failed", "status": "failed", "message": "合成回收日志；未重新执行账号操作"}}, "hasMore": false})
				return
			}
			unexpected = append(unexpected, key)
			w.WriteHeader(404)
			write(map[string]string{"error": "unexpected fixture route"})
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	processStdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
	browser, commandErr := runFrontendBrowserPaced(ctx, frontendBrowserPacedOptions{Executable: *uploadBrowser, Profile: t.TempDir(), URL: server.URL + "/", Terminal: capture.done, Stdout: processStdout, Stderr: stderr})
	stdout := &protectedBrowserOutput{}
	_, _ = stdout.Write(browser.DOM)
	t.Logf("paced browser clock: granted_ms=%d expired_ms=%d wall_ms=%d process_stdout={%s}", browser.GrantedMS, browser.ExpiredMS, browser.WallMS, processStdout.summary())
	evidence := protectedBrowserEvidence(stdout, stderr)
	t.Log("bounded browser evidence:", evidence)
	if commandErr != nil {
		if ctx.Err() != nil {
			t.Fatal("isolated Chromium exceeded or cancelled its existing 30s budget")
		}
		t.Fatal("isolated Chromium host:", commandErr)
	}
	select {
	case <-capture.done:
		receipt := capture.result
		diagnostics, valid := decodeProtectedBrowserDiagnostics(receipt.Diagnostics)
		if !valid {
			t.Fatal("browser returned invalid or oversized structured diagnostics")
		}
		if !receipt.OK {
			// The driver emits only static fixture assertion labels here; raw
			// production exceptions are classified without their message/stack.
			t.Fatal("production settings UI:", receipt.Error)
		}
		if diagnostics.Status != "passed" || !diagnostics.originalIsolationObserved() {
			t.Fatal("browser success receipt lacks completed isolation evidence")
		}
		if fromDOM, state := protectedBrowserDOMDiagnostics(stdout.data); state != "valid" || fromDOM.Status != "passed" || !fromDOM.originalIsolationObserved() {
			t.Fatal("browser DOM fallback lacks completed isolation evidence")
		}
	default:
		t.Fatal("browser did not finish settings checks")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unexpected) > 0 {
		t.Fatalf("unexpected fixture calls: %v", unexpected)
	}
	for key, count := range map[string]int{"PUT /api/integration-settings/mail": 1, "POST /api/integration-settings/mail/confirm": 2, "POST /api/mail-request-config": 2, "POST /api/maintenance": 2, "POST /api/account-trash/restore": 1, "POST /api/account-trash/purge": 1, "POST /api/pool-monitor": 1, "POST /api/pool-monitor/check": 1} {
		if calls[key] != count {
			t.Errorf("%s = %d, want %d", key, calls[key], count)
		}
	}
}

const protectedSettingsBrowserDriver = protectedBrowserDiagnosticsDriver + `
import React from "react";
import { createRoot } from "react-dom/client";
import { PelicanPreview } from "./src/components/pelican-preview.jsx";
const diagnostics=createProtectedBrowserDiagnostics(window,parent.document.getElementById("protected-browser-diagnostics"));
const params=new URLSearchParams(location.search),legacy="chatgpt-onboarding.mail-request-settings-v1";
localStorage.setItem(legacy,JSON.stringify({method:"GET",url:"",headersText:'{"Browser":"old"}',unknown:{enabled:false,count:0,models:[]}}));
localStorage.setItem("tosub2.console.theme",params.get("theme"));location.hash="settings";
const pause=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(value,message)=>{if(!value)throw new ProtectedBrowserAssertion(message)};
const one=selector=>document.querySelector(selector);
const button=(label,root=document)=>[...(root?.querySelectorAll("button")||[])].find(node=>node.textContent.trim()===label);
const tab=label=>button(label,one(".settings-tabs"));
async function waitFor(test,message){for(let i=0;i<240;i++){const value=test();if(value)return value;await pause();}throw new ProtectedBrowserAssertion(message,"wait_timeout");}
function input(node,value){const prototype=node.tagName==="TEXTAREA"?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;Object.getOwnPropertyDescriptor(prototype,"value").set.call(node,value);node.dispatchEvent(new Event("input",{bubbles:true}));}
async function ledger(){return fetch("/ledger").then(r=>r.json());}
async function navigate(title){const menu=one('button[aria-label="切换导航菜单"]');if(menu&&menu.getClientRects().length&&one('#main-navigation').getAttribute('role')!=="dialog"){menu.click();await waitFor(()=>one('#main-navigation').getAttribute('role')==="dialog","mobile navigation did not open");}one('#main-navigation button[title="'+title+'"]').click();await pause();}
function select(node,value){Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype,"value").set.call(node,value);node.dispatchEvent(new Event("change",{bubbles:true}));}
function layout(node){const rect=node.getBoundingClientRect();require(innerWidth===Number(params.get("width"))&&rect.left>=0&&rect.right<=innerWidth+1&&node.scrollWidth<=node.clientWidth+1,"settings overflow viewport");require(document.documentElement.dataset.theme===params.get("theme"),"theme missing");}
(async()=>{
 await import("./src/main.jsx");await waitFor(()=>tab("Sub2API"),"sub2 tab missing");tab("Sub2API").click();
 await waitFor(()=>document.body.textContent.includes("默认值也尚未应用到当前进程"),"missing protected config falsely claimed default application");
 require(!button("应用已保存配置"),"missing config offered unsaved process apply");tab("邮件 API").click();
 const editor=await waitFor(()=>one(".mail-request-settings-dialog"),"mail editor missing");
 await waitFor(()=>editor.textContent.includes("当前浏览器仍有旧配置"),"legacy migration explanation missing");layout(editor);
 require(one(".settings-textarea").value.includes("Browser"),"initial GET overwrote legacy draft");
 let counts=await ledger();require(!counts["POST /api/mail-request-config"]&&!counts["PUT /api/integration-settings/mail"],"initial settings read applied or wrote config");
 input(one(".settings-textarea"),'{"Saved":"candidate"}');await pause();
 const save=editor.querySelector('button[type="submit"]');await waitFor(()=>!save.disabled,"secure read did not allow explicit save");save.click();save.click();
 await waitFor(()=>button("核实安全保存结果"),"unknown result lost verification barrier");require(localStorage.getItem(legacy),"unknown result cleared browser source");
 input(one(".settings-textarea"),'{"Draft":"later"}');await pause();button("核实安全保存结果").click();
 await waitFor(()=>!localStorage.getItem(legacy),"verified migration did not clean browser source");
 require(one(".settings-textarea").value.includes("later"),"confirm overwrote later draft");
 const apply=await waitFor(()=>button("应用已保存配置"),"separate process apply missing");await waitFor(()=>!apply.disabled,"apply stayed disabled");await fetch("/fixture/mail-advance",{method:"POST"});apply.click();
 await waitFor(()=>button("核实安全保存结果"),"cross-browser apply conflict missing");require(one(".settings-textarea").value.includes("later"),"apply conflict erased unsaved draft");
 button("核实安全保存结果").click();await waitFor(()=>document.body.textContent.includes("服务端当前配置与上次提交不同"),"conflict did not require an explicit config choice");
 counts=await ledger();require(counts["POST /api/mail-request-config"]===1&&counts["PUT /api/integration-settings/mail"]===1,"conflict automatically repeated application or protected save");
 button("采用服务端配置并清理浏览器副本").click();await waitFor(()=>one(".settings-textarea").value.includes("Other"),"explicit server config choice not adopted");
 input(one(".settings-textarea"),'{"Draft":"later"}');await pause();const chosenApply=button("应用已保存配置");await waitFor(()=>!chosenApply.disabled,"chosen config stayed blocked");chosenApply.click();
 await waitFor(()=>!button("应用已保存配置"),"process apply did not settle");require(one(".settings-textarea").value.includes("later"),"apply overwrote later draft");
 const network=tab("代理网络");network.focus();network.click();const dialog=await waitFor(()=>one('[aria-labelledby="unsaved-title"]'),"dirty navigation guard missing");layout(dialog);
 require(dialog.contains(document.activeElement),"navigation dialog did not claim focus");document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!one('[aria-labelledby="unsaved-title"]'),"Escape did not cancel navigation");require(document.activeElement===network,"cancel did not restore navigation focus");require(one(".settings-textarea").value.includes("later"),"cancel lost draft");
 network.click();await waitFor(()=>button("放弃更改并离开"),"repeat navigation missing");button("放弃更改并离开").click();await waitFor(()=>one('input[aria-label="代理 IP 地址"]'),"network page missing");
 diagnostics.record("settings_checked");
 tab("配置概览").click();const maintenance=await waitFor(()=>one('[aria-label="维护与排空"]'),"maintenance card missing");layout(maintenance);
 const enter=await waitFor(()=>button("进入维护并暂停准入"),"maintenance enter missing");await waitFor(()=>!enter.disabled,"maintenance not loaded");enter.click();enter.click();await waitFor(()=>maintenance.textContent.includes("当前工作已排空"),"maintenance drain not shown");require(maintenance.textContent.includes("正常停服"),"drained claimed upgrade safe without stop");button("退出维护并恢复准入").click();await waitFor(()=>maintenance.textContent.includes("正常运行"),"maintenance exit failed");
 diagnostics.record("maintenance_checked");
 await navigate("回收站");const trash=await waitFor(()=>one('.trash-page'),"trash page missing");await waitFor(()=>trash.querySelectorAll('tbody tr[data-trash-id]').length===20,"trash first page missing");layout(trash);
 const firstRow=trash.querySelector('tr[data-trash-id]'),firstId=firstRow.dataset.trashId;firstRow.querySelector('input[type="checkbox"]').click();one('button[aria-label="下一页回收记录"]').click();await waitFor(()=>trash.querySelectorAll('tbody tr[data-trash-id]').length===3,"trash second page missing");const secondRow=trash.querySelector('tr[data-trash-id]'),secondId=secondRow.dataset.trashId;secondRow.querySelector('input[type="checkbox"]').click();await pause();require(trash.textContent.includes("已选 2 个")&&trash.textContent.includes("1 个不在当前页"),"cross-page selection scope lost");
 input(one('input[aria-label="搜索回收账号或记录 ID"]'),"trash-10");await waitFor(()=>trash.querySelectorAll('tbody tr[data-trash-id]').length===1,"trash search failed");require(trash.textContent.includes("2 个不在当前页"),"filter erased selection");button("恢复所选",trash).click();let confirmation=await waitFor(()=>one('[aria-labelledby="confirmation-title"]'),"restore confirmation missing");require(confirmation.textContent.includes("2 个不在当前页")&&confirmation.textContent.includes("不会自动登录"),"restore confirmation lost scope or paused semantics");button("取消",confirmation).click();await pause();counts=await ledger();require(!counts["POST /api/account-trash/restore"],"cancel restored data");
 button("恢复所选",trash).click();confirmation=await waitFor(()=>one('[aria-labelledby="confirmation-title"]'),"restore re-open missing");const restore=button("恢复账号",confirmation);restore.click();restore.click();await waitFor(()=>trash.textContent.includes("共 21 个账号"),"restore did not reconcile list");require(!trash.textContent.includes("正在重新读取回收站核实"),"restore verification never settled");
 const detail=button("详情与日志",trash);detail.focus();detail.click();let details=await waitFor(()=>one('[aria-labelledby="trash-detail-title"]'),"archived details missing");layout(details);await waitFor(()=>details.textContent.includes("合成回收日志"),"archive log read failed");let copied="";Object.defineProperty(navigator,"clipboard",{configurable:true,value:{writeText:async text=>{copied=text;}}});one('button[aria-label="复制当前显示的日志"]').click();await waitFor(()=>copied.includes("合成回收日志"),"real React clipboard dispatch missing");require(copied.includes("复制范围")&&copied.includes("已读所有保留页"),"copy scope omitted");document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!one('[aria-labelledby="trash-detail-title"]'),"details Escape failed");require(document.activeElement===detail,"details close lost focus");
 const purge=button("彻底删除",trash);purge.click();confirmation=await waitFor(()=>one('[aria-labelledby="confirmation-title"]'),"purge confirmation missing");require(confirmation.textContent.includes("无法恢复")&&confirmation.textContent.includes("凭据"),"purge boundary missing");button("取消",confirmation).click();await pause();counts=await ledger();require(!counts["POST /api/account-trash/purge"],"cancel purged data");purge.click();confirmation=await waitFor(()=>one('[aria-labelledby="confirmation-title"]'),"purge re-open missing");button("彻底删除，无法恢复",confirmation).click();await waitFor(()=>trash.textContent.includes("共 20 个账号"),"purge did not reconcile list");button("清除筛选",trash).click();await waitFor(()=>trash.querySelectorAll('tbody tr[data-trash-id]').length===20,"clear search failed");select(one('select[aria-label="回收记录排序"]'),"oldest");await waitFor(()=>trash.querySelector('tr[data-trash-id]').dataset.trashId==="trash-00","trash sort failed");select(one('select[aria-label="筛选回收状态"]'),"purging");await waitFor(()=>trash.textContent.includes("没有匹配的回收记录"),"trash state filter failed");button("清除筛选",trash).click();
 diagnostics.record("trash_checked");
 await navigate("号池监控");const monitor=await waitFor(()=>one('.monitor-page'),"monitor page missing");const interval=await waitFor(()=>one('input[name="pool-monitor-interval"]'),"monitor config missing");await waitFor(()=>!interval.disabled,"monitor not loaded");layout(monitor);input(interval,"9");await pause();button("读取状态",monitor).click();await pause();require(interval.value==="9","read overwrote unsaved monitor draft");await fetch("/fixture/monitor-advance",{method:"POST"});button("读取状态",monitor).click();await waitFor(()=>button("读取并核实",monitor),"other-browser monitor conflict missing");require(interval.value==="9"&&button("保存巡检规则",monitor).disabled,"conflict overwrote draft or allowed blind save");button("读取并核实",monitor).click();await waitFor(()=>interval.value==="7"&&!interval.disabled,"explicit monitor reload failed");
 select(one('select[name="pool-monitor-target"]'),"cpa");input(one('input[name="pool-monitor-cooldown"]'),"0");one('input[name="pool-monitor-enabled"]').click();await pause();require(monitor.textContent.includes("保存后切换至 CPA"),"target switch applied before save");counts=await ledger();require(!counts["POST /api/pool-monitor"],"target selection wrote policy");const monitorSave=button("保存巡检规则",monitor);monitorSave.click();monitorSave.click();await waitFor(()=>monitor.textContent.includes("巡检策略已保存"),"monitor save failed");require(one('input[name="pool-monitor-cooldown"]').value==="0","explicit zero cooldown changed");button("立即检查",monitor).click();await waitFor(()=>monitor.textContent.includes("巡检已完成"),"manual monitor check failed");const records=one('#pool-monitor-config-tab');records.focus();records.dispatchEvent(new KeyboardEvent("keydown",{key:"Home",bubbles:true}));await waitFor(()=>one('#pool-monitor-records-panel').hidden===false,"monitor keyboard tab switch failed");require(one('#pool-monitor-records-panel').textContent.includes("已检查授权文件"),"CPA records missing");await navigate("账号管理");await waitFor(()=>one('.account-add-form'),"accounts page missing after navigation");require(document.body.textContent.includes("暂无账号")||one('.empty-state'),"accounts empty state missing");
 diagnostics.record("monitor_checked");
 const holder=document.createElement("div");document.body.append(holder);const artworkRoot=createRoot(holder);let isolatedResult,observedFrame,observedSource;
 const previewChannel=[...crypto.getRandomValues(new Uint8Array(16))].map(value=>value.toString(16).padStart(2,"0")).join("");
 const bindFrame=()=>{if(!observedFrame){observedFrame=holder.querySelector("iframe");if(observedFrame)observedSource={contentWindow:observedFrame.contentWindow};}return observedFrame;};
 const receive=event=>{bindFrame();const result=observeProtectedPreviewMessage(event,observedSource,previewChannel,diagnostics);if(result)isolatedResult=result;};
 const loaded=event=>{if(event.target===bindFrame())diagnostics.record("iframe_loaded");};
 addEventListener("message",receive);holder.addEventListener("load",loaded,true);
 const probeURL=new URL("/iframe-network-forbidden",location.origin).href;
 const markup='<html><body><h1>fixture artwork</h1><script>'+ 'const channel='+JSON.stringify(previewChannel)+',probeURL='+JSON.stringify(probeURL)+';'+
  'const report=(stage,extra={})=>parent.postMessage({fixture:"opaque-preview",channel,stage,...extra},"*");'+
  'report("script_started");addEventListener("securitypolicyviolation",event=>{if(event.effectiveDirective==="connect-src"&&event.blockedURI===probeURL&&event.disposition==="enforce"&&event.originalPolicy.includes("connect-src \'none\'"))report("csp_connect_blocked");});'+
  'let blocked=false;try{parent.document.body.dataset.unsafe="true"}catch{blocked=true}report(blocked?"parent_access_denied":"parent_access_allowed");'+
  'fetch(probeURL).then(()=>{report("fetch_resolved");report("final_message",{blocked,networkFailed:false});}).catch(()=>{report("fetch_rejected");report("final_message",{blocked,networkFailed:true});});<\/script></body></html>';
 diagnostics.record("preview_requested");artworkRoot.render(<PelicanPreview output={markup}/>);const frame=await waitFor(bindFrame,"preview did not mount");diagnostics.record("iframe_mounted");require(frame.getAttribute("sandbox")==="allow-scripts"&&!frame.hasAttribute("allow-same-origin"),"preview gained same-origin privileges");
 const policyDocument=new DOMParser().parseFromString(frame.srcdoc,"text/html"),policy=policyDocument.querySelector('meta[http-equiv="Content-Security-Policy" i]');
 require(policy?.content.split(";").some(directive=>directive.trim()==="connect-src 'none'")&&policy.compareDocumentPosition(policyDocument.querySelector("script"))&Node.DOCUMENT_POSITION_FOLLOWING,"preview network policy missing or after script");
 await waitFor(()=>isolatedResult,"preview final message did not arrive");require(isolatedResult.blocked&&isolatedResult.networkFailed&&!document.body.dataset.unsafe,"preview escaped parent or network isolation");
 require(diagnostics.has("script_started")&&diagnostics.has("parent_access_denied")&&diagnostics.has("fetch_rejected"),"preview isolation stages missing");diagnostics.record("preview_checked");
 const expand=button("放大",holder);expand.focus();expand.click();const previewDialog=await waitFor(()=>one('[aria-labelledby="pelican-preview-title"]'),"preview zoom dialog missing");layout(previewDialog);require(previewDialog.contains(document.activeElement),"preview focus missing");document.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));await waitFor(()=>!one('[aria-labelledby="pelican-preview-title"]'),"preview Escape failed");require(document.activeElement===expand,"preview close lost focus");
 const original='<svg xmlns="http://www.w3.org/2000/svg" width="200" height="150"><text x="5" y="30">synthetic original</text></svg>';artworkRoot.render(<PelicanPreview output={original}/>);await waitFor(()=>button("下载原始 SVG",holder),"original SVG action missing");
 const urls=new Map(),create=URL.createObjectURL.bind(URL),click=HTMLAnchorElement.prototype.click;let download;
 URL.createObjectURL=blob=>{const url=create(blob);urls.set(url,blob);return url;};HTMLAnchorElement.prototype.click=function(){download={name:this.download,blob:urls.get(this.href)};};
 button("下载原始 SVG",holder).click();require(download?.name.endsWith(".svg")&&await download.blob.text()===original,"download changed original SVG bytes");HTMLAnchorElement.prototype.click=click;URL.createObjectURL=create;for(const url of urls.keys())URL.revokeObjectURL(url);artworkRoot.unmount();holder.remove();
 diagnostics.record("download_checked");
 counts=await ledger();require(counts["PUT /api/integration-settings/mail"]===1&&counts["POST /api/mail-request-config"]===2&&counts["POST /api/maintenance"]===2,"write replay or unintended automatic apply");
 diagnostics.finish("passed");await fetch("/capture",{method:"POST",body:JSON.stringify({ok:true,diagnostics:diagnostics.snapshot()})});diagnostics.dispose();
})().catch(async error=>{const failure=protectedBrowserFailure(error);diagnostics.finish("failed",failure.failure);try{await fetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:failure.label,diagnostics:diagnostics.snapshot()})});}finally{diagnostics.dispose();}});
`
