package console

import "testing"

func TestFrontendAccountPollingReportsOfflineBeforeFirstRequest(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, `
var connected=true,error="",jobs=[{id:"saved",email:"saved@example.invalid"}];
var props={token:"synthetic",page:1,emailFilter:[],setJobs:value=>{jobs=value;},setJobSelectionIndex(){},setPagination(){},setStats(){},setPage(){},setConnected:value=>{connected=value;},setConnectionError:value=>{error=value;}};
function renderAccounts(){hookCursor=0;const refresh=Insights.useAccountPolling(props);for(const run of effectQueue.splice(0))run();return refresh;}
navigator.onLine=false;renderAccounts();
assert(!connected&&error.includes("浏览器已离线"),"offline account view retained a connected indicator or lost its diagnostic");
assert(jobs.length===1&&jobs[0].id==="saved","offline view discarded the last valid account data");
assert(requests.length===0,"offline poll dispatched a request");
navigator.onLine=true;fireEvent("window:online");
`)
	frontendPollingRun(t, vm, `assert(requests.length===1,"account polling did not resume when online");ok(requests[0],{jobs:[{id:"current",email:"current@example.invalid"}],stats:{active:0,queued:0,completed:1}});`)
	frontendPollingRun(t, vm, `assert(connected&&!error&&jobs[0].id==="current","successful reconnect did not restore account status");unmount();assert(timers.size===0,"account polling leaked timers");`)
}
