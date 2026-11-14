package console

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests execute the production React component with the existing Go
// esbuild/Sobek harness. Native browser layout and focus checks remain separate.
const frontendAccountOperationTreeQueries = `
function operationVisibleLabel(label){return ({"本次已完成":"已完成","本次操作失败":"操作失败","本次已中断":"已中断","本次已取消":"已取消","授权已保存 · 自动回传暂停":"授权已存 · 回传暂停"})[label]||label;}
function operationNodes(tree,predicate){
 if(!tree||typeof tree!=="object")return [];
 return (predicate(tree)?[tree]:[]).concat((Array.isArray(tree)?tree:tree.props?.children||[]).flatMap(child=>operationNodes(child,predicate)));
}
function operationClass(tree,name){return operationNodes(tree,node=>(node.props?.className||"").split(" ").includes(name));}
function operationElements(tree,type){return operationNodes(tree,node=>node.type===type);}
function assertOperationHierarchy(tree){
 const heading=operationClass(tree,"operation-summary-heading"),metadata=operationClass(tree,"operation-summary-meta");
 assert(heading.length===1&&metadata.length===1&&operationNodes(tree,node=>node===heading[0]||node===metadata[0])[0]===heading[0],"operation name and status context lost their two-row order");
 const name=operationClass(heading[0],"operation-name"),pills=operationClass(metadata[0],"operation-status-pill"),methods=operationClass(heading[0],"operation-method");
 assert(name.length===1&&pills.length===1&&methods.length===1&&!operationClass(heading[0],"operation-status-pill").length&&!operationClass(metadata[0],"operation-method").length&&!operationClass(metadata[0],"operation-name").length,"operation identity and decisive outcome lost their separate rows");
 assert(operationNodes(heading[0],node=>node===name[0]||node===methods[0])[0]===name[0]&&!operationClass(methods[0],"account-pill").length&&operationClass(tree,"account-pill").length===1,"saved authorization identity displaced the operation name or duplicated the status");
 assert(operationElements(tree,"button").length===1&&tree.type==="button"&&tree.props.type==="button","two-row operation added another hit target");
 for(const node of operationNodes(tree,node=>node!==tree)){
  const props=node.props||{};
  assert(!["button","a","input","select","textarea"].includes(node.type)&&!["button","link"].includes(props.role)&&!(props.tabIndex!==undefined&&Number(props.tabIndex)>=0)&&!Object.keys(props).some(key=>/^on[A-Z]/.test(key)&&typeof props[key]==="function"),"operation context introduced an independent action or keyboard stop");
 }
}
`

