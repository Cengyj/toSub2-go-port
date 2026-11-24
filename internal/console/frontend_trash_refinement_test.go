package console

import "testing"

func TestFrontendTrashRefinementSettingsUnmountIgnoresLateSaveCompletion(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var unmounted=false,lateStateWrites=0,resolveSave;
const originalUseState=React.useState;
React.useState=initial=>{const [value,set]=originalUseState(initial);return [value,next=>{if(unmounted)lateStateWrites++;set(next);}];};
var settingsTrash={settings:{schemaVersion:1,revision:1,retentionDays:0},retentionDraft:"7",dirty:true,setRetentionDraft(){},reloadSettings(){},resetDraft(){},saveSettings:value=>{settingsSaves.push(value);return new Promise(resolve=>{resolveSave=resolve;});}};
confirmAccepted=true;var tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});tree.props.onSubmit({preventDefault(){}});
`)
	frontendPollingRun(t, vm, `assert(settingsSaves.length===1,"confirmed save did not reach its request boundary");unmount();unmounted=true;resolveSave(true);`)
	frontendPollingRun(t, vm, `assert(lateStateWrites===0,"late settings response updated an unmounted component");`)
}

func TestFrontendTrashRefinementSettingsUnmountRejectsDeferredConfirmation(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var settingsTrash={settings:{schemaVersion:1,revision:1,retentionDays:0},retentionDraft:"7",dirty:true,setRetentionDraft(){},reloadSettings(){},resetDraft(){},saveSettings:async value=>{settingsSaves.push(value);return true;}};
var resolveConfirmation;confirmAccepted=new Promise(resolve=>{resolveConfirmation=resolve;});
var tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});tree.props.onSubmit({preventDefault(){}});
assert(settingsSaves.length===0,"retention strategy saved before confirmation");unmount();resolveConfirmation(true);
`)
	frontendPollingRun(t, vm, `assert(settingsSaves.length===0,"unmounted settings page submitted a deferred irreversible retention change");`)
}

func TestFrontendTrashRefinementTargetConfirmationChecksWholeHistoricalIdentity(t *testing.T) {
	vm := frontendBrowserVM(t, "accountTrash")
	frontendPollingRun(t, vm, `
const original={id:"retained",email:"synthetic@example.invalid",deletedAt:"2026-11-03T08:00:00Z",state:"trashed",canRestore:true,recordCount:2,jobs:[{id:"first"},{id:"second"}]};
const before=JSON.stringify(original),same={...original,jobs:[{id:"second",status:"failed"},{id:"first",status:"completed"}],lastError:"A later safe diagnostic"};
assert(Browser.trashTargetsUnchanged([original],[same],"restore"),"unchanged identity was rejected merely because history order or diagnostics changed");
for(const patch of [{email:"other@example.invalid"},{deletedAt:"2026-11-03T09:00:00Z"},{state:"purging"},{recordCount:3},{canRestore:false},{jobs:[{id:"first"},{id:"replacement"}]}]) {
  assert(!Browser.trashTargetsUnchanged([original],[{...original,...patch}],"restore"),"confirmation accepted changed restoration identity: "+JSON.stringify(patch));
  assert(!Browser.trashTargetsUnchanged([original],[{...original,...patch}],"purge"),"confirmation accepted changed permanent deletion scope: "+JSON.stringify(patch));
}
assert(!Browser.trashTargetsUnchanged([original],[],"purge"),"removed record remained confirmed");
const irreversible={...original,state:"purging",canRestore:true};
assert(!Browser.trashTargetsUnchanged([irreversible],[irreversible],"restore"),"capability flag overrode irreversible state");
assert(JSON.stringify(original)===before&&same.jobs[0].id==="second","identity verification mutated authoritative history order");
`)
}

