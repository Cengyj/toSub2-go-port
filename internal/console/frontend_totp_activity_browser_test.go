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

func TestFrontendTOTPActivityInBrowser(t *testing.T) {
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
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: activeTOTPFixture + totpActivityBrowserDriver, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Outfile: "app.js", Write: false, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) != 0 {
		t.Fatal("could not bundle production account row", built.Errors)
	}
	assets := map[string][]byte{}
	for _, output := range built.OutputFiles {
		assets[filepath.Base(output.Path)] = output.Contents
	}
	if len(assets["app.js"]) == 0 || len(assets["app.css"]) == 0 {
		t.Fatal("2FA fixture is missing production CSS or JS")
	}
	for _, scenario := range []struct {
		width int
		theme string
	}{{1280, "light"}, {390, "dark"}} {
		t.Run(fmt.Sprintf("%d/%s", scenario.width, scenario.theme), func(t *testing.T) {
			verifyTOTPActivityBrowser(t, assets, scenario.width, scenario.theme)
		})
	}
}

func verifyTOTPActivityBrowser(t *testing.T, assets map[string][]byte, width int, theme string) {
	t.Helper()
	capture := newProtectedSettingsBrowserCapture()
	var mu sync.Mutex
	inputs, seedReads, forbidden := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'")
		switch r.Method + " " + r.URL.Path {
		case "GET /":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><iframe title="Synthetic 2FA viewport" width="%d" height="850" src="/fixture?width=%d"></iframe>`, width, width)
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
		case "POST /api/jobs/reset-current/input":
			var payload struct {
				Action      string `json:"action"`
				Value       string `json:"value"`
				Expectation struct {
					OperationAt string `json:"operationAt"`
					Stage       string `json:"stage"`
				} `json:"expectation"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
			decoder.DisallowUnknownFields()
			valid := decoder.Decode(&payload) == nil && decoder.Decode(new(any)) == io.EOF && r.Header.Get("x-console-token") == "synthetic" && payload.Action == "email_otp" && payload.Value == "123456" && payload.Expectation.OperationAt == "2026-11-24T02:00:00.123456789Z" && payload.Expectation.Stage == "email_otp"
			mu.Lock()
			if valid {
				inputs++
			} else {
				forbidden++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !valid {
				w.WriteHeader(http.StatusBadRequest)
			}
			io.WriteString(w, `{"ok":true}`)
		case "GET /api/jobs/reset-current/totp-setup":
			mu.Lock()
			seedReads++
			if r.Header.Get("x-console-token") != "synthetic" {
				forbidden++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"secret":"SYNTHETIC-CANDIDATE-ONLY"}`)
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
	t.Logf("2FA activity real React: granted_ms=%d expired_ms=%d wall_ms=%d", browser.GrantedMS, browser.ExpiredMS, browser.WallMS)
	if err != nil {
		t.Fatal("isolated 2FA browser:", err)
	}
	select {
	case <-capture.done:
		if !capture.result.OK {
			t.Fatal("real 2FA activity:", capture.result.Error)
		}
	default:
		t.Fatal("real 2FA activity checks did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if inputs != 1 || seedReads != 1 || forbidden != 0 {
		t.Fatalf("2FA fixture calls: inputs=%d seed_reads=%d forbidden=%d", inputs, seedReads, forbidden)
	}
}

const totpActivityBrowserDriver = `
import React from "react";
import {createRoot} from "react-dom/client";
import {JobRow,TotpActionDialog} from "./src/components/accounts.jsx";
import {ConfirmationProvider} from "./src/components/ui.jsx";
import "./src/styles.css";
const one=selector=>document.querySelector(selector),pause=()=>new Promise(resolve=>setTimeout(resolve,25));
const require=(value,message)=>{if(!value)throw new Error(message);};
async function waitFor(check,message){for(let i=0;i<240;i++){if(check())return;await pause();}throw new Error(message);}
const width=Number(new URL(location.href).searchParams.get("width"));
const root=createRoot(one("#root")),intents=[],localSubmissions=[],errors=[];let revision=0;
const base={...currentReset,createdAt:"2026-11-24T01:00:00Z",authorizationMethod:"chatgpt"};
const meta=()=>one(".account-meta .mail-mode"),action=()=>one(".row-totp-button"),summary=()=>one(".operation-summary"),prompt=()=>one(".prompt-line"),upload=()=>one(".row-upload-button");
function ActivityFixture({job,version}){
 const [dialog,setDialog]=React.useState(null);
 return <ConfirmationProvider><div className="table-frame accounts-table-frame" data-revision={version} tabIndex={0}><table className="accounts-table"><thead><tr><th className="select-heading"/><th className="account-heading">账号</th><th className="usage-heading">用量</th><th className="expiration-heading">到期</th><th className="current-operation-heading">当前步骤</th><th className="activity-time-heading">时间</th><th className="operation-type-heading">本次操作</th><th className="actions-heading">操作</th></tr></thead><tbody><JobRow job={job} token="synthetic" selected={false} selectionSupported={true} onToggleSelected={()=>{}} onToggleLogs={()=>{}} onError={value=>{if(value)errors.push(value);}} totpResetAvailable={true} totpSetupAvailable={true} onSetupTotp={(reset,local)=>{intents.push({reset,local});setDialog({email:job.email,resetTotp:reset,completeTotp:local});}} sub2apiUploadAvailable={true} accountInfoAvailable={false} deleteSupported={false}/></tbody></table></div>{dialog&&<TotpActionDialog action={dialog} logoutAvailable={true} reloginAvailable={true} uploadAvailable={true} uploadConfigured={true} onClose={()=>setDialog(null)} onSubmit={async payload=>{localSubmissions.push(payload);setDialog(null);}}/>}</ConfirmationProvider>;
}
async function show(patch={}){
 const version=++revision,job={...base,...patch};root.render(<ActivityFixture job={job} version={version}/>);
 await waitFor(()=>one("[data-revision]")?.dataset.revision===String(version),"React row did not advance state");await pause();return job;
}
function check(badge,button,status,hint){
 require(meta().textContent.includes(badge),"account second line disagrees: "+badge);
 require(action().textContent===button&&action().disabled,"security action disagrees or is enabled: "+button);
 require(summary().textContent.includes(status)&&prompt().textContent.includes(hint),"operation summary or current step disagrees: "+status);
 require(upload().disabled,"unfinished workflow enabled upload");
 action().click();require(intents.length===0,"disabled action created a duplicate intent");
}
function layout(){
 require(innerWidth===width&&matchMedia("(max-width:600px)").matches===(width<=600),"fixture did not use requested CSS viewport");
 const frame=one(".accounts-table-frame");require(frame.clientWidth<=width&&frame.scrollWidth>=frame.clientWidth,"account scroll frame lost width");
 for(const node of [meta(),action(),summary()]){const rect=node.getBoundingClientRect();require(rect.width>0&&rect.height>0,"2FA state became invisible");}
 if(width<600){frame.scrollLeft=frame.scrollWidth;require(frame.scrollLeft>0,"narrow table cannot reach actions");frame.scrollLeft=0;}
}
(async()=>{
 await show({operationStatus:"queued",status:"queued",totpWorkflowStatus:"pending",stage:"",prompt:""});check("2FA 排队中","等待重置","排队中","已加入队列");layout();
 await show();check("2FA 重置中","重置中","重置 2FA中","正在创建新的 2FA 密钥");require(!one(".job-row").textContent.includes("需人工处理"),"normal write-ahead protection became manual intervention");
 const waiting={status:"email_otp",operationStatus:"waiting",prompt:"请提供本次邮箱验证码"};
 await show(waiting);check("等待邮箱码","等待邮箱码","等待邮箱码","请提供本次邮箱验证码");
 const input=one('input[placeholder="6 位邮箱验证码"]');require(input,"real reset lost email input");input.focus();
 Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(input,"123456");input.dispatchEvent(new Event("input",{bubbles:true}));
 await waitFor(()=>!one('button[title="提交邮箱验证码"]').disabled,"React did not receive real OTP input");
 await show({...waiting,prompt:"继续等待本次邮箱验证码"});require(document.activeElement===input&&input.value==="123456","same operation rerender erased draft or focus");
 one('button[title="提交邮箱验证码"]').click();await waitFor(()=>input.value==="","synthetic manual input did not finish exactly once");
 await show({totpCompletionPending:true,totpActivationVerified:true,totpResetPhase:"activation_verified"});check("2FA 正在保存","保存中","本地保存中","正在完成本地");require(!prompt().textContent.includes("新 2FA 已保存"),"activation evidence became local commit");
 const saved={totpRecoveryPending:false,totpCredentialInvalidated:false,totpResetCommitted:true,totpActivationVerified:true,totpResetPhase:"credential_persisted",totpWorkflowStep:"relogin",reloginAfterTotp:true,totpReloginStatus:"running",prompt:"正在重新登录并生成新授权"};
 await show(saved);check("2FA 已保存","后续处理中","重登授权中","正在重新登录");
 const warning={...saved,totpWorkflowStep:"logout",logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:"pending",stage:"totp_logout_unknown",prompt:"新 2FA 已保存；退出结果未知，后续步骤停止"};
 await show(warning);check("退出结果待核实","结果待核实","退出结果待核实","退出结果未知");require(!summary().querySelector(".spin"),"explicit unknown outcome kept an execution spinner");
 await show({...warning,totpWorkflowStatus:"failed",stage:"totp_logout_save_failed",logoutAllDevicesStatus:"unknown"});check("2FA 已保存","收尾中","结果待核实","退出设备结果待核实");require(!summary().querySelector(".spin"),"terminal workflow during runner cleanup appeared active");
 await show({status:"failed",operationStatus:"failed",totpWorkflowStatus:"failed",totpResetStatus:"unknown",totpOperationInFlight:false,totpActiveOperationAt:""});check("2FA 需人工处理","需人工处理","2FA 需人工处理","需人工处理");
 for(const owner of ["","2026-11-24T02:00:00.123456790Z"]){
  await show({...waiting,totpActiveOperationAt:owner,stage:"totp_setup_seed_unverified"});check("同账号 2FA 未结束","2FA处理中","同账号 2FA 未结束","流程尚未收尾");require(!one(".inline-entry"),"historical owner accepted the previous operation's input");
 }
 await show({...waiting,status:"totp_setup_otp",totpActiveOperationAt:"",totpOperationInFlight:false});check("2FA 状态同步中","状态同步中","2FA 状态同步中","等待最新结果");require(!one(".inline-entry")&&!one(".totp-setup-secret")&&!summary().querySelector(".spin"),"stale idle snapshot offered old input, secret or running stage");
 await show({lastOperationType:"setup_2fa",status:"totp_setup_otp",operationStatus:"waiting",hasTotpKey:false,prompt:"请激活本次候选密钥"});check("等待激活 2FA","等待激活 2FA","等待激活 2FA","请激活本次");
 await waitFor(()=>one(".totp-setup-secret code")?.textContent==="SYNTHETIC-CANDIDATE-ONLY","current setup did not read its synthetic candidate");require(one('input[placeholder="新 2FA 的 6 位验证码"]'),"real first setup lost activation input");
 await show({lastOperationType:"setup_2fa",stage:"totp_setup_seed_unverified",prompt:"人工验证码已激活，但候选密钥尚未证明正确"});check("2FA 密钥待核实","结果待核实","2FA 密钥待核实","候选密钥尚未证明正确");require(!one(".totp-setup-secret"),"candidate leaked beyond its input stage");
 await show({status:"failed",operationStatus:"failed",totpWorkflowStatus:"failed",totpResetStatus:"unknown",totpCompletionPending:true,totpActivationVerified:true,totpOperationInFlight:false,totpActiveOperationAt:"",canCompleteTotp:true});
 require(meta().textContent.includes("待保存")&&summary().textContent.includes("待保存")&&action().textContent==="完成本地保存"&&!action().disabled&&upload().disabled&&one(".job-notice").textContent.includes("已有受保护的激活确认材料"),"terminal local recovery became saved, unknown or inaccessible");action().focus();action().click();require(intents.length===1&&intents[0].reset&&intents[0].local,"local completion opened a remote-reset intent");
 await waitFor(()=>one('[role="dialog"]'),"trusted local completion dialog did not open");require(one('[role="dialog"]').textContent.includes("不连接远端")&&!one('[role="switch"]')&&one(".totp-flow-preview-steps").children.length===1,"local completion offered remote followups");one('[role="dialog"] button[type="submit"]').click();await waitFor(()=>!one('[role="dialog"]'),"local completion dialog did not finish");require(localSubmissions.length===1&&JSON.stringify(localSubmissions[0])==="{}"&&document.activeElement===action(),"local completion submitted remote options or failed to restore focus");
 await show({lastOperationType:"complete_2fa",totpWorkflowStatus:"failed",totpResetStatus:"unknown",totpCompletionPending:true,totpActivationVerified:true});require(meta().textContent.includes("正在保存")&&action().disabled&&action().textContent==="保存中"&&summary().textContent.includes("本地保存中")&&one(".job-notice").textContent.includes("已有受保护的激活确认材料"),"running local recovery borrowed old failed or unknown workflow");
 await show({...saved,status:"completed",operationStatus:"succeeded",totpWorkflowStatus:"succeeded",totpWorkflowStep:"totp",totpResetStatus:"succeeded",totpOperationInFlight:false,totpActiveOperationAt:"",totpExportBlocked:false,canResetTotp:true,canDownload:true,reloginAfterTotp:false,prompt:"",stage:""});
 require(meta().textContent==="密码 + 2FA"&&action().textContent==="重置 2FA"&&!action().disabled&&!upload().disabled&&summary().textContent.includes("流程已完成"),"confirmed completion did not restore authoritative capabilities");
 require(errors.length===0,"row reported a fixture request failure");layout();root.unmount();await fetch("/capture",{method:"POST",body:JSON.stringify({ok:true})});
})().catch(async error=>{await fetch("/capture",{method:"POST",body:JSON.stringify({ok:false,error:error.message})});});
`
