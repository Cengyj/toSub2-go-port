package console

import "testing"

func TestFrontendTrashDurabilityWarningRequiresExplicitSameValueSave(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();var warningSettings={...trashSettings,retentionDays:30,persistenceWarning:"Synthetic directory sync uncertain; cleanup paused"};ok(requests[0],{settings:warningSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.persistenceUnconfirmed&&!hook.uncertain&&!hook.dirty&&hook.settings.persistenceWarning.includes("paused"),"read discarded durability warning or permanently blocked explicit save");hook.reloadSettings();`)
	frontendPollingRun(t, vm, `ok(requests[1],{settings:warningSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.persistenceUnconfirmed&&!hook.uncertain,"GET incorrectly confirmed persistence");hook.saveSettings({draft:"30",revision:1});const body=JSON.parse(requests[2].options.body);assert(Object.keys(body).sort().join()==="retentionDays,revision,schemaVersion"&&body.retentionDays===30,"same-value acknowledgement omitted policy or serialized response-only warning");ok(requests[2],{settings:{...trashSettings,revision:2,retentionDays:30}});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(!hook.persistenceUnconfirmed&&!hook.uncertain,"successful explicit re-save did not clear durability warning");ok(requests[3],trashList([],{...trashSettings,revision:2,retentionDays:30}));`)
	frontendPollingRun(t, vm, `unmount();assert(timers.size===0,"durability recovery leaked timers");`)
}
func TestFrontendTrashWarningAllowsSameValueAcknowledgementButton(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, `var trash={settings:{schemaVersion:1,revision:1,retentionDays:30,persistenceWarning:"Synthetic warning"},retentionDraft:"30",dirty:false,persistenceUnconfirmed:true,saveSettings:async()=>true,setRetentionDraft(){},reloadSettings(){},resetDraft(){}};var tree=renderComponent("AccountTrashSettings",{trash});assert(content(tree).includes("自动清理暂停")&&!findButton(tree,"重新确认保存").props.disabled,"durability warning cannot be explicitly acknowledged at same value");unmount();`)
}
func TestFrontendTrashUnmountCancelsPendingSettingsRead(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `renderTrash();unmount();assert(requests[0].options.signal.aborted,"pending settings read was not aborted");`)
	frontendPollingRun(t, vm, `assert(timers.size===0,"pending settings read leaked timeout after unmount");`)
}

func TestFrontendAppMoveUsesServerIdentityAndKeepsUnknownRecords(t *testing.T) {
	vm := frontendAppTrashLifecycleVM(t)
	frontendPollingRun(t, vm, `
location.hash="#accounts";var tree=render();appState.setToken("synthetic");
var rows=[{id:"one",email:"same@example.invalid",status:"completed"},{id:"other-history",email:"same@example.invalid",status:"completed"}];
appState.setJobs(rows);appState.setJobSelectionIndex(rows);appState.setSelectedJobIds(new Set(["one"]));appState.setFeatures({accountTrash:true,bulkActions:true});tree=render();
findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="one").props.onViewInsights();tree=render();assert(find(tree,"AccountInsightsDialog"),"test did not open insight detail");
confirmAccepted=true;var remove=findButton(tree,"删除");remove.props.onClick();remove.props.onClick();
`)
	frontendPollingRun(t, vm, `assert(requests.length===1&&requests[0].url==="/api/account-trash/move","delete did not move or double submitted");requests[0].resolve({ok:true,status:200,json:async()=>({moved:1,affectedIds:["one"],outcome:"completed"})});`)
	frontendPollingRun(t, vm, `tree=render();assert(!findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="one"),"confirmed moved record remained");assert(findMatching(tree,node=>node.type==="JobRow"&&node.props.job.id==="other-history"),"browser guessed same-email affected identity");assert(!find(tree,"AccountInsightsDialog"),"archived record retained active insight dialog");assert(requests.length===2&&requests[1].url==="/api/account-trash","move did not reconcile recycle list");find(tree,"AppShell").props.onNavigate("trash");tree=render();assert(find(tree,"AccountTrashPage")&&find(tree,"AppShell").props.page==="trash","trash navigation is not a real page");`)
}

func TestFrontendActiveAccountsRejectPreMutationRead(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, `
var displayed=[];
var props={token:"synthetic",page:1,emailFilter:[],setJobs:value=>{displayed=value;},setJobSelectionIndex(){},setPagination(){},setStats(){},setConnected(){},setPage(){},setConnectionError(){}};
function readAccounts(){hookCursor=0;const value=Insights.useAccountPolling(props);for(const run of effectQueue.splice(0))run();return value;}
var refresh=readAccounts();
`)
	frontendPollingRun(t, vm, `ok(requests[0],{jobs:[{id:"retained",email:"saved@example.invalid"}]});`)
	frontendPollingRun(t, vm, `refresh();`)
	frontendPollingRun(t, vm, `refresh.invalidate();refresh();ok(requests[1],{jobs:[{id:"obsolete",email:"old@example.invalid"}]});`)
	frontendPollingRun(t, vm, `assert(displayed[0].id==="retained","pre-mutation account read overwrote newer lifecycle");fire(0);`)
	frontendPollingRun(t, vm, `ok(requests[2],{jobs:[]});`)
	frontendPollingRun(t, vm, `assert(displayed.length===0,"fresh post-mutation account read not applied");unmount();assert(timers.size===0,"active account read leaked timers");`)
}

const frontendTrashHarness = `
options={token:"synthetic",enabled:true,active:false,onMutationStart(){starts++;},onMutation(receipt){receipts.push(receipt);}};
var starts=0,receipts=[];
function renderTrash(){hookCursor=0;const value=Insights.useAccountTrash(options);for(const run of effectQueue.splice(0))run();return value;}
const trashSettings={schemaVersion:1,revision:1,retentionDays:0};
function trashEntry(id="archive-one"){return {id,email:"synthetic@example.invalid",deletedAt:"2026-11-03T10:00:00Z",state:"trashed",recordCount:1,canRestore:true,jobs:[{id:"one",status:"completed",hasLogs:true}]};}
function trashList(entries=[],settings=trashSettings){return {entries,settings,status:{running:false}};}
`

func TestFrontendTrashRejectsIncompleteListAndUsesSafeSettings(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `
for(const value of [{},null,{schemaVersion:1,revision:1,retentionDays:-1},{schemaVersion:1,revision:1,retentionDays:1.5},{schemaVersion:1,revision:1,retentionDays:3651}]){let threw=false;try{Insights.validateTrashSettings(value);}catch{threw=true;}assert(threw,"invalid cleanup policy accepted");}
assert(Insights.validateTrashSettings(trashSettings).retentionDays===0,"default silently enabled automatic deletion");
for(const value of [null,{},trashList([null]),trashList([trashEntry(),trashEntry()]),trashList([{...trashEntry(),state:"complete"}])]){let threw=false;try{Insights.validateTrashList(value);}catch{threw=true;}assert(threw,"invalid recycle list accepted");}
var hook=renderTrash();hook.saveSettings();assert(requests.length===1&&requests[0].url.endsWith("/settings"),"unread settings started deletion or saved a fallback");ok(requests[0],{settings:trashSettings});
`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.settings.retentionDays===0&&!hook.dirty,"server default not adopted");hook.setRetentionDraft("");hook=renderTrash();hook.saveSettings();assert(requests.length===1&&renderTrash().settingsError.includes("整数"),"empty draft silently disabled cleanup");unmount();`)
}

