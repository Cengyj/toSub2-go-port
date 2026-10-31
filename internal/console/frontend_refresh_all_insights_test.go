package console

import "testing"

func TestFrontendSelectedUsageRefreshIsDirectToolbarAction(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
const tree=prepareBatch();
const toolbar=findMatching(tree,node=>node.props?.["aria-label"]==="账号批量操作");
const button=toolbar.props.children.flat(Infinity).find(node=>node?.type==="button"&&node.props["aria-label"]==="刷新所选账号用量");
assert(button&&content(button)==="刷新所选用量","usage refresh is not directly visible with the concise title");
const more=findMatching(toolbar,node=>node.type==="ActionMenu"&&node.props.ariaLabel==="打开其他批量操作菜单");
assert(more&&!findButton(more,"刷新所选用量")&&!content(more).includes("刷新所选账号用量"),"usage refresh is duplicated or remains inside More");
assert(button.props.disabled&&button.props.title.includes("启用账号查询"),"moving the action bypassed the disabled-query guard");
assert(typeof find(tree,"AccountsOverview").props.onRefreshAllInsights==="function","moving selected refresh removed the independent all-account action");
assert(requests.length===0,"rendering the direct action submitted work");
`)
}

func TestFrontendRefreshAllInsightsUsesExplicitGlobalScopeAndSingleSubmission(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{},revision:1});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);
var result,duplicate;hook.submitAll().then(value=>{result=value;});hook.submitAll().then(value=>{duplicate=value;});
var writes=requests.filter(r=>r.options?.method==="POST");
assert(writes.length===1&&writes[0].url==="/api/account-insights/refresh","all refresh bypassed single-flight admission");
assert(JSON.stringify(JSON.parse(writes[0].options.body))===JSON.stringify({all:true}),"all refresh used visible or selected account IDs");
assert(renderHook(options).actionBusy,"submission did not disable duplicate actions");
ok(writes[0],{accepted:["off-page"],merged:["inflight"],skipped:[{id:"unauthorized",reason:"尚无授权"}]});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.actionBusy&&duplicate===null&&result.accepted[0]==="off-page","all refresh lost admission result or left actions locked");
hook.submit(["selected","selected"]).then(value=>{result=value;}).catch(error=>{result=error.message;});
writes=requests.filter(r=>r.options?.method==="POST");
assert(writes.length===2&&JSON.stringify(JSON.parse(writes[1].options.body))===JSON.stringify({ids:["selected"]}),"selected refresh changed to all accounts");
writes[1].reject(new Error("synthetic offline"));
unmount();
`)
}

func TestFrontendRefreshAllInsightsRequiresEnabledConfirmedSettings(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options),message="";hook.submitAll().catch(error=>{message=error.message;});`)
	frontendPollingRun(t, vm, `
assert(message.includes("请先确认")&&!requests.some(r=>r.options?.method==="POST"),"unloaded all refresh reached server");
ok(requests[0],{...config,enabled:false});ok(requests[1],{snapshots:{},revision:1});
`)
	frontendPollingRun(t, vm, `hook=renderHook(options);message="";hook.submitAll().catch(error=>{message=error.message;});`)
	frontendPollingRun(t, vm, `assert(message.includes("启用")&&!requests.some(r=>r.options?.method==="POST"),"disabled all refresh reached server");unmount();assert(timers.size===0,"all refresh leaked timers");`)
}

func TestFrontendRefreshAllInsightsNetworkFailureRetainsCacheWithoutReplay(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:{id:"one",usage:{windows:[{usedPercent:37}]}}},revision:1});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);var message="";hook.submitAll().catch(error=>{message=error.message;});
requests.find(r=>r.options?.method==="POST").reject(new Error("synthetic connection interrupted"));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(message&&!hook.actionBusy,"all-query failure was hidden or left the control busy");
assert(hook.snapshots.one.usage.windows[0].usedPercent===37,"failed all-query submission erased valid usage");
assert(requests.filter(r=>r.options?.method==="POST").length===1,"uncertain all-query submission was automatically replayed");
unmount();assert(timers.size===0,"failed all-query submission leaked timers");
`)
}

func TestFrontendAccountOverviewRefreshAllActionAndTitles(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
var clicks=0,props={stats:{active:1,queued:2,completed:3},total:6,insightsEnabled:true,onRefreshAllInsights(){clicks++;}};
var tree=RepairUI.AccountsOverview(props);
assert(content(tree).includes("账号状态与用量"),"overview title did not describe usage");
var button=findButton(tree,"刷新全部账号用量");
assert(button&&!button.props.disabled&&button.props.title.includes("筛选"),"all refresh lacks an explicit global scope");
button.props.onClick();assert(clicks===1,"status bar refresh callback lost");
tree=RepairUI.AccountsOverview({...props,insightsEnabled:false});
assert(!findButton(tree,"刷新全部账号用量")&&content(tree).includes("账号状态"),"disabled feature still showed refresh action");
tree=RepairUI.AccountsOverview({...props,refreshDisabled:true});assert(findButton(tree,"刷新全部账号用量").props.disabled,"unavailable query can still submit");
tree=RepairUI.AccountsOverview({...props,refreshBusy:true});button=findButton(tree,"正在提交查询…");
assert(button?.props.disabled&&button.props["aria-busy"]===true,"busy state is not accessible or permits duplicates");
assert(requests.length===0,"overview performed direct side effects");
`)
}