func TestFrontendTrashRefinementChangedTargetOrServiceDuringConfirmationNeverSubmits(t *testing.T) {
	for _, scenario := range []struct{ name, change string }{
		{"removed", `pageTrash.entries=[];`},
		{"history-expanded", `pageTrash.entries=pageTrash.entries.map(entry=>({...entry,recordCount:3,jobs:[...entry.jobs,{id:"new-history"}]}));`},
		{"purging", `pageTrash.entries=pageTrash.entries.map(entry=>({...entry,state:"purging",canRestore:false}));`},
		{"verification", `pageTrash.verificationRequired=true;`},
		{"action-busy", `pageTrash.actionBusy="purge";`},
		{"automatic-purge", `pageTrash.status={running:true};`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/trash-records.jsx")
			frontendPollingRun(t, vm, frontendTrashRefinementHarness)
			frontendPollingRun(t, vm, `pageTrash.entries=[refinementEntry(1)];var tree=renderRefinementPage();var resolveConfirmation;confirmAccepted=new Promise(resolve=>{resolveConfirmation=resolve;});findButton(tree,"恢复").props.onClick();assert(mutations.length===0,"restore submitted before confirmation");`)
			frontendPollingRun(t, vm, scenario.change+`tree=renderRefinementPage();resolveConfirmation(true);`)
			frontendPollingRun(t, vm, `tree=renderRefinementPage();assert(mutations.length===0&&content(tree).includes("确认期间回收记录或服务状态发生变化，本次未提交"),"changed target or service state was submitted or silently ignored");unmount();`)
		})
	}
}

func TestFrontendTrashRefinementExpiredConfirmationSessionCannotRevive(t *testing.T) {
	for _, scenario := range []struct{ name, change string }{
		{"token-changed", `pageProps.token="another-synthetic-session";tree=renderRefinementPage();`},
		{"token-round-trip", `pageProps.token="another-synthetic-session";tree=renderRefinementPage();pageProps.token="synthetic";tree=renderRefinementPage();`},
		{"feature-round-trip", `pageProps.available=false;tree=renderRefinementPage();pageProps.available=true;tree=renderRefinementPage();`},
		{"unmounted", `unmount();`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/trash-records.jsx")
			frontendPollingRun(t, vm, frontendTrashRefinementHarness)
			frontendPollingRun(t, vm, `pageTrash.entries=[refinementEntry(1)];var tree=renderRefinementPage();var resolveConfirmation;confirmAccepted=new Promise(resolve=>{resolveConfirmation=resolve;});findButton(tree,"彻底删除").props.onClick();assert(mutations.length===0,"purge submitted before confirmation");`)
			frontendPollingRun(t, vm, scenario.change+`resolveConfirmation(true);`)
			frontendPollingRun(t, vm, `assert(mutations.length===0,"an expired confirmation session submitted a destructive operation");unmount();`)
		})
	}
}

