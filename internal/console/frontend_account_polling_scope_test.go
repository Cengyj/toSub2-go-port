package console

import "testing"

const accountPollingScopeFixture = `
var presented={},published=0;
function publish(key){return value=>{presented[key]=value;published++;};}
var props={token:"session-A",page:1,emailFilter:[],setJobs:publish("jobs"),setJobSelectionIndex:publish("selection"),setPagination:publish("pagination"),setStats:publish("stats"),setPage:publish("page"),setConnected:value=>{presented.connected=value;},setConnectionError:value=>{presented.error=value;}};
function renderAccounts(commit=true){hookCursor=0;const refresh=Insights.useAccountPolling(props);if(commit)for(const run of effectQueue.splice(0))run();return refresh;}
function snapshot(id,page=props.page){const job={id,email:id+"@example.invalid",status:"completed",operationStatus:"succeeded",canDownload:true};return {jobs:[job],selection:[job],pagination:{page,pageSize:20,total:40,totalPages:2},stats:{active:0,queued:0,completed:40}};}
var refresh=renderAccounts();
`

func TestFrontendAccountPollingRejectsPreviousRenderBeforeEffectCleanup(t *testing.T) {
	for _, change := range []string{"token", "page", "filter"} {
		for _, outcome := range []string{"success", "failure"} {
			t.Run(change+"/"+outcome, func(t *testing.T) {
				vm := frontendInsightsVM(t, "hooks/serverState.js")
				if err := vm.Set("scopeChange", change); err != nil {
					t.Fatal(err)
				}
				if err := vm.Set("oldOutcome", outcome); err != nil {
					t.Fatal(err)
				}
				frontendPollingRun(t, vm, accountPollingScopeFixture)
				frontendPollingRun(t, vm, `ok(requests[0],snapshot("retained"));`)
				frontendPollingRun(t, vm, `refresh();`)
				frontendPollingRun(t, vm, `
var previous=JSON.stringify(presented),publishCount=published;
props={...props,...(scopeChange==="token"?{token:"session-B"}:scopeChange==="page"?{page:2}:{emailFilter:["current@example.invalid"]})};
refresh=renderAccounts(false);
// A render may precede its passive effect cleanup. The former read must already
// have lost permission to publish, even while its request is still outstanding.
assert(!requests[1].options.signal.aborted&&effectQueue.length>0,"fixture did not retain the render-to-cleanup boundary");
if(oldOutcome==="success")ok(requests[1],snapshot("obsolete",1));else requests[1].reject(new Error("obsolete scope read failure"));
`)
				frontendPollingRun(t, vm, `
assert(published===publishCount&&JSON.stringify(presented)===previous,"previous render published rows, selection, page, stats or connection state before cleanup");
for(const run of effectQueue.splice(0))run();
`)
				frontendPollingRun(t, vm, `
assert(requests.length===3,"new scope did not receive exactly one initial read");
assert(requests[2].options.headers["x-console-token"]===props.token,"new scope reused the previous connection token");
ok(requests[2],snapshot("current"));
`)
				frontendPollingRun(t, vm, `
assert(presented.jobs[0].id==="current"&&presented.selection[0].id==="current"&&presented.connected&&!presented.error,"current scope did not recover after rejecting old data");
unmount();assert(timers.size===0&&[...events.values()].every(listeners=>listeners.size===0),"scope transition leaked polling resources");
`)
			})
		}
	}
}

func TestFrontendAccountPollingOldRefreshCannotInvalidateReturnedToken(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, accountPollingScopeFixture)
	frontendPollingRun(t, vm, `var initialRefresh=refresh;props={...props,token:"session-B"};renderAccounts();`)
	frontendPollingRun(t, vm, `props={...props,token:"session-A"};refresh=renderAccounts();`)
	frontendPollingRun(t, vm, `
assert(requests.length===3&&requests[0].options.signal.aborted&&requests[1].options.signal.aborted,"token round trip did not replace its polling ownership");
initialRefresh.invalidate();initialRefresh();ok(requests[2],snapshot("current-session"));
`)
	frontendPollingRun(t, vm, `
assert(presented.jobs?.[0].id==="current-session"&&presented.connected,"old token callback invalidated the returned token's current response");
assert(![...timers.values()].some(timer=>timer.delay===0),"old token callback queued an extra current-session read");
assert(requests.length===3,"old token callback dispatched an unintended request");
unmount();assert(timers.size===0&&[...events.values()].every(listeners=>listeners.size===0),"returned token leaked polling resources");
`)
}