func TestFrontendAccountOperationPreservesFinalOutcomesAndLifecycle(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "account-operation")
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
const base={id:"synthetic-operation",email:"synthetic-operation@example.invalid",authorizationMethod:"chatgpt",lastOperationType:"relogin"};
const cases=[
 ["imported",{status:"imported",lastOperationType:"account_import"},"imported","待授权","neutral","ListPlus",false,"资料已导入"],
 ["queue over saved grant",{status:"completed",operationStatus:"queued",queuePosition:3,canDownload:true},"queued","排队中","neutral","LoaderCircle",false,"前方还有 2 条"],
 ["running over old success",{status:"completed",operationStatus:"running",canDownload:true},"running","处理中","active","LoaderCircle",true,"正在重登授权"],
 ["waiting over old success",{status:"completed",operationStatus:"waiting",canDownload:true},"waiting_input","等待输入","warning","CircleAlert",false,"请完成当前验证"],
 ["refresh",{status:"refreshing",lastOperationType:"refresh"},"refreshing","刷新授权","active","RefreshCw",true,"重新授权"],
 ["saving",{status:"finalizing"},"finalizing","保存授权","active","LoaderCircle",true,"重登授权"],
 ["success",{status:"completed",operationStatus:"succeeded",canDownload:true},"operation_succeeded","本次已完成","success","CheckCircle2",false,"授权文件可下载"],
 ["first failure",{status:"failed",operationStatus:"failed",operationError:"Synthetic failure"},"operation_failed","本次操作失败","danger","CircleAlert",false,"重登授权未完成"],
 ["failure keeps old grant",{status:"completed",operationStatus:"failed",operationError:"Synthetic reauthorization failure",canDownload:true},"operation_failed","本次操作失败","danger","CircleAlert",false,"已有授权文件仍可下载"],
 ["cancel keeps old grant",{status:"completed",operationStatus:"canceled",canDownload:true},"operation_canceled","本次已取消","neutral","Ban",false,"已有授权文件仍可下载"],
 ["interruption keeps checkpoint",{status:"resume_available",operationStatus:"interrupted",canResume:true},"operation_interrupted","本次已中断","warning","CircleAlert",false,"已取得授权可本地补保存"],
 ["reauthentication required",{status:"reauth_required"},"reauth_required","待重登授权","warning","LogIn",false,"需要重新登录"],
 ["resumable",{status:"resume_available",canResume:true},"resume_available","可保存授权结果","warning","RotateCcw",false,"已取得授权"],
 ["credential warning preserves success",{status:"completed",operationStatus:"succeeded",canDownload:true,credentialWarning:"Synthetic credential warning"},"operation_succeeded","本次已完成","success","CheckCircle2",false,"授权文件可下载"],
 ["legacy reminder",{status:"completed",canDownload:true,lastError:"Synthetic legacy warning"},"completed_warning","完成 · 有提醒","warning","CircleAlert",false,"授权文件已保存"],
 ["2FA recovery takes precedence",{status:"completed",operationStatus:"succeeded",totpRecoveryPending:true,canDownload:true},"totp_manual_required","2FA 需人工处理","warning","ShieldCheck",false,"需人工处理"],
 ["invalidated seed takes precedence",{status:"completed",operationStatus:"succeeded",totpCredentialInvalidated:true,canDownload:true},"totp_manual_required","2FA 需人工处理","warning","ShieldCheck",false,"需人工处理"],
 ["new 2FA queue discards old terminal flow",{status:"completed",operationStatus:"queued",lastOperationType:"reset_2fa",totpWorkflowStatus:"succeeded",totpWorkflowStep:"upload"},"queued","排队中","neutral","LoaderCircle",false,"等待重置 2FA"],
 ["automatic skipped",{status:"canceled",operationStatus:"skipped",lastOperationType:"automatic_relogin",canDownload:true,autoRepairSkippedReason:"Synthetic plan exclusion"},"automatic_relogin_skipped","自动重登已跳过","neutral","Ban",false,"未执行"],
 ["automatic transfer paused",{status:"completed",operationStatus:"succeeded",lastOperationType:"automatic_relogin",canDownload:true,autoRepairPendingAccountIds:[107],autoRepairSkippedReason:"Synthetic plan exclusion"},"automatic_upload_paused","授权已保存 · 自动回传暂停","warning","CircleAlert",false,"待处理记录保留"],
 ["unknown status",{status:"future_state"},"future_state","状态待确认","warning","CircleAlert",false,"重登授权"],
];
for(const [raw,label,icon] of [["password","等待密码","KeyRound"],["email_otp","等待邮箱码","Mail"],["mfa_otp","等待 2FA","ShieldCheck"],["totp_setup_otp","等待激活 2FA","ShieldCheck"],["phone","等待手机号","Smartphone"],["phone_otp","等待手机码","Smartphone"],["sms_otp","等待手机码","Smartphone"]]){
 cases.push([raw,{status:raw,operationStatus:"waiting",prompt:"Synthetic verification input"},raw,label,"warning",icon,false,"Synthetic verification input"]);
}
for(const [name,fields,code,label,tone,icon,spinning,summary] of cases){
 const job=Object.freeze({...base,...fields}),before=JSON.stringify(job);
 const tree=RepairUI.AccountOperationSummary({job,onToggleLogs(){}}),button=operationClass(tree,"operation-status")[0];
 assertOperationHierarchy(tree);
 assert(tree.props["data-status"]===code,"account capability replaced current verdict: "+name);
 assert(content(operationClass(button,"operation-status-pill")[0])===operationVisibleLabel(label)&&button.props.className.includes("tone-"+tone),"verdict label/tone changed: "+name);
 const statusIcon=operationElements(button,icon)[0];
 assert(statusIcon&&Boolean(statusIcon.props.className?.includes("spin"))===spinning&&String(statusIcon.props["aria-hidden"])==="true","verdict icon/spinner changed: "+name);
 assert(button.props.title.includes(summary)&&button.props["aria-label"].includes(summary),"outcome context disappeared: "+name);
 assert(JSON.stringify(job)===before,"render mutated authoritative job: "+name);
}
assert(requests.length===0&&timers.size===0,"presenting statuses started independent work");
`)
}

func TestFrontendAccountOperationSeparatesAuthorizationFromSynchronization(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "account-operation")
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
const grant={id:"synthetic-sync",email:"synthetic-sync@example.invalid",status:"completed",lastOperationType:"relogin",canDownload:true,reloginAuthorizationSaved:true,uploadAfterRelogin:true};
for(const [upload,operation,code,label,tone,summary] of [
 ["running","running","relogin_upload_running","同步 Sub2API","active","新授权已保存"],
 ["succeeded","succeeded","relogin_upload_succeeded","授权并同步成功","success","同步至 Sub2API"],
 ["failed","succeeded","relogin_upload_failed","授权成功 · 同步失败","warning","新授权文件可下载"],
 ["unknown","succeeded","relogin_upload_unknown","授权成功 · 同步待核实","warning","同步结果待核实"],
 ["skipped","succeeded","relogin_upload_skipped","授权成功 · 同步未执行","warning","同步未执行"],
]){
 const tree=RepairUI.AccountOperationSummary({job:{...grant,operationStatus:operation,reloginUploadStatus:upload},onToggleLogs(){}}),button=operationClass(tree,"operation-status")[0];
 assertOperationHierarchy(tree);
 assert(tree.props["data-status"]===code&&content(operationClass(button,"operation-status-pill")[0])===operationVisibleLabel(label)&&button.props.className.includes("tone-"+tone),"sync outcome replaced authorization outcome: "+upload);
 assert(button.props.title.includes(summary),"sync outcome lost durable grant context: "+upload);
}
const totp={id:"synthetic-totp",email:"synthetic-totp@example.invalid",status:"failed",operationStatus:"failed",lastOperationType:"reset_2fa",canDownload:true,totpWorkflowStatus:"failed",totpWorkflowStep:"upload",totpResetStatus:"activated",totpResetPhase:"credential_persisted",logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:"succeeded",reloginAfterTotp:true,totpReloginStatus:"succeeded",uploadAfterTotp:true};
for(const [upload,label,detail] of [["failed","部分完成","新授权已保存 · Sub2API 同步失败"],["unknown","结果待核实","新授权已保存 · Sub2API 同步结果待核实"]]){
 const tree=RepairUI.AccountOperationSummary({job:{...totp,totpUploadStatus:upload},onToggleLogs(){}}),button=operationClass(tree,"operation-status")[0];
 assertOperationHierarchy(tree);
 assert(tree.props["data-status"]==="workflow_partial"&&content(operationClass(button,"operation-status-pill")[0])===operationVisibleLabel(label)&&button.props.className.includes("tone-warning"),"later TOTP step failure became total failure: "+upload);
 assert(button.props.title.includes(detail),"TOTP saved authorization disappeared: "+upload);
}
const interrupted=RepairUI.AccountOperationSummary({job:{...totp,operationStatus:"interrupted",totpWorkflowStatus:"interrupted",totpUploadStatus:"unknown"},onToggleLogs(){}});
assert(interrupted.props["data-status"]==="operation_interrupted"&&content(operationClass(interrupted,"operation-status-pill")[0])==="已中断","interruption became ordinary partial success");
assert(requests.length===0&&timers.size===0,"presenting failed synchronization retried it");
`)
}