func TestFrontendTrashRefinementConfirmedCrossPageSelectionSubmitsOnce(t *testing.T) {
	vm := frontendInsightsVM(t, "components/trash-records.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var tree=renderRefinementPage();refinementAria(tree,"选择本页回收记录").props.onChange();tree=renderRefinementPage();
const firstIDs=refinementRows(tree).map(row=>row.props["data-trash-id"]);refinementAria(tree,"下一页回收记录").props.onClick();tree=renderRefinementPage();
find(refinementRows(tree)[0],"input").props.onChange();tree=renderRefinementPage();
assert(content(tree).includes("已选 21 个")&&content(tree).includes("其中 20 个不在当前页"),"cross-page operation scope was not disclosed");
confirmAccepted=true;const restore=findButton(tree,"恢复所选");restore.props.onClick();restore.props.onClick();
`)
	frontendPollingRun(t, vm, `assert(mutations.length===1&&mutations[0].action==="restore"&&mutations[0].ids.length===21&&firstIDs.every(id=>mutations[0].ids.includes(id)),"confirmation duplicated submission or silently reduced scope to the current page");unmount();`)
}

func TestFrontendTrashRefinementDetailCloseRestoresOnlyLostFocusAfterCommit(t *testing.T) {
	for _, scenario := range []struct {
		name, activeElement string
		triggerConnected    bool
		restoreFocus        bool
	}{
		{"body", `owner.body`, false, true},
		{"missing", `null`, false, true},
		{"disconnected", `{isConnected:false}`, false, true},
		{"another-control", `{isConnected:true}`, false, false},
		{"connected-trigger", `owner.body`, true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/trash-records.jsx")
			frontendPollingRun(t, vm, frontendTrashRefinementHarness)
			frontendPollingRun(t, vm, `
var focusCalls=0,owner={body:{isConnected:true},activeElement:{isConnected:true}},trigger={isConnected:true};
var tree=renderRefinementPage(),heading=findMatching(tree,node=>node.type==="h2"&&content(node)==="回收记录");
heading.props.ref.current={ownerDocument:owner,isConnected:true,focus(){focusCalls++;owner.activeElement=this;}};
findButton(refinementRows(tree)[0],"详情与日志").props.onClick({currentTarget:trigger});tree=renderRefinementPage();
trigger.isConnected=false;const detail=refinementComponent(tree,"AccountTrashDetails");detail.props.onClose();
assert(focusCalls===0,"closing a dialog focused the list before its focus trap could detach");
`)
			frontendPollingRun(t, vm, `const expectedFocus=`+scenario.activeElement+`;owner.activeElement=expectedFocus;`)
			if scenario.triggerConnected {
				frontendPollingRun(t, vm, `trigger.isConnected=true;`)
			}
			if scenario.restoreFocus {
				frontendPollingRun(t, vm, `tree=renderRefinementPage();assert(focusCalls===1&&!refinementComponent(tree,"AccountTrashDetails"),"lost focus was not restored after detail unmounted");tree=renderRefinementPage();assert(focusCalls===1,"detail cleanup repeated focus restoration");unmount();`)
			} else {
				frontendPollingRun(t, vm, `tree=renderRefinementPage();assert(focusCalls===0&&owner.activeElement===expectedFocus&&!refinementComponent(tree,"AccountTrashDetails"),"detail cleanup stole focus or overrode the dialog's connected return target");unmount();`)
			}
		})
	}
}

func TestFrontendTrashRefinementSettingsBlockEditingAndSubmissionDuringOperations(t *testing.T) {
	for _, flag := range []string{"verificationRequired", "actionBusy"} {
		t.Run(flag, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/account-trash.jsx")
			frontendPollingRun(t, vm, frontendTrashRefinementHarness)
			frontendPollingRun(t, vm, `var settingsTrash={settings:{schemaVersion:1,revision:1,retentionDays:0},retentionDraft:"7",dirty:true,setRetentionDraft(){},reloadSettings(){},resetDraft(){},saveSettings:async value=>{settingsSaves.push(value);return true;}};settingsTrash.`+flag+`=true;confirmAccepted=true;var tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});assert(find(tree,"input").props.disabled&&findButton(tree,"保存保留策略").props.disabled&&findButton(tree,"放弃更改").props.disabled,"pending account operation left retention edits enabled");tree.props.onSubmit({preventDefault(){}});`)
			frontendPollingRun(t, vm, `assert(settingsSaves.length===0,"programmatic form submission bypassed the pending-operation guard");settingsTrash.`+flag+`=false;tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});assert(!find(tree,"input").props.disabled&&!findButton(tree,"保存保留策略").props.disabled,"settled operation never released editing");unmount();`)
		})
	}
}

func TestFrontendTrashRefinementSettingsVerificationReadsOnceAndUnlocksFromProps(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var verificationReads=0,resolveVerification;
var settingsTrash={...refinementTrash([]),verificationRequired:true,error:"Synthetic verification unavailable",retentionDraft:"7",dirty:true,
  setRetentionDraft(){},reloadSettings(){},resetDraft(){},saveSettings:async value=>{settingsSaves.push(value);return true;},
  reloadList:()=>{verificationReads++;return new Promise(resolve=>{resolveVerification=resolve;});}};
var tree=renderComponent("AccountTrashSettings",{trash:settingsTrash}),verify=findButton(tree,"核实回收站状态");
assert(verify&&!verify.props.disabled&&find(tree,"input").props.disabled&&findButton(tree,"保存保留策略").props.disabled,"verification gate did not offer an enabled read-only recovery action");
assert(content(tree).includes("核实未完成：Synthetic verification unavailable"),"verification read error was hidden");
verify.props.onClick();verify.props.onClick();tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});
assert(verificationReads===1&&findButton(tree,"核实回收站状态").props.disabled,"repeated verification clicks duplicated reads or left the in-flight button enabled");
findButton(tree,"核实回收站状态").props.onClick();tree.props.onSubmit({preventDefault(){}});
assert(verificationReads===1&&settingsSaves.length===0&&mutations.length===0,"verification replayed a mutation or bypassed the in-flight guard");
resolveVerification();
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});
assert(!findButton(tree,"核实回收站状态").props.disabled&&find(tree,"input").props.disabled&&findButton(tree,"保存保留策略").props.disabled,"read completion unlocked settings before authoritative verification props changed");
settingsTrash={...settingsTrash,verificationRequired:false,error:""};tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});
assert(!find(tree,"input").props.disabled&&!findButton(tree,"保存保留策略").props.disabled&&!content(tree).includes("核实回收站状态"),"verified props did not release the settings gate");
assert(verificationReads===1&&settingsSaves.length===0&&mutations.length===0,"read-only verification changed retention policy or account state");unmount();
`)
}