func TestFrontendTrashUnknownSettingsNeedsExplicitReReadAndNeverReplays(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.setRetentionDraft("7");hook=renderTrash();hook.saveSettings({draft:"7",revision:1});hook.saveSettings();assert(requests.length===2&&requests[1].options.method==="PUT","duplicate cleanup write");requests[1].resolve({ok:false,status:503,json:async()=>({settings:{...trashSettings,revision:2,retentionDays:7},outcome:"unknown",uncertain:true,error:"synthetic persistence uncertainty"})});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.uncertain&&!hook.saving&&hook.settingsError.includes("尚未确认"),"unknown save claimed completion");assert(requests.length===3&&requests[2].url==="/api/account-trash","save did not verify list");hook.saveSettings();assert(requests.length===3,"unknown save replayed");ok(requests[2],trashList([],{...trashSettings,revision:2,retentionDays:7}));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.uncertain,"list read incorrectly confirmed policy durability");hook.reloadSettings();assert(requests.length===4&&requests[3].url.endsWith("/settings"),"explicit policy re-read missing");ok(requests[3],{settings:{...trashSettings,revision:2,retentionDays:7}});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(!hook.uncertain&&!hook.dirty&&hook.retentionDraft==="7","policy re-read did not reconcile durable value");unmount();assert(timers.size===0,"trash settings leaked timers");`)
}

func TestFrontendTrashMutationsKeepPartialReceiptAndGateRepetition(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.mutate("move",["one","one","two"]);hook.mutate("move",["one"]);assert(starts===1&&requests.length===2,"duplicate move before rerender");assert(requests[1].url==="/api/account-trash/move"&&JSON.parse(requests[1].options.body).ids.join()==="one,two","move used old permanent-delete route");requests[1].resolve({ok:false,status:503,json:async()=>({moved:1,affectedIds:["one"],outcome:"partial",error:"synthetic second item failed"})});`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(receipts.length===1&&receipts[0].affectedIds.join()==="one"&&hook.notice.outcome==="partial","partial receipt hid confirmed identities");assert(hook.verificationRequired&&requests.length===3,"partial write was not followed by authoritative read");hook.mutate("restore",["archive-one"]);assert(requests.length===3,"unverified write was repeated");ok(requests[2],trashList([trashEntry()]));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(!hook.verificationRequired&&hook.entries.length===1,"verified recycle snapshot not adopted");hook.mutate("purge",["archive-one"]);assert(JSON.parse(requests[3].options.body).confirmPermanent===true,"purge omitted explicit permanent flag");requests[3].reject(new Error("synthetic response lost"));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.notice.outcome==="unknown"&&hook.entries.length===1&&requests[4].options.method===undefined,"unknown purge erased cache or replayed mutation");ok(requests[4],trashList());`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.entries.length===0&&!hook.verificationRequired,"read-after-unknown did not reconcile");unmount();assert(timers.size===0,"mutation leaked timers");`)
}

