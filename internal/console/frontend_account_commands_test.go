package console

import "testing"

func TestFrontendAccountCommandReceiptBoundary(t *testing.T) {
	vm := frontendBrowserVM(t, "accountCommandOutcome")
	frontendPollingRun(t, vm, `
function unverified(run,label){let caught;try{run()}catch(error){caught=error}assert(caught?.code==="ACCOUNT_RECEIPT_UNVERIFIED"&&caught.outcome==="unknown","invalid receipt accepted: "+label);}
const ids=["one","two"],good={ok:true,outcome:"queued",selected:2,queued:1,started:1,skipped:1,failed:0,unknown:0,uncertain:false,jobs:[{id:"one"}],failures:[]};
assert(Browser.decodeBatchSubmission(good,ids).confirmedIds.join()==="one","queued receipt did not confirm exactly the accepted ID");
assert(Browser.decodeBatchSubmission({started:1,skipped:1,jobs:[{id:"one"}]},ids).queued===1,"legacy started alias was lost");
for(const patch of [{queued:2},{started:0},{skipped:"1"},{selected:3},{outcome:"completed"},{jobs:[{id:"outside"}]},{jobs:[{id:"one"},{id:"one"}],queued:2,started:2,skipped:0},{failures:[{id:"one",error:"contradiction"}],failed:1,skipped:0,ok:false,outcome:"partial"},{unknown:1,skipped:0,uncertain:false,ok:false,outcome:"partial"}])unverified(()=>Browser.decodeBatchSubmission({...good,...patch},ids),JSON.stringify(patch));
unverified(()=>Browser.decodeSingleSubmission({ok:true,job:{id:"other"}},{id:"one"}),"single identity");
unverified(()=>Browser.decodeSingleSubmission({ok:true},{id:"one"}),"empty single receipt");
for(const outcome of ["unknown","failed","partial","future_status",null,0]){
 unverified(()=>Browser.decodeSingleSubmission({job:{id:"one"},outcome},{id:"one"}),"single explicit outcome");
 unverified(()=>Browser.decodeCancellationReceipt({canceled:1,outcome}),"cancellation explicit outcome");
}
unverified(()=>Browser.decodeCancellationReceipt({canceled:1,outcome:"queued"}),"cancellation only accepted");
assert(Browser.decodeCancellationReceipt({canceled:0}).canceled===0,"current zero-cancellation success changed");
assert(Browser.decodeSingleSubmission({job:{id:"one"}},{id:"one"}).id==="one","current single success contract changed");
unverified(()=>Browser.decodeImportReceipt({created:1,updated:0,jobs:[{id:"one",email:"one@example.invalid"}]},2),"import lost row");
const repeated=Browser.decodeImportReceipt({created:1,updated:1,jobs:[{id:"one",email:"one@example.invalid"},{id:"one",email:"one@example.invalid"}]},2);
assert(repeated.completed===2,"repeated email updates were incorrectly rejected");
const cpa={ok:false,outcome:"partial",uncertain:true,selected:2,uploaded:1,attempted:2,skipped:0,result:{success:1},results:[{id:"one",outcome:"succeeded"},{id:"two",outcome:"unknown"}]};
assert(Browser.decodeUploadReceipt(cpa,ids,"cpa").confirmedIds.join()==="one","CPA partial confirmation lost exact success");
for(const patch of [{ok:true},{outcome:"completed"},{uncertain:false},{uploaded:2},{skipped:1},{results:[{id:"one",outcome:"succeeded"},{id:"one",outcome:"unknown"}]},{result:{success:1,unknown:0}},{result:{success:1,failed:1}},{result:{success:1,not_attempted:1}}])unverified(()=>Browser.decodeUploadReceipt({...cpa,...patch},ids,"cpa"),JSON.stringify(patch));
assert(Browser.decodeUploadReceipt({ok:true,result:{success:2},uploaded:2},ids,"cpa").confirmedIds.length===0,"old aggregate CPA receipt fabricated per-ID proof");
for(const outcome of ["partial","unknown"]){const result=Browser.decodeUploadReceipt({ok:false,outcome,uncertain:outcome==="unknown",selected:2,uploaded:2,skipped:0,result:{created:1,updated:0,success:1,failed:outcome==="partial"?1:0,unknown:outcome==="unknown"?1:0}},ids,"sub2api");assert(result.confirmedIds.length===0,"Sub2 aggregate receipt invented local success IDs");}
assert(Browser.decodeUploadReceipt({ok:true,selected:2,uploaded:2,skipped:0,result:{created:1,updated:1,success:2}},ids,"sub2api").confirmedIds.join()==="one,two","complete Sub2 receipt did not confirm the batch");
for(const receipt of [
 {ok:true,outcome:"partial",result:{created:2,success:2}},
 {ok:true,result:{created:1,success:1}},
 {ok:true,result:{ok:false,created:2,success:2}},
 {ok:true,result:{ok:"false",created:2,success:2}},
 {ok:true,result:{created:1,success:1,unknown:1}}
])unverified(()=>Browser.decodeUploadReceipt(receipt,ids,"sub2api"),"Sub2 contradictory success");
`)
}

