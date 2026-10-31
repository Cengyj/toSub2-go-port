package console

import "testing"

// The private seed read is safe to retry, but a refused read must not become a
// hidden busy loop. The fixture never performs any 2FA mutation.
func TestFrontendTotpReadPausesAndRejectsLateSecrets(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/totpSetup.js")
	frontendPollingRun(t, vm, `
function renderTotp(props) {hookCursor=0;const value=Insights.useTotpSetupSecret(props);for(const run of effectQueue.splice(0))run();return value;}
var props={token:"synthetic",jobId:"synthetic/one",status:"totp_setup_otp",lastOperationAt:"first",terminal:false};
var seed=renderTotp(props);
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url.endsWith("synthetic%2Fone/totp-setup"),"seed read was not scoped to the current job");
document.hidden=true;fireEvent("document:visibilitychange");
assert(requests[0].options.signal.aborted&&timers.size===0,"hidden seed read continued");
ok(requests[0],{secret:"SYNTHETIC-OLD-SEED"});
`)
	frontendPollingRun(t, vm, `
seed=renderTotp(props);assert(!seed.secret,"canceled seed leaked into the row");
document.hidden=false;fireEvent("document:visibilitychange");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2,"returning to the row did not resume reading");
ok(requests[1],{secret:"SYNTHETIC-CURRENT-SEED"});
`)
	frontendPollingRun(t, vm, `
seed=renderTotp(props);assert(seed.secret==="SYNTHETIC-CURRENT-SEED"&&!seed.error,"valid seed was not adopted");
assert(timers.size===0&&[...events.values()].every(s=>s.size===0),"successful seed read kept polling or listeners");
props={...props,status:"completed",terminal:true};renderTotp(props);seed=renderTotp(props);
assert(!seed.secret&&!seed.error&&requests.length===2,"leaving seed step retained private material or started a request");unmount();
`)
}

func TestFrontendTotpReadReportsRefusalAndAllowsExplicitRetry(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/totpSetup.js")
	frontendPollingRun(t, vm, `
function renderTotp(props) {hookCursor=0;const value=Insights.useTotpSetupSecret(props);for(const run of effectQueue.splice(0))run();return value;}
var props={token:"synthetic",jobId:"one",status:"totp_setup_otp",lastOperationAt:"first",terminal:false};var seed=renderTotp(props);
`)
	frontendPollingRun(t, vm, `requests[0].resolve({ok:false,status:403,json:async()=>({error:"读取被拒绝"})});`)
	frontendPollingRun(t, vm, `
seed=renderTotp(props);assert(seed.error==="读取被拒绝"&&!seed.secret,"private endpoint refusal was swallowed");
assert(requests.length===1&&timers.size===0,"business refusal entered automatic retries");
seed.retry();
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2,"explicit read retry did not dispatch");ok(requests[1],{});
`)
	frontendPollingRun(t, vm, `
seed=renderTotp(props);assert(seed.error.includes("密钥读取响应不完整")&&!seed.secret,"HTTP 200 missing a seed was accepted");
assert(requests.every(r=>!r.options.method||r.options.method==="GET"),"seed retry dispatched a mutation");
unmount();assert(timers.size===0,"unmount leaked read timers");
`)
}
