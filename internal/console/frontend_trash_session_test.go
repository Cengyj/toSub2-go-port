package console

import (
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func TestFrontendTrashRevisionZeroIsNotAValidPolicy(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `
assert(Insights.validateTrashSettings(trashSettings).revision===1,"valid initial revision was rejected");
for(const value of [{...trashSettings,revision:0},{settings:{...trashSettings,revision:0}}]){
 let rejected=false;try{Insights.validateTrashSettings(value);}catch{rejected=true;}
 assert(rejected,"zero policy revision was accepted");
}
let rejectedList=false;try{Insights.validateTrashList(trashList([],{...trashSettings,revision:0}));}catch{rejectedList=true;}
assert(rejectedList,"list accepted a zero policy revision");
var hook=renderTrash();ok(requests[0],{settings:{...trashSettings,revision:0}});
`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.settings===null&&hook.settingsError.includes("不完整"),"invalid initial policy was adopted");hook.saveSettings();assert(requests.length===1,"invalid revision allowed policy save");unmount();`)
}

func TestFrontendTrashBackgroundPolicyReadPreservesUnsubmittedDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.setRetentionDraft("7");hook=renderTrash();hook.reloadList();ok(requests[1],trashList([],{...trashSettings,revision:2,retentionDays:30}));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.settings.revision===2&&hook.settings.retentionDays===30&&hook.retentionDraft==="7"&&hook.dirty,"background policy read discarded unsaved draft");hook.saveSettings({draft:"7",revision:1});assert(requests.length===2&&renderTrash().settingsError.includes("已变化"),"stale confirmation bypassed current revision");hook.reloadSettings();ok(requests[2],{settings:{...trashSettings,revision:2,retentionDays:30}});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.retentionDraft==="30"&&!hook.dirty,"explicit policy re-read failed to reconcile draft");unmount();`)
}