func TestFrontendSingleTOTPReceiptRejectsUnconfirmedOutcome(t *testing.T) {
	for _, outcome := range []string{"unknown", "failed", "partial", "future_status"} {
		for _, flags := range []bool{false, true} {
			vm := frontendAppVM(t)
			vm.Set("receiptOutcome", outcome)
			vm.Set("contradictoryFlags", flags)
			frontendPollingRun(t, vm, `
prepareBatch();var errors=[];
findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id==="queued").props.onSetupTotp(true);
const dialog=find(render(),"TotpActionDialog"),intent=dialog.props.action.totpIntentId;
dialog.props.onSubmit({}).catch(error=>errors.push(error));
requests[0].resolve({ok:true,status:200,json:async()=>({job:{id:"queued"},outcome:receiptOutcome,...(contradictoryFlags?{ok:true,uncertain:false}:{})})});
`)
			frontendPollingRun(t, vm, `
assert(errors.length===1&&errors[0].code==="ACCOUNT_RECEIPT_UNVERIFIED"&&find(render(),"TotpActionDialog"),"unconfirmed single TOTP response was acknowledged or dismissed");
assert(appState.selectedJobIds.size===3&&storage.get("tosub2.totp-intent.queued")===intent,"unconfirmed single TOTP response cleared intent or selection");
find(render(),"TotpActionDialog").props.onClose();findMatching(render(),node=>node.type==="JobRow"&&node.props.job.id==="queued").props.onSetupTotp(true);
assert(find(render(),"TotpActionDialog").props.action.totpIntentId===intent&&requests.length===1,"reopened TOTP dialog lost original intent or replayed mutation");
`)
		}
	}
}

