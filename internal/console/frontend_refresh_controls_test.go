package console

import "testing"

func TestFrontendOverviewUsesAuthoritativeRefreshControl(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
const props={stats:{},total:6,insightsEnabled:true,refreshSummary:{disabled:true,busy:true,phase:"queued",label:"排队中（2）",description:"2 个账号排队，等待查询完成后可再次刷新。"}};
const tree=RepairUI.AccountsOverview(props);
assert(content(tree).includes(props.refreshSummary.description),"removing the global action also hid authoritative query status");
assert(findMatching(tree,node=>node.props?.role==="status"&&content(node)===props.refreshSummary.description),"query status lost its accessible live description");
assert(!findMatching(tree,node=>node.type==="button"),"read-only query summary recreated a refresh action");
assert(requests.length===0,"rendering refresh state submitted a request");
`)
}

func TestFrontendOverviewOffersReadOnlyStatusRecovery(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
var checks=0;
const props={stats:{},total:6,insightsEnabled:true,refreshError:"合成网络中断",onVerifyRefresh(){checks++;},refreshSummary:{disabled:true,busy:false,label:"状态待确认",description:"先核对查询状态，不会重新提交。"}};
const tree=RepairUI.AccountsOverview(props);
assert(!findButton(tree,"状态待确认")&&!findButton(tree,"刷新全部用量"),"unknown summary recreated a global submission control");
const verify=findButton(tree,"核对刷新状态");
assert(verify,"unknown state has no read-only recovery action");
verify.props.onClick();
assert(checks===1&&requests.length===0,"status recovery used a refresh submission");
`)
}

func TestFrontendRefreshReconciliationKeepsCancellationAvailable(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, `
var canceled=0;
var job={id:"one",email:"synthetic@example.invalid",authorizationMethod:"chatgpt"};
var snapshot={id:"one",authorizationMethod:"chatgpt",canQuery:false,lastAttempt:{status:"running"}};
var insights={settings:config,settingsLoaded:true,settingsLoading:false,verificationRequired:false,actionBusy:false,refreshPending:true,saving:false,revision:0,cancel:async()=>{canceled++;return {cancelled:true};}};
var tree=renderComponent("AccountInsightsDialog",{job,token:"synthetic",initialSnapshot:snapshot,insights,onClose(){}});
assert(findButton(tree,"立即查询").props.disabled,"pending reconciliation permits duplicate refresh");
assert(!findButton(tree,"取消查询").props.disabled,"refresh lifecycle locked the cancellation control");
findButton(tree,"取消查询").props.onClick();
`)
	frontendPollingRun(t, vm, `assert(canceled===1,"running query could not be canceled");unmount();assert(timers.size===0,"dialog leaked polling after cancellation");`)
}
