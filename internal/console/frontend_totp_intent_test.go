package console

import "testing"

func TestFrontendResetDialogPromisesOnlyNewKeyAndLogoutReceipt(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
var submitted=0;
const tree=RepairUI.TotpActionDialog({action:{resetTotp:true}, logoutAvailable:true,onSubmit(){submitted++;return Promise.resolve()},onClose(){}});
const text=content(tree), logout=findMatching(tree,n=>n.props?.title==='退出所有设备'&&n.props.description);
assert(text.includes('新密钥')&&!text.includes('旧验证器随即失效')&&text.includes('不能证明所有旧密钥已失效'),'reset promised blanket revocation');
assert(logout&&logout.props.description.includes('请求')&&!logout.props.description.includes('退出所有登录会话。'),'logout presented as verified disconnection');
find(tree,'form').props.onSubmit({preventDefault(){}});assert(submitted===1&&requests.length===0,'dialog added a confirmation or unsolicited network read');
`)
}

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
requests[1].resolve({ok:true,status:200,json:async()=>({ok:true,job:recoveryJob})});
`)
	frontendPollingRun(t, vm, `
tree=render();findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="intent").props.onSetupTotp(true);
assert(find(render(),"TotpActionDialog").props.action.totpIntentId!==firstIntent,"new explicit operation reused an acknowledged request");
`)
}
