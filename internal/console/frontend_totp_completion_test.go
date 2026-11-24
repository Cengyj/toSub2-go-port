package console

import "testing"

func TestFrontendOAuthLocalCompletionKeepsGrantMethodAndOmitsRemoteOptions(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
var tree=prepareBatch(),handlerErrors=[];
var job={id:"issued-grant",email:"issued@synthetic.invalid",status:"resume_available",operationStatus:"interrupted",authorizationMethod:"bps",canResume:true,canRetry:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setSelectedJobIds(new Set());tree=render();
findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id===job.id).props.onAuthorize("retry");
var dialog=find(render(),"ReloginActionDialog");assert(dialog&&dialog.props.action.lockedMethod,"issued grant offered a fresh authorization dialog");
dialog.props.onSubmit({authorizationMethod:"chatgpt",uploadAfterRelogin:true}).catch(error=>handlerErrors.push(error));
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/jobs/issued-grant/retry"&&requests[0].options.body==='{"authorizationMethod":"bps"}',"local save changed method, proxy, or remote workflow options");
requests[0].resolve({ok:true,status:200,json:async()=>({ok:true,job})});
`)
	frontendPollingRun(t, vm, `
assert(handlerErrors.length===0&&!find(render(),"ReloginActionDialog")&&requests.length===1,"local grant save repeated work or stayed open");
assert(content(render()).includes("已提交本地授权保存")&&content(render()).includes("不连接远端"),"local save was presented as a new remote authorization");
`)
}

func TestFrontendTOTPCompletionUsesOnlyLocalRoute(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
var tree=prepareBatch(),handlerErrors=[];
var job={id:"confirmed",email:"confirmed@synthetic.invalid",status:"failed",operationStatus:"failed",totpCompletionPending:true,canCompleteTotp:true,canResetTotp:false};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setSelectedJobIds(new Set());tree=render();
findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="confirmed").props.onSetupTotp(true,true);
var dialog=find(render(),"TotpActionDialog");assert(dialog&&dialog.props.action.completeTotp,"completion mode lost");
dialog.props.onSubmit({reloginAfterTotp:true,uploadAfterTotp:true,logoutAllDevicesAfterTotp:true}).catch(error=>handlerErrors.push(error));
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/jobs/confirmed/complete-2fa"&&requests[0].options.method==="POST"&&requests[0].options.body==="{}","completion submitted credentials, followups, or a remote workflow");
requests[0].resolve({ok:true,status:200,json:async()=>({ok:true,job})});
`)
	frontendPollingRun(t, vm, `
assert(handlerErrors.length===0&&!find(render(),"TotpActionDialog")&&requests.length===1,"completion repeated work or stayed open");
`)
}

func TestFrontendTOTPCompletionRowDialogAndUnknownManualState(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
var submitted=null,called=[];
const job={id:"pending",email:"pending@synthetic.invalid",status:"failed",operationStatus:"failed",totpCompletionPending:true,canCompleteTotp:true,canResetTotp:false,totpCompletionHint:"仅完成本地保存；不连接远端"};
var tree=RepairUI.JobRow({job,token:"synthetic",totpResetAvailable:true,totpSetupAvailable:true,onSetupTotp(...args){called=args;}});
var button=findButton(tree,"完成本地保存");assert(button&&!button.props.disabled&&!findButton(tree,"恢复 2FA")&&!findButton(tree,"核实原会话"),"local completion action missing or old action remains");
button.props.onClick();assert(called[0]===true&&called[1]===true,"completion did not select narrow action");
hookSlots=[];hookCursor=0;
tree=RepairUI.TotpActionDialog({action:{completeTotp:true,resetTotp:true,email:job.email},logoutAvailable:true,reloginAvailable:true,uploadAvailable:true,uploadConfigured:true,onSubmit(value){submitted=value;return Promise.resolve()},onClose(){}});
assert(content(tree).includes("完成本地保存")&&content(tree).includes("本次激活证据")&&content(tree).includes("不连接远端"),"completion boundary missing");
assert(!findMatching(tree,node=>typeof node.type==="function"&&["TotpWorkflowOption","AuthorizationMethodPicker"].includes(node.type.name)),"completion retained remote followup options");
find(tree,"form").props.onSubmit({preventDefault(){}});assert(JSON.stringify(submitted)==="{}","completion dialog submitted remote options");
hookSlots=[];hookCursor=0;
const unknown={...job,totpCompletionPending:false,canCompleteTotp:false,totpRecoveryPending:true,totpCredentialInvalidated:true};
tree=RepairUI.JobRow({job:unknown,token:"synthetic",totpResetAvailable:true,totpSetupAvailable:true,onSetupTotp(){throw Error("must not run")}});
button=findButton(tree,"需人工处理");assert(button&&button.props.disabled&&!findButton(tree,"设置 2FA")&&!findButton(tree,"完成本地保存"),"unknown state offered recovery or first setup");
assert(requests.length===0,"row or dialog made unsolicited requests");
`)
}

func TestFrontendTOTPCompletionPreservesConfirmedSuccessAndUploadGate(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const job={status:"failed",operationStatus:"failed",lastOperationType:"reset_2fa",totpWorkflowStatus:"failed",totpWorkflowStep:"totp",totpResetPhase:"activation_verified",totpRecoveryPending:true,totpCredentialInvalidated:true,totpActivationVerified:true,totpCompletionPending:true,canCompleteTotp:true};
const result=Browser.describeJob(job);
assert(result.status.code==="totp_completion_pending"&&result.summary.includes("新 2FA 已确认成功")&&result.summary.includes("不连接远端"),"confirmed activation was presented as failed or unknown");
assert(Browser.describeWorkflow(job).steps[0].state==="confirmed"&&Browser.describeWorkflow(job).steps[0].stateLabel.includes("待本地保存")&&Browser.uploadBlockedJob(job),"confirmed activation became finished local persistence or enabled upload");
const unknown=Browser.describeJob({...job,totpCompletionPending:false,canCompleteTotp:false,totpActivationVerified:false,totpResetPhase:"activation_unconfirmed"});
assert(unknown.status.code==="totp_manual_required"&&unknown.summary.includes("人工处理")&&!unknown.status.spinning,"unknown result offered session recovery");
const active=Browser.describeJob({...job,status:"running",operationStatus:"running",lastOperationType:"complete_2fa"});
assert(active.operation.label==="完成本地保存"&&!active.terminal&&active.status.spinning&&Browser.uploadBlockedJob({...job,status:"running",operationStatus:"running"}),"local completion lost progress or upload gate");
`)
}