func TestFrontendTrashLateReadCannotUndoMutationReconciliation(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `options.active=true;var hook=renderTrash();`)
	frontendPollingRun(t, vm, `ok(requests[0],{settings:trashSettings});ok(requests[1],trashList([trashEntry("retained")]));`)
	frontendPollingRun(t, vm, `hook=renderTrash();fire(5000);`)
	frontendPollingRun(t, vm, `hook.mutate("move",["one"]);requests[3].resolve({ok:true,status:200,json:async()=>({moved:1,affectedIds:["one"],outcome:"completed"})});`)
	frontendPollingRun(t, vm, `ok(requests[2],trashList([trashEntry("obsolete")]));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.entries[0].id==="retained"&&hook.verificationRequired,"pre-mutation read replaced current snapshot or released verification");fire(0);`)
	frontendPollingRun(t, vm, `ok(requests[4],trashList([trashEntry("fresh")]));`)
	frontendPollingRun(t, vm, `hook=renderTrash();assert(hook.entries[0].id==="fresh"&&!hook.verificationRequired,"fresh post-mutation read was not applied");unmount();assert(timers.size===0,"trash poll leaked timers");`)
}

func TestFrontendTrashPolicyConfirmationAndRevalidation(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, `
var saves=[];var trash={settings:{schemaVersion:1,revision:8,retentionDays:0},retentionDraft:"7",dirty:true,saveSettings:async expected=>{saves.push(expected);return true;},setRetentionDraft(){},reloadSettings(){},resetDraft(){}};
var tree=renderComponent("AccountTrashSettings",{trash});confirmAccepted=false;tree.props.onSubmit({preventDefault(){}});
`)
	frontendPollingRun(t, vm, `assert(saves.length===0,"declined retention activation still saved");confirmAccepted=true;tree.props.onSubmit({preventDefault(){}});tree.props.onSubmit({preventDefault(){}});`)
	frontendPollingRun(t, vm, `assert(saves.length===1&&saves[0].revision===8&&saves[0].draft==="7","confirmation lost its exact policy or duplicated save");unmount();`)
}
