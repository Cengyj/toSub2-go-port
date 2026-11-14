package console

import "testing"

func TestFrontendNoticeExpiresOnlyExplicitTransientReceipts(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/notice.js")
	frontendPollingRun(t, vm, `
var notice=renderComponent("useNotice");
notice.showNotice("已受理 1 个","info",{dismissAfterMs:6000});
assert(renderComponent("useNotice").message==="已受理 1 个" && timers.size===1,"receipt was not displayed temporarily");
fire(6000);
assert(renderComponent("useNotice").message==="" && timers.size===0,"completed receipt remains displayed");
for (const tone of ["active","warning","success","info"]) {
  notice.showNotice("Other operation",tone);
  const current=renderComponent("useNotice");
  assert(current.message==="Other operation" && current.tone===tone && timers.size===0,"persistent operation notice was automatically dismissed");
}
assert(requests.length===0,"notice lifecycle issued requests");
`)
}

func TestFrontendExpiredReceiptCannotClearNewOperationNotice(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/notice.js")
	frontendPollingRun(t, vm, `
var notice=renderComponent("useNotice");
for (const tone of ["active","warning","success"]) {
  notice.showNotice("已受理 1 个","info",{dismissAfterMs:6000});
  const oldTimeout=[...timers.values()][0].fn;
  notice.showNotice("New operation",tone);
  assert(timers.size===0,"replaced notice left its timer running");
  oldTimeout();
  const current=renderComponent("useNotice");
  assert(current.message==="New operation" && current.tone===tone,"queued old timeout erased the newer operation");
}
notice.showNotice("Same receipt","info",{dismissAfterMs:6000});
var oldTimeout=[...timers.values()][0].fn;
notice.showNotice("Same receipt","info",{dismissAfterMs:6000});
oldTimeout();
assert(renderComponent("useNotice").message==="Same receipt" && timers.size===1,"identical newer receipt lost its own display lifetime");
fire(6000);
assert(renderComponent("useNotice").message==="","newer receipt never expired");
`)
}

func TestFrontendNoticeDismissAndUnmountCancelPendingTimeout(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/notice.js")
	frontendPollingRun(t, vm, `
var notice=renderComponent("useNotice");
notice.showNotice("Receipt","info",{dismissAfterMs:6000});
var dismissedCallback=[...timers.values()][0].fn;
notice.clearNotice();
assert(renderComponent("useNotice").message==="" && timers.size===0,"manual close did not clear receipt and timer");
notice.showNotice("Persistent warning","warning");
dismissedCallback();
assert(renderComponent("useNotice").message==="Persistent warning","manually closed receipt erased later diagnostics");
notice.showNotice("Unmounting receipt","info",{dismissAfterMs:6000});
var unmountedCallback=[...timers.values()][0].fn;
unmount();
assert(timers.size===0,"unmount leaked a notification timer");
unmountedCallback();
assert(renderComponent("useNotice").message==="Unmounting receipt","timeout updated state after unmount");
`)
}
