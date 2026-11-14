package console

import (
	"fmt"
	"testing"
)

func TestFrontendActiveLogsEarlierFailurePausesUntilRefresh(t *testing.T) {
	vm := frontendInsightsVM(t, "components/job-logs.jsx")
	frontendPollingRun(t, vm, `Intl.DateTimeFormat = function() {return {format() {return "合成时间";}};};`)
	frontendPollingRun(t, vm, `var props={token:"synthetic",jobId:"one",job:{status:"running"}};var tree=renderComponent("JobLogs",props);`)
	frontendPollingRun(t, vm, `ok(requests[0],{id:"one",logs:"Synthetic retained latest",entries:[],hasMore:true,nextBefore:"older"});`)
	frontendPollingRun(t, vm, `tree=renderComponent("JobLogs",props);assert(content(tree).includes("Synthetic retained latest"),"latest log missing before page read");findButton(tree,"加载更早日志").props.onClick();`)
	frontendPollingRun(t, vm, `assert(requests[1].url==="/api/jobs/one/logs?before=older","earlier cursor missing");requests[1].resolve({ok:false,status:503,json:async()=>({error:"synthetic earlier read failed"})});`)
	frontendPollingRun(t, vm, `tree=renderComponent("JobLogs",props);assert(content(tree).includes("自动更新已暂停")&&content(tree).includes("Synthetic retained latest")&&content(tree).includes("earlier read failed"),"failed earlier read hid paused state or retained entries");assert(timers.size===0,"paused history still polled");findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="刷新操作日志").props.onClick();tree=renderComponent("JobLogs",props);`)
	frontendPollingRun(t, vm, `assert(requests.length===3&&requests[2].url==="/api/jobs/one/logs","explicit refresh did not resume latest route");ok(requests[2],{id:"one",logs:"Synthetic fresh latest",entries:[],hasMore:true,nextBefore:"new cursor"});`)
	frontendPollingRun(t, vm, `tree=renderComponent("JobLogs",props);assert(!content(tree).includes("自动更新已暂停")&&!content(tree).includes("earlier read failed")&&content(tree).includes("Synthetic fresh latest"),"refresh did not clear pause/failure or accept new page");fire(1800);`)
	frontendPollingRun(t, vm, `assert(requests.length===4&&requests[3].url==="/api/jobs/one/logs","refresh did not restore active polling");unmount();assert(requests[3].options.signal.aborted&&timers.size===0,"resumed polling leaked on close");`)
}

const frontendLogPagesHarness = `
options={token:"synthetic",jobId:"one",archiveId:"archive-one",job:{status:"completed"}};
function renderPages(){hookCursor=0;const value=Insights.useJobLogPages(options);for(const run of effectQueue.splice(0))run();return value;}
function logPage(logs,entries=[],hasMore=false,nextBefore="",warning=""){return {id:options.jobId,logs,entries,hasMore,nextBefore,warning};}
`

func TestFrontendArchivedLogsPrependPagesAndPreserveWarning(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/jobLogs.js")
	frontendPollingRun(t, vm, frontendLogPagesHarness)
	frontendPollingRun(t, vm, `var hook=renderPages();`)
	frontendPollingRun(t, vm, `assert(requests[0].url==="/api/account-trash/archive-one/logs?jobId=one","archive used active job logs route");ok(requests[0],logPage("new\nlast",[{message:"new"},{message:"last"}],true,"opaque:/older?x=1","Synthetic legacy truncation"));`)
	frontendPollingRun(t, vm, `hook=renderPages();hook.loadEarlier();hook.loadEarlier();`)
	frontendPollingRun(t, vm, `assert(requests.length===2&&requests[1].url.endsWith("&before=opaque%3A%2Folder%3Fx%3D1"),"opaque cursor changed or duplicate read started");ok(requests[1],logPage("old",[{message:"old"}],false));`)
	frontendPollingRun(t, vm, `hook=renderPages();assert(hook.logs==="old\nnew\nlast"&&hook.entries.map(entry=>entry.message).join()==="old,new,last","earlier page dropped or reordered retained logs");assert(!hook.hasMore&&hook.historyLoaded&&hook.warning.includes("truncation"),"pagination completeness hid warning");hook.loadEarlier();assert(requests.length===2,"complete log history fetched again");unmount();assert(timers.size===0,"archive read leaked timer");`)
}
func TestFrontendArchivedLogsLatePageCannotCrossArchiveIdentity(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/jobLogs.js")
	frontendPollingRun(t, vm, frontendLogPagesHarness)
	frontendPollingRun(t, vm, `var hook=renderPages();`)
	frontendPollingRun(t, vm, `ok(requests[0],logPage("latest",[],true,"older"));`)
	frontendPollingRun(t, vm, `hook=renderPages();hook.loadEarlier();`)
	frontendPollingRun(t, vm, `options={...options,archiveId:"archive-two"};hook=renderPages();assert(requests[1].options.signal.aborted,"identity change did not cancel old archive read");`)
	frontendPollingRun(t, vm, `ok(requests[2],logPage("new archive"));ok(requests[1],logPage("obsolete archive"));`)
	frontendPollingRun(t, vm, `hook=renderPages();assert(hook.logs==="new archive"&&!hook.historyLoaded,"late old archive response crossed identity");unmount();`)
}
func TestFrontendArchivedLogsRefusalKeepsHistoryAndDoesNotClaimComplete(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/jobLogs.js")
	frontendPollingRun(t, vm, frontendLogPagesHarness)
	frontendPollingRun(t, vm, `var hook=renderPages();`)
	frontendPollingRun(t, vm, `ok(requests[0],logPage("latest",[],true,"older"));`)
	frontendPollingRun(t, vm, `hook=renderPages();hook.loadEarlier();`)
	frontendPollingRun(t, vm, `requests[1].resolve({ok:false,status:503,json:async()=>({error:"synthetic archive read failed"})});`)
	frontendPollingRun(t, vm, `hook=renderPages();assert(hook.logs==="latest"&&hook.hasMore&&hook.error.includes("archive read failed"),"failed earlier read lost latest page or claimed completion");assert(requests.length===2,"earlier log request automatically replayed");hook.refresh();hook=renderPages();`)
	frontendPollingRun(t, vm, `ok(requests[2],{id:"one",logs:"legacy result",entries:[]});`)
	frontendPollingRun(t, vm, `hook=renderPages();assert(hook.hasMore===null,"legacy response falsely claimed all retained logs");unmount();`)
}
func TestFrontendLogParserKeepsEveryLoadedHistoryPage(t *testing.T) {
	vm := jobLogsVM(t)
	entries := make([]map[string]any, 620)
	for index := range entries {
		entries[index] = map[string]any{"at": "2026-11-03T10:00:00Z", "operationAt": "2026-11-03T09:00:00Z", "operation": "refresh", "level": "info", "message": fmt.Sprintf("Synthetic retained entry %d", index)}
	}
	got := parseLogsFixture(t, vm, "", entries)
	if len(got) != len(entries) || got[0].Message != "Synthetic retained entry 0" || got[len(got)-1].Message != "Synthetic retained entry 619" {
		t.Fatalf("loaded history was truncated or reordered: %d", len(got))
	}
}