// Exercise only the trash hook's real effects in the App handler harness.
// Other modules retain their existing isolated boundaries. Settings reads use
// synthetic server data; mutation requests and business assertions are intact.
func frontendAppTrashLifecycleVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendAppVM(t, api.Plugin{Name: "trash-lifecycle-react", Setup: func(build api.PluginBuild) {
		build.OnResolve(api.OnResolveOptions{Filter: `^react$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			if strings.HasSuffix(args.Importer, "accountTrash.js") {
				return api.OnResolveResult{Path: "synthetic-trash-react", External: true}, nil
			}
			return api.OnResolveResult{}, nil
		})
	}})
	frontendPollingRun(t, vm, `
var trashEffects=[];
TrashReact.useEffect=(run,deps)=>{const id=hookCursor++,previous=hookSlots[id];if(!previous||deps.some((v,i)=>!Object.is(v,previous.deps[i]))){hookSlots[id]={deps,cleanup:previous?.cleanup};trashEffects.push(()=>{hookSlots[id].cleanup?.();hookSlots[id].cleanup=run();});}};
var renderBeforeTrashLifecycle=render;
render=()=>{const value=renderBeforeTrashLifecycle();for(const run of trashEffects.splice(0))run();return value;};
var document={hidden:false,addEventListener(){},removeEventListener(){}},navigator={onLine:true};
window.addEventListener=()=>{};window.removeEventListener=()=>{};
var fetchBeforeTrashLifecycle=fetch;
fetch=(url,options)=>url==="/api/account-trash/settings"&&(!options.method||options.method==="GET")
 ? Promise.resolve({ok:true,status:200,json:async()=>({settings:{schemaVersion:1,revision:1,retentionDays:0}})})
 : fetchBeforeTrashLifecycle(url,options);
`)
	t.Cleanup(func() { frontendPollingRun(t, vm, `for(const slot of hookSlots)slot?.cleanup?.();`) })
	return vm
}

func TestFrontendTrashTokenRoundTripRejectsObsoleteMutationAndCallbacks(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var first=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `first=renderTrash();first.mutate("restore",["archive-one"]);options={...options,token:"synthetic-next"};var hook=renderTrash();`)
	frontendPollingRun(t, vm, `options={...options,token:"synthetic"};hook=renderTrash();first.mutate("purge",["archive-one"]);assert(requests.length===4,"stale handler submitted after token round trip");ok(requests[1],{restored:1,affectedIds:["one"],outcome:"completed"});ok(requests[3],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(receipts.length===0&&!hook.notice&&!hook.verificationRequired&&!hook.actionBusy,"old mutation receipt crossed session generation");assert(requests[1].options.signal.aborted&&requests[2].options.signal.aborted,"identity change left old operation/read running");hook.mutate("restore",["archive-two"]);hook.mutate("restore",["archive-two"]);assert(requests.length===5,"obsolete completion released new session duplicate guard");unmount();`)
}

func TestFrontendTrashFeatureToggleCancelsAndResetsSession(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `options.active=true;var hook=renderTrash();`)
	frontendPollingRun(t, vm, `ok(requests[0],{settings:{...trashSettings,retentionDays:30}});ok(requests[1],trashList([trashEntry()],{...trashSettings,retentionDays:30}));`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.reloadSettings();options={...options,enabled:false};hook=renderTrash();assert(hook.settings===null&&hook.entries.length===0&&!hook.loaded&&!hook.dirty,"disabled feature retained previous session data");`)
	frontendPollingRun(t, vm, `options={...options,enabled:true};hook=renderTrash();`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.reloadSettings();assert(requests.length===5,"old canceled read unlocked or blocked current settings read");ok(requests[2],{settings:{...trashSettings,revision:9,retentionDays:99}});ok(requests[3],{settings:trashSettings});ok(requests[4],trashList());`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.retentionDraft==="0"&&hook.entries.length===0&&hook.loaded&&!hook.settingsError,"disabled-session data returned after re-enable");unmount();assert(timers.size===0,"feature lifecycle leaked timers");`)
}

func TestFrontendTrashTokenChangeReplacesPendingSettingsRead(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();assert(requests.length===1,"missing initial settings read");options={...options,token:"synthetic-next"};hook=renderTrash();`)
	frontendPollingRun(t, vm, `assert(requests[0].options.signal.aborted,"old settings request survived identity change");assert(requests.length===2&&requests[1].options.headers["x-console-token"]==="synthetic-next","new identity was blocked by old settings-read lock");ok(requests[0],{settings:{...trashSettings,revision:9,retentionDays:30}});ok(requests[1],{settings:{...trashSettings,retentionDays:7}});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.settings.revision===1&&hook.retentionDraft==="7"&&!hook.settingsLoading,"old identity replaced current settings or spinner");unmount();assert(timers.size===0,"identity change leaked request timer");`)
}

func TestFrontendTrashIdentityChangeHidesPreviousAuthorityAndDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:{...trashSettings,revision:9,retentionDays:30}});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.setRetentionDraft("3");hook=renderTrash();options={...options,token:"synthetic-next"};hook=renderTrash();assert(hook.settings===null&&!hook.dirty&&hook.retentionDraft==="0","new identity displayed previous authority or unsaved draft");hook.saveSettings();assert(requests.every(request=>request.options.method!=="PUT"),"new identity saved previous configuration revision");`)
	frontendPollingRun(t, vm, `ok(requests[1],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.settings.revision===1&&hook.retentionDraft==="0","lower revision from new identity was rejected by old authority");unmount();`)
}

func TestFrontendTrashVerificationBlocksCleanupPolicyWrite(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.mutate("purge",["archive-one"]);requests[1].reject(new Error("synthetic lost response"));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.verificationRequired,"unknown mutation did not require read verification");hook.setRetentionDraft("7");hook=renderTrash();hook.saveSettings({draft:"7",revision:1});assert(requests.length===3&&requests[2].options.method===undefined,"unverified destructive result allowed cleanup-policy write");ok(requests[2],trashList());`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.saveSettings({draft:"7",revision:1});assert(requests.length===4&&requests[3].options.method==="PUT","verified state permanently blocked intended policy save");unmount();`)
}
