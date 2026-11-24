package console

import "testing"

func TestFrontendUploadAdmissionReadsOnlyCompactSafetyFields(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const allowed=new Set(["status","operationStatus","totpExportBlocked","totpRecoveryPending","totpCredentialInvalidated","totpCompletionPending","totpWorkflowStatus"]);
const minimal=new Proxy({status:"completed",operationStatus:"succeeded"},{get(object,key){assert(allowed.has(key),"upload admission read unrelated presentation field "+String(key));return object[key];}});
assert(!Browser.uploadBlockedJob(minimal),"terminal compact account was blocked");
`)
}

func TestFrontendJobTerminalRulePreservesOperationAndLegacySemantics(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const cases=[
 [{status:"completed",operationStatus:"queued"},false],
 [{status:"completed",operationStatus:"running"},false],
 [{status:"completed",operationStatus:"waiting"},false],
 [{status:"working",operationStatus:"succeeded"},true],
 [{status:"working",operationStatus:"failed"},true],
 [{status:"working",operationStatus:"canceled"},true],
 [{status:"working",operationStatus:"interrupted"},true],
 [{status:"working",operationStatus:"skipped"},true],
 [{status:"working",operationStatus:" failed "},true],
 [{status:"completed",operationStatus:"unknown"},true],
 [{status:"working",operationStatus:"unknown"},false],
 ...["imported","completed","failed","canceled","reauth_required","resume_available","interrupted"].map(status=>[{status},true]),
 ...["queued","starting","running","working","refreshing","finalizing","email_otp","mfa_otp","phone_otp","unknown",""].map(status=>[{status},false]),
];
for(const [job,terminal] of cases){
 assert(Browser.isJobTerminal(job)===terminal&&Browser.describeJob(job).terminal===terminal,"shared terminal rule changed current or legacy operation meaning");
 assert(Browser.uploadBlockedJob(job)===!terminal,"upload admission diverged from the reviewed terminal rule");
}
for(const field of ["totpExportBlocked","totpRecoveryPending","totpCredentialInvalidated","totpCompletionPending"]){const job={status:"completed",operationStatus:"succeeded",[field]:true};assert(Browser.isJobTerminal(job)&&Browser.uploadBlockedJob(job),"terminal extraction removed an independent recovery gate");}
for(const status of ["pending","running"]){const job={status:"completed",operationStatus:"succeeded",totpWorkflowStatus:status};assert(Browser.isJobTerminal(job)&&Browser.uploadBlockedJob(job),"terminal extraction removed the active follow-up gate");}
`)
}
