package console

import "testing"

// Exercise production callbacks with the existing Go esbuild/Sobek harness.
// Native focus, keyboard dispatch and visual hit targets are checked separately.
func TestFrontendAccountQualityActionsMenuSeparatesDetailsFromInvocation(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
const base={id:"synthetic-menu",email:"synthetic-menu@example.invalid",status:"completed",operationStatus:"succeeded",canDownload:true};
let viewed=0,checked=0;
for(const [authorizationMethod,qualityCanCheck] of [["chatgpt",true],["chatgpt",false],["bps",false]]){
 hookCursor=0;
 const row=RepairUI.JobRow({job:{...base,authorizationMethod},token:"synthetic",qualityAvailable:true,qualityCanCheck,onViewQuality(){viewed++;},onCheckQuality(){checked++;}});
 const more=findMatching(row,node=>node.type==="ActionMenu"&&node.props.ariaLabel==="账号操作 "+base.email);
 assert(more,"probe actions lost the account's More menu");
 const details=findButton(more,"推理探针详情"),check=findButton(more,"发起推理探针");
 assert(details&&details.props.type==="button"&&!details.props.disabled,"ineligible account lost its read-only history entry");
 assert(check&&check.props.type==="button"&&check.props.disabled===!qualityCanCheck,"More-menu invocation diverged from shared eligibility");
 const beforeViewed=viewed,beforeChecked=checked;
 details.props.onClick();
 assert(viewed===beforeViewed+1&&checked===beforeChecked,"opening More-menu details submitted a probe");
 if(!check.props.disabled){check.props.onClick();assert(checked===beforeChecked+1&&viewed===beforeViewed+1,"explicit More-menu invocation did not delegate independently");}
}
hookCursor=0;
const unavailable=RepairUI.JobRow({job:base,token:"synthetic",qualityAvailable:false,onViewQuality(){viewed++;},onCheckQuality(){checked++;}});
assert(!findButton(unavailable,"推理探针详情")&&!findButton(unavailable,"发起推理探针"),"unavailable feature exposed account probe actions");
assert(viewed===3&&checked===1&&requests.length===0&&timers.size===0,"menu presentation started its own request or changed callback count");
`)
}

func TestFrontendAccountQualityActionsDialogFollowsEligibilityAndCurrentState(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
renderComponent("AccountQualityDialog",{job:{id:"synthetic-dialog",authorizationMethod:"chatgpt"},quality:{settings:{model:"synthetic-dialog-model",reasoningEffort:"high"},snapshots:{},modelsAPI:async()=>({models:[]}),canSubmit(){return false;}},onCheck(){throw new Error("directory lookup started detection");},onClose(){}});
`)
	frontendPollingRun(t, vm, `
const id="synthetic-dialog",email="synthetic-dialog@example.invalid";
let checked=0,closed=0;const cancelled=[],eligibility=[];
for(const [authorizationMethod,state,attempt,eligible,busy,expectedCheck] of [
 ["chatgpt","eligible","never",true,"",true],
 ["chatgpt","eligible","never",false,"",false],
 ["bps","eligible","never",true,"",true],
 ["chatgpt","queued","queued",false,"",false],
 ["chatgpt","active","running",false,"",false],
 ["chatgpt","active","running",false,"cancel",false],
 ["chatgpt","coolingDown","success",false,"",false],
 ["chatgpt","unavailable","failed",false,"",false],
]){
 const quality={settings:{enabled:true,minimumScore:1,model:"synthetic-dialog-model",reasoningEffort:"high"},modelsAPI:async()=>({models:[]}),settingsLoaded:true,snapshots:{[id]:{id,state,lastAttempt:{status:attempt},persisted:true}},actionBusy:busy,
  canSubmit(ids){eligibility.push(ids);return eligible;},cancel:async ids=>{cancelled.push(ids);}};
 const beforeChecked=checked,beforeCancelled=cancelled.length,beforeClosed=closed,beforeEligibility=eligibility.length;
 const tree=renderComponent("AccountQualityDialog",{job:{id,email,authorizationMethod},quality,onCheck(options){assert(options.model==="synthetic-dialog-model"&&options.reasoningEffort==="high","dialog dropped single-run parameters");checked++;},onClose(){closed++;}});
 const dialog=findMatching(tree,node=>node.type==="section"&&node.props.role==="dialog");
 assert(dialog&&dialog.props["aria-modal"]==="true"&&dialog.props["aria-labelledby"]==="account-quality-title"&&content(dialog).includes(email),"dialog lost its account or accessible identity");
 assert(checked===beforeChecked&&cancelled.length===beforeCancelled&&closed===beforeClosed&&requests.length===0,"opening details itself submitted or cancelled a probe");
 assert(eligibility.length===beforeEligibility+1&&JSON.stringify(eligibility.at(-1))===JSON.stringify([id]),"dialog checked a different account's eligibility");
 const active=state==="queued"||state==="active",check=findButton(tree,active?(state==="queued"?"排队中":"检测中"):"开始检测"),cancel=findButton(tree,"取消");
 assert(check&&check.props.type==="button"&&check.props.disabled===!expectedCheck,"dialog bypassed shared eligibility or BPS restriction");
 assert(Boolean(cancel)===active,"cancel action does not reflect the current queued/running attempt");
 if(cancel)assert(cancel.props.type==="button"&&cancel.props.disabled===Boolean(busy),"pending mutation allowed conflicting cancellation");
 if(expectedCheck){check.props.onClick();assert(checked===beforeChecked+1&&cancelled.length===beforeCancelled,"explicit dialog action did not call only the existing submit entry");}
 if(cancel&&!cancel.props.disabled){cancel.props.onClick();assert(cancelled.length===beforeCancelled+1&&JSON.stringify(cancelled.at(-1))===JSON.stringify([id]),"explicit cancellation targeted the wrong account");}
 const beforeCloseChecks=checked,beforeCloseCancels=cancelled.length;
 const headerClose=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="关闭推理探针");
 assert(headerClose&&tree.type==="DialogFrame","dialog lost its shared close/focus boundary");
 headerClose.props.onClick();tree.props.onClose();
 assert(closed===beforeClosed+2&&checked===beforeCloseChecks&&cancelled.length===beforeCloseCancels,"closing details altered an accepted probe");
}
assert(checked===2&&cancelled.length===2&&requests.length===0&&[...timers.values()].every(timer=>timer.delay===20000),"dialog presentation started content generation instead of only model metadata reads");
unmount();assert(timers.size===0,"dialog unmount retained catalog request deadline");
`)
}

