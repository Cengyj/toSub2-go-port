package console

import "testing"

func TestFrontendInsightRefreshInitialFailureBelongsToRequestedScope(t *testing.T) {
	for _, failure := range []string{"http", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/accountInsights.js")
			frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options);`)
			if failure == "http" {
				frontendPollingRun(t, vm, `requests[2].resolve({ok:false,status:503,json:async()=>({error:"synthetic initial status failure"})});`)
			} else {
				frontendPollingRun(t, vm, `ok(requests[2],{enabled:true,automatic:true});`)
			}
			frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{}});`)
			frontendPollingRun(t, vm, `
hook=renderHook(options);
assert(hook.status===null&&hook.statusError,"first failed status read invented cached data or hid its error");
for(const control of [hook.refreshSummary,hook.selectedRefreshControl]) {
  assert(control.disabled&&!control.busy&&control.phase==="unknown"&&!control.label.includes("正在核对"),"first scope failure was ignored and left an endless checking spinner");
}
hook.reloadStatus();
assert(renderHook(options).refreshSummary.phase==="checking","explicit verification did not start a new read");
`)
			frontendPollingRun(t, vm, `ok(requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1),availableRefreshStatus(1));`)
			frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.statusError&&!hook.refreshSummary.disabled&&!hook.selectedRefreshControl.disabled,"successful explicit first verification did not restore eligibility");
options={...options,selectedIds:["two"]};renderHook(options);
`)
			frontendPollingRun(t, vm, `requests.at(-1).resolve({ok:false,status:403,json:async()=>({error:"synthetic new scope refusal"})});`)
			frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.statusError&&hook.status.refresh.eligible===2,"new-scope failure erased the previous cache or hid refusal");
assert(hook.selectedRefreshControl.phase==="unknown"&&!hook.selectedRefreshControl.busy&&hook.refreshSummary.phase==="unknown","new-scope failure was attributed to the old scope");
assert(requests.every(r=>!r.options.method||r.options.method==="GET"),"status error recovery submitted an upstream refresh");
unmount();assert(timers.size===0,"first-read error recovery leaked timers");
`)
		})
	}
}

func TestFrontendInsightRefreshControlsUseScopedServerActivity(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `
options={...options,selectedIds:["one"]};
var hook=renderHook(options);
var aggregate={total:2,eligible:2,queued:0,active:0,coolingDown:0,unavailable:0,retryAfterSeconds:0};
var selectedAggregate={...aggregate,total:1,eligible:1};
var statusData={enabled:true,automatic:true,running:true,queued:0,active:0,counts:{},revision:1,refresh:aggregate,selection:selectedAggregate};
var statusRequests=()=>requests.filter(r=>r.url.startsWith("/api/account-insights/status"));
`)
	frontendPollingRun(t, vm, `
assert(statusRequests().length===1&&statusRequests()[0].url.endsWith("?ids=one"),"accounts page did not request authoritative scoped refresh status");
assert(hook.refreshSummary.disabled&&hook.selectedRefreshControl.disabled,"unread status allowed a refresh");
ok(requests.find(r=>r.url.endsWith("/settings")),config);
ok(requests.find(r=>r.url.startsWith("/api/account-insights?ids=")),{snapshots:{},revision:1});
ok(statusRequests()[0],{...statusData,active:1,refresh:{...aggregate,eligible:1,active:1}});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);
assert(hook.refreshSummary.disabled&&hook.refreshSummary.busy&&hook.refreshSummary.phase==="running","global in-flight work was not represented");
assert(!hook.selectedRefreshControl.disabled&&!hook.selectedRefreshControl.busy&&hook.selectedRefreshControl.label==="刷新所选用量","another account blocked the independently eligible selection");
options={...options,selectedIds:[]};hook=renderHook(options);
assert(hook.selectedRefreshControl.disabled&&!hook.selectedRefreshControl.busy,"empty selection looked actionable or busy");
options={...options,selectedIds:Array.from({length:1001},(_,i)=>"synthetic-"+i)};hook=renderHook(options);
assert(hook.selectedRefreshControl.disabled&&hook.selectedRefreshControl.description.includes("1000"),"oversized selection was not explained");
assert(statusRequests().every(r=>r.url.length<200),"oversized selection entered the status URL");
unmount();assert(timers.size===0,"refresh status leaked timers");
`)
}

