package console

import "testing"

const frontendCPASetup = `
location.hash = "#settings"; render();
appState.setToken("synthetic-console-token");
appState.setFeatures({cpaUpload:true,sub2apiUpload:true,reloginAutoUpload:true,forceRelogin:true,totpReset:true,totpSetup:true,bulkActions:true});
function cpaState() { return find(render(), "ConfigurationPage").props.cpa; }
function respondCPA(index, data, status = 200) { requests[index].resolve({ok:status<400,status,json:async()=>data}); }
const cpaReady = {baseUrl:"https://cpa.synthetic.invalid",configured:true,hasApiKey:true,credentialPersistence:"secure",revision:1};
`

func TestFrontendCPAPreferenceDefaultsAndNeverStoresConfiguration(t *testing.T) {
	vm := frontendBrowserVM(t, "uploadTarget")
	frontendPollingRun(t, vm, `
var storage=new Map(),fail=false;var window={localStorage:{getItem:key=>storage.get(key),setItem(key,value){if(fail)throw new Error("denied");storage.set(key,value)}}};
assert(Browser.readUploadTargetPreference()==="sub2api","legacy default changed");
Browser.saveUploadTargetPreference("cpa");assert(Browser.readUploadTargetPreference()==="cpa","CPA choice did not persist");
Browser.saveUploadTargetPreference({adminApiKey:"private"});assert(Browser.readUploadTargetPreference()==="sub2api"&&!JSON.stringify([...storage]).includes("private"),"preference stored non-target data");
fail=true;assert(!Browser.saveUploadTargetPreference("cpa"),"storage failure was hidden");
assert(Browser.uploadTargetConflict("cpa","bps")&&!Browser.uploadTargetConflict("sub2api","bps"),"authorization compatibility incorrect");
`)
}

func TestFrontendCPASettingsSaveKeepsSecretsOutOfBrowserStorage(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();`)
	frontendPollingRun(t, vm, `assert(requests.length===1&&requests[0].url==="/api/cpa/settings","settings read missing");respondCPA(0,{...cpaReady,configured:false,hasApiKey:false,baseUrl:"",adminApiKey:"unexpected-secret"});`)
	frontendPollingRun(t, vm, `
assert(cpaState().loaded&&!cpaState().settings.adminApiKey&&!cpaState().draft.adminApiKey,"read exposed a returned secret");
cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"synthetic-management-secret"});
find(render(),"ConfigurationPage").props.onSectionChange("cpa");
// Opening a category resets prior drafts; edit the actual CPA form now.
cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"synthetic-management-secret"});
cpaState().save({preventDefault(){}});cpaState().save({preventDefault(){}});
assert(requests.length===2,"save double-submitted");
const body=JSON.parse(requests[1].options.body);assert(body.adminApiKey==="synthetic-management-secret"&&body.revision===1,"save lost explicit secret or revision");
assert(!JSON.stringify([...storage]).includes("synthetic-management-secret"),"secret reached localStorage");
respondCPA(1,{...cpaReady,revision:2,credentialPersistence:"memory_only"});
`)
	frontendPollingRun(t, vm, `
assert(cpaState().configured&&cpaState().draft.adminApiKey===""&&cpaState().settings.credentialPersistence==="memory_only","successful save retained key or lost persistence status");
cpaState().setDraft({baseUrl:"https://other.synthetic.invalid",adminApiKey:""});cpaState().save();
assert(requests.length===2&&cpaState().error.includes("更换"),"new address reused prior key");
cpaState().setDraft({baseUrl:"",adminApiKey:"discard-me"});cpaState().save();
const clear=JSON.parse(requests[2].options.body);assert(clear.baseUrl===""&&clear.adminApiKey===""&&clear.revision===2,"clear resent secret or stale revision");
`)
}

func TestFrontendCPAUnknownSaveRequiresExplicitReadBeforeAnotherWrite(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"synthetic-new-secret"});cpaState().save();respondCPA(1,{error:"synthetic uncertain save"},503);`)
	frontendPollingRun(t, vm, `
assert(cpaState().uncertain&&!cpaState().configured&&cpaState().draft.adminApiKey==="","uncertain configuration stayed usable or retained submitted secret");
cpaState().save();assert(requests.length===2,"uncertain save replayed automatically");
cpaState().reload();assert(requests.length===3&&(!requests[2].options.method||requests[2].options.method==="GET"),"verification wrote configuration");respondCPA(2,{...cpaReady,revision:2});
`)
	frontendPollingRun(t, vm, `assert(!cpaState().uncertain&&cpaState().configured&&requests.length===3,"verification automatically repeated save");cpaState().save();assert(JSON.parse(requests[3].options.body).revision===2,"verified revision unused");`)
}