func TestFrontendAccountOperationUsesSavedMethodAndAccessibleLogCallback(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "account-operation")
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
let opened=0;
const job={id:"synthetic-logs",email:"long.synthetic.account+layout@example.invalid",status:"completed"};
for(const [method,label,preference] of [["chatgpt","ChatGPT","bps"],["bps","BPS","chatgpt"],[undefined,"ChatGPT","bps"]]){
 storage.set("tosub2.authorization-method-v1",preference);
 const tree=RepairUI.AccountOperationSummary({job:{...job,authorizationMethod:method},onToggleLogs(){opened++;}}),saved=operationClass(tree,"operation-method")[0];
 assertOperationHierarchy(tree);
 assert(content(saved)===label&&saved.props.title==="此账号保存的授权方式："+label&&saved.props["aria-label"]==="授权方式："+label,"browser preference replaced saved account identity");
}
const summary="Synthetic full outcome. "+"Saved authorization remains available; remote synchronization is unknown. ".repeat(20);
const presentation={operation:{type:"relogin",label:"重登授权"},status:{code:"relogin_upload_unknown",label:"授权成功 · 同步待核实",tone:"warning",icon:"CircleAlert",spinning:false},summary};
for(const expanded of [false,true]){
 const tree=RepairUI.AccountOperationSummary({job,presentation,expanded,onToggleLogs(){opened++;}}),button=operationClass(tree,"operation-status")[0];
 assertOperationHierarchy(tree);
 assert(tree.props["data-status"]===presentation.status.code&&content(operationClass(button,"operation-status-pill")[0])===operationVisibleLabel(presentation.status.label),"supplied final presentation was replaced with raw job.status");
 assert(button.type==="button"&&button.props.type==="button"&&button.props["aria-expanded"]===expanded,"log disclosure lost native keyboard or expanded semantics");
 assert(button.props.title.includes(summary)&&button.props["aria-label"].includes(summary)&&button.props["aria-label"].includes(job.email),"full context/account identity became inaccessible");
 assert(button.props.title.includes(expanded?"收起执行日志":"查看执行日志"),"log action label contradicts expanded state");
 assert(operationClass(tree,"operation-name")[0].props.title==="重登授权"&&operationClass(button,"operation-status-pill").length===1,"operation context or compact status label missing");
 const before=opened;button.props.onClick();assert(opened===before+1,"status button did not delegate exactly once");
}
assert(requests.length===0&&timers.size===0,"status click performed an account request instead of delegating logs");
`)
	raw, err := os.ReadFile("../../web/src/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	declarations := func(selector string) string {
		t.Helper()
		match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
		if len(match) != 2 {
			t.Fatalf("missing operation style %q", selector)
		}
		return match[1]
	}
	name, method := declarations(".operation-name"), declarations(".operation-method")
	status, metadata := declarations(".account-pill.operation-status-pill"), declarations(".operation-summary-meta")
	if !strings.Contains(name, "font-size: 12px") || !strings.Contains(method, "font-size: 11px") || !strings.Contains(status, "min-height: 18px") || !strings.Contains(metadata, "flex-wrap: wrap") {
		t.Fatal("operation name, compact status and saved method lost their readable relative hierarchy")
	}
	if !strings.Contains(status, "height: auto") || !strings.Contains(status, "white-space: normal") || strings.Contains(status, "text-overflow") || strings.Contains(status, "overflow: hidden") {
		t.Fatal("compound authorization/synchronization outcomes must wrap without clipping their meaning")
	}
}

func TestFrontendAccountOperationHintOnlyTrimsExactTerminalPrefix(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "account-operation")
	frontendPollingRun(t, vm, `