func TestFrontendAccountQualityActionsDialogReportsCancelFailureAndVerifiesReadOnly(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
const job={id:"synthetic-cancel",email:"synthetic-cancel@example.invalid",authorizationMethod:"chatgpt"};
let checked=0,closed=0,reloads=0,rejectCancel;const cancelled=[];
const quality={settings:{enabled:true,minimumScore:1,model:"synthetic-dialog-model",reasoningEffort:"high"},modelsAPI:async()=>({models:[]}),settingsLoaded:true,snapshots:{[job.id]:{id:job.id,state:"active",lastAttempt:{status:"running"},persisted:false}},
 canSubmit(){return false;},cancel(ids){cancelled.push(ids);return new Promise((resolve,reject)=>{rejectCancel=reject;});},reload(){reloads++;}};
const props={job,quality,onCheck(){checked++;},onClose(){closed++;}};
var tree=renderComponent("AccountQualityDialog",props);
assert(cancelled.length===0&&checked===0,"opening active details implicitly cancelled or restarted work");
findButton(tree,"取消").props.onClick();
assert(cancelled.length===1&&cancelled[0].join()===job.id,"cancel did not use the selected account");
rejectCancel(new Error("Synthetic cancellation response lost; verify current status"));
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);
const error=findMatching(tree,node=>node.props?.role==="alert");
assert(error&&content(error).includes("Synthetic cancellation response lost"),"cancel failure was silently swallowed");
assert(cancelled.length===1&&checked===0&&reloads===0,"failed cancellation was replayed by rendering");
quality.verificationRequired=true;quality.error="Synthetic result must be verified";
tree=renderComponent("AccountQualityDialog",props);
const verify=findButton(tree,"核对检测状态");assert(verify&&verify.props.type==="button","uncertain result lost its read-only verification entry");
verify.props.onClick();assert(reloads===1&&cancelled.length===1&&checked===0,"status verification replayed a mutation");
quality.cancel=async ids=>{cancelled.push(ids);return {cancelled:ids,unchanged:[]};};
quality.verificationRequired=false;quality.error="";
tree=renderComponent("AccountQualityDialog",props);findButton(tree,"取消").props.onClick();
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);
assert(cancelled.length===2&&!findMatching(tree,node=>node.props?.role==="alert"),"explicit successful retry retained stale local cancellation error");
findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="关闭推理探针").props.onClick();unmount();
assert(closed===1&&checked===0&&cancelled.length===2&&requests.length===0&&timers.size===0,"close/unmount submitted, cancelled or leaked independent work");
`)
}

func TestFrontendAccountQualityActionsAppOpensCorrectAccountWithoutSubmitting(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#accounts";var tree=render();
const jobs=["first","second"].map(id=>({id,email:id+"@synthetic.invalid",status:"completed",operationStatus:"succeeded",authorizationMethod:id==="first"?"chatgpt":"bps",canDownload:true}));
appState.setToken("synthetic");appState.setFeatures({accountQuality:true,bulkActions:true});appState.setJobs(jobs);appState.setJobSelectionIndex(jobs);tree=render();
for(const job of jobs){
 const row=findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id===job.id);
 assert(row&&typeof row.props.onViewQuality==="function"&&!find(tree,"AccountQualityDialog"),"row detail entry or initial dialog state missing");
 const before=requests.length;row.props.onViewQuality();tree=render();
 const dialog=find(tree,"AccountQualityDialog");
 assert(dialog&&dialog.props.job.id===job.id&&dialog.props.job.email===job.email,"detail opened a different account or stale identity");
 assert(typeof dialog.props.onCheck==="function"&&requests.length===before,"opening App details submitted a probe");
 dialog.props.onClose();tree=render();
 assert(!find(tree,"AccountQualityDialog")&&requests.length===before,"closing App details submitted work or retained the dialog");
}
assert(requests.length===0&&timers.size===0,"viewing probe details started an account mutation");
`)
}

