package console

import "testing"

func TestFrontendSelectedUsageRefreshIsDirectToolbarAction(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
const tree=prepareBatch();
const toolbar=findMatching(tree,node=>node.props?.["aria-label"]==="账号批量操作");
const button=toolbar.props.children.flat(Infinity).find(node=>node?.type==="button"&&node.props["aria-label"]==="刷新所选账号用量");
assert(button&&content(button)==="刷新所选用量","selected usage refresh is no longer a visible toolbar action");
const description=findMatching(toolbar,node=>node.props?.id==="selected-insights-refresh-description");
assert(button.props["aria-describedby"]===description?.props.id,"selected refresh lost its accessible scope description");
assert(description.props.className==="sr-only"&&!description.props.role,"selected toolbar adds duplicate live announcements");
const more=findMatching(toolbar,node=>node.type==="ActionMenu"&&node.props.ariaLabel==="打开其他批量操作菜单");
assert(more&&!findButton(more,"刷新所选用量"),"selected refresh moved into More or was duplicated");
assert(button.props.disabled&&button.props.title.includes("启用账号查询"),"toolbar action bypassed the disabled-query guard");
appState.setFeatures({bulkActions:true,accountTrash:true});const pageTree=render();
assert(!find(pageTree,"AccountsPageActions")&&!findMatching(pageTree,node=>node.type==="button"&&node.props["aria-label"]==="刷新全部账号用量"),"deleted all-account refresh reappeared");
assert(requests.length===0,"rendering selected refresh submitted work");
`)
}

func TestFrontendSelectedInsightsRefreshUsesOnlySelectedIDsAndSingleSubmission(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,ids:["selected","unselected"],selectedIds:["selected"]};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{},revision:1});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(typeof hook.submitAll==="undefined"&&!("allRefreshControl" in hook),"removed global UI left a callable hook entrypoint");
var result,duplicate;hook.submit(["selected","selected"]).then(value=>result=value);hook.submit(["selected"]).then(value=>duplicate=value);
var writes=requests.filter(r=>r.options?.method==="POST");
assert(writes.length===1&&writes[0].url==="/api/account-insights/refresh","selected refresh bypassed single-flight admission");
assert(JSON.stringify(JSON.parse(writes[0].options.body))===JSON.stringify({ids:["selected"]}),"selected refresh included unselected accounts or used global scope");
assert(renderHook(options).actionBusy&&renderHook(options).selectedRefreshControl.disabled,"submission did not disable duplicate actions");
ok(writes[0],{queryId:"synthetic-selected",accepted:["selected"],merged:[],skipped:[]});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.actionBusy&&hook.refreshPending&&duplicate===null&&result.accepted[0]==="selected","selected refresh lost receipt or treated admission as completion");
var latestStatus=requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1);
ok(latestStatus,{...availableRefreshStatus(1),refresh:{...availableRefreshStatus().refresh,total:3,eligible:1,active:2}});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.refreshPending&&hook.refreshSummary.busy&&!hook.selectedRefreshControl.disabled,"global summary activity blocked unrelated eligible selected accounts");
assert(requests.filter(r=>r.options?.method==="POST").length===1,"status reconciliation automatically repeated the selected query");unmount();
`)
}

func TestFrontendSelectedInsightsRefreshRequiresEnabledConfirmedSettings(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options),message="";hook.submit(["one"]).catch(error=>message=error.message);`)
	frontendPollingRun(t, vm, `
assert(message.includes("请先确认")&&!requests.some(r=>r.options?.method==="POST"),"unloaded selected refresh reached server");
ok(requests[0],{...config,enabled:false});ok(requests[1],{snapshots:{},revision:1});
`)
	frontendPollingRun(t, vm, `hook=renderHook(options);message="";hook.submit(["one"]).catch(error=>message=error.message);`)
	frontendPollingRun(t, vm, `assert(message.includes("启用")&&!requests.some(r=>r.options?.method==="POST"),"disabled selected refresh reached server");unmount();assert(timers.size===0,"selected refresh leaked timers");`)
}

func TestFrontendSelectedInsightsRefreshNetworkFailureRetainsCacheWithoutReplay(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:{id:"one",usage:{windows:[{usedPercent:37}]}}},revision:1});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);var message="";hook.submit(["one"]).catch(error=>message=error.message);
requests.find(r=>r.options?.method==="POST").reject(new Error("synthetic connection interrupted"));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(message&&!hook.actionBusy&&hook.refreshPending&&hook.selectedRefreshControl.disabled,"selected-query uncertainty was hidden or unlocked before verification");
assert(hook.snapshots.one.usage.windows[0].usedPercent===37,"failed selected query erased valid usage");
assert(requests.filter(r=>r.options?.method==="POST").length===1,"uncertain selected query was automatically replayed");
ok(requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1),availableRefreshStatus(1));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.refreshPending&&!hook.selectedRefreshControl.disabled&&message,"fresh status did not permit explicit retry or erased unknown outcome");
unmount();assert(timers.size===0,"failed selected query leaked timers");
`)
}

func TestFrontendAccountOverviewKeepsStatusWithoutGlobalRefreshAction(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
for(const insightsEnabled of [true,false]) {
 const overview=RepairUI.AccountsOverview({stats:{active:1,queued:2,completed:3},total:6,insightsEnabled});
 assert(content(overview).includes(insightsEnabled?"账号状态与用量":"账号状态"),"overview title changed with removal of global controls");
 assert(!findButton(overview,"刷新全部用量")&&!findMatching(overview,node=>node.props?.id==="all-insights-refresh-description"),"overview retained an orphan global-refresh action or description ID");
}
assert(requests.length===0,"rendering the read-only overview submitted account work");
`)
}