func TestFrontendCPAReadDiagnosticAllowsExplicitCredentialRepair(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,{...cpaReady,configured:false,hasApiKey:false,error:"安全存储凭据暂不可读"});`)
	frontendPollingRun(t, vm, `
assert(!cpaState().uncertain&&!cpaState().configured&&cpaState().error.includes("暂不可读"),"startup diagnostic became a permanent write lock");
cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"explicit-repair-key"});cpaState().save();
assert(requests.length===2&&JSON.parse(requests[1].options.body).adminApiKey==="explicit-repair-key","explicit credential repair was blocked");
`)
}

func TestFrontendCPAConfirmedSaveFailureAllowsExplicitRetry(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"explicit-retry-key"});cpaState().save();respondCPA(1,{error:"保存前已确定失败",outcome:"failed",uncertain:false},500);`)
	frontendPollingRun(t, vm, `
assert(!cpaState().uncertain&&cpaState().error==="保存前已确定失败"&&requests.length===2,"confirmed save failure became unknown or auto-retried");
cpaState().save();assert(requests.length===3&&JSON.parse(requests[2].options.body).adminApiKey==="explicit-retry-key","explicit retry lost unsaved secret");
`)
}

func TestFrontendCPASettingsDraftGuardsNavigationAndClearsDiscardedSecret(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("cpa");cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"unsaved-private"});
find(render(),"AppShell").props.onNavigate("accounts");
assert(findButton(render(),"继续编辑"),"secret draft lost navigation guard");
findButton(render(),"放弃更改并离开").props.onClick();
find(render(),"AppShell").props.onNavigate("settings");
assert(cpaState().draft.adminApiKey===""&&requests.length===1,"discarded key retained or navigation wrote it");
`)
}

func TestFrontendCPAManualUploadRoutesWithoutSub2Secrets(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `
const job={id:"cpa-job",email:"cpa@example.invalid",status:"completed",canDownload:true,authorizationMethod:"chatgpt"};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setSelectedJobIds(new Set([job.id]));
appState.setUploadTarget("cpa");
let upload=findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="选择上传目标");assert(upload&&!upload.props.disabled,"CPA manual upload unavailable");upload.props.onClick();
assert(requests.length===1,"manual upload bypassed destination choice");find(render(),"ManualUploadDialog").props.onSubmit();
assert(requests[1].url==="/api/cpa/upload"&&JSON.stringify(JSON.parse(requests[1].options.body))===JSON.stringify({ids:[job.id]}),"CPA request leaked Sub2 settings or credentials");
respondCPA(1,{ok:true,outcome:"completed",selected:1,uploaded:1,skipped:0,result:{created:1,updated:0,success:1}});
`)
	frontendPollingRun(t, vm, `
assert(content(render()).includes("CPA · 同步已完成"),"completion lost target");
appState.setJobSelectionIndex([{...job,authorizationMethod:"bps"}]);appState.setSelectedJobIds(new Set([job.id]));
findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="选择上传目标").props.onClick();
find(render(),"ManualUploadDialog").props.onSubmit();
assert(requests.length===2&&content(render()).includes("取消选择 BPS"),"CPA sent incompatible BPS grant");
`)
}

func TestFrontendCPAPartialReceiptRetainsOnlyUnconfirmedSelections(t *testing.T) {
	for _, status := range []int{200, 502} {
		vm := frontendAppVM(t)
		frontendPollingRun(t, vm, frontendCPASetup)
		frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
		if err := vm.Set("uploadStatus", status); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `
