package console

import "testing"

func TestFrontendManualUploadChoosesDestinationBeforeSending(t *testing.T) {
	for _, entry := range []string{"row", "batch"} {
		for _, target := range []string{"sub2api", "cpa"} {
			t.Run(entry+"/"+target, func(t *testing.T) {
				vm := frontendAppVM(t)
				vm.Set("entry", entry)
				vm.Set("destination", target)
				frontendPollingRun(t, vm, frontendCPASetup)
				frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
				frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const jobs=["first","second"].map(id=>({id,email:id+"@example.invalid",status:"completed",canDownload:true,authorizationMethod:"chatgpt"}));
appState.setJobs(jobs);appState.setJobSelectionIndex(jobs);appState.setSelectedJobIds(new Set(jobs.map(job=>job.id)));
const open=entry==="row"?find(render(),"JobRow").props.onUpload:findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="选择上传目标").props.onClick;
open();open();
let dialog=find(render(),"ManualUploadDialog");
assert(dialog&&requests.length===1,"manual upload sent before choosing a destination");
assert(JSON.stringify(dialog.props.action.ids)===JSON.stringify(entry==="row"?["first"]:["first","second"]),"dialog lost its original account scope");
dialog.props.onTargetChange(destination);
appState.setSelectedJobIds(new Set(["second"]));appState.setUploadTarget(destination==="cpa"?"sub2api":"cpa");
dialog=find(render(),"ManualUploadDialog");
assert(dialog.props.action.target===destination,"unrelated preference changed this upload destination");
const submit=dialog.props.onSubmit;submit();submit();
assert(requests.length===2&&requests[1].url==="/api/"+destination+"/upload","confirmation sent a duplicate or wrong-destination request");
const body=JSON.parse(requests[1].options.body);
assert(JSON.stringify(body.ids)===JSON.stringify(entry==="row"?["first"]:["first","second"]),"selection changes retargeted the open upload");
assert(destination!=="cpa"||Object.keys(body).join() === "ids","CPA upload received another provider's credentials");
assert(!find(render(),"ManualUploadDialog"),"submitted dialog remained actionable");
respondCPA(1,{ok:true,outcome:"completed",result:{created:destination==="cpa"?0:body.ids.length,updated:0,success:body.ids.length},results:body.ids.map(id=>({id,outcome:"succeeded"}))});
`)
				frontendPollingRun(t, vm, `
submit();assert(requests.length===2,"stale dialog callback replayed a completed upload");
assert(content(render()).includes((destination==="cpa"?"CPA":"Sub2API")+" · 同步已完成"),"receipt lost the confirmed destination");
if(entry==="row")assert(findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id==="second").props.selected,"upload receipt cleared an unrelated selected account");
`)
			})
		}
	}
}

func TestFrontendManualUploadRechecksPendingAccount(t *testing.T) {
	for _, change := range []string{"missing", "recovery", "unavailable", "bps"} {
		t.Run(change, func(t *testing.T) {
			vm := frontendAppVM(t)
			vm.Set("accountChange", change)
			frontendPollingRun(t, vm, frontendCPASetup)
			frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
			frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"first",email:"first@example.invalid",status:"completed",canDownload:true,authorizationMethod:"chatgpt"};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setUploadTarget("cpa");
find(render(),"JobRow").props.onUpload();
const changed=accountChange==="missing"?[]:[{...job,...(accountChange==="recovery"?{totpRecoveryPending:true}:accountChange==="unavailable"?{canDownload:false}:{authorizationMethod:"bps"})}];
appState.setJobs(changed);appState.setJobSelectionIndex(changed);
const dialog=find(render(),"ManualUploadDialog");assert(dialog.props.problem,"pending account change left confirmation enabled");
dialog.props.onSubmit();assert(requests.length===1,"confirmation bypassed current account safety checks");
`)
		})
	}
}

func TestFrontendManualUploadCancelPreservesSelectionAndPreference(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const jobs=["first","second"].map(id=>({id,email:id+"@example.invalid",status:"completed",canDownload:true}));
appState.setJobs(jobs);appState.setJobSelectionIndex(jobs);appState.setSelectedJobIds(new Set(["second"]));
find(render(),"JobRow").props.onUpload();
let dialog=find(render(),"ManualUploadDialog");assert(dialog,"row upload has no destination dialog");
dialog.props.onTargetChange("cpa");dialog=find(render(),"ManualUploadDialog");
const staleSubmit=dialog.props.onSubmit;dialog.props.onClose();staleSubmit();
assert(!find(render(),"ManualUploadDialog")&&requests.length===0,"cancel sent an upload");
assert(!storage.has("tosub2.upload-target-v1"),"cancel persisted an unconfirmed destination");
assert(findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id==="second").props.selected,"cancel cleared selection");
findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id==="second").props.onUpload();
assert(find(render(),"ManualUploadDialog").props.action.ids.join()==="second","new dialog reused canceled accounts");
`)
}

func TestFrontendManualUploadCPARequiresKnownConfiguration(t *testing.T) {
	for _, state := range []string{"unread", "loading", "failed-read", "unconfigured", "uncertain-save"} {
		t.Run(state, func(t *testing.T) {
			vm := frontendAppVM(t)
			vm.Set("configurationState", state)
			frontendPollingRun(t, vm, frontendCPASetup)
			frontendPollingRun(t, vm, `
