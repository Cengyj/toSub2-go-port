package console

import "testing"

func TestFrontendPoolMonitorInitiallyOfflineReportsDiagnostic(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`
navigator.onLine=false;poolOptions={...poolOptions,active:true};renderPool();hook=renderPool();
assert(hook.error.includes("浏览器已离线"),"initial offline monitor suppressed its diagnostic");
assert(requests.length===0,"offline monitor dispatched a request");
navigator.onLine=true;fireEvent("window:online");
`)
	frontendPollingRun(t, vm, `assert(requests.length===1,"monitor failed to resume when online");replyPool(0,sub2PoolReady);`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(hook.loaded&&!hook.error&&hook.status.target==="sub2api","reconnected monitor did not adopt the current policy");unmount();assert(timers.size===0,"monitor leaked timers");`)
}

func TestFrontendTrashInitiallyOfflineRetainsRecordsAndReportsDiagnostic(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness+`var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.reloadList();ok(requests[1],trashList([trashEntry()]));`)
	frontendPollingRun(t, vm, `
navigator.onLine=false;options={...options,active:true};renderTrash();hook=renderTrash();
assert(hook.error.includes("浏览器已离线")&&!hook.loading,"initial offline trash view suppressed its diagnostic");
assert(hook.entries.length===1&&hook.entries[0].id==="archive-one","offline trash view discarded retained records");
assert(requests.length===2,"offline trash list dispatched a request");
navigator.onLine=true;fireEvent("window:online");
`)
	frontendPollingRun(t, vm, `assert(requests.length===3&&!requests[2].options.method,"reconnect replayed a mutation or failed to read");ok(requests[2],trashList());`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(!hook.error&&!hook.loading&&hook.entries.length===0,"reconnected trash list did not adopt current state");unmount();assert(timers.size===0,"trash poll leaked timers");`)
}

func TestFrontendManualProbeDiscardsPollStartedBeforeOrDuringAdmission(t *testing.T) {
	for _, tc := range []struct{ outcome, timing, response string }{
		{"confirmed", "before", "idle"}, {"unknown", "before", "idle"},
		{"confirmed", "during", "idle"}, {"unknown", "during", "idle"},
		{"confirmed", "before", "error"}, {"unknown", "before", "error"},
		{"confirmed", "during", "timeout"}, {"unknown", "during", "timeout"},
	} {
		t.Run(tc.outcome+"/"+tc.timing+"/"+tc.response, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/manual-probe.jsx")
			for name, value := range map[string]string{"admissionOutcome": tc.outcome, "pollTiming": tc.timing, "pollResponse": tc.response} {
				if err := vm.Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			frontendPollingRun(t, vm, `
var calls=[],crypto={randomUUID:()=>"synthetic-current"},activity=[];
var quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},reload(){},manualAPI(suffix,options){return new Promise((resolve,reject)=>calls.push({suffix,options,resolve,reject}));}};
var props={job:{id:"one"},quality,options:{model:"synthetic-model",reasoningEffort:"high"},onActivityChange:value=>activity.push(value)};
renderComponent("ManualProbePanel",props);
`)
			frontendPollingRun(t, vm, `calls[0].resolve({id:"one",task:"pelican-svg",state:"idle"});`)
			frontendPollingRun(t, vm, `var panel=renderComponent("ManualProbePanel",props);var start=findButton(panel,"开始手动检测");if(pollTiming==="during")start.props.onClick();fire(1500);`)
			frontendPollingRun(t, vm, `
if(pollTiming==="before")start.props.onClick();
assert(calls.length===3,"missing overlapping read and admission");
var posted=calls.find(call=>call.options.method==="POST"),oldRead=calls.at(-1).options.method?calls[1]:calls[2];
assert(posted&&!oldRead.options.method,"admission did not use production action");
if(admissionOutcome==="confirmed")posted.resolve({id:"one",task:"pelican-svg",state:"running",requestId:"synthetic-current"});
else posted.reject(new Error("synthetic response lost"));
`)
			frontendPollingRun(t, vm, `
var operationText=content(renderComponent("ManualProbePanel",props));
if(pollResponse==="idle")oldRead.resolve({id:"one",task:"pelican-svg",state:"idle"});
else if(pollResponse==="error")oldRead.reject(new Error("obsolete poll failure"));
else fire(10000);
`)
			frontendPollingRun(t, vm, `
panel=renderComponent("ManualProbePanel",props);
assert(admissionOutcome==="confirmed"?findButton(panel,"正在绘制…")?.props.disabled:findButton(panel,"开始手动检测")?.props.disabled&&content(panel).includes("待核实"),"old idle poll replaced current admission or released unknown-result protection");
assert(content(panel)===operationText,"obsolete poll replaced current operation diagnostic");
assert(activity.at(-1)===true,"old poll unlocked parent model/topic controls");
start.props.onClick();assert(calls.filter(call=>call.options.method==="POST").length===1,"old poll enabled another admission");
fire(0);
`)
			frontendPollingRun(t, vm, `assert(calls.length===4&&!calls[3].options.method,"fresh verification missing or replayed mutation");calls[3].resolve({id:"one",task:"pelican-svg",state:"running",requestId:"synthetic-current"});`)
			frontendPollingRun(t, vm, `panel=renderComponent("ManualProbePanel",props);assert(findButton(panel,"正在绘制…")?.props.disabled&&!content(panel).includes("待核实"),"current read did not settle admission");unmount();assert(timers.size===0,"manual probe leaked timers");`)
		})
	}
}