const label="重登授权";
for(const [ending,tail] of [
 ["已完成，","授权文件可下载"],
 ["未完成，","已有授权文件仍可下载"],
 ["已取消，","已取得授权可本地补保存"],
 ["已中断，","已有授权文件仍可下载；已取得授权可本地补保存"],
]){
 const summary=label+ending+tail,presentation=Object.freeze({terminal:true,operation:Object.freeze({label}),summary});
 assert(RepairUI.operationExecutionHint(presentation)===tail,"terminal simplification lost historical authorization guidance: "+ending);
 assert(presentation.summary===summary,"simplification mutated the full diagnostic summary");
 const emptyTail={...presentation,summary:label+ending};
 assert(RepairUI.operationExecutionHint(emptyTail)===emptyTail.summary,"terminal simplification returned an empty summary: "+ending);
 assert(RepairUI.operationExecutionHint({...presentation,terminal:false})===summary,"active or waiting operation was simplified: "+ending);
}
for(const presentation of [
 {terminal:false,operation:{label},summary:"正在重登授权，等待远端确认"},
 {terminal:false,operation:{label},summary:"重登授权等待输入，请完成当前验证"},
 {terminal:true,operation:{label:"重置 2FA"},summary:"2FA 已安全保存 · 新授权已保存 · Sub2API 同步失败"},
 {terminal:true,operation:{label:"重置 2FA"},summary:"新授权已保存 · Sub2API 同步结果待核实"},
 {terminal:false,operation:{label},summary:"状态待确认，请核实操作结果"},
 {terminal:true,operation:{label},summary:"授权已完成，授权文件可下载"},
 {terminal:true,operation:{label},summary:"重登授权已完成；授权文件可下载"},
 {terminal:true,operation:{label},summary:"此前重登授权已完成，授权文件可下载"},
 {terminal:true,operation:{label},summary:"重登授权已完成"},
]){
 assert(RepairUI.operationExecutionHint(presentation)===presentation.summary,"nonmatching workflow, uncertainty or diagnostic summary was simplified: "+presentation.summary);
}
assert(requests.length===0&&timers.size===0,"formatting an execution hint started independent work");
`)
}

func TestFrontendAccountOperationRowRetainsHistoryAndColumnAlignment(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
const job={id:"synthetic-history",email:"synthetic-history@example.invalid",authorizationMethod:"bps",status:"completed",operationStatus:"failed",operationError:"Synthetic current password failure",lastOperationType:"add_password",canDownload:true,canResume:false,totpSetupError:"Synthetic previous 2FA failure",mailApiError:"Synthetic previous mail failure"};
let toggles=0;
for(const qualityAvailable of [false,true]){
 hookCursor=0;
 const props={job,token:"synthetic",qualityAvailable,onToggleLogs(){toggles++;}},row=RepairUI.JobRow(props),cells=operationElements(row,"td"),count=qualityAvailable?9:8;
 const operation=operationNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountOperationSummary");
 assert(cells.length===count&&cells[2].props.className==="usage-cell"&&cells[count-5].props.className==="expiration-cell"&&cells[count-4].props.className==="step-cell"&&cells[count-3].props.className==="activity-time-cell"&&cells[count-2].props.className==="operation-type-cell"&&cells[count-1].props.className==="actions-cell","current operation, time and type are not the final three information columns");
 assert(operation.length===1&&findMatching(cells[count-2],node=>node===operation[0]),"operation verdict duplicated or outside its moved column");
 assert(operationClass(row,"quality-cell").length===Number(qualityAvailable)&&operationNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountQualityStatus").length===Number(qualityAvailable)&&!operationNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountQualitySummary").length,"probe column lost its complete state or mounted removed history blocks");
 assert(!operationClass(row,"status-cell").length&&operationClass(row,"step-cell").length===1&&!operationNodes(row,node=>typeof node.type==="function"&&node.type.name==="StatusBadge").length,"current-operation cell disappeared or duplicate legacy status returned");
 assert(row.type==="tr"&&operationElements(row,"tr").length===1&&!operationClass(row,"job-execution-row").length,"current operation moved into a separate disclosure row");
 const view=operation[0].props.presentation;
 assert(view.status.code==="operation_failed"&&view.authorization.available&&view.summary.includes("已有授权文件仍可下载"),"failed current action erased the retained authorization");
 const execution=cells[count-4];
 const hint=operationClass(execution,"prompt-line")[0];
 assert(content(hint)==="已有授权文件仍可下载"&&hint.props.title===view.summary,"row hint lost retained authorization or its complete diagnostic title");
 assert(view.notices.some(notice=>notice.key==="operation")&&view.historyNotices.some(notice=>notice.key==="old-totp-setup")&&view.historyNotices.some(notice=>notice.key==="old-mail"),"current and historical diagnostics collapsed together");
 const notices=operationNodes(execution,node=>typeof node.type==="function"&&node.type.name==="JobNotice");
 assert(notices.some(node=>node.props.notice.key==="operation"&&JSON.stringify(node.props.notice).includes(job.operationError))&&notices.some(node=>node.props.notice.key==="old-totp-setup"&&JSON.stringify(node.props.notice).includes(job.totpSetupError))&&notices.some(node=>node.props.notice.key==="old-mail"&&JSON.stringify(node.props.notice).includes(job.mailApiError)),"moved current-operation cell dropped current or historical error detail");
 const history=operationClass(execution,"job-history-notices")[0];
 assert(history?.type==="details"&&!history.props.open&&find(history,"summary"),"historical errors lost their accessible collapsed entry");
 hookCursor=0;const expanded=RepairUI.JobRow({...props,expanded:true}),expandedCells=operationElements(expanded,"td");
 assert(expanded.type==="tr"&&expandedCells.length===count&&expandedCells[count-4].props.className==="step-cell"&&operationClass(expandedCells[count-4],"prompt-line").length===1&&operationNodes(expandedCells[count-4],node=>typeof node.type==="function"&&node.type.name==="JobNotice").length===notices.length,"log expansion hid or moved the permanent current-operation content");
 assert(findMatching(row,node=>node.type==="button"&&node.props["aria-label"]==="下载授权文件 "+job.email),"history-preserving failure hid the existing authorization download");
 const button=operationClass(operation[0].type(operation[0].props),"operation-status")[0];button.props.onClick();
 assert(toggles===(qualityAvailable?2:1),"row did not forward the existing log callback");
 for(const filtered of [false,true]){
  const empty=RepairUI.EmptyState({filtered,columnCount:count});
  assert(Number(find(empty,"td").props.colSpan)===count,"empty/filtered table stopped spanning every column");
 }
}
assert(requests.length===0,"row integration retried a failed action");
`)
}