func TestFrontendTrashRefinementSettingsRechecksOperationsAfterConfirmation(t *testing.T) {
	for _, flag := range []string{"verificationRequired", "actionBusy"} {
		t.Run(flag, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/account-trash.jsx")
			frontendPollingRun(t, vm, frontendTrashRefinementHarness)
			frontendPollingRun(t, vm, `var settingsTrash={settings:{schemaVersion:1,revision:1,retentionDays:0},retentionDraft:"7",dirty:true,setRetentionDraft(){},reloadSettings(){},resetDraft(){},saveSettings:async value=>{settingsSaves.push(value);return true;}};var resolveConfirmation;confirmAccepted=new Promise(resolve=>{resolveConfirmation=resolve;});var tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});tree.props.onSubmit({preventDefault(){}});assert(settingsSaves.length===0,"cleanup strategy saved before confirmation");`)
			frontendPollingRun(t, vm, `settingsTrash={...settingsTrash,`+flag+`:true};tree=renderComponent("AccountTrashSettings",{trash:settingsTrash});resolveConfirmation(true);`)
			frontendPollingRun(t, vm, `assert(settingsSaves.length===0,"new account operation during confirmation did not block retention write");unmount();`)
		})
	}
}

const frontendTrashRefinementHarness = `
function refinementEntry(index, overrides={}) {
  const id="refined-"+String(index).padStart(2,"0"), at=new Date(Date.UTC(2026,10,1,0,index)).toISOString();
  return {id,email:"refined-"+index+"@example.invalid",deletedAt:at,state:"trashed",canRestore:true,recordCount:2,
    jobs:[{id:id+"-history-1",createdAt:at,status:"completed",lastOperationType:"relogin",hasLogs:true},{id:id+"-history-2",createdAt:at,status:"failed",lastOperationType:"refresh",hasLogs:false}],...overrides};
}
function refinementNodes(tree,predicate) {
  const out=[];
  function visit(node) {
    if(Array.isArray(node)){node.forEach(visit);return;}
    if(!node||typeof node!=="object")return;
    if(predicate(node))out.push(node);
    (node.props?.children||[]).forEach(visit);
  }
  visit(tree);return out;
}
function refinementRows(tree){return refinementNodes(tree,node=>node.type==="tr"&&node.props["data-trash-id"]);}
function refinementAria(tree,label){return findMatching(tree,node=>node.props?.["aria-label"]===label);}
function refinementComponent(tree,name){return findMatching(tree,node=>typeof node.type==="function"&&node.type.name===name);}
var mutations=[],settingsSaves=[],closedDetails=0;
function refinementTrash(entries) {
  return {entries,loaded:true,loading:false,error:"",actionBusy:"",verificationRequired:false,
    settings:{schemaVersion:1,revision:1,retentionDays:0},status:{running:false},
    reloadList(){},mutate:async(action,ids)=>{mutations.push({action,ids});return {count:ids.length,outcome:"completed"};}};
}
var pageTrash=refinementTrash(Array.from({length:45},(_,index)=>refinementEntry(index+1)));
var pageProps={token:"synthetic",trash:pageTrash,available:true,onBack(){},onConfigure(){}};
function renderRefinementPage(){return renderComponent("AccountTrashPage",pageProps);}
`