func TestFrontendAccountPollingKeepsEquivalentFilterReadAndCurrentRefresh(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, accountPollingScopeFixture)
	frontendPollingRun(t, vm, `
props={...props,emailFilter:[]};var sameScopeRefresh=renderAccounts();
assert(requests.length===1&&!requests[0].options.signal.aborted,"equal filter values restarted the same account read");
sameScopeRefresh.invalidate();refresh();ok(requests[0],snapshot("pre-mutation"));
`)
	frontendPollingRun(t, vm, `assert(!presented.jobs,"same-scope invalidation failed to reject the older read");fire(0);`)
	frontendPollingRun(t, vm, `assert(requests.length===2,"current-scope refresh was lost or repeated");ok(requests[1],snapshot("verified"));`)
	frontendPollingRun(t, vm, `assert(presented.jobs[0].id==="verified"&&presented.connected,"same scope did not publish its current verification");unmount();assert(timers.size===0,"stable filter scope leaked timers");`)
}

func TestFrontendAccountPollingReportsOfflineAfterHiddenInvalidation(t *testing.T) {
	for _, invalidate := range []string{"refresh()", "refresh.invalidate()"} {
		t.Run(invalidate, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/serverState.js")
			frontendPollingRun(t, vm, accountPollingScopeFixture)
			frontendPollingRun(t, vm, `ok(requests[0],snapshot("retained"));`)
			frontendPollingRun(t, vm, `refresh();`)
			frontendPollingRun(t, vm, `
assert(requests.length===2&&presented.connected,"fixture did not retain a connected view and pending read");
document.hidden=true;fireEvent("document:visibilitychange");
assert(requests[1].options.signal.aborted&&timers.size===0,"hidden view did not suspend its pending read");
`+invalidate+`;
requests[1].reject(new Error("obsolete request failure"));
`)
			frontendPollingRun(t, vm, `
assert(presented.connected&&!presented.error&&presented.jobs[0].id==="retained","canceled read published an obsolete failure");
navigator.onLine=false;fireEvent("window:offline");
assert(!presented.connected&&presented.error.includes("浏览器已离线"),"hidden invalidation suppressed the current offline event");
document.hidden=false;fireEvent("document:visibilitychange");refresh();refresh();
assert(requests.length===2&&timers.size===0&&presented.jobs[0].id==="retained","offline view dispatched a read or discarded saved rows");
navigator.onLine=true;fireEvent("window:online");
`)
			frontendPollingRun(t, vm, `assert(requests.length===3,"online recovery did not issue exactly one current read");ok(requests[2],snapshot("reconnected"));`)
			frontendPollingRun(t, vm, `
assert(presented.connected&&!presented.error&&presented.jobs[0].id==="reconnected","successful current read did not clear offline status");
unmount();assert(timers.size===0&&[...events.values()].every(listeners=>listeners.size===0),"offline recovery leaked polling resources");
`)
		})
	}
}

func TestFrontendAccountPollingRejectsOldScopeOfflineBeforeEffectCleanup(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, accountPollingScopeFixture)
	frontendPollingRun(t, vm, `ok(requests[0],snapshot("retained"));`)
	frontendPollingRun(t, vm, `
props={...props,token:"session-B"};refresh=renderAccounts(false);
navigator.onLine=false;fireEvent("window:offline");
assert(presented.connected&&!presented.error,"old scope's offline listener changed the new render before its own effect");
for(const run of effectQueue.splice(0))run();
assert(!presented.connected&&presented.error.includes("浏览器已离线")&&requests.length===1,"new scope did not report its own offline state without dispatch");
unmount();assert(timers.size===0&&[...events.values()].every(listeners=>listeners.size===0),"offline scope replacement leaked polling resources");
`)
}