const outcomes=["succeeded","failed","unknown","skipped"];
const jobs=outcomes.map(id=>({id,email:id+"@example.invalid",status:"completed",canDownload:true,authorizationMethod:"chatgpt"}));
appState.setJobs(jobs);appState.setJobSelectionIndex(jobs);appState.setSelectedJobIds(new Set(outcomes));
appState.setUploadTarget("cpa");
findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="选择上传目标").props.onClick();
find(render(),"ManualUploadDialog").props.onSubmit();
assert(!find(render(),"UploadTargetPicker"),"manual upload page reintroduced a global destination selector");
respondCPA(1,{ok:false,outcome:"partial",uncertain:true,uploaded:1,result:{success:1},results:outcomes.map(id=>({id,outcome:id}))},uploadStatus);
`)
		frontendPollingRun(t, vm, `
for(const id of outcomes){const row=findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id===id);assert(row.props.selected===(id!=="succeeded"),"receipt lost unconfirmed selection or retained success: "+id);}
assert(!find(render(),"UploadTargetPicker"),"receipt reintroduced a global destination selector");
findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="选择上传目标").props.onClick();
find(render(),"ManualUploadDialog").props.onSubmit();
assert(JSON.stringify(JSON.parse(requests[2].options.body).ids)===JSON.stringify(["failed","unknown","skipped"]),"explicit retry included confirmed success");
`)
	}
}

func TestFrontendCPASelectionRequiresValidPerAccountReceipts(t *testing.T) {
	vm := frontendBrowserVM(t, "uploadTarget")
	frontendPollingRun(t, vm, `
for(const receipt of [
 {uploaded:1,result:{success:1}},
 {results:[{id:"outside",outcome:"succeeded"}]},
 {results:[{id:"one",outcome:"succeeded"},{id:"one",outcome:"failed"}]},
 {results:[{id:"one",outcome:"succeeded"},{id:"two",outcome:"invalid"}]},
 {results:[{id:"one",outcome:"succeeded"},null]}
])assert(Browser.confirmedCPAUploadIds(receipt,["one","two"]).length===0,"invalid or aggregate-only receipt cleared selection");
assert(JSON.stringify(Browser.confirmedCPAUploadIds({results:[{id:"one",outcome:"succeeded"},{id:"two",outcome:"unknown"}]},["one","two"]))==='["one"]',"valid receipt did not select exactly confirmed IDs");
`)
}

func TestFrontendCPAWorkflowRequestsCarryOnlyDestination(t *testing.T) {
	for _, workflow := range []string{"relogin", "totp"} {
		t.Run(workflow, func(t *testing.T) {
			vm := frontendAppVM(t)
			frontendPollingRun(t, vm, frontendCPASetup)
			frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
			if err := vm.Set("workflowKind", workflow); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
const job={id:"cpa-job",email:"cpa@example.invalid",status:"completed",canDownload:true,canForceRelogin:true,canResetTotp:true,authorizationMethod:"chatgpt"};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setSelectedJobIds(new Set([job.id]));
appState.setUploadTarget("cpa");
if(workflowKind==="relogin")find(render(),"JobRow").props.onRelogin();else find(render(),"JobRow").props.onSetupTotp(true,false);
var dialog=find(render(),workflowKind==="relogin"?"ReloginActionDialog":"TotpActionDialog");
var failures=[];dialog.props.onSubmit({uploadTarget:"cpa",authorizationMethod:"bps",uploadAfterRelogin:true,uploadAfterTotp:true,reloginAfterTotp:true}).catch(error=>failures.push(error));
`)
			frontendPollingRun(t, vm, `
assert(requests.length===1&&failures.length===1&&failures[0].message.includes("CPA"),"BPS automatic upload was not rejected");
dialog.props.onSubmit({uploadTarget:"cpa",authorizationMethod:"chatgpt",uploadAfterRelogin:true,uploadAfterTotp:true,reloginAfterTotp:true}).catch(error=>failures.push(error));
const body=JSON.parse(requests[1].options.body);assert(body.uploadTarget==="cpa"&&!body.sub2apiConfig&&!body.cpaConfig&&!JSON.stringify(body).includes("synthetic-test-key"),"workflow sent the wrong target or credentials");
assert(workflowKind==="relogin"?body.uploadAfterRelogin:body.uploadAfterTotp&&body.reloginAfterTotp,"automatic push flag missing");
`)
		})
	}
}

