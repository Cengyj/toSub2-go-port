package console

import "testing"

func TestFrontendQualityInitiallyOfflineReportsCurrentReadFailure(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `
navigator.onLine=false;renderQuality(qualityOptions);
var quality=renderQuality(qualityOptions);
assert(quality.error.includes("浏览器已离线"),"initial offline state lost its current read diagnostic");
assert(requests.every(request=>request.url!=="/api/account-quality?ids=one"),"offline poll dispatched a request");
unmount();
`)
	frontendPollingRun(t, vm, `assert(timers.size===0,"offline quality cleanup leaked timers");`)
}

func TestFrontendQualityMutationDiscardsObsoletePollFailure(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown"} {
		for _, failure := range []string{"rejected", "timeout"} {
			t.Run(outcome+"/"+failure, func(t *testing.T) {
				vm := frontendInsightsVM(t, "hooks/accountQuality.js")
				if err := vm.Set("mutationOutcome", outcome); err != nil {
					t.Fatal(err)
				}
				if err := vm.Set("readFailure", failure); err != nil {
					t.Fatal(err)
				}
				frontendPollingRun(t, vm, qualityHookSetup)
				frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
				frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);ok(requests[1],qualityList());`)
				frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);fire(5000);`)
				frontendPollingRun(t, vm, `
assert(requests[2].url==="/api/account-quality?ids=one","missing in-flight old poll");
quality.submit(["one"]).catch(()=>{});
assert(requests[3].url==="/api/account-quality/refresh"&&requests[3].options.method==="POST","fixture did not submit the production action");
if(mutationOutcome==="confirmed")ok(requests[3],{checkId:"synthetic",accepted:["one"],merged:[],skipped:[]});
else requests[3].reject(new Error("synthetic lost mutation response"));
`)
				frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);var operationError=quality.error;
assert(quality.verificationRequired,"mutation did not retain its readback barrier");
if(mutationOutcome==="unknown")assert(operationError.includes("结果待核实"),"missing unknown mutation warning");
if(readFailure==="timeout")fire(10000);else requests[2].reject(new Error("obsolete poll failure"));
`)
				frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);
assert(quality.error===operationError&&quality.verificationRequired,"obsolete poll error replaced the current operation outcome");
fire(0);
`)
				frontendPollingRun(t, vm, `
assert(requests.length===5&&!requests[4].options.method,"readback was lost or mutation replayed");
requests[4].reject(new Error("current read failure"));
`)
				frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(quality.error.includes("网络连接中断")&&quality.verificationRequired,"current read failure was swallowed or released verification");
assert(requests.filter(request=>request.options.method==="POST").length===1,"failure replayed detection");
unmount();assert(timers.size===0,"quality polling cleanup leaked timers");
`)
			})
		}
	}
}
