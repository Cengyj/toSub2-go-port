package console

import "testing"

const activeTOTPFixture = `
var currentReset={id:"reset-current",email:"reset-current@example.invalid",lastOperationType:"reset_2fa",lastOperationAt:"2026-11-24T02:00:00.123456789Z",totpActiveOperationAt:"2026-11-24T02:00:00.123456789Z",operation:"reset-2fa",operationStatus:"running",status:"working",stage:"totp_reset_enroll",prompt:"正在创建新的 2FA 密钥",totpWorkflowStatus:"running",totpWorkflowStep:"totp",totpResetStatus:"running",totpRecoveryPending:true,totpCredentialInvalidated:true,totpExportBlocked:true,totpOperationInFlight:true,canResetTotp:false,canSetupTotp:false,canCompleteTotp:false,canDownload:false,loginMode:"password",hasTotpKey:true};
`

func TestFrontendTOTPActiveProtectionDoesNotAskForManualRecovery(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, activeTOTPFixture+`
var badge=RepairUI.LoginMethodBadge({job:currentReset});
assert(content(badge).includes("重置中")&&!content(badge).includes("人工处理"),"active reset safety barrier became manual recovery in account second line");
var tree=RepairUI.JobRow({job:currentReset,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true});
var action=findButton(tree,"重置中");
assert(action&&action.props.disabled&&!findButton(tree,"需人工处理"),"active reset button mislabeled its safety lock");
assert(!findButton(tree,"设置 2FA")&&requests.length===0,"activity explanation enabled another security request");
`)
}

func TestFrontendTOTPActiveWorkflowKeepsProgressAndUncommittedEvidence(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, activeTOTPFixture+`
var before=JSON.stringify(currentReset),view=Browser.describeJob(currentReset);
assert(view.workflow.steps[0].state==="running"&&!view.workflow.uncertain,"normal write-ahead safety flags turned running workflow into unknown result");
assert(!view.status.label.includes("人工")&&view.status.spinning&&Browser.uploadBlockedJob(currentReset),"active presentation removed the upload safety gate");
var saving={...currentReset,stage:"totp_reset_complete",totpActivationVerified:true,totpCompletionPending:true,totpResetPhase:"activation_verified"};
view=Browser.describeJob(saving);
assert(view.summary.includes("正在")&&view.summary.includes("本地")&&!view.summary.includes("待完成"),"automatic local commit was described as waiting for a new manual action");
assert(view.workflow.steps[0].state!=="succeeded"&&!view.summary.includes("新 2FA 已保存"),"activation evidence became completed local persistence");
var stopped={...saving,status:"failed",operationStatus:"failed",totpWorkflowStatus:"failed",totpOperationInFlight:false,canCompleteTotp:true};
view=Browser.describeJob(stopped);
assert(view.status.code==="totp_completion_pending"&&view.workflow.steps[0].state!=="succeeded"&&Browser.uploadBlockedJob(stopped),"stopped local-only recovery lost its distinct pending-save evidence");
assert(JSON.stringify(currentReset)===before,"display changed authoritative state");
`)
}

func TestFrontendTOTPSharedProtectionDoesNotInventManualFailure(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, activeTOTPFixture+`
var peer={...currentReset,lastOperationType:"relogin",operationStatus:"succeeded",status:"completed",totpRecoveryPending:false,totpCredentialInvalidated:false,totpWorkflowStatus:"",totpOperationInFlight:true};
var tree=RepairUI.JobRow({job:peer,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true}),button=findButton(tree,"2FA处理中");
assert(button&&button.props.disabled&&content(RepairUI.LoginMethodBadge({job:peer})).includes("同账号"),"same-email running reset became a manual failure on its historical row");
hookSlots=[];hookCursor=0;var protectedOnly={...peer,totpOperationInFlight:false};
tree=RepairUI.JobRow({job:protectedOnly,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true});
button=findButton(tree,"安全保护");assert(button&&button.props.disabled&&!findButton(tree,"需人工处理"),"export barrier alone invented an unknown 2FA modification");
assert(requests.length===0,"display performed account work");
`)
}