func TestFrontendCancellationReceiptRejectsUnconfirmedOutcome(t *testing.T) {
	for _, outcome := range []string{"unknown", "failed", "partial", "future_status", "queued"} {
		for _, reselection := range []bool{false, true} {
			vm := frontendAppVM(t)
			vm.Set("receiptOutcome", outcome)
			vm.Set("reselection", reselection)
			frontendPollingRun(t, vm, `
prepareBatch();appState.setFeatures({...appState.features,cancelAll:true});appState.setStats({active:1,queued:1,completed:1});
findButton(render(),"停止全部").props.onClick();
`)
			frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/jobs/cancel-all","cancel command was not submitted once");
if(reselection){appState.setSelectedJobIds(new Set(["queued"]));render();}
requests[0].resolve({ok:true,status:200,json:async()=>({canceled:1,outcome:receiptOutcome,...(reselection?{ok:true,uncertain:false}:{})})});
`)
			frontendPollingRun(t, vm, `
assert(content(render()).includes("操作回执不完整")&&appState.selectedJobIds.size===(reselection?1:3)&&appState.selectedJobIds.has("queued"),"unconfirmed cancellation cleared selection or suppressed uncertainty");
assert(!findButton(render(),"停止全部").props.disabled&&requests.length===1,"rejected cancellation receipt retained busy lock or replayed mutation");
`)
		}
	}
}

func TestFrontendAccountCommandsRejectMalformedBatchWithoutLosingIntent(t *testing.T) {
	for _, mode := range []string{"foreign", "duplicate", "count", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			vm := frontendAppVM(t)
			vm.Set("mode", mode)
			frontendPollingRun(t, vm, `
prepareBatch();var errors=[];
findButton(render(),"重置 2FA").props.onClick();
var action=find(render(),"TotpActionDialog"),intent=action.props.action.totpIntentId;
action.props.onSubmit({}).catch(error=>errors.push(error));
const receipt={ok:true,outcome:"queued",selected:3,queued:3,started:3,skipped:0,failed:0,unknown:0,uncertain:false,jobs:[{id:"queued"},{id:"failed"},{id:"skipped"}],failures:[]};
if(mode==="foreign")receipt.jobs[0]={id:"foreign"};
if(mode==="duplicate")receipt.jobs[1]={id:"queued"};
if(mode==="count")receipt.started=0;
if(mode==="unknown"){Object.assign(receipt,{ok:false,outcome:"partial",queued:1,started:1,unknown:2,uncertain:true,jobs:[{id:"queued"}]});}
requests[0].resolve({ok:true,status:200,json:async()=>receipt});
`)
			frontendPollingRun(t, vm, `
if(mode==="unknown"){
 assert(!find(render(),"TotpActionDialog")&&errors.length===0,"structured unknown receipt was not presented");
 assert(appState.selectedJobIds.size===2&&appState.selectedJobIds.has("failed"),"unknown item lost selection");
 appState.setSelectedJobIds(new Set(["queued","failed","skipped"]));render();
}else{
 assert(errors.length===1&&errors[0].outcome==="unknown"&&appState.selectedJobIds.size===3,"unverified response cleared selection or lost uncertainty");
 find(render(),"TotpActionDialog").props.onClose();
}
findButton(render(),"重置 2FA").props.onClick();
assert(find(render(),"TotpActionDialog").props.action.totpIntentId===intent,"unconfirmed reset discarded its deduplication intent");
assert(requests.length===1,"invalid/unknown receipt retried a mutation");
`)
		})
	}
}

func TestFrontendAccountCommandsIgnoreEarlierTokenGeneration(t *testing.T) {
	for _, failure := range []bool{false, true} {
		vm := frontendAppVM(t)
		vm.Set("failure", failure)
		frontendPollingRun(t, vm, `
prepareBatch();var errors=[];
appState.setToken("A");render();findButton(render(),"重登授权").props.onClick();
var old=find(render(),"ReloginActionDialog");old.props.onSubmit({authorizationMethod:"chatgpt"}).catch(error=>errors.push(error));
appState.setToken("B");assert(!find(render(),"ReloginActionDialog"),"token change kept an old command dialog");
appState.setToken("A");render();findButton(render(),"重登授权").props.onClick();
var fresh=find(render(),"ReloginActionDialog").props.action;
old.props.onClose();old.props.onSubmit({authorizationMethod:"chatgpt"});
assert(find(render(),"ReloginActionDialog").props.action===fresh&&requests.length===1,"stale callback closed or submitted a new dialog");
if(failure)requests[0].reject(new Error("synthetic lost old response"));
else requests[0].resolve({ok:true,status:200,json:async()=>({selected:3,queued:3,started:3,skipped:0,jobs:[{id:"queued"},{id:"failed"},{id:"skipped"}]})});
`)
		frontendPollingRun(t, vm, `
assert(find(render(),"ReloginActionDialog").props.action===fresh&&appState.selectedJobIds.size===3&&errors.length===0,"A-B-A response changed the current dialog, selection, or error");
assert(!content(render()).includes("已排队 3 个")&&requests.length===1,"old command was reported or replayed");
`)
	}
}

func TestFrontendAccountCommandsRecheckConfirmationScope(t *testing.T) {
	for _, change := range []string{"eligibility", "operation", "email", "missing"} {
		vm := frontendAppVM(t)
		vm.Set("change", change)
		frontendPollingRun(t, vm, `
prepareBatch();var errors=[];
findButton(render(),"重登授权").props.onClick();var old=find(render(),"ReloginActionDialog");
const changed=appState.jobSelectionIndex.flatMap(job=>job.id!=="queued"?[job]:change==="missing"?[]:[{...job,...(change==="eligibility"?{canForceRelogin:false}:change==="operation"?{lastOperationAt:"later-operation"}:{email:"other@example.invalid"})}]);
appState.setJobSelectionIndex(changed);appState.setJobs(changed);render();
old.props.onSubmit({authorizationMethod:"chatgpt"}).catch(error=>errors.push(error));
`)
		frontendPollingRun(t, vm, `assert(requests.length===0&&errors.length===1&&errors[0].message.includes("重新选择"),"stale confirmation dispatched despite changed account authority");`)
	}
}

func TestFrontendAccountCommandsRejectLateConfirmationAndPreserveReselection(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
prepareBatch();var resolveConfirm;confirmAccepted=new Promise(resolve=>resolveConfirm=resolve);
findButton(render(),"批量添加密码").props.onClick();
appState.setToken("new-session");render();resolveConfirm(true);
`)
	frontendPollingRun(t, vm, `
assert(requests.length===0,"confirmation from an earlier session sent a password mutation");
confirmAccepted=true;findButton(render(),"重登授权").props.onClick();find(render(),"ReloginActionDialog").props.onSubmit({authorizationMethod:"chatgpt"});
appState.setSelectedJobIds(new Set());render();appState.setSelectedJobIds(new Set(["queued","failed"]));render();
requests[0].resolve({ok:true,status:200,json:async()=>({selected:3,queued:3,started:3,skipped:0,jobs:[{id:"queued"},{id:"failed"},{id:"skipped"}]})});
`)
	frontendPollingRun(t, vm, `render();assert(appState.selectedJobIds.size===2&&appState.selectedJobIds.has("queued")&&appState.selectedJobIds.has("failed"),"old receipt cleared new selection of the same account IDs");`)
}

func TestFrontendSub2PartialHTTP200PreservesSelection(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		vm := frontendAppVM(t)
		vm.Set("unknown", unknown)
		frontendPollingRun(t, vm, `
prepareBatch();appState.setFeatures({sub2apiUpload:true,bulkActions:true});render();
findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="上传 Sub2API").props.onClick();
requests[0].resolve({ok:true,status:200,json:async()=>({ok:false,outcome:"partial",uncertain:unknown,selected:3,uploaded:3,skipped:0,result:{created:1,updated:0,success:1,failed:unknown?0:1,unknown:unknown?1:0,not_attempted:1}})});
`)
		frontendPollingRun(t, vm, `render();assert(appState.selectedJobIds.size===3&&requests.length===1&&!content(render()).includes("同步已完成"),"HTTP200 partial aggregate upload was treated as whole-batch success");assert(content(render()).includes(unknown?"结果待核实":"部分同步未完成"),"partial upload lost its outcome");`)
	}
}

