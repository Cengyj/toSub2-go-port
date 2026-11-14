package console

import "testing"

func TestFrontendTrashSearchPageAndSelectionStayExplicit(t *testing.T) {
	vm := frontendBrowserVM(t, "accountTrash")
	frontendPollingRun(t, vm, `
const entries=Array.from({length:45},(_,index)=>({id:"trash-"+String(index).padStart(2,"0"),email:"synthetic-"+index+"@example.invalid",deletedAt:new Date(Date.UTC(2026,10,1,0,index)).toISOString(),state:index===12?"purging":"trashed",canRestore:index!==12,jobs:[{id:"history-"+index}]}));
const original=JSON.stringify(entries);
const first=Browser.selectTrashPage(entries),second=Browser.selectTrashPage(entries,{page:2});
assert(first.rows.length===20&&second.rows.length===20&&first.totalPages===3&&first.rows[0].id==="trash-44","page size or latest-first order incorrect");
assert(Browser.selectTrashPage(entries,{query:" HISTORY-12 "}).rows[0].id==="trash-12","historical identity search is not normalized");
assert(Browser.selectTrashPage(entries,{filter:"restorable"}).total===44&&Browser.selectTrashPage(entries,{filter:"purging"}).total===1,"restore eligibility differs from filtering");
assert(Browser.selectTrashPage(entries,{query:"absent",page:99}).page===1&&Browser.selectTrashPage(entries,{page:99}).page===3,"empty/removed last page not clamped");
const selected=new Set([first.rows[0].id,second.rows[0].id,"already-removed"]);
const scope=Browser.trashSelectionScope(entries,selected,first.rows);
assert(scope.selectedEntries.length===2&&scope.hiddenCount===1&&!scope.allVisibleSelected&&scope.someVisibleSelected,"hidden or stale selection lost its scope");
assert(JSON.stringify(entries)===original,"presentation reordered authoritative records");
`)
}

func TestFrontendTrashDeadlineDistinguishesRetentionAndDeletion(t *testing.T) {
	vm := frontendBrowserVM(t, "accountTrash")
	frontendPollingRun(t, vm, `
const entry={state:"trashed",expiresAt:"2026-11-04T00:00:00Z"}, now=Date.parse("2026-11-03T23:30:00Z");
const policy={retentionDays:7};
assert(Browser.describeTrashDeadline(entry,policy,{now}).label==="不足 1 天","near expiry not clear");
assert(Browser.describeTrashDeadline(entry,policy,{now:now+3600000}).label==="已到期，待清理","expiry incorrectly claimed deletion");
assert(Browser.describeTrashDeadline(entry,{retentionDays:0},{now}).at===null,"disabled policy retained stale deletion date");
assert(Browser.describeTrashDeadline(entry,policy,{now,uncertain:true}).label==="自动清理暂停","unconfirmed policy claimed a deadline");
assert(Browser.describeTrashDeadline({...entry,state:"purging"},{retentionDays:0},{now}).label==="清理未完成","disabling policy hid irreversible cleanup");
assert(Browser.describeTrashDeadline({...entry,expiresAt:"invalid"},policy,{now}).label==="清理时间待确认","unknown date fabricated expiry");
assert(!Browser.canRestoreTrash({state:"purging",canRestore:true}),"purging accepted as restorable");
`)
}
