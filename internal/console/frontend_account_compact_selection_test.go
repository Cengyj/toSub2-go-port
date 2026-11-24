package console

import (
	"context"
	"embed"
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

const compactAccountSelectionFixture = `
const compactStrings=["id","email","createdAt","lastOperationAt","authorizationMethod","status","operationStatus","totpWorkflowStatus"];
const compactBooleans=["canResume","canDownload","canRegenerate","canRetry","canForceRelogin","canSetupTotp","canResetTotp","canAddPassword","canCompleteTotp","totpExportBlocked","totpRecoveryPending","totpCredentialInvalidated","totpCompletionPending"];
function compactRow(id="saved"){return {id,email:id+"@example.invalid",createdAt:"0001-01-01T00:00:00Z",lastOperationAt:"2026-11-24T00:00:00.123456789Z",attempt:0,authorizationMethod:"chatgpt",status:"completed",operationStatus:"succeeded",totpWorkflowStatus:"",...Object.fromEntries(compactBooleans.map(key=>[key,false]))};}
function compactSnapshot(rows=[compactRow()]){return {selectionMode:"compact",jobs:rows.map(row=>({...row,prompt:"visible row detail"})),selection:rows,pagination:{page:1,pageSize:20,total:rows.length,totalPages:1},stats:{active:0,queued:0,completed:rows.length}};}
`

func TestFrontendAccountPollingRequestsCompactSelectionForBothReadRoutes(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, accountPollingScopeFixture)
	frontendPollingRun(t, vm, `
assert(requests[0].url==="/api/jobs?selection=compact&page=1"&&!requests[0].options.method,"ordinary polling did not opt into the compatible compact response");
props={...props,page:2,emailFilter:["one@example.invalid","two@example.invalid"]};renderAccounts();
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2&&requests[1].url==="/api/jobs/query?selection=compact"&&requests[1].options.method==="POST","filtered read did not request compact selection");
const body=JSON.parse(requests[1].options.body);assert(Object.keys(body).sort().join()==="emails,page"&&body.page===2&&body.emails.join()===props.emailFilter.join(),"compact negotiation changed the existing filter body contract");
unmount();assert(timers.size===0,"compact read negotiation leaked request timers");
`)
}

