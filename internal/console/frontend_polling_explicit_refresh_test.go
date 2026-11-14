package console

import "testing"

func TestFrontendPollingKeepsExplicitRefreshAfterBusinessRefusal(t *testing.T) {
	vm := frontendPollingVM(t)
	frontendPollingRun(t, vm, `
polling.refresh();polling.refresh();
assert(requests.length===1,"explicit verification overlapped the active read");
const obsoleteRefusal=new Error("synthetic obsolete refusal");obsoleteRefusal.status=403;
requests[0].reject(obsoleteRefusal);
`)
	frontendPollingRun(t, vm, `
assert(errors.length===1,"business refusal disappeared");
fire(0);
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2,"explicit verification was discarded or repeated");
const currentRefusal=new Error("synthetic current refusal");currentRefusal.status=403;
requests[1].reject(currentRefusal);
`)
	frontendPollingRun(t, vm, `
assert(errors.length===2&&timers.size===0,"explicit verification turned a refusal into an automatic retry loop");
polling.stop();assert(events.size===0,"explicit verification leaked listeners");
`)
}

func TestFrontendPollingSuspensionDiscardsQueuedExplicitRefresh(t *testing.T) {
	for _, unavailable := range []string{"hidden", "offline"} {
		t.Run(unavailable, func(t *testing.T) {
			vm := frontendPollingVM(t)
			frontendPollingRun(t, vm, `polling.refresh();assert(requests.length===1,"refresh overlapped the active read");`)
			if unavailable == "hidden" {
				frontendPollingRun(t, vm, `environment.document.hidden=true;event("document:visibilitychange");`)
			} else {
				frontendPollingRun(t, vm, `environment.navigator.onLine=false;event("window:offline");`)
			}
			frontendPollingRun(t, vm, `
assert(requests[0].signal.aborted&&timers.size===0,"suspension did not cancel the active read");
const obsoleteRefusal=new Error("synthetic obsolete refusal");obsoleteRefusal.status=403;
requests[0].reject(obsoleteRefusal);polling.refresh();
`)
			frontendPollingRun(t, vm, `assert(requests.length===1&&timers.size===0,"unavailable page replayed queued verification");`)
			if unavailable == "hidden" {
				frontendPollingRun(t, vm, `environment.document.hidden=false;event("document:visibilitychange");`)
			} else {
				frontendPollingRun(t, vm, `environment.navigator.onLine=true;event("window:online");`)
			}
			frontendPollingRun(t, vm, `
assert(requests.length===2,"availability recovery did not issue exactly one read");
requests[1].resolve("recovered");
`)
			frontendPollingRun(t, vm, `
assert(results.join()==="recovered","obsolete refusal prevented current recovery");
polling.stop();assert(timers.size===0&&events.size===0,"availability recovery leaked resources");
`)
		})
	}
}
