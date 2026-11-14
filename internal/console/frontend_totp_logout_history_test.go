package console

import "testing"

func TestFrontendTOTPCompletionKeepsPriorLogoutFailureInHistory(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
for (const operation of ["complete-2fa"]) {
  for (const operationStatus of ["succeeded", "failed"]) {
    const job = {operation, lastOperationType:operation, operationStatus, status:"failed", totpRecoveryPending:true, totpCredentialInvalidated:true, totpWorkflowStatus:operationStatus, logoutAllDevicesAfterTotp:false, logoutAllDevicesStatus:"unknown", logoutAllDevicesError:"synthetic prior logout failed"};
    const view = Browser.describeJob(job);
    const historical = view.historyNotices.find(notice=>notice.key==="old-logout");
    assert(historical && historical.title==="此前退出设备" && historical.text===job.logoutAllDevicesError, "recovery lost historical logout evidence");
    assert(!view.notices.some(notice=>notice.key==="logout"), "recovery presented prior logout as this attempt");
    assert(view.workflow===null && Browser.uploadBlockedJob(job), "history enabled a follow-up workflow or upload");
  }
}

`)
}

func TestFrontendTOTPCompletionBlocksUploadForEveryPendingState(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const complete = {operation:"complete-2fa", lastOperationType:"complete-2fa", operationStatus:"succeeded", status:"failed", totpWorkflowStatus:"succeeded", canDownload:true};
assert(!Browser.uploadBlockedJob(complete), "finished observation without recovery state was blocked");
for (const field of ["totpExportBlocked", "totpRecoveryPending", "totpCredentialInvalidated", "totpCompletionPending"]) {
  assert(Browser.uploadBlockedJob({...complete, [field]:true}), "pending recovery state enabled upload: "+field);
}
`)
}