if(configurationState!=="unread") { cpaState().reload(); if(configurationState!=="loading") respondCPA(0,configurationState==="unconfigured"?{...cpaReady,configured:false,hasApiKey:false,baseUrl:""}:cpaReady); }
`)
			frontendPollingRun(t, vm, `
if(configurationState==="failed-read") {cpaState().reload();requests[1].reject(new Error("synthetic read failure"));}
if(configurationState==="uncertain-save") {cpaState().setDraft({baseUrl:cpaReady.baseUrl,adminApiKey:"synthetic-private-repair"});cpaState().save();requests[1].reject(new Error("synthetic lost save"));}
`)
			frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"first",email:"first@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setUploadTarget("cpa");
find(render(),"JobRow").props.onUpload();
let dialog=find(render(),"ManualUploadDialog");
assert(dialog.props.action.target==="cpa"&&!dialog.props.targets.cpa.ready&&dialog.props.problem,"unknown/unconfigured destination remained actionable or silently switched");
const before=requests.length;dialog.props.onSubmit();
assert(requests.length===before&&!requests.some(r=>r.url.endsWith("/upload")),"configuration failure triggered an upload");
if(configurationState==="loading")respondCPA(0,cpaReady);else {dialog.props.onReloadCPA();respondCPA(before,cpaReady);}
`)
			frontendPollingRun(t, vm, `
dialog=find(render(),"ManualUploadDialog");
assert(!dialog.props.problem&&dialog.props.targets.cpa.ready,"explicit configuration read did not enable the selected destination");
assert(!requests.some(r=>r.url.endsWith("/upload")),"configuration read automatically uploaded");
dialog.props.onSubmit();assert(requests.filter(r=>r.url==="/api/cpa/upload").length===1,"explicit confirmation did not send exactly one CPA upload");
assert(!JSON.stringify([...storage]).includes("synthetic-private-repair"),"destination preference stored a credential");
`)
		})
	}
}

func TestFrontendManualUploadMissingConfigurationOffersCorrectSettings(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `storage.delete("chatgpt-onboarding.sub2api-upload-settings-v1");`)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,{...cpaReady,configured:false,hasApiKey:false,baseUrl:""});`)
	frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"first",email:"first@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);
find(render(),"JobRow").props.onUpload();
let dialog=find(render(),"ManualUploadDialog");assert(dialog.props.problem&&!dialog.props.targets.sub2api.ready&&!dialog.props.targets.cpa.ready,"empty configuration looked ready");
dialog.props.onSubmit();dialog.props.onTargetChange("cpa");dialog=find(render(),"ManualUploadDialog");dialog.props.onSubmit();
assert(requests.length===1,"unconfigured destination sent an upload");
dialog.props.onConfigure("cpa");assert(!find(render(),"ManualUploadDialog")&&find(render(),"ConfigurationPage").props.section==="cpa","configure link opened the wrong settings");
`)
}

func TestFrontendManualUploadSingleProviderIgnoresUnavailablePreference(t *testing.T) {
	for _, target := range []string{"sub2api", "cpa"} {
		t.Run(target, func(t *testing.T) {
			vm := frontendAppVM(t)
			vm.Set("destination", target)
			frontendPollingRun(t, vm, frontendCPASetup)
			frontendPollingRun(t, vm, `appState.setFeatures({cpaUpload:destination==="cpa",sub2apiUpload:destination==="sub2api"});if(destination==="cpa"){cpaState().reload();respondCPA(0,cpaReady);}`)
			frontendPollingRun(t, vm, `
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"first",email:"first@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setUploadTarget(destination==="cpa"?"sub2api":"cpa");
const row=find(render(),"JobRow");row.props.onUpload();row.props.onUpload();
assert(!find(render(),"ManualUploadDialog"),"single provider added a redundant selection");
const writes=requests.filter(r=>r.options.method==="POST");assert(writes.length===1&&writes[0].url==="/api/"+destination+"/upload","unavailable preference or duplicate click misrouted the upload");
`)
		})
	}
}