func TestFrontendInsightRefreshReceiptWaitsForFreshStatus(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `
options={...options,selectedIds:["one"]};var hook=renderHook(options);
var aggregate={total:2,eligible:2,queued:0,active:0,coolingDown:0,unavailable:0,retryAfterSeconds:0};
var statusData={enabled:true,automatic:true,running:true,queued:0,active:0,counts:{},revision:1,refresh:aggregate,selection:{...aggregate,total:1,eligible:1}};
var statusRequests=()=>requests.filter(r=>r.url.startsWith("/api/account-insights/status"));
`)
	frontendPollingRun(t, vm, `
ok(requests.find(r=>r.url.endsWith("/settings")),config);
ok(requests.find(r=>r.url.startsWith("/api/account-insights?ids=")),{snapshots:{},revision:1});
ok(statusRequests()[0],statusData);
`)
	frontendPollingRun(t, vm, `hook=renderHook(options);var oldSubmit=hook.submit.bind(null,["one"]);fire(5000);fire(5000);`)
	frontendPollingRun(t, vm, `
var oldRead=statusRequests().at(-1),receipt,duplicate;
oldSubmit().then(v=>receipt=v);oldSubmit().then(v=>duplicate=v);
var writes=requests.filter(r=>r.options.method==="POST");
assert(writes.length===1,"same-frame refresh was submitted twice");
assert(renderHook(options).refreshSummary.phase==="submitting","submission lacked a loading phase");
ok(writes[0],{queryId:"synthetic-query",accepted:["one"],merged:[],skipped:[]});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);
assert(!hook.actionBusy&&hook.refreshPending&&hook.refreshSummary.phase==="checking","receipt unlocked refresh or prevented independent cancellation");
assert(receipt.accepted[0]==="one"&&duplicate===null,"admission receipt was duplicated or lost");
oldSubmit();assert(requests.filter(r=>r.options.method==="POST").length===1,"old handler bypassed reconciliation barrier");
ok(oldRead,statusData);
`)
	frontendPollingRun(t, vm, `
assert(renderHook(options).refreshPending,"GET started before the receipt cleared the barrier");fire(0);
`)
	frontendPollingRun(t, vm, `
var fresh=statusRequests().at(-1);assert(fresh!==oldRead,"receipt did not trigger a fresh GET");
ok(fresh,{...statusData,active:1,refresh:{...aggregate,eligible:1,active:1},selection:{...statusData.selection,eligible:0,active:1}});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.refreshPending&&hook.refreshSummary.busy&&hook.selectedRefreshControl.busy,"fresh status failed to replace reconciliation with server activity");
var error;oldSubmit().catch(e=>error=e);
`)
	frontendPollingRun(t, vm, `
assert(error&&requests.filter(r=>r.options.method==="POST").length===1,"old handler ignored current in-flight state");
unmount();assert(timers.size===0,"refresh lifecycle leaked timers");
`)
}

func TestFrontendInsightRefreshMalformedReceiptVerifiesWithoutReplay(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options),errors=[];`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:{id:"one",canQuery:true,usage:{windows:[{usedPercent:37}]}}}});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);var oldSubmit=hook.submit.bind(null,["one"]);
oldSubmit().catch(error=>errors.push(error));
ok(requests.find(r=>r.options.method==="POST"),{});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(errors.length===1&&errors[0].outcome==="unknown"&&hook.refreshPending,"malformed 202 receipt became a confirmed admission");
var statusRead=requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1);
statusRead.resolve({ok:false,status:503,json:async()=>({error:"synthetic status unavailable"})});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.statusError&&hook.refreshSummary.phase==="unknown"&&hook.refreshSummary.disabled&&!hook.refreshSummary.busy,"failed verification became ready or an endless spinner");
assert(hook.status.refresh.eligible===2&&hook.snapshots.one.usage.windows[0].usedPercent===37,"failed status read erased cached data");
oldSubmit();hook.reloadStatus();
assert(requests.filter(r=>r.options.method==="POST").length===1,"unknown receipt replayed the mutation");
`)
	frontendPollingRun(t, vm, `
var fresh=requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1);
var cooling=availableRefreshStatus(1);cooling.refresh={...cooling.refresh,eligible:0,coolingDown:2,retryAfterSeconds:30};cooling.selection={...cooling.selection,eligible:0,coolingDown:1,retryAfterSeconds:30};
ok(fresh,cooling);
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.refreshPending&&!hook.statusError&&hook.refreshSummary.phase==="cooldown"&&!hook.refreshSummary.busy&&hook.selectedRefreshControl.description.includes("30 秒"),"fresh verification did not settle into server cooldown");
assert(errors.length===1&&requests.filter(r=>r.options.method==="POST").length===1,"verification invented admission success or repeated work");
unmount();assert(timers.size===0,"failed verification leaked resources");
`)
}

func TestFrontendInsightRefreshSelectionDiscardsOldReadsAndPausesOffline(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{}});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);options={...options,selectedIds:["two"]};hook=renderHook(options);
assert(hook.selectedRefreshControl.disabled,"new selection reused the old scope's eligibility");
`)
	frontendPollingRun(t, vm, `