func TestFrontendAccountPollingValidatesEveryCompactSelectionFieldBeforePublishing(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, compactAccountSelectionFixture+accountPollingScopeFixture)
	frontendPollingRun(t, vm, `ok(requests[0],compactSnapshot());`)
	frontendPollingRun(t, vm, `
var kept={jobs:presented.jobs,selection:presented.selection,pagination:presented.pagination,stats:presented.stats},invalidSnapshots=[];
for(const key of [...compactStrings,"attempt",...compactBooleans]){
 const missing=compactRow("obsolete");delete missing[key];invalidSnapshots.push(compactSnapshot([missing]));
 invalidSnapshots.push(compactSnapshot([{...compactRow("obsolete"),[key]:null}]));
 invalidSnapshots.push(compactSnapshot([{...compactRow("obsolete"),[key]:compactBooleans.includes(key)?"false":key==="attempt"?"0":false}]));
}
for(const patch of [{attempt:-1},{attempt:0.5},{attempt:9007199254740992},{id:""},{status:""},{authorizationMethod:"unsupported"},{createdAt:""},{createdAt:"not-a-date"},{lastOperationAt:"2026-11-24"}])invalidSnapshots.push(compactSnapshot([{...compactRow("obsolete"),...patch}]));
invalidSnapshots.push({...compactSnapshot(),selectionMode:null},{...compactSnapshot(),selectionMode:"future-mode"},{...compactSnapshot(),selection:undefined},{...compactSnapshot(),selection:null});
var invalidCount=invalidSnapshots.length;
`)
	count := vm.Get("invalidCount").ToInteger()
	for index := int64(0); index < count; index++ {
		if err := vm.Set("invalidIndex", index); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `var publishedBefore=published;refresh();`)
		frontendPollingRun(t, vm, `ok(requests.at(-1),invalidSnapshots[invalidIndex]);`)
		frontendPollingRun(t, vm, `
assert(published===publishedBefore,"invalid compact response published part of the snapshot at index "+invalidIndex);
for(const key of Object.keys(kept))assert(presented[key]===kept[key],"invalid compact response replaced "+key+" at index "+invalidIndex);
assert(!presented.connected&&presented.error==="账号列表响应不完整","invalid compact response was not reported safely");
`)
	}
	frontendPollingRun(t, vm, `refresh();`)
	frontendPollingRun(t, vm, `
const compatible={...compactRow("empty-email"),email:"",operationStatus:"",lastOperationAt:"0001-01-01T00:00:00Z",future:{enabled:false,weight:0,models:[]}};
ok(requests.at(-1),compactSnapshot([compatible]));
`)
	frontendPollingRun(t, vm, `
assert(presented.connected&&!presented.error&&presented.selection[0].email===""&&presented.selection[0].lastOperationAt==="0001-01-01T00:00:00Z","compact recovery rejected empty email or zero Go timestamps");
for(const key of compactBooleans)assert(presented.selection[0][key]===false,"explicit false capability was defaulted or lost");
assert(presented.selection[0].attempt===0&&presented.selection[0].future.models.length===0&&presented.selection[0].future.enabled===false,"compact validation stripped explicit zero or unknown fields");
refresh();
`)
	frontendPollingRun(t, vm, `const legacy={jobs:[{id:"legacy",email:""}],stats:{active:0,queued:0,completed:0}};ok(requests.at(-1),legacy);`)
	frontendPollingRun(t, vm, `
assert(presented.connected&&presented.jobs[0].id==="legacy"&&presented.selection[0]===presented.jobs[0],"legacy full response without marker lost compatibility");
refresh();
`)
	frontendPollingRun(t, vm, `ok(requests.at(-1),compactSnapshot([]));`)
	frontendPollingRun(t, vm, `assert(presented.connected&&presented.jobs.length===0&&presented.selection.length===0,"valid empty compact snapshot failed");unmount();assert(timers.size===0,"compact validation leaked timers");`)
}

func TestFrontendAccountPollingAcceptsActualCompactHTTPProjection(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	root, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	manager := jobs.NewManager(context.Background(), 1, root, nil)
	manager.SetCredentialStore(vault)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error("synthetic compact manager did not shut down:", err)
		}
	})
	if _, err := manager.Submit(jobs.Spec{ID: "compact-empty-email", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{OutputRoot: root.Root, StartupToken: "synthetic"}, manager, embed.FS{}, nil, vault)
	t.Cleanup(server.Close)
	response := frontendRequest(server, http.MethodGet, "/api/jobs?selection=compact&page=1", nil, "synthetic")
	if response.Code != http.StatusOK {
		t.Fatalf("compact list returned HTTP %d", response.Code)
	}
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	if err := vm.Set("actualSnapshot", response.Body.String()); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, accountPollingScopeFixture)
	frontendPollingRun(t, vm, `const data=JSON.parse(actualSnapshot);assert(data.selectionMode==="compact"&&Object.keys(data.selection[0]).length===22,"server did not produce the reviewed compact contract");ok(requests[0],data);`)
	frontendPollingRun(t, vm, `
assert(presented.connected&&!presented.error&&presented.jobs[0].id==="compact-empty-email"&&presented.selection[0].email==="","actual compact HTTP projection was rejected or changed identity");
assert(presented.selection[0].canCompleteTotp===false&&typeof presented.selection[0].lastOperationAt==="string","actual compact index omitted a command generation or explicit safety capability");
unmount();
`)
}