func TestFrontendManualUploadDialogShowsActionAndBlocksInvalidSubmission(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "upload-target")
	frontendPollingRun(t, vm, `
var sends=0,closes=0,changed="",configured="",reads=0;
const targets={sub2api:{available:true,ready:true,status:"已配置"},cpa:{available:true,ready:false,status:"配置待核实"}};
const props={action:{ids:["one","two"],target:"cpa"},targets,problem:"CPA 配置尚未核实，请读取当前配置后再上传。",onTargetChange:target=>changed=target,onSubmit:()=>sends++,onClose:()=>closes++,onConfigure:target=>configured=target,onReloadCPA:()=>reads++};
let tree=RepairUI.ManualUploadDialog(props);
assert(content(tree).includes("2 个账号")&&content(tree).includes("同名授权文件将替换")&&content(tree).includes("优先级和权重")&&!content(tree).includes("保留配置"),"CPA dialog lost scope or promised Sub2 semantics");
assert(findButton(tree,"上传到 CPA").props.disabled,"unverified configuration left submit enabled");
find(tree,"form").props.onSubmit({preventDefault(){}});assert(sends===0,"form bypassed its configuration gate");
findButton(tree,"取消").props.onClick();findButton(tree,"读取 CPA 配置").props.onClick();findButton(tree,"配置 CPA").props.onClick();
assert(closes===1&&reads===1&&configured==="cpa","dialog recovery actions are not wired");
const picker=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="UploadTargetPicker");
const choices=RepairUI.UploadTargetPicker(picker.props);
assert(content(choices).includes("已配置")&&content(choices).includes("配置待核实"),"destination options hide configuration state");
findMatching(choices,node=>node.type==="input"&&node.props.value==="sub2api").props.onChange({target:{checked:true}});assert(changed==="sub2api","radio does not change destination");
tree=RepairUI.ManualUploadDialog({...props,action:{...props.action,target:"sub2api"},problem:""});
assert(content(tree).includes("保留原有号池配置")&&!findButton(tree,"上传到 Sub2API").props.disabled,"Sub2 destination lost its operation semantics");
find(tree,"form").props.onSubmit({preventDefault(){}});assert(sends===1,"valid form did not submit");
`)
}

func TestFrontendUploadDestinationHasNoGlobalPanels(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
location.hash="#accounts";render();
appState.setFeatures({sub2apiUpload:true,cpaUpload:true});
const accounts=findMatching(render(),node=>node.type==="section"&&node.props.className==="accounts-section");
assert(!find(accounts,"UploadTargetPicker"),"account page still has a global destination panel");
assert(!findMatching(accounts,node=>(node.props?.className||"").includes("upload-settings-shortcut")),"account page gained an unrequested replacement control");
find(render(),"AppShell").props.onNavigate("settings");
const tree=RepairUI.ConfigurationPage(find(render(),"ConfigurationPage").props);
assert(!findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="UploadTargetPicker"),"configuration page still has a global destination panel");
assert(findButton(tree,"Sub2API")&&findButton(tree,"CPA"),"separate configuration categories were merged");
assert(!findButton(tree,"推送配置"),"unrequested shared configuration category remained");
assert(requests.length===0,"removing the panels introduced requests");
`)
}

func TestFrontendProviderConfigurationDoesNotChangeUploadPreference(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#settings";render();appState.setFeatures({sub2apiUpload:true,cpaUpload:true});
appState.setUploadTarget("cpa");
const before=JSON.stringify([...storage]);
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
assert(find(render(),"Sub2ApiSettingsDialog")&&!find(render(),"Sub2ApiSettingsDialog").props.targetPicker,"Sub2 settings was merged with a global choice");
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"unchanged-target",email:"synthetic@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setFeatures({sub2apiUpload:true,cpaUpload:true,bulkActions:true});
find(render(),"JobRow").props.onUpload();
assert(find(render(),"ManualUploadDialog").props.action.target==="cpa","viewing Sub2 settings changed the upload preference");
assert(JSON.stringify([...storage])===before,"viewing provider configuration changed target preference");
`)
}

func TestFrontendWorkflowDestinationChoiceStillUpdatesPreference(t *testing.T) {
	for _, workflow := range []string{"relogin", "totp"} {
		vm := frontendAppVM(t)
		if err := vm.Set("workflow", workflow); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `
location.hash="#accounts";render();
appState.setFeatures({sub2apiUpload:true,cpaUpload:true,reloginAutoUpload:true,forceRelogin:true,totpReset:true,totpSetup:true});
const job={id:"workflow-target",email:"synthetic@example.invalid",status:"completed",canDownload:true,canForceRelogin:true,canResetTotp:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);
const row=find(render(),"JobRow");
if(workflow==="relogin")row.props.onRelogin();else row.props.onSetupTotp(true,false);
const type=workflow==="relogin"?"ReloginActionDialog":"TotpActionDialog";
find(render(),type).props.onUploadTargetChange("cpa");
assert(find(render(),type).props.uploadTarget==="cpa"&&storage.get("tosub2.upload-target-v1")==="cpa","workflow destination preference no longer works");
assert(requests.length===0,"workflow target choice submitted an operation");
`)
	}
}
