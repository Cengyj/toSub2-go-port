package console

import (
	"strconv"
	"testing"
)

func TestFrontendInsightRowRefreshUsesProjectedFlightActivity(t *testing.T) {
	for _, test := range []struct {
		name, fields, expected string
	}{
		{"active terminal", `refreshState:"active",lastAttempt:{status:"success"}`, "执行中 1 个"},
		{"active cancelled", `refreshState:"active",lastAttempt:{status:"cancelled"}`, "执行中 1 个"},
		{"active new grant", `refreshState:"active",lastAttempt:{status:"never"}`, "执行中 1 个"},
		{"queued new grant", `refreshState:"queued",lastAttempt:{status:"never"}`, "排队 1 个"},
		{"legacy running", `lastAttempt:{status:"running"}`, "执行中 1 个"},
		{"legacy pending", `lastAttempt:{status:"pending"}`, "排队 1 个"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/accountInsights.js")
			frontendPollingRun(t, vm, `var hook=renderHook(options),error;var row={id:"one",canQuery:false,usage:{observedAt:"2026-11-02T12:00:00Z",windows:[{usedPercent:37}]},`+test.fields+`};var before=JSON.stringify(row);`)
			frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:row}});ok(requests[2],availableRefreshStatus());`)
			frontendPollingRun(t, vm, `hook=renderHook(options);hook.submit(["one"]).catch(value=>error=value);`)
			frontendPollingRun(t, vm, `
assert(error?.message.includes(`+strconv.Quote(test.expected)+`),"row action ignored actual flight state: "+error?.message);
assert(!requests.some(request=>request.options.method==="POST"),"live or queued row submitted duplicate work");
assert(JSON.stringify(hook.snapshots.one)===before,"row eligibility changed the historical observation");
unmount();assert(timers.size===0,"row eligibility leaked polling timers");
`)
		})
	}
}

func TestFrontendInsightRowEligibleProjectionOverridesOldAttemptAndCooldown(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `
var hook=renderHook(options),error,receipt,duplicate;
var row={id:"one",canQuery:true,refreshState:"eligible",lastAttempt:{status:"running"},nextAllowedAt:new Date(Date.now()+60000).toISOString(),usage:{observedAt:"2026-11-02T12:00:00Z",windows:[{usedPercent:0},{usedPercent:73}]}};var before=JSON.stringify(row);
`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:row}});ok(requests[2],availableRefreshStatus());`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.submit(["one"]).then(value=>receipt=value).catch(value=>error=value);hook.submit(["one"]).then(value=>duplicate=value);
var writes=requests.filter(request=>request.options.method==="POST");
assert(writes.length===1&&JSON.stringify(JSON.parse(writes[0].options.body))===JSON.stringify({ids:["one"]}),"eligible projection was blocked by obsolete attempt or cooldown");
ok(writes[0],{queryId:"synthetic-row",accepted:["one"],merged:[],skipped:[]});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.submit(["one"]);
assert(!error&&receipt.accepted[0]==="one"&&duplicate===null&&hook.refreshPending,"row refresh lost admission or reconciliation guard");
assert(requests.filter(request=>request.options.method==="POST").length===1,"projected eligibility bypassed fresh-status reconciliation");
assert(JSON.stringify(hook.snapshots.one)===before,"refresh replaced cached values or observation time");
unmount();assert(timers.size===0,"accepted row refresh leaked resources");
`)
}

func TestFrontendInsightRowProjectedUnavailableAndUnknownFailClosed(t *testing.T) {
	for _, test := range []struct {
		name, fields, expected string
	}{
		{"projected cooldown without deadline", `canQuery:true,refreshState:"coolingDown",lastAttempt:{status:"running"}`, "冷却 1 个"},
		{"projected cooldown elapsed deadline", `canQuery:true,refreshState:"coolingDown",nextAllowedAt:"2000-01-01T00:00:00Z"`, "冷却 1 个"},
		{"projected unavailable", `canQuery:true,refreshState:"unavailable",lastAttempt:{status:"running"}`, "不可查询 1 个"},
		{"unknown projection", `canQuery:true,refreshState:"synthetic-future-state"`, "不可查询 1 个"},
		{"empty projection", `canQuery:true,refreshState:""`, "不可查询 1 个"},
		{"null projection", `canQuery:true,refreshState:null`, "不可查询 1 个"},
		{"eligible without permission", `canQuery:false,refreshState:"eligible"`, "不可查询 1 个"},
		{"eligible without known permission", `refreshState:"eligible"`, "查询状态尚未确认"},
		{"legacy cooldown", `canQuery:true,nextAllowedAt:new Date(Date.now()+60000).toISOString()`, "冷却 1 个"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/accountInsights.js")
			frontendPollingRun(t, vm, `var hook=renderHook(options),error;var row={id:"one",`+test.fields+`};`)
			frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:row}});ok(requests[2],availableRefreshStatus());`)
			frontendPollingRun(t, vm, `hook=renderHook(options);hook.submit(["one"]).catch(value=>error=value);`)
			frontendPollingRun(t, vm, `
assert(error?.message.includes(`+strconv.Quote(test.expected)+`),"row action did not respect projected eligibility: "+error?.message);
assert(!requests.some(request=>request.options.method==="POST"),"unavailable, unknown or denied row submitted refresh");
unmount();assert(timers.size===0,"blocked row leaked resources");
`)
		})
	}
}

func TestFrontendInsightRowProjectionRequiresCurrentStatusAuthority(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options),errors=[];var row={id:"one",canQuery:true,refreshState:"eligible"};`)
	frontendPollingRun(t, vm, `ok(requests[0],config);ok(requests[1],{snapshots:{one:row}});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.submit(["one"]).catch(error=>errors.push(error));`)
	frontendPollingRun(t, vm, `
assert(errors.length===1&&!requests.some(request=>request.options.method==="POST"),"row projection bypassed unread global status");
requests[2].resolve({ok:false,status:503,json:async()=>({error:"synthetic status failure"})});
`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.submit(["one"]).catch(error=>errors.push(error));`)
	frontendPollingRun(t, vm, `
assert(errors.length===2&&!requests.some(request=>request.options.method==="POST"),"row projection bypassed failed status verification");
unmount();assert(timers.size===0,"unknown row authority leaked timers");
`)
}