func TestFrontendTrashRefinementSelectsOnlyCurrentPageAndKeepsHiddenScope(t *testing.T) {
	vm := frontendInsightsVM(t, "components/trash-records.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var tree=renderRefinementPage();
assert(refinementRows(tree).length===20&&refinementRows(tree)[0].props["data-trash-id"]==="refined-45"&&content(tree).includes("显示 1–20 / 45 个账号"),"first page does not render exactly twenty newest records");
refinementAria(tree,"选择本页回收记录").props.onChange();tree=renderRefinementPage();
assert(content(tree).includes("已选 20 个")&&refinementRows(tree).every(row=>find(row,"input").props.checked),"select-page selected a different scope");
refinementAria(tree,"下一页回收记录").props.onClick();tree=renderRefinementPage();
assert(refinementRows(tree).length===20&&content(tree).includes("显示 21–40 / 45 个账号")&&content(tree).includes("其中 20 个不在当前页"),"page change lost the selected hidden-account warning");
assert(refinementRows(tree).every(row=>!find(row,"input").props.checked),"page one selection silently selected page two");
refinementAria(tree,"选择本页回收记录").props.onChange();tree=renderRefinementPage();
assert(content(tree).includes("已选 40 个")&&content(tree).includes("其中 20 个不在当前页"),"second page selection replaced existing selected records");
findButton(tree,"清空选择").props.onClick();tree=renderRefinementPage();
assert(content(tree).includes("已选 0 个")&&!content(tree).includes("个不在当前页")&&findButton(tree,"恢复所选").props.disabled&&findButton(tree,"彻底删除所选").props.disabled,"clear selection retained hidden or actionable records");
refinementAria(tree,"下一页回收记录").props.onClick();tree=renderRefinementPage();
assert(refinementRows(tree).length===5&&content(tree).includes("显示 41–45 / 45 个账号")&&refinementAria(tree,"下一页回收记录").props.disabled,"last page has invented rows or allows a nonexistent next page");
assert(mutations.length===0&&requests.length===0,"selection or pagination initiated account operations");unmount();
`)
}

func TestFrontendTrashRefinementPurgingFilterRefusesRestoreAndClearsSearch(t *testing.T) {
	vm := frontendInsightsVM(t, "components/trash-records.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
pageTrash.entries=pageTrash.entries.map(entry=>["refined-10","refined-35"].includes(entry.id)?{...entry,state:"purging",canRestore:false,lastError:"Synthetic local cleanup unavailable"}:entry);
var tree=renderRefinementPage();refinementAria(tree,"筛选回收状态").props.onChange({target:{value:"purging"}});tree=renderRefinementPage();
assert(refinementRows(tree).length===2&&refinementRows(tree).every(row=>findButton(row,"恢复").props.disabled&&!findButton(row,"继续清理").props.disabled),"purging filter made irreversible records restorable or hid retry");
refinementAria(tree,"选择本页回收记录").props.onChange();tree=renderRefinementPage();
assert(findButton(tree,"恢复所选").props.disabled&&!findButton(tree,"彻底删除所选").props.disabled&&content(tree).includes("所选包含不可恢复记录"),"mixed-operation eligibility did not follow irreversible state");
refinementAria(tree,"搜索回收账号或记录 ID").props.onChange({target:{value:"missing synthetic record"}});tree=renderRefinementPage();
assert(refinementRows(tree).length===0&&content(tree).includes("没有匹配的回收记录")&&content(tree).includes("其中 2 个不在当前页"),"empty filter erased retained selections or looked like an empty recycle bin");
findButton(tree,"清除筛选").props.onClick();tree=renderRefinementPage();
assert(refinementRows(tree).length===20&&refinementAria(tree,"筛选回收状态").props.value==="all"&&refinementAria(tree,"搜索回收账号或记录 ID").props.value==="","clear filters did not restore first-page scope");
assert(mutations.length===0,"filter or selection initiated a mutation");unmount();
`)
}