func TestFrontendTOTPWaitingStillAcceptsOnlyItsActualInputStage(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const [status,label,placeholder] of [["password","等待密码","输入账号密码"],["email_otp","等待邮箱码","6 位邮箱验证码"],["mfa_otp","等待 2FA","6 位 2FA 验证码"],["phone","等待手机号","+60123456789"],["phone_otp","等待手机码","手机验证码"]]){
 hookSlots=[];hookCursor=0;const job={...currentReset,status,operationStatus:"waiting",prompt:"请提供本次验证所需输入"};
 const tree=RepairUI.JobRow({job,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true}),button=findButton(tree,label);
 assert(button&&button.props.disabled&&content(RepairUI.LoginMethodBadge({job})).includes(label),"actual waiting stage became generic recovery: "+status);
 const input=findMatching(tree,node=>node.type==="input"&&node.props.placeholder===placeholder);assert(input,"active reset lost required input: "+status);
}
hookSlots=[];hookCursor=0;const setup={...currentReset,lastOperationType:"setup_2fa",operation:"setup-2fa",status:"totp_setup_otp",operationStatus:"waiting",hasTotpKey:false};
var tree=RepairUI.JobRow({job:setup,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true});
assert(findButton(tree,"等待激活 2FA")?.props.disabled&&findMatching(tree,node=>node.type==="input"&&node.props.placeholder==="新 2FA 的 6 位验证码"),"first setup's protected candidate lost the real activation input");
`)
}

func TestFrontendTOTPActiveWarningsAndSavedFollowupsRemainDistinct(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const [stage,label,prompt,patch] of [
 ["totp_logout_unknown","退出结果待核实","新 2FA 已保存；退出结果未知，后续步骤停止",{totpResetCommitted:true,totpActivationVerified:true,totpRecoveryPending:false,totpCredentialInvalidated:false,totpWorkflowStep:"logout",logoutAllDevicesStatus:"unknown",logoutAllDevicesAfterTotp:true}],
 ["totp_logout_failed","退出设备失败","新 2FA 已保存；退出被拒绝，后续步骤停止",{totpResetCommitted:true,totpActivationVerified:true,totpRecoveryPending:false,totpCredentialInvalidated:false,totpWorkflowStep:"logout",logoutAllDevicesStatus:"failed",logoutAllDevicesAfterTotp:true}],
 ["totp_setup_seed_unverified","2FA 密钥待核实","人工验证码已激活，但候选密钥尚未证明正确",{lastOperationType:"setup_2fa",totpResetStatus:"",totpActivationVerified:false}],
 ["profile_unknown","资料结果待核实","资料提交结果未确认，不重复提交",{}],
]){
 const job={...currentReset,...patch,stage,prompt},view=Browser.describeJob(job),totp=Browser.describeTotpPresentation(job);
 assert(view.status.label===label&&view.status.tone==="warning"&&!view.status.spinning&&view.summary===prompt,"in-flight safety state hid a real stage warning: "+stage);
 assert(totp.active&&totp.actionLabel==="结果待核实"&&Browser.uploadBlockedJob(job),"warning offered a repeat operation: "+stage);
}
var unknown={...currentReset,totpResetStatus:"unknown"},view=Browser.describeJob(unknown);
assert(view.status.code==="totp_result_unknown"&&view.workflow.steps[0].state==="unknown"&&view.notices.some(note=>note.key==="totp-uncertain"),"running request concealed a genuinely unknown modification");
unknown={...unknown,status:"email_otp",operationStatus:"waiting"};view=Browser.describeJob(unknown);
assert(view.status.label==="等待邮箱码"&&view.notices.some(note=>note.key==="totp-uncertain"),"actual input or independent unknown evidence disappeared");
for(const step of ["logout","relogin","upload"]){
 const job={...currentReset,totpRecoveryPending:false,totpCredentialInvalidated:false,totpResetCommitted:true,totpActivationVerified:true,totpResetPhase:"credential_persisted",totpWorkflowStep:step,logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:step==="logout"?"running":"succeeded",reloginAfterTotp:true,totpReloginStatus:step==="relogin"?"running":"succeeded",uploadAfterTotp:true,totpUploadStatus:step==="upload"?"running":"pending",prompt:"正在执行所选后续步骤"};
 const view=Browser.describeJob(job);assert(view.totp.label==="2FA 已保存"&&view.totp.actionLabel==="后续处理中"&&view.workflow.steps[0].state==="succeeded"&&Browser.uploadBlockedJob(job),"confirmed local key or unfinished followup gate changed: "+step);
}
const security=Browser.describeJob({...currentReset,securityCheckRequired:true}).notices.find(note=>note.key==="security");
assert(security&&security.tone==="warning"&&security.text.includes("网站验证")&&security.text.includes("自动重登仍暂停")&&!security.text.includes("需要人工核验"),"active security reminder was removed or demanded unrelated manual intervention");
`)
}

