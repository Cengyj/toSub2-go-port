package console

import "testing"

const maintenanceHarness = `
var maintenanceOptions={token:"A",enabled:true,active:true};
function renderMaintenance(){hookCursor=0;const value=Insights.useMaintenance(maintenanceOptions);for(const run of effectQueue.splice(0))run();return value;}
function maintenanceSnapshot(enabled=false,revision=0,uncertain=false){return {schema:1,revision,enabled,uncertain,activeRequests:0,jobs:{queued:0,running:0,queries:0,accountChanges:0,drained:true},insights:0,probes:0,sms:0,recycleRunning:false,drained:enabled&&!uncertain};}
`

func TestFrontendMaintenanceUnknownWriteNeedsExplicitReadback(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/maintenance.js")
	frontendPollingRun(t, vm, maintenanceHarness+`renderMaintenance();`)
	frontendPollingRun(t, vm, `assert(requests.length===1&&!requests[0].options.method,"maintenance mount mutated state");ok(requests[0],maintenanceSnapshot());`)
	frontendPollingRun(t, vm, `var maintenance=renderMaintenance(),oldChange=maintenance.change;oldChange(true);oldChange(true);assert(requests.length===2,"maintenance double submit");requests[1].reject(new Error("lost response"));`)
	frontendPollingRun(t, vm, `oldChange(true);maintenance=renderMaintenance();assert(maintenance.verificationRequired&&!maintenance.canChange&&requests.length===2,"maintenance unknown replayed");fire(3000);`)
	frontendPollingRun(t, vm, `ok(requests[2],maintenanceSnapshot(true,1));`)
	frontendPollingRun(t, vm, `maintenance=renderMaintenance();assert(maintenance.verificationRequired&&!maintenance.canChange,"passive poll cleared explicit verification barrier");maintenance.reload();maintenance.reload();assert(requests.length===4,"explicit read duplicated");ok(requests[3],maintenanceSnapshot(true,1));`)
	frontendPollingRun(t, vm, `maintenance=renderMaintenance();assert(!maintenance.verificationRequired&&maintenance.canChange&&maintenance.status.drained,"explicit read failed to settle actual state");maintenance.change(false);assert(JSON.parse(requests[4].options.body).revision===1,"exit used stale revision");ok(requests[4],maintenanceSnapshot(false,2));`)
	frontendPollingRun(t, vm, `maintenance=renderMaintenance();assert(!maintenance.status.enabled&&!maintenance.status.drained&&requests.filter(r=>r.options.method==="POST").length===2,"exit replayed or claimed drained while open");unmount();assert(timers.size===0,"maintenance leaked timers");`)
}

func TestFrontendMaintenanceSuspendsReadPollingAndRejectsOldToken(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/maintenance.js")
	frontendPollingRun(t, vm, maintenanceHarness+`renderMaintenance();`)
	frontendPollingRun(t, vm, `ok(requests[0],maintenanceSnapshot());`)
	frontendPollingRun(t, vm, `var previous=renderMaintenance();document.hidden=true;fireEvent("document:visibilitychange");assert(timers.size===0,"hidden maintenance page kept polling");maintenanceOptions={...maintenanceOptions,token:"B"};renderMaintenance();maintenanceOptions={...maintenanceOptions,token:"A"};renderMaintenance();previous.change(true);previous.reload();assert(requests.length===1,"old token handler dispatched");document.hidden=false;fireEvent("document:visibilitychange");`)
	frontendPollingRun(t, vm, `assert(requests.length===2&&!requests[1].options.method,"visibility restore issued mutation");navigator.onLine=false;fireEvent("window:offline");assert(requests[1].options.signal.aborted,"offline did not cancel read");unmount();assert(timers.size===0,"unmount leaked polling");`)
}

func TestFrontendMaintenanceRejectsIncompleteSnapshot(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/maintenance.js")
	frontendPollingRun(t, vm, maintenanceHarness+`renderMaintenance();`)
	frontendPollingRun(t, vm, `ok(requests[0],{schema:1,revision:1,enabled:true,uncertain:false,drained:true});`)
	frontendPollingRun(t, vm, `var maintenance=renderMaintenance();assert(!maintenance.canChange&&!maintenance.status&&maintenance.error.includes("不完整"),"partial state was accepted as drained");unmount();`)
}
