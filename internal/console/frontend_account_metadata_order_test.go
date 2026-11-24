package console

import "testing"

func TestFrontendAccountMetadataReadCannotReplaceNewerAccountState(t *testing.T) {
	for _, order := range []string{"older", "older-same-millisecond", "current"} {
		t.Run(order, func(t *testing.T) {
			vm := frontendExpirationComponentVM(t, "account-info-dialog")
			if err := vm.Set("responseOrder", order); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
// Keep the production App callbacks and dialog HTTP reader in the same VM,
// with independent component hook state. Only App background effects are off.
var appHooks=[],infoHooks=[],effectfulUseEffect=React.useEffect;
React.useState=initial=>{const slots=hookSlots,id=hookCursor++;if(!slots[id])slots[id]={value:typeof initial==="function"?initial():initial};return [slots[id].value,value=>{slots[id].value=typeof value==="function"?value(slots[id].value):value;}];};
function renderApp(){hookSlots=appHooks;hookCursor=0;React.useEffect=()=>{};try{return AppHarness.App();}finally{React.useEffect=effectfulUseEffect;}}
function renderDialog(props){hookSlots=infoHooks;return renderComponent("AccountInfoDialog",props);}
location.hash="#accounts";renderApp();
var original={id:"synthetic-account",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",status:"completed",canDownload:true,canForceRelogin:true,updatedAt:"2026-11-14T01:00:00.000100Z",manualExpiresAt:"2026-12-01T00:00:00Z"};
appState.setToken("synthetic");appState.setJobs([original]);appState.setJobSelectionIndex([original]);appState.setSelectedJobIds(new Set([original.id]));appState.setFeatures({bulkActions:true,sub2apiUpload:true});
find(renderApp(),"JobRow").props.onViewAccount();
var dialogProps=find(renderApp(),"AccountInfoDialog").props;
renderDialog(dialogProps);
assert(requests.length===1&&requests[0].url==="/api/jobs/synthetic-account","real dialog did not read account metadata");
var latest={...original,status:"queued",canForceRelogin:false,totpExportBlocked:true,totpRecoveryPending:true,updatedAt:responseOrder==="older-same-millisecond"?"2026-11-14T01:00:00.000200Z":"2026-11-14T01:00:01Z",manualExpiresAt:"2026-12-02T00:00:00Z"};
// A newer server snapshot is already queued even before React renders it.
appState.setJobs([latest]);appState.setJobSelectionIndex([latest]);
var reply=responseOrder==="current"?{...latest,updatedAt:"2026-11-14T01:00:02Z",manualExpiresAt:"2026-12-03T00:00:00Z"}:original;
requests[0].resolve({ok:true,status:200,json:async()=>reply});
`)
			frontendPollingRun(t, vm, `
var tree=renderApp(),row=find(tree,"JobRow");
assert(row.props.job.status==="queued"&&row.props.job.totpExportBlocked===true&&row.props.job.totpRecoveryPending===true&&!row.props.job.canForceRelogin,"late metadata read restored old account operation or security capabilities");
assert(row.props.job.manualExpiresAt===(responseOrder==="current"?reply:latest).manualExpiresAt,"metadata merge rejected a current reply or erased a newer expiration");
var bulkUpload=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="上传 Sub2API");
assert(bulkUpload.props.disabled,"metadata response restored upload admission from the old selection index");
assert(requests.length===2&&requests[1].url.endsWith("/account-info")&&requests.every(request=>!request.options.method),"metadata reconciliation replayed a mutation or bypassed the private-info endpoint");
requests[1].resolve({ok:true,status:200,json:async()=>privateInfo(original)});
`)
			frontendPollingRun(t, vm, `hookSlots=infoHooks;unmount();assert(timers.size===0,"metadata dialog leaked its request timer");`)
		})
	}
}

func TestFrontendSubscriptionResponsePreservesNanosecondAccountOrdering(t *testing.T) {
	vm := frontendSubscriptionHookVM(t)
	frontendPollingRun(t, vm, `
subscriptionJob={...subscriptionJob,updatedAt:"2026-11-14T01:00:00.123100Z"};subscriptionOptions={...subscriptionOptions,jobs:[subscriptionJob]};
var hook=renderSubscription();hook.refresh(subscriptionJob);var reply=subscriptionReply();reply.job.updatedAt="2026-11-14T01:00:00.123200Z";
var newer={...subscriptionJob,updatedAt:"2026-11-14T01:00:00.123300Z",canRefreshSubscription:false,totpRecoveryPending:true};
subscriptionOptions={...subscriptionOptions,jobs:[newer]};renderSubscription();ok(requests[0],reply);
`)
	frontendPollingRun(t, vm, `
assert(updates.length===0&&verifications===1,"sub-millisecond old subscription reply replaced newer account security state");
assert(requests.length===1&&!renderSubscription().states[subscriptionJob.id].busy,"stale subscription reply replayed a query or kept its action busy");unmount();
`)
}