var obsolete=requests.at(-1);assert(obsolete.url.endsWith("?ids=two"),"selection was not reflected in the aggregate request");
options={...options,selectedIds:["three"]};renderHook(options);assert(obsolete.options.signal.aborted,"selection change left the old scoped request active");
`)
	frontendPollingRun(t, vm, `
ok(requests.at(-1),availableRefreshStatus(1));
var busy=availableRefreshStatus(1);busy.selection={...busy.selection,eligible:0,active:1};ok(obsolete,busy);
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.selectedRefreshControl.disabled&&hook.status.selection.eligible===1,"obsolete selection response contaminated the current scope");var staleVisibleSubmit=hook.submit.bind(null,["three"]);
document.hidden=true;fireEvent("document:visibilitychange");assert(timers.size===0,"hidden aggregate status kept polling");
document.hidden=false;fireEvent("document:visibilitychange");hook=renderHook(options);
assert(hook.refreshSummary.disabled&&hook.selectedRefreshControl.disabled,"restored visible tab reused eligibility before its fresh GET completed");
staleVisibleSubmit().catch(()=>{});
document.hidden=true;fireEvent("document:visibilitychange");
navigator.onLine=false;fireEvent("window:offline");hook=renderHook(options);assert(hook.refreshSummary.disabled&&hook.selectedRefreshControl.disabled,"offline browser permitted refresh");
var previousCount=requests.length;document.hidden=false;fireEvent("document:visibilitychange");assert(requests.length===previousCount,"hidden/offline recovery submitted prematurely");
navigator.onLine=true;fireEvent("window:online");
`)
	frontendPollingRun(t, vm, `
assert(requests.every(r=>!r.options.method||r.options.method==="GET"),"availability recovery submitted or replayed an upstream refresh");
assert(requests.at(-1).url.endsWith("?ids=three"),"online recovery lost the current selected scope");
unmount();assert(timers.size===0&&[...events.values()].every(set=>set.size===0),"scoped status leaked listeners or timers");
`)
}

func TestFrontendInsightRefreshOldRefusalCannotStrandVerification(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{}});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `hook=renderHook(options);fire(5000);fire(5000);`)
	frontendPollingRun(t, vm, `
var oldRead=requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1);
hook.submit(["one"]);ok(requests.find(r=>r.options.method==="POST"),{queryId:"synthetic-refusal",accepted:["one"],merged:[],skipped:[]});
`)
	frontendPollingRun(t, vm, `
oldRead.resolve({ok:false,status:403,json:async()=>({error:"synthetic obsolete refusal"})});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.refreshPending&&!hook.statusError,"old refusal contaminated current verification");fire(0);
`)
	frontendPollingRun(t, vm, `
var fresh=requests.filter(r=>r.url.startsWith("/api/account-insights/status")).at(-1);assert(fresh!==oldRead,"old refusal swallowed explicitly requested fresh verification");ok(fresh,availableRefreshStatus(1));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.refreshPending&&!hook.refreshSummary.disabled,"explicit verification remained permanently checking");
assert(requests.filter(r=>r.options.method==="POST").length===1,"read verification replayed the admission");unmount();
`)
}

func TestFrontendInsightRefreshRejectsInvalidCountsAndStaleConfigurationHandlers(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options),errors=[];`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{}});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `hook=renderHook(options);var oldSubmit=hook.submit.bind(null,["one"]);hook.reloadStatus();`)
	frontendPollingRun(t, vm, `var invalid=availableRefreshStatus(1);invalid.refresh.active=1;ok(requests.at(-1),invalid);`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.statusError&&hook.refreshSummary.disabled&&hook.status.refresh.active===0,"inconsistent aggregate counts replaced valid status");
oldSubmit().catch(error=>errors.push(error));hook.reloadStatus();
`)
	frontendPollingRun(t, vm, `ok(requests.at(-1),availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.saveSettings({...config,enabled:false});oldSubmit().catch(error=>errors.push(error));
var write=requests.find(r=>r.options.method==="PUT");write.reject(new Error("synthetic settings response lost"));
`)
	frontendPollingRun(t, vm, `
// Intentionally keep the old hook object: refs must enforce the new uncertainty
// before the next React render publishes verificationRequired.
oldSubmit().catch(error=>errors.push(error));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.verificationRequired&&errors.length===3,"stale handler bypassed invalid counts, active settings write or required verification");
assert(!requests.some(r=>r.options.method==="POST"),"stale configuration handler submitted a query");
unmount();assert(timers.size===0,"configuration uncertainty leaked status polling");
`)
}