func TestFrontendAccountOperationAppHeadersEmptyStateAndLogsAlign(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
location.hash="#accounts";
let tree=render();
const job={id:"synthetic-table",email:"synthetic-table@example.invalid",authorizationMethod:"chatgpt",status:"completed",operationStatus:"succeeded",canDownload:true};
for(const qualityAvailable of [false,true]){
 appState.setFeatures({bulkActions:true,accountQuality:qualityAvailable});appState.setJobs([]);appState.setJobSelectionIndex([]);tree=render();
 const headers=operationElements(find(tree,"thead"),"th"),count=qualityAvailable?9:8;
 const expected=["select-heading","account-heading","usage-heading",...(qualityAvailable?["quality-heading"]:[]),"expiration-heading","current-operation-heading","activity-time-heading","operation-type-heading","actions-heading"];
 assert(headers.length===count&&headers.map(header=>header.props.className).join()===expected.join()&&content(headers[count-4])==="当前操作"&&content(headers[count-3])==="操作时间"&&content(headers[count-2])==="操作类型","header order differs from the requested final operation columns");
 assert(!headers.some(header=>header.props.className==="status-heading"||content(header)==="状态"),"duplicate standalone status header returned");
 assert(operationClass(tree,"quality-heading").length===Number(qualityAvailable)&&findMatching(tree,node=>node.type==="table"&&node.props.className==="accounts-table").props["data-quality"]===qualityAvailable,"probe column header or layout flag stopped following its feature gate");
 assert(find(tree,"EmptyState").props.columnCount===count,"App passed the wrong empty-state colspan");
 appState.setJobs([job]);appState.setJobSelectionIndex([job]);tree=render();
 let row=find(tree,"JobRow");assert(row.props.qualityAvailable===qualityAvailable&&!row.props.expanded,"row feature or initial disclosure state differs from header");
 const before=requests.length;row.props.onToggleLogs();tree=render();row=find(tree,"JobRow");
 const logRow=findMatching(tree,node=>node.type==="tr"&&node.props.className==="log-row");
 assert(row.props.expanded&&logRow&&Number(find(logRow,"td").props.colSpan)===count&&find(logRow,"JobLogs").props.jobId===job.id,"existing log disclosure no longer spans this account's table");
 row.props.onToggleLogs();tree=render();assert(!findMatching(tree,node=>node.type==="tr"&&node.props.className==="log-row")&&!find(tree,"JobRow").props.expanded,"second click did not close the existing log disclosure");
 assert(requests.length===before,"table disclosure submitted an account mutation");
}
`)
}

func TestFrontendAccountOperationMovedCurrentCellRetainsInputActions(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendAccountOperationTreeQueries)
	frontendPollingRun(t, vm, `
const cases=[
 ["password","输入账号密码","提交密码","text","password","current-password","synthetic-password"],
 ["mfa_otp","6 位 2FA 验证码","提交 2FA 验证码","numeric","text","one-time-code","123456"],
 ["totp_setup_otp","新 2FA 的 6 位验证码","激活新的 2FA","numeric","text","one-time-code","234567"],
 ["email_otp","6 位邮箱验证码","提交邮箱验证码","numeric","text","one-time-code","345678"],
 ["phone","+60123456789","发送手机验证码","tel","text","one-time-code","+15555550123"],
 ["phone_otp","+15555550123 的验证码","提交手机验证码","numeric","text","one-time-code","456789"],
];
for(const qualityAvailable of [false,true]) for(const [status,placeholder,submitLabel,inputMode,type,autoComplete,value] of cases){
 hookSlots=[];hookCursor=0;
 const job={id:"synthetic-input-"+status,email:"synthetic-input@example.invalid",status,operationStatus:"waiting",lastOperationType:status==="totp_setup_otp"?"setup_2fa":"relogin",currentPhone:"+15555550123",prompt:"Synthetic verification input remains actionable."};
 const props={job,token:"synthetic",expanded:false,qualityAvailable,smsProviderAvailable:true,smsProvider:{id:"synthetic-provider",name:"合成平台",ready:true,config:{}},onError(){}};
 const count=qualityAvailable?9:8,before=requests.length,row=RepairUI.JobRow(props),cells=operationElements(row,"td"),current=cells[count-4];
 assert(row.type==="tr"&&cells.length===count&&current.props.className==="step-cell"&&operationClass(row,"step-cell").length===1&&!operationClass(row,"job-execution-row").length,"input workflow moved behind a disclosure or escaped the current-operation column: "+status);
 const input=findMatching(current,node=>node.type==="input"&&node.props["aria-label"]===placeholder),submit=findMatching(current,node=>node.type==="button"&&node.props.title===submitLabel);
 assert(input&&input.props.inputMode===inputMode&&input.props.type===type&&input.props.autoComplete===autoComplete&&input.props.value===""&&submit?.props.disabled===true,"moved verification field lost its accessible label, private input type or empty-submit guard: "+status);
 assert(operationClass(current,"prompt-line")[0].props.title.includes(job.prompt)&&requests.length===before,"rendering verification changed its full prompt or submitted work: "+status);
 if(status==="email_otp")assert(findButton(current,"重发")&&!findButton(current,"重发").props.disabled,"email verification lost explicit resend");
 if(status==="phone_otp")assert(findButton(current,"重发")&&findButton(current,"换号")&&operationClass(current,"phone-target").some(node=>content(node).includes(job.currentPhone)),"phone verification lost its target, resend or change-number control");
 if(status==="phone")assert(findButton(current,"合成平台取号")&&!findButton(current,"合成平台取号").props.disabled,"phone input lost the configured synthetic provider action");
 input.props.onChange({target:{value}});hookCursor=0;const filled=RepairUI.JobRow(props),filledCurrent=operationElements(filled,"td")[count-4],filledInput=findMatching(filledCurrent,node=>node.type==="input"&&node.props["aria-label"]===placeholder);
 assert(filledInput.props.value===value&&!findMatching(filledCurrent,node=>node.type==="button"&&node.props.title===submitLabel).props.disabled,"valid user input did not remain editable after column movement");
 filledInput.props.onKeyDown({key:"Enter"});
 assert(requests.length===before+1&&requests[before].url==="/api/jobs/"+job.id+"/input"&&requests[before].options.method==="POST","keyboard input did not use the existing single submission endpoint");
 const body=JSON.parse(requests[before].options.body);assert(body.action===status&&body.value===value,"column movement changed the verification action or user-supplied value");
 hookCursor=0;const busy=RepairUI.JobRow(props),busyCurrent=operationElements(busy,"td")[count-4];
 assert(findMatching(busyCurrent,node=>node.type==="button"&&node.props.title===submitLabel).props.disabled,"in-flight verification became resubmittable");
 findMatching(busyCurrent,node=>node.type==="input"&&node.props["aria-label"]===placeholder).props.onKeyDown({key:"Enter"});
 assert(requests.length===before+1,"repeated keyboard input submitted while the first attempt remained pending");
 hookCursor=0;const terminal=RepairUI.JobRow({...props,job:{...job,operationStatus:"succeeded"}});
 assert(!operationClass(terminal,"inline-entry").length&&!operationClass(terminal,"totp-setup-secret").length,"completed operation retained active input or setup secret controls");
}
assert(requests.length===12,"input discovery or column movement emitted an unexpected request");
`)
}