func TestFrontendAccountImportLateResponseKeepsNewPrivateDraft(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
prepareBatch();findButton(render(),"批量导入").props.onClick();
findMatching(render(),node=>node.props?.id==="batch-input").props.onChange({target:{value:"old@example.invalid---- old password "}});
findMatching(render(),node=>node.props?.["aria-labelledby"]==="batch-title").props.onSubmit({preventDefault(){}});
appState.setToken("B");render();appState.setToken("synthetic-console-token");render();findButton(render(),"批量导入").props.onClick();
const draft="new@example.invalid----  new password  ";findMatching(render(),node=>node.props?.id==="batch-input").props.onChange({target:{value:draft}});
requests[0].resolve({ok:true,status:201,json:async()=>({created:1,updated:0,jobs:[{id:"old",email:"old@example.invalid",status:"imported"}]})});
`)
	frontendPollingRun(t, vm, `assert(findMatching(render(),node=>node.props?.id==="batch-input").props.value===draft&&!appState.jobs.some(job=>job.id==="old")&&requests.length===1,"old import response cleared a new secret draft or inserted stale account data");`)
}

func TestFrontendAccountImportCancelledFormCannotSubmitReplacementDraft(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
prepareBatch();findButton(render(),"批量导入").props.onClick();
findMatching(render(),node=>node.props?.id==="batch-input").props.onChange({target:{value:"old@example.invalid"}});
var stale=findMatching(render(),node=>node.props?.["aria-labelledby"]==="batch-title"),oldInput=findMatching(render(),node=>node.props?.id==="batch-input");
findButton(stale,"取消").props.onClick();findButton(render(),"批量导入").props.onClick();
const draft="new@example.invalid----  new password  ";findMatching(render(),node=>node.props?.id==="batch-input").props.onChange({target:{value:draft}});
stale.props.onSubmit({preventDefault(){}});oldInput.props.onChange({target:{value:"stale secret"}});
assert(requests.length===0&&findMatching(render(),node=>node.props?.id==="batch-input").props.value===draft,"cancelled import form submitted or changed its replacement draft");
`)
}

func TestFrontendAccountImportReceiptDoesNotReplaceNewerAccountRead(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
prepareBatch();findButton(render(),"批量导入").props.onClick();
findMatching(render(),node=>node.props?.id==="batch-input").props.onChange({target:{value:"queued@synthetic.invalid"}});
findMatching(render(),node=>node.props?.["aria-labelledby"]==="batch-title").props.onSubmit({preventDefault(){}});
const newer=appState.jobs.map(job=>({...job,lastOperationAt:"newer-operation",status:"running"}));appState.setJobs(newer);render();
requests[0].resolve({ok:true,status:201,json:async()=>({created:0,updated:1,jobs:[{id:"queued",email:"queued@synthetic.invalid",status:"imported"}]})});
`)
	frontendPollingRun(t, vm, `render();assert(appState.jobs.find(job=>job.id==="queued").lastOperationAt==="newer-operation"&&appState.jobs.find(job=>job.id==="queued").status==="running","old import receipt overwrote a newer authoritative account read");`)
}

func TestFrontendPrivateDownloadChecksScopeAfterReadingBlob(t *testing.T) {
	vm := frontendBrowserVM(t, "console")
	frontendPollingRun(t, vm, `
var current=true,resolveBlob,created=0,clicked=0;
var URL={createObjectURL(){created++;return "blob:synthetic"},revokeObjectURL(){}},window={setTimeout(){}},document={createElement(){return {click(){clicked++},remove(){}}},body:{append(){}}};
Browser.saveDownloadResponse({blob:()=>new Promise(resolve=>resolveBlob=resolve),headers:{get(){return ""}}},"synthetic.json",()=>current);
current=false;resolveBlob({});
`)
	frontendPollingRun(t, vm, `assert(created===0&&clicked===0,"private download from an earlier session reached a new context");`)
}