func TestFrontendTOTPInactiveEvidenceDoesNotUseStaleRuntimeOrGrantEligibility(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const status of ["failed","canceled","interrupted","succeeded"]){
 hookSlots=[];hookCursor=0;const job={...currentReset,status:"completed",operationStatus:status,totpOperationInFlight:false,totpWorkflowStatus:status,canResetTotp:true};
 const tree=RepairUI.JobRow({job,token:"fixture",totpResetAvailable:true,totpSetupAvailable:true});
 assert(findButton(tree,"需人工处理")?.props.disabled&&content(RepairUI.LoginMethodBadge({job})).includes("需人工处理"),"terminal quarantine was hidden or stale capability became actionable: "+status);
}
hookSlots=[];hookCursor=0;const saving={...currentReset,lastOperationType:"complete_2fa",totpCompletionPending:true,totpActivationVerified:true,canCompleteTotp:true};
var tree=RepairUI.JobRow({job:saving,token:"fixture",totpResetAvailable:true});assert(findButton(tree,"保存中")?.props.disabled,"in-flight local save became a duplicate action from stale capability");
hookSlots=[];hookCursor=0;const done={...currentReset,status:"completed",operationStatus:"succeeded",totpWorkflowStatus:"succeeded",totpRecoveryPending:false,totpCredentialInvalidated:false,totpOperationInFlight:false,totpExportBlocked:false,totpResetCommitted:true,totpActivationVerified:true,canResetTotp:true};
tree=RepairUI.JobRow({job:done,token:"fixture",totpResetAvailable:true});assert(!findButton(tree,"重置 2FA").props.disabled&&content(RepairUI.LoginMethodBadge({job:done}))==="密码 + 2FA","completed authoritative eligibility was lost");
assert(requests.length===0,"presentation or eligibility check initiated requests");
`)
}

func TestFrontendTOTPCommitEvidenceAndRunnerOwnershipRemainDistinct(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const verified of [false,undefined]){
 const job={...currentReset,totpResetCommitted:true,totpActivationVerified:verified,totpResetPhase:"credential_persisted",totpWorkflowStep:"logout",logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:"running"};
 const view=Browser.describeJob(job);assert(view.totp.label!=="2FA 已保存"&&view.workflow.steps[0].state!=="succeeded","local commit marker alone invented verified saved credentials");
}
const incomplete={...currentReset,status:"completed",operationStatus:"succeeded",totpWorkflowStatus:"succeeded",totpOperationInFlight:false,totpRecoveryPending:false,totpCredentialInvalidated:false,totpResetCommitted:false,totpActivationVerified:true,totpResetPhase:"credential_persisted"};
assert(Browser.describeWorkflow(incomplete).steps[0].state!=="succeeded","modern uncommitted activation fell through the legacy saved phase");
for(const patch of [{totpCompletionPending:true},{totpRecoveryPending:true},{totpResetStatus:"unknown"},{stage:"totp_setup_seed_unverified",lastOperationType:"setup_2fa"}]){
 const job={...incomplete,totpResetCommitted:true,...patch};assert(!Browser.describeTotpPresentation(job).saved&&Browser.describeWorkflow(job).steps[0].state!=="succeeded","old saved flags suppressed explicit incomplete or unknown evidence");
}
for(const status of ["failed","canceled","interrupted"]){
 const job={...currentReset,status:"failed",operationStatus:status,totpWorkflowStatus:"failed",stage:"totp_logout_save_failed",totpWorkflowStep:"logout",totpResetCommitted:true,totpActivationVerified:true,totpRecoveryPending:false,totpCredentialInvalidated:false,logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:"unknown"};
 const view=Browser.describeJob(job);assert(!view.totp.active&&view.totp.hint.includes("流程尚未收尾")&&!view.totp.hint.includes("另一个"),"same-email in-flight flag claimed another operation or restarted a stopped one");
 assert(!view.status.spinning&&view.summary.includes("新 2FA 已保存")&&view.summary.includes(status==="canceled"?"取消":status==="interrupted"?"中断":"退出设备结果待核实"),"runner cleanup replaced the independent stopped outcome");
}
const stale=Browser.describeJob({...currentReset,totpOperationInFlight:false});assert(!stale.totp.active&&stale.totp.phase==="syncing"&&stale.status.code==="totp_state_syncing"&&!stale.status.spinning&&stale.workflow===null,"stale running strings overrode an explicit idle ownership snapshot");
const legacy={...currentReset};delete legacy.totpOperationInFlight;delete legacy.totpActiveOperationAt;assert(Browser.describeTotpPresentation(legacy).active,"older server response lost compatible current progress");
const queued=Browser.describeTotpPresentation({...currentReset,lastOperationType:"complete_2fa",operationStatus:"queued",totpCompletionPending:true,totpActivationVerified:true});assert(queued.phase==="queued"&&queued.actionLabel==="等待保存","queued local completion appeared manual or already running");
`)
}