func TestFrontendTrashRefinementDetailsAreIndependentAndKeepMissingIdentityVisible(t *testing.T) {
	vm := frontendInsightsVM(t, "components/trash-records.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var tree=renderRefinementPage();assert(!refinementComponent(tree,"AccountTrashDetails"),"list eagerly opened a detail dialog");
const row=refinementRows(tree)[0],detailID=row.props["data-trash-id"];
findButton(row,"详情与日志").props.onClick({currentTarget:{isConnected:true}});tree=renderRefinementPage();
var detail=refinementComponent(tree,"AccountTrashDetails");
assert(detail&&detail.props.entry.id===detailID&&refinementRows(tree).length===20&&!refinementComponent(tree,"JobLogs"),"opening details expanded logs inside list rows or lost selected identity");
pageTrash.entries=pageTrash.entries.filter(entry=>entry.id!==detailID);tree=renderRefinementPage();detail=refinementComponent(tree,"AccountTrashDetails");
assert(detail&&detail.props.entry===undefined,"disappeared record silently closed detail or switched to another identity");
detail.props.onClose();tree=renderRefinementPage();assert(!refinementComponent(tree,"AccountTrashDetails"),"detail cannot close after the record disappears");
assert(mutations.length===0&&requests.length===0,"opening or closing detail caused an account mutation or inline log fetch");unmount();
`)
}

func TestFrontendTrashRefinementDetailHistoryAndIrreversibleStateRemainExplicit(t *testing.T) {
	vm := frontendInsightsVM(t, "components/trash-details.jsx")
	frontendPollingRun(t, vm, frontendTrashRefinementHarness)
	frontendPollingRun(t, vm, `
var entry=refinementEntry(1),detailProps={token:"synthetic",entry,settings:{retentionDays:0},onClose(){closedDetails++;}};
Object.assign(entry.jobs[0],{status:"completed",operationStatus:"failed",lastOperationType:"complete-2fa",lastOperationAt:"2026-11-24T04:03:02.123Z"});
var tree=renderComponent("AccountTrashDetails",detailProps),logs=refinementComponent(tree,"JobLogs");
assert(logs&&logs.props.archiveId===entry.id&&logs.props.jobId===entry.jobs[0].id&&content(tree).includes("持续保留"),"detail did not use the selected archive identity and current retention policy");
assert(content(tree).includes("完成本地保存 · 操作失败")&&logs.props.job.lastOperationAt===entry.jobs[0].lastOperationAt,"history presented retained authorization as operation success or dropped its grouping time");
refinementAria(tree,"选择历史日志 "+entry.email).props.onChange({target:{value:entry.jobs[1].id}});tree=renderComponent("AccountTrashDetails",detailProps);
assert(refinementComponent(tree,"JobLogs").props.jobId===entry.jobs[1].id,"history selection did not select the requested job logs");
detailProps={...detailProps,entry:{...entry,state:"purging",canRestore:false,lastError:"Synthetic cleanup still pending"}};tree=renderComponent("AccountTrashDetails",detailProps);
assert(content(tree).includes("永久清理未完成 · 已不可恢复")&&content(tree).includes("剩余资料保留供重试，已不能恢复")&&!content(tree).includes("持续保留"),"zero-day policy concealed an irreversible purge");
detailProps={...detailProps,readError:"Synthetic cache read failed"};tree=renderComponent("AccountTrashDetails",detailProps);
assert(content(tree).includes("列表暂未更新，以下为最近读取的信息")&&content(tree).includes("Synthetic cleanup still pending"),"read failure hid retained history or the last cleanup failure");
detailProps={...detailProps,entry:undefined};tree=renderComponent("AccountTrashDetails",detailProps);
assert(content(tree).includes("记录已不在回收站")&&content(tree).includes("这条记录已被恢复或清理")&&!refinementComponent(tree,"JobLogs"),"missing archive continued querying stale history or lacked an explicit explanation");
refinementAria(tree,"关闭回收记录详情").props.onClick();assert(closedDetails===1,"missing detail lost its close action");unmount();
`)
}
