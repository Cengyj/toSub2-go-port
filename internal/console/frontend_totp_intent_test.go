package console

import "testing"

func TestFrontendTOTPIntentSurvivesLostResponseAndDialogRetry(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
var tree=prepareBatch(), handlerErrors=[];
var recoveryJob={id:"intent",email:"intent@synthetic.invalid",status:"failed",operationStatus:"failed",canResetTotp:true,totpKnownEnabled:true};
appState.setJobs([recoveryJob]);appState.setJobSelectionIndex([recoveryJob]);appState.setSelectedJobIds(new Set());tree=render();
findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="intent").props.onSetupTotp(true);
var dialog=find(render(),"TotpActionDialog"),firstIntent=dialog.props.action.totpIntentId;
dialog.props.onSubmit({}).catch(error=>handlerErrors.push(error));
`)
	frontendPollingRun(t, vm, `requests[0].reject(new Error("response lost"));`)
	frontendPollingRun(t, vm, `
assert(handlerErrors.length===1,"lost response was not reported");
find(render(),"TotpActionDialog").props.onClose();tree=render();
findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="intent").props.onSetupTotp(true);
dialog=find(render(),"TotpActionDialog");assert(dialog.props.action.totpIntentId===firstIntent,"lost response replaced the original request intent");
dialog.props.onSubmit({}).catch(error=>handlerErrors.push(error));
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2&&JSON.parse(requests[1].options.body).totpIntentId===firstIntent,"retry created a second reset intent");
requests[1].resolve({ok:true,status:200,json:async()=>({ok:true})});
`)
	frontendPollingRun(t, vm, `
tree=render();findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="intent").props.onSetupTotp(true);
assert(find(render(),"TotpActionDialog").props.action.totpIntentId!==firstIntent,"new explicit operation reused an acknowledged request");
`)
}