func TestFrontendCPAHistoryUsesRecordedDestination(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const base={status:"completed",operationStatus:"succeeded",lastOperationType:"relogin",canDownload:true,uploadAfterRelogin:true,reloginAuthorizationSaved:true,reloginUploadStatus:"succeeded"};
assert(Browser.describeJob({...base,uploadTarget:"cpa"}).summary.includes("CPA"),"CPA success was labeled Sub2");
assert(Browser.describeJob(base).summary.includes("Sub2API"),"old job default changed");
const unknown=Browser.describeJob({...base,uploadTarget:"cpa",reloginUploadStatus:"unknown"});
assert(unknown.summary.includes("CPA")&&unknown.notices.some(note=>note.text.includes("CPA")),"CPA unknown lost destination or verification guidance");
assert(Browser.describeUpload({}, {running:true,target:"cpa"}).label.includes("CPA"),"CPA active label wrong");
const receipt=Browser.describeUpload({ok:true,result:{success:2,created:0,updated:0}},{target:"cpa"});assert(receipt.text==="确认推送 2 条","CPA completion invented create/update counts");
const pending=Browser.describeUpload({ok:false,uncertain:true,result:{success:0,created:0,updated:0}},{target:"cpa"});assert(pending.text.includes("核实 CPA 授权文件")&&!pending.text.includes("重复创建"),"unknown CPA result gave Sub2 recovery advice");
const workflow=Browser.describeWorkflow({lastOperationType:"reset_2fa",totpWorkflowStatus:"running",totpWorkflowStep:"upload",uploadTarget:"cpa",uploadAfterTotp:true});assert(workflow.currentStep.label==="上传 CPA","2FA step hardcoded Sub2");
`)
}

func TestFrontendCPAWorkflowDialogsBlockIncompatibleAuthorization(t *testing.T) {
	for _, component := range []string{"relogin-action-dialog", "accounts"} {
		t.Run(component, func(t *testing.T) {
			vm := frontendAutoRepairComponentVM(t, component)
			if err := vm.Set("componentName", component); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
storage.set("tosub2.relogin-workflow-preferences-v1",JSON.stringify({uploadAfterRelogin:true}));
storage.set("tosub2.totp-workflow-preferences-v1",JSON.stringify({reloginAfterTotp:true,uploadAfterTotp:true}));
hookSlots=[];hookCursor=0;var submissions=[];
const props={action:{email:"cpa@example.invalid",authorizationMethod:"bps",resetTotp:true},uploadTarget:"cpa",uploadAvailable:true,uploadConfigured:true,reloginAvailable:true,logoutAvailable:true,onSubmit:async value=>submissions.push(value),onClose(){}};
const dialog=componentName==="accounts"?RepairUI.TotpActionDialog(props):RepairUI.ReloginActionDialog(props);
assert(content(dialog).includes("CPA 仅支持")&&findMatching(dialog,node=>node.type==="button"&&node.props.type==="submit").props.disabled,"BPS conflict was not visible or submit remained enabled");
findMatching(dialog,node=>node.type==="form").props.onSubmit({preventDefault(){}});
assert(submissions.length===0,"BPS dialog submitted automatic CPA upload");
`)
		})
	}
}

func TestFrontendCPASettingsShowsMemoryOnlyKeyAndExplicitVerification(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "cpa-settings")
	frontendPollingRun(t, vm, `
var reads=0;const cpa={loaded:true,draft:{baseUrl:"http://127.0.0.1:8317",adminApiKey:""},settings:{hasApiKey:true,credentialPersistence:"memory_only"},uncertain:true,error:"保存待核实",reload(){reads++},save(){},setDraft(){}};
const tree=RepairUI.CPASettings({cpa});
assert(content(tree).includes("仅本次服务运行有效")&&content(tree).includes("服务重启后"),"temporary credential persistence hidden");
assert(findMatching(tree,node=>node.type==="button"&&node.props.type==="submit").props.disabled,"unknown save allowed another write");
findButton(tree,"读取当前配置").props.onClick();assert(reads===1,"explicit verification not wired");
const key=findMatching(tree,node=>node.type==="input"&&node.props.type==="password");assert(key.props.value===""&&key.props.autoComplete==="new-password","saved key reappeared in form");
`)
}
