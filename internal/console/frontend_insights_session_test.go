package console

import "testing"

func TestFrontendInsightsTokenChangeHidesOldSettingsAndDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],{...config,revision:9});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.setDraft({...hook.draft,intervalMinutes:30});hook=renderHook(options);var oldHook=hook;options={...options,token:"synthetic-next"};hook=renderHook(options);assert(!hook.settingsLoaded&&!hook.dirty&&hook.settings.revision===0&&hook.draft.intervalMinutes===15,"new connection displayed previous authority or draft");oldHook.saveSettings();assert(requests.every(request=>request.options.method!=="PUT"),"obsolete handler submitted old configuration with current token");`)
	frontendPollingRun(t, vm, `ok(requests[1],{...config,revision:1,intervalMinutes:7});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.settingsLoaded&&hook.settings.revision===1&&hook.draft.intervalMinutes===7,"new connection rejected lower revision because old authority survived");unmount();`)
}

func TestFrontendInsightsExplicitReadCannotCrossTokenRoundTrip(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.reloadSettings();var staleRead=requests[1];options={...options,token:"synthetic-next"};renderHook(options);options={...options,token:"synthetic"};hook=renderHook(options);assert(staleRead.options.signal.aborted,"token change left explicit read active");`)
	frontendPollingRun(t, vm, `ok(staleRead,{...config,revision:99,intervalMinutes:99});ok(requests[3],{...config,intervalMinutes:7});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.settings.revision===1&&hook.draft.intervalMinutes===7&&!hook.settingsError&&!hook.settingsLoading,"obsolete read or cancellation crossed token generation");unmount();assert(timers.size===0,"session switch leaked request timers");`)
}

func TestFrontendInsightsOldWriteCannotReleaseNewWriteGuard(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.saveSettings();var oldWrite=requests[1];options={...options,token:"synthetic-next"};hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[2],{...config,intervalMinutes:7});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.saveSettings();assert(requests.length===4&&requests[3].options.method==="PUT","old write blocked new session save");ok(oldWrite,{...config,revision:99,intervalMinutes:99});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.saving&&hook.settings.intervalMinutes===7&&!hook.uncertain,"old completion changed new session state");hook.saveSettings();assert(requests.length===4,"old cleanup released current write guard");ok(requests[3],{...config,revision:2,intervalMinutes:7});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(!hook.saving&&hook.settings.revision===2,"current write failed to settle");unmount();`)
}

func TestFrontendInsightsDisconnectRejectsOldActionHandlersAndHidesCache(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:{id:"one",canQuery:true}}});ok(requests[2],availableRefreshStatus());`)
	frontendPollingRun(t, vm, `hook=renderHook(options);var oldHook=hook;options={...options,token:""};hook=renderHook(options);assert(!hook.settingsLoaded&&!hook.status&&Object.keys(hook.snapshots).length===0&&hook.refreshSummary.disabled,"disconnected view exposed previous connection data");oldHook.cancel("one");oldHook.reloadSettings();oldHook.submit(["one"]);assert(requests.length===3,"disconnected or obsolete handler submitted network work");unmount();`)
}

func TestFrontendInsightsOutdatedReadbackCannotClearConflict(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.setDraft({...hook.draft,intervalMinutes:30});hook=renderHook(options);hook.saveSettings();requests[1].resolve({ok:false,status:409,json:async()=>({error:"synthetic conflict"})});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.reloadSettings();fire(30000);`)
	frontendPollingRun(t, vm, `ok(requests[3],{...config,revision:3,intervalMinutes:7});`)
	frontendPollingRun(t, vm, `ok(requests[2],{...config,revision:2,intervalMinutes:20});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.verificationRequired&&hook.settings.revision===3&&hook.draft.intervalMinutes===30&&hook.settingsError.includes("过期"),"outdated verification cleared conflict or replaced current authority/draft");hook.saveSettings();assert(requests.length===4,"outdated readback permitted automatic replay");unmount();`)
}

func TestFrontendInsightsObsoleteAdmissionSettlesWithoutReceipt(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,selectedIds:["one"]};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{}});ok(requests[2],availableRefreshStatus(1));`)
	frontendPollingRun(t, vm, `hook=renderHook(options);var outcomes=[];hook.submit(["one"]).then(value=>outcomes.push({value}),error=>outcomes.push({error}));options={...options,token:"synthetic-next",active:false};hook=renderHook(options);`)
	frontendPollingRun(t, vm, `assert(outcomes.length===1&&outcomes[0].value===null&&!outcomes[0].error,"obsolete admission published a receipt or error into another connection");hook=renderHook(options);assert(!hook.actionBusy&&!hook.refreshPending,"old admission kept new connection pending");assert(requests.filter(r=>r.options.method==="POST").length===1,"identity change replayed admission");unmount();`)
}

func TestFrontendInsightsUnknownWriteBlocksRetainedHandlerBeforeRender(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.saveSettings();requests[1].reject(new Error("synthetic lost response"));`)
	frontendPollingRun(t, vm, `hook.saveSettings();assert(requests.length===2,"retained handler replayed unknown write before React rendered its verification guard");unmount();`)
}
