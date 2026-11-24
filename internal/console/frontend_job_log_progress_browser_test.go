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

func TestFrontendJobLogProgressInBrowser(t *testing.T) {
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
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: jobLogProgressBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal("could not bundle production logs", built.Errors)
	}
	assets := map[string][]byte{}
	for _, output := range built.OutputFiles {
		assets[filepath.Base(output.Path)] = output.Contents
	}
	if len(assets["app.js"]) == 0 || len(assets["app.css"]) == 0 {
		t.Fatal("log fixture is missing production CSS or JS")
	}
	for _, scenario := range []struct {
		width int
		theme string
	}{{1280, "light"}, {390, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", scenario.width, scenario.theme), func(t *testing.T) {
			verifyJobLogProgressBrowser(t, assets, scenario.width, scenario.theme)
		})
	}
}

func logProgressBrowserPage() map[string]any {
	entries := make([]map[string]any, 0, 56)
	for index := range 36 {
		entry := normalLogRead()
		entry["message"] = fmt.Sprintf("old generic normal read %d", index)
		detail := entry["reset_diagnostic"].(map[string]any)
		detail["seq"], detail["attempt_ref"], detail["duration_ms"] = index+1, fmt.Sprintf("A%d", index+1), 653
		entries = append(entries, entry)
	}
	write := normalLogRead()
	write["message"] = "old generic security write"
	detail := write["reset_diagnostic"].(map[string]any)
	detail["stage"], detail["method"], detail["path"], detail["access"] = "activation", "POST", "/backend-api/accounts/mfa/user/activate_enrollment", "write"
	entries = append(entries, write)
	for _, evidence := range []struct{ reason, message, level string }{
		{"DISPATCH_INTENT", "已记录写前意图，可能已发；不重发", "info"},
		{"INCOMPLETE_RESPONSE", "回包截断，结果未知；保留证据", "warning"},
		{"NO_RESPONSE", "退出结果未知，禁止重发", "warning"},
	} {
		entry := normalLogRead()
		entry["message"], entry["level"] = evidence.message, evidence.level
		detail := entry["reset_diagnostic"].(map[string]any)
		detail["reason_code"], detail["receipt"], detail["dispatch"], detail["access"] = evidence.reason, "not_received", "possibly_sent", "write"
		entries = append(entries, entry)
	}
	terminal := normalLogRead()
	terminal["message"], terminal["level"] = "本轮结果：新密钥已保存，退出待核实", "warning"
	terminal["reset_diagnostic"] = map[string]any{"op_ref": "O0123456789abcdef", "seq": 48, "stage": "local_commit", "reason_code": "RESET_COMPLETE_LOGOUT_UNCONFIRMED", "terminal": true, "activation_receipt": "accepted", "local_commit": "committed", "next_action": "review_logout"}
	entries = append(entries, terminal)
	step := normalLogRead()
	delete(step, "reset_diagnostic")
	step["stage"], step["status"], step["message"] = "totp_workflow_relogin", "succeeded", "新授权已保存"
	entries = append(entries, step)
	for index := range 14 {
		entry := normalLogRead()
		delete(entry, "reset_diagnostic")
		entry["stage"], entry["message"] = "login", fmt.Sprintf("合成阶段进展 %d", index)
		entries = append(entries, entry)
	}
	return map[string]any{"id": "logs-synthetic", "logs": "", "entries": entries, "hasMore": true, "nextBefore": "older"}
}

func verifyJobLogProgressBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	capture := newProtectedSettingsBrowserCapture()
	var mu sync.Mutex
	reads, older, forbidden := 0, 0, 0
	failNext := false
	page := logProgressBrowserPage()
	plain := "2026-11-23T02:00:00Z [info] identical retained history"
	multiline := "2026-11-23T02:00:01Z [warning] historical warning first line\nhistorical warning second line"
	history := map[string]any{"id": "logs-synthetic", "logs": strings.Join([]string{plain, plain, multiline}, "\n"), "entries": []map[string]any{
		{"at": "2026-11-23T02:00:00Z", "level": "info", "message": plain, "legacyText": true},
		{"at": "2026-11-23T02:00:00Z", "level": "info", "message": plain, "legacyText": true},
		{"at": "2026-11-23T02:00:01Z", "level": "info", "message": multiline, "legacyText": true},
	}, "hasMore": false, "warning": "合成保留范围警告"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'")
		switch r.Method + " " + r.URL.Path {
		case "GET /":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><iframe title="Synthetic log viewport" width="%d" height="850" src="/fixture?width=%d"></iframe>`, width, width)
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
		case "GET /api/jobs/logs-synthetic/logs":
			mu.Lock()
			reads++
			fail := failNext
			failNext = false
			if r.URL.Query().Get("before") == "older" {
				older++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if fail {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":"合成日志读取失败"}`)
			} else if r.URL.Query().Get("before") == "older" {
				_ = json.NewEncoder(w).Encode(history)
			} else {
				_ = json.NewEncoder(w).Encode(page)
			}
		case "POST /fixture/fail-next":
			mu.Lock()
			failNext = true
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
	browser, err := runFrontendBrowserPaced(ctx, frontendBrowserPacedOptions{Executable: *uploadBrowser, Profile: t.TempDir(), URL: fmt.Sprintf("%s/?width=%d", server.URL, width), Terminal: capture.done, Stdout: stdout, Stderr: stderr})
	t.Logf("log real React: granted_ms=%d expired_ms=%d wall_ms=%d", browser.GrantedMS, browser.ExpiredMS, browser.WallMS)
	if err != nil {
		t.Fatal("isolated log browser:", err)
	}
	select {
	case <-capture.done:
		if !capture.result.OK {
			t.Fatal("real log progress:", capture.result.Error)
		}
	default:
		t.Fatal("real log progress checks did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if reads < 4 || older != 1 || forbidden != 0 {
		t.Fatalf("log fixture calls: reads=%d older=%d forbidden=%d", reads, older, forbidden)
	}
}