func TestFrontendTOTPCurrentGenerationOwnsStageAndObservedOutcome(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const [owner,operationAt] of [["",currentReset.lastOperationAt],["2026-11-24T02:00:01Z",currentReset.lastOperationAt],["invalid","invalid"],["0001-01-01T00:00:00Z","0001-01-01T00:00:00Z"],[null,currentReset.lastOperationAt]]){
 const job={...currentReset,totpActiveOperationAt:owner,lastOperationAt:operationAt,stage:"totp_setup_seed_unverified"};
 const view=Browser.describeJob(job);assert(!view.totp.active&&view.totp.settling&&view.totp.hint.includes("流程尚未收尾")&&!view.totp.label.includes("密钥待核实")&&view.status.code==="totp_owner_in_flight"&&view.workflow===null,"different email owner or same-ID previous generation borrowed this row's stage");
}
const saved={...currentReset,totpResetCommitted:true,totpActivationVerified:true,totpRecoveryPending:false,totpCredentialInvalidated:false,totpResetPhase:"credential_persisted",totpWorkflowStep:"logout",logoutAllDevicesAfterTotp:true,logoutAllDevicesStatus:"pending"};
for(const [stage,outcome] of [["totp_logout_unknown","unknown"],["totp_logout_failed","failed"]]){
 const view=Browser.describeJob({...saved,stage});assert(view.workflow.steps[1].state===outcome&&!view.status.spinning&&view.status.tone==="warning","current stage receipt was overwritten by the older logout status");
}
const finishing=Browser.describeJob({...saved,totpWorkflowStatus:"failed",stage:"totp_logout_save_failed",logoutAllDevicesStatus:"unknown"});
assert(finishing.totp.active&&finishing.totp.finishing&&finishing.totp.actionLabel==="收尾中"&&finishing.workflow.finished&&!finishing.status.spinning&&finishing.summary.includes("退出设备结果待核实"),"current generation's committed terminal workflow became another reset in progress");
const old={...saved,totpWorkflowStatus:"succeeded"};delete old.totpActiveOperationAt;
assert(Browser.describeWorkflow(old)===null,"legacy active row borrowed an unversioned old successful workflow");
`)
}

func TestFrontendTOTPTrustedLocalCompletionAndCurrentInputOwnershipStayIndependent(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const inFlight of [false,true]){
 for(const status of ["email_otp","mfa_otp","totp_setup_otp"]){
  hookSlots=[];hookCursor=0;const job={...currentReset,operationStatus:"waiting",status,totpActiveOperationAt:"",totpOperationInFlight:inFlight};
  const tree=RepairUI.JobRow({job,token:"fixture",totpResetAvailable:true});assert(!findMatching(tree,node=>node.type==="input"&&node.props.autoComplete==="one-time-code"),"old waiting stage offered input without current ownership");
 }
}
hookSlots=[];hookCursor=0;const job={...currentReset,totpResetStatus:"unknown",status:"failed",operationStatus:"failed",totpWorkflowStatus:"failed",totpOperationInFlight:false,totpActiveOperationAt:"",totpCompletionPending:true,totpActivationVerified:true,canCompleteTotp:true};
const tree=RepairUI.JobRow({job,token:"fixture",totpResetAvailable:true});assert(!findButton(tree,"完成本地保存")?.props.disabled,"older unknown metadata suppressed trusted journal-backed local completion");
assert(requests.length===0,"inactive owner read credentials or sent account input");
`)
	vm = frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, activeTOTPFixture+`
for(const active of [false,true]){
 const job={...currentReset,lastOperationType:active?"complete_2fa":"reset_2fa",totpResetStatus:"unknown",operationStatus:active?"running":"failed",status:active?"working":"failed",totpWorkflowStatus:"failed",totpOperationInFlight:active,totpActiveOperationAt:active?currentReset.lastOperationAt:"",totpCompletionPending:true,totpActivationVerified:true,canCompleteTotp:!active};
 const view=Browser.describeJob(job);assert(view.status.code===(active?"totp_local_saving":"totp_completion_pending")&&view.notices.some(note=>note.key==="totp-confirmed-recovery")&&Browser.uploadBlockedJob(job),"trusted current completion or independent old metadata reminder disappeared");
}
`)
}