func TestFrontendAccountQualityDialogPassesSelectedRunParametersWithoutChangingDefaultPolicy(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var checks=[],catalogReads=[],job={id:"selected-probe-account",email:"selected@synthetic.invalid",authorizationMethod:"bps"};
var quality={manualSession:{},settings:{enabled:true,model:"saved-default-model",reasoningEffort:"high",beforeUpload:true,blockFailed:true,blockDegraded:true},settingsLoaded:true,snapshots:{[job.id]:{id:job.id,state:"eligible",persisted:true,lastAttempt:{status:"never"}}},canSubmit(ids){assert(ids.join()===job.id,"parameterized invocation checked another account");return true;},modelsAPI(id,options){catalogReads.push({id,options});return Promise.resolve({models:[{id:"catalog-visual-model",displayName:"Visual",reasoningEfforts:["low","medium"]}]});}};
var savedPolicy=JSON.stringify(quality.settings),props={job,quality,onCheck(options){checks.push(options);},onClose(){}};
var tree=renderComponent("AccountQualityDialog",props);
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);
var picker=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect");
assert(picker&&catalogReads.length===1&&catalogReads[0].id===job.id&&!catalogReads[0].options.method,"dialog did not use account-scoped read-only model directory");
picker.props.onChange("catalog-visual-model");
findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="推理强度").props.onChange({target:{value:"low"}});
tree=renderComponent("AccountQualityDialog",props);findButton(tree,"开始检测").props.onClick();
assert(checks.length===1&&checks[0].model==="catalog-visual-model"&&checks[0].reasoningEffort==="low"&&JSON.stringify(quality.settings)===savedPolicy,"single-run selection changed the saved upload policy or lost its parameters");
var question=findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="检测题目");
assert(question&&!findMatching(tree,node=>node.props?.role==="tablist"),"question selection regained competing tabs");question.props.onChange({target:{value:"pelican"}});
tree=renderComponent("AccountQualityDialog",props);var manual=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ManualProbePanel");
assert(manual&&JSON.stringify(manual.props.options)===JSON.stringify(checks[0]),"manual question received different single-run model parameters");manual.props.onActivityChange(true);
tree=renderComponent("AccountQualityDialog",props);
assert(findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="检测题目").props.disabled&&findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect").props.disabled,"active/unknown manual work unlocked topic or model switching");
assert(JSON.stringify(quality.settings)===savedPolicy&&requests.length===0,"question switch wrote global policy or started implicit generation");unmount();assert(timers.size===0,"dialog model deadline leaked");
`)
}