func TestFrontendAccountPollingRejectsInconsistentCompactPage(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, compactAccountSelectionFixture+accountPollingScopeFixture)
	frontendPollingRun(t, vm, `
var rows=[compactRow("first"),compactRow("second"),compactRow("third")];
var complete={...compactSnapshot(rows),jobs:rows.slice(0,2).map(row=>({...row,prompt:"visible detail"})),pagination:{page:1,pageSize:2,total:3,totalPages:2,totalAll:9}};
ok(requests[0],complete);
`)
	frontendPollingRun(t, vm, `
assert(presented.connected&&presented.selection.length===3&&presented.jobs.length===2,"valid cross-page compact snapshot was rejected");
var retained={jobs:presented.jobs,selection:presented.selection,pagination:presented.pagination,stats:presented.stats};
var inconsistent=[
 {...complete,selection:rows.slice(0,2)},
 {...complete,jobs:complete.jobs.slice(0,1)},
 {...complete,jobs:rows},
 {...complete,jobs:[compactRow("outside"),rows[1]]},
 {...complete,pagination:undefined},
 ...[{page:3},{totalPages:1},{totalPages:3},{total:2},{totalAll:2},{page:2}].map(patch=>({...complete,pagination:{...complete.pagination,...patch}})),
 ...[...compactStrings,"attempt",...compactBooleans].map(key=>({...complete,jobs:complete.jobs.map((row,index)=>index?row:{...row,[key]:compactBooleans.includes(key)?!row[key]:key==="attempt"?row[key]+1:row[key]+"-different"})})),
];
var inconsistentCount=inconsistent.length;
`)
	for index := int64(0); index < vm.Get("inconsistentCount").ToInteger(); index++ {
		if err := vm.Set("inconsistentIndex", index); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `var before=published;refresh();`)
		frontendPollingRun(t, vm, `ok(requests.at(-1),inconsistent[inconsistentIndex]);`)
		frontendPollingRun(t, vm, `
assert(published===before&&!presented.connected&&presented.error==="账号列表响应不完整","inconsistent compact page was published at index "+inconsistentIndex);
for(const key of Object.keys(retained))assert(presented[key]===retained[key],"inconsistent response replaced retained "+key);
`)
	}
	frontendPollingRun(t, vm, `refresh();`)
	frontendPollingRun(t, vm, `ok(requests.at(-1),{...complete,jobs:[{...rows[2],prompt:"last-page detail"}],pagination:{...complete.pagination,page:2}});`)
	frontendPollingRun(t, vm, `assert(presented.connected&&presented.jobs[0].id==="third"&&presented.selection.length===3&&presented.page===2,"valid final compact page or server page correction was rejected");unmount();assert(timers.size===0,"compact page consistency validation leaked timers");`)
}

func TestFrontendCompactSelectionPreservesCrossPageActions(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountCollection.js")
	frontendPollingRun(t, vm, compactAccountSelectionFixture+`
function renderCollection(){hookCursor=0;const result=Insights.useAccountCollection();for(const run of effectQueue.splice(0))run();return result;}
var collection=renderCollection(),one={...compactRow("one"),canDownload:true,canForceRelogin:true},two={...compactRow("two"),canDownload:true,canResetTotp:true};
collection.setJobs([one]);collection.setJobSelectionIndex([one,two]);collection.setSelectedJobIds(new Set(["one","two"]));collection=renderCollection();
assert(collection.selectedJobs.length===2&&collection.canUploadSelected&&collection.forceReloginSelectedCount===1&&collection.totpResetSelectedCount===1,"compact index lost cross-page command eligibility");
collection.setPage(2);collection.setJobs([two]);collection=renderCollection();
assert(collection.selectedJobIds.size===2&&collection.selectedJobs.length===2&&collection.allPageSelected,"page change discarded the off-page selection");
collection.toggleAllOnPage();collection=renderCollection();assert(collection.selectedJobIds.size===1&&collection.selectedJobIds.has("one"),"page-only toggle changed the off-page selection");
collection.setJobSelectionIndex([{...one,totpCompletionPending:true,totpExportBlocked:true,canDownload:false},two]);collection=renderCollection();
assert(!collection.canUploadSelected&&!collection.canDownloadSelected,"compact security update retained old bulk capabilities");
collection.setJobSelectionIndex([two]);renderCollection();collection=renderCollection();assert(collection.selectedJobIds.size===0,"removed account survived selection revalidation");unmount();
`)
}