const jobLogProgressBrowserDriver = `
import React from "react";
import {createRoot} from "react-dom/client";
import {JobLogs} from "./src/components/job-logs.jsx";
import "./src/styles.css";
const one=selector=>document.querySelector(selector),all=selector=>[...document.querySelectorAll(selector)],pause=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(value,message)=>{if(!value)throw new Error(message);};
async function waitFor(check,message){for(let i=0;i<240;i++){if(check())return;await pause();}throw new Error(message);}
const button=label=>all("button").find(node=>node.getAttribute("aria-label")===label||node.textContent===label),text=()=>one(".job-log-panel")?.textContent||"",rows=()=>all(".job-log-entry");
const copies=[];let refuse=false;
Object.defineProperty(navigator,"clipboard",{configurable:true,value:{writeText:async value=>{if(refuse)throw new Error("合成复制拒绝");copies.push(value);}}});
const width=Number(new URL(location.href).searchParams.get("width"));one("#root").style.width=width+"px";
const root=createRoot(one("#root"));root.render(<JobLogs token="synthetic" jobId="logs-synthetic" job={{operationStatus:"running",lastOperationAt:"2026-11-24T01:59:00Z"}}/>);
function layout(){const panel=one(".job-log-panel");require(innerWidth===width&&matchMedia("(max-width:600px)").matches===(width<=600),"fixture did not use its requested CSS viewport");require(panel.scrollWidth<=panel.clientWidth+1,"log panel overflowed its width");for(const node of all(".job-log-tools button")){const rect=node.getBoundingClientRect(),box=panel.getBoundingClientRect();require(rect.left>=box.left-1&&rect.right<=box.right+1&&rect.height>=20,"log toolbar control clipped or collapsed");}for(const row of rows()){require(row.querySelector(".job-log-kind").getBoundingClientRect().right<=row.querySelector(".job-log-message").getBoundingClientRect().left,"log kind badge overlaps the message column");}}
(async()=>{
 await waitFor(()=>rows().length===20&&text().includes("已收起 36 条正常网络读取"),"56-entry progress did not retain exactly20 important records");layout();
 require(button("关键进展").getAttribute("aria-pressed")==="true"&&text().includes("2FA步骤结果")&&text().includes("步骤完成")&&text().includes("可能已发")&&text().includes("退出结果未知")&&text().includes("回包截断"),"progress hid phase outcome or safety evidence");
 require(!text().includes("old generic normal read")&&text().includes("激活新 2FA：收到完整响应")&&text().includes("待解析业务结果"),"routine read remained noisy or write became business success");
 button("复制可读日志").click();await waitFor(()=>copies.length===1,"readable copy missing");require(copies[0].includes("复制范围：本次")&&copies[0].includes("筛选：关键进展")&&copies[0].includes("尚有更早页")&&copies[0].includes("已收起 36 条正常网络读取")&&copies[0].includes("[2FA步骤结果]")&&!copies[0].includes('"reset_diagnostic"'),"readable copy lost scope or contains nested diagnostic JSON");
 button("复制当前显示的日志").click();await waitFor(()=>copies.length===2,"raw copy missing");require(copies[1].split("\n").length===21&&copies[1].includes('"activation_receipt":"accepted"')&&copies[1].includes('"next_action":"review_logout"'),"raw current-filter copy lost original security fields");
 const allButton=button("全部");allButton.focus();allButton.click();await waitFor(()=>rows().length===56,"all did not reveal all36 normal reads");require(document.activeElement===allButton,"filter change lost keyboard focus");layout();
 require(text().includes("读取 2FA 设置：收到完整响应（HTTP 200 · 653 ms）")&&all(".job-log-entry.kind-success").length===1,"transport HTTP was labelled successful business completion");
 const details=one(".job-log-message"),summary=details.querySelector("summary");summary.focus();summary.click();await waitFor(()=>details.open&&button("继续跟随"),"expanding raw details did not pause follow");require(details.textContent.includes('"attempt_ref":"A1"'),"raw details removed request identity");
 button("复制当前显示的日志").click();await waitFor(()=>copies.length===3,"all raw copy missing");require(copies[2].includes("筛选：全部")&&copies[2].split("\n").length===57&&JSON.parse(copies[2].split("\n")[1]).reset_diagnostic.duration_ms===653,"all raw export changed original JSON/count");
 const abnormal=all("button").find(node=>node.textContent.startsWith("异常"));abnormal.click();await waitFor(()=>rows().length===3,"abnormal filter lost unknown and truncated results");button("复制可读日志").click();await waitFor(()=>copies.length===4,"abnormal readable copy missing");require(copies[3].includes("筛选：异常")&&copies[3].includes("筛选未显示 53 条")&&copies[3].includes("退出结果未知"),"abnormal copy claimed full coverage");
 button("关键进展").click();button("加载更早日志").click();await waitFor(()=>rows().length===23&&text().includes("合成保留范围警告"),"history lost filter or duplicated legacy wrappers");require(text().includes("自动更新已暂停")&&!text().includes("已读所有保留日志")&&text().includes("2026-11-24 09:59:00")&&all(".job-log-entry.kind-warning").length===4,"history completeness, timezone or multiline severity was wrong");
 button("复制可读日志").click();await waitFor(()=>copies.length===5,"history readable copy missing");require(copies[4].includes("复制范围：已加载历史")&&copies[4].includes("分页完整性未知")&&!copies[4].includes("已读所有保留页")&&copies[4].includes("读取或归档有警告")&&copies[4].includes("historical warning second line"),"history copy lost partial-retention/multiline evidence");
 button("本次").click();await waitFor(()=>rows().length===20&&!text().includes("identical retained history"),"current scope restored old history");
 await fetch("/fixture/fail-next",{method:"POST"});button("刷新操作日志").click();await waitFor(()=>text().includes("合成日志读取失败"),"read error did not retain diagnostic");require(rows().length===20&&text().includes("保留上次读取内容"),"read failure erased valid logs");
 button("刷新操作日志").click();await waitFor(()=>!text().includes("合成日志读取失败")&&rows().length===20,"explicit refresh did not recover");
 refuse=true;button("复制可读日志").click();await waitFor(()=>text().includes("合成复制拒绝"),"clipboard rejection was concealed");layout();
 root.unmount();await fetch("/capture",{method:"POST",body:JSON.stringify({ok:true})});
})().catch(async error=>{await fetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:error.message})});});
`
