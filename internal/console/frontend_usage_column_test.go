package console

import (
	"testing"

	"github.com/grafana/sobek"
)

// Exercise the production component through the existing synthetic React
// boundary. Browser layout and focus verification remain separate checks.
func frontendUsageColumnVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	return vm
}

const frontendUsageTreeQueries = `
function collectUsageNodes(tree, predicate) {
  if (!tree || typeof tree !== "object") return [];
  const matches = predicate(tree) ? [tree] : [];
  return matches.concat((Array.isArray(tree) ? tree : tree.props?.children || []).flatMap(child => collectUsageNodes(child, predicate)));
}
function usageNodes(tree, type) {return collectUsageNodes(tree, node => node.type === type);}
function usageClass(tree, name) {return collectUsageNodes(tree, node => (node.props?.className || "").split(" ").includes(name));}
function assertEmptyUsageTrack(tree, expectedLabel) {
  const placeholders=usageClass(tree,"usage-placeholder"),tracks=usageClass(tree,"usage-track-empty");
  assert(tree&&placeholders.length===1&&tracks.length===1&&usageClass(placeholders[0],"usage-track-empty")[0]===tracks[0],"unmeasured quota lost its single static empty track");
  const labels=usageClass(placeholders[0],"metric-state-label");
  assert(labels.length===1&&content(labels[0]).trim()&&(expectedLabel===undefined||content(labels[0])===expectedLabel),"empty track lost its explicit state description");
  assert(!usageNodes(tree,"progress").length&&!collectUsageNodes(tree,node=>node.props?.["aria-valuenow"]!==undefined).length&&!usageClass(tree,"usage-percent").length&&!usageClass(tree,"usage-window").length,"empty track fabricated a measured value or window");
  assert(!content(tree).includes("0%")&&!content(tree).includes("未知"),"empty track displayed zero or unknown quota text");
}
`

func TestFrontendUsageColumnLabelsUseReportedDurations(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
for (const [window, short, full] of [
  [{name:"primary",windowSeconds:604800},"7d","7 天窗口"],
  [{name:"secondary",windowSeconds:18000},"5h","5 小时窗口"],
  [{windowSeconds:86400},"1d","1 天窗口"],
  [{windowSeconds:5400},"90m","90 分钟窗口"],
  [{windowSeconds:45},"45s","45 秒窗口"],
  [{name:"primary"},"primary","primary"],
  [{name:"自定义窗口",windowSeconds:0},"自定义窗口","自定义窗口"],
  [{windowSeconds:Infinity},"额度窗口","额度窗口"],
]) {
  assert(Browser.insightWindowShortLabel(window) === short, "compact label guessed a window from its position or name: " + JSON.stringify(window));
  assert(Browser.insightWindowLabel(window) === full, "accessible window label lost reported duration");
}
`)
}

func TestFrontendUsageColumnResetDescriptionPreservesUnconfirmedState(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const now = Date.parse("2026-10-31T12:00:00Z");
for (const value of [undefined,null,"","synthetic-invalid-date",now]) {
  const result = Browser.describeInsightReset({resetsAt:value},now);
  assert(result.text === "重置时间未知" && !result.pending, "invalid reset time became a countdown");
}
for (const offset of [-3600000,0]) {
  const window = Object.freeze({usedPercent:83,resetsAt:new Date(now+offset).toISOString()});
  const result = Browser.describeInsightReset(window,now);
  assert(result.pending && result.text.includes("待确认") && result.title.includes("保留上次用量"), "elapsed reset claimed confirmed recovery");
  assert(window.usedPercent === 83, "elapsed reset modified cached usage");
}
for (const [offset,expected] of [[1,"1分钟后重置"],[9000000,"2小时30分后重置"],[90000000,"1天1小时后重置"]]) {
  const result = Browser.describeInsightReset({resetsAt:new Date(now+offset).toISOString()},now);
  assert(result.text === expected && !result.pending, "future reset countdown lost its time boundary: " + result.text);
}
`)
}

func TestFrontendUsageColumnShowsTwoWindowsWithoutReordering(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const snapshot = {usage:{observedAt:"2026-10-31T12:00:00Z",windows:[
  {name:"primary",windowSeconds:604800,usedPercent:82},
  {name:"secondary",windowSeconds:18000,usedPercent:0},
  {windowSeconds:3600,usedPercent:36},
  {windowSeconds:86400,usedPercent:12},
]}};
const before = JSON.stringify(snapshot);
const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){},accountLabel:"synthetic@example.invalid"});
assert(usageClass(tree,"usage-window-label").map(content).join() === "7 天,5 小时", "summary reordered reported windows or guessed 5h/7d from position");
assert(usageNodes(tree,"progress").map(node => node.props.value).join() === "82,0", "summary changed reported window usage");
assert(usageClass(tree,"usage-window").length === 2 && content(tree).includes("另有 2 个窗口"), "summary omitted overflow or exceeded its compact two-window limit");
assert(tree.props["aria-label"].includes("7 天窗口已用 82%") && tree.props["aria-label"].includes("5 小时窗口已用 0%"), "screen-reader summary lost exact window identities");
assert(JSON.stringify(snapshot) === before && requests.length === 0 && timers.size === 0, "render modified or independently refreshed cached usage");
`)
}

func TestFrontendUsageColumnNextResetUsesEarliestFutureAcrossAllWindows(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-04T05:00:00Z"),observedAt="2026-11-02T12:34:00Z",next="2026-11-04T14:17:00+08:00";
const windows=Object.freeze([
  Object.freeze({windowSeconds:604800,usedPercent:82,resetsAt:"2026-11-10T05:00:00Z"}),
  Object.freeze({windowSeconds:18000,usedPercent:0,resetsAt:"2026-11-04T04:00:00Z"}),
  Object.freeze({windowSeconds:3600,usedPercent:36,resetsAt:next}),
  Object.freeze({windowSeconds:86400,usedPercent:12,resetsAt:"2026-11-04T07:00:00Z"}),
  Object.freeze({windowSeconds:7200,usedPercent:4,resetsAt:"invalid-reset"}),
]);
const snapshot=Object.freeze({usage:Object.freeze({observedAt,windows}),lastAttempt:Object.freeze({status:"success"})}),before=JSON.stringify(snapshot);
const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now}),reset=usageClass(tree,"usage-next-reset")[0],time=find(reset,"time");
const date=new Date(next),pad=value=>String(value).padStart(2,"0");
const full=date.getFullYear()+"-"+pad(date.getMonth()+1)+"-"+pad(date.getDate())+" "+pad(date.getHours())+":"+pad(date.getMinutes());
assert(time?.props.dateTime===next&&content(time)===full.slice(5)&&content(reset)===full.slice(5),"next reset did not choose the nearest future timestamp from the hidden windows or lost its date and minute");
assert(reset.props.title.includes("1 小时窗口")&&reset.props.title.includes("预计重置")&&reset.props.title.includes(full)&&tree.props.title.includes(full)&&tree.props["aria-label"].includes(full),"selected reset lost its window, year or estimated-reset meaning in disclosure");
assert(usageClass(tree,"usage-window-label").map(content).join()==="7 天,5 小时"&&usageNodes(tree,"progress").map(node=>node.props.value).join()==="82,0","choosing the next reset reordered the visible quota windows");
assert(!usageClass(tree,"usage-observed").length&&!usageClass(tree,"usage-record-empty").length&&!tree.props.title.includes("2026-11-02")&&!usageNodes(tree,"time").some(node=>node.props.dateTime===observedAt),"compact quota summary still exposed a refresh or observation date");
assert(JSON.stringify(snapshot)===before&&requests.length===0&&timers.size===0,"selecting a reset mutated or refreshed the saved observations");
`)
}

func TestFrontendUsageColumnNextResetNeverExtrapolatesPastOrUnknownTimes(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-04T05:00:00Z"),observedAt="2026-11-04T04:59:00Z";
for(const [resets,expected] of [
  [[],""],
  [[undefined,null,"","invalid-reset",now],""],
  [["2026-11-04T04:00:00Z",undefined,"invalid-reset"],"重置待确认"],
  [["2026-11-04T05:00:00Z"],"重置待确认"],
]) {
  const snapshot={usage:{observedAt,windows:resets.map(resetsAt=>({windowSeconds:18000,usedPercent:73,resetsAt}))},bps:{observedAt:"2099-01-01T00:00:00Z",allowed:true},subscription:{observedAt:"2099-01-02T00:00:00Z"}};
  const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now}),reset=usageClass(tree,"usage-next-reset")[0];
  assert(content(reset)===expected&&!usageNodes(tree,"time").length,"past, equal or unknown reset was shown as a future date or replaced by a source observation: "+JSON.stringify(resets));
  assert(!tree.props.title.includes("2099-")&&!tree.props.title.includes("04:59"),"missing next reset fell back to another source's observation time");
  if(!expected)assert(!reset&&!content(tree).includes("未知"),"unknown reset rendered a placeholder or estimated timestamp");
  else assert(reset.props.title.includes("确认")&&reset.props.title.includes("保留上次用量"),"past reset lost its confirmation requirement or implied recovered quota");
  if(resets.length)assert(usageNodes(tree,"progress").every(node=>node.props.value===73),"elapsed reset invented unused quota");
  assert(JSON.stringify(snapshot)===before,"reset uncertainty rewrote saved windows");
}
assert(requests.length===0&&timers.size===0,"missing next reset scheduled a query or extrapolation");
`)
}

func TestFrontendUsageColumnDistinguishesZeroUnknownAndClampedBars(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const value of [null,undefined,"0",false,NaN,Infinity,-Infinity]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:value}]}},onOpen(){}});
  assertEmptyUsageTrack(tree);
}
for (const [value,barValue] of [[0,0],[12.5,12.5],[125,100],[-8,0]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:value}]}},onOpen(){}});
  const bar = find(tree,"progress");
  assert(bar && Number(bar.props.max) === 100 && bar.props.value === barValue, "valid progress value was absent or escaped visual bounds");
  assert(usageClass(tree,"usage-percent").map(content).join() === value + "%" && bar.props["aria-label"].includes(value + "%"), "visual clamping changed the reported numeric value");
  assert(!usageClass(tree,"usage-placeholder").length&&!usageClass(tree,"usage-track-empty").length,"reported zero or measured quota retained the unqueried empty track");
}
assert(requests.length === 0, "invalid usage triggered a hidden recovery request");
`)
}

func TestFrontendUsageColumnRetainsHistoricalUsageAfterPartialBPSSuccess(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now = Date.parse("2026-10-31T12:00:00Z");
const snapshot = {persisted:true,lastAttempt:{status:"partial",startedAt:"2026-10-31T12:00:00Z"},
  usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:92,resetsAt:"2026-10-31T11:00:00Z"}]},
  bps:{observedAt:"2026-10-31T12:00:01Z",allowed:true},errors:[{component:"usage",code:"permission_denied"}]};
const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
assert(!content(tree).includes("已更新") && !content(tree).includes("历史额度") && !tree.props.title.includes("历史额度") && !tree.props["aria-label"].includes("历史额度") && tree.props["aria-label"].includes("BPS 模型权限已更新；Codex 额度未更新") && tree.props.className.includes("has-history"), "compact source outcome was repeated visibly or fresh BPS permission made retained usage current");
const reset=usageClass(tree,"usage-next-reset")[0];
assert(content(reset)==="重置待确认"&&!find(reset,"time")&&!usageClass(tree,"usage-meta").length&&!usageClass(tree,"usage-history-label").length, "expired historical usage was presented as a future reset or an observation time");
assert(find(tree,"progress").props.value === 92 && content(tree).includes("92%"), "partial permission failure erased or reset historical usage");
assert(content(tree).includes("待确认") && !tree.props.title.includes("历史额度") && !tree.props.title.includes("2026-10-30"), "partial result hid expired reset uncertainty or exposed an unrelated observation date");
assert(!content(tree).includes("权限可用") && !content(tree).includes("授权失效"), "permission result replaced usage or became an authorization failure");
`)
}

func TestFrontendUsageColumnElapsedResetNeverInventsFreshCapacity(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now = Date.parse("2026-10-31T12:00:00Z");
for (const value of [0,73,null]) {
  const snapshot = {lastAttempt:{status:"success"},usage:{observedAt:"2026-10-31T10:00:00Z",windows:[{windowSeconds:18000,usedPercent:value,resetsAt:"2026-10-31T11:00:00Z"}]}};
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  if(value===null){assertEmptyUsageTrack(tree);assert(!find(tree,"time")&&!usageClass(tree,"usage-next-reset").length,"an elapsed reset alone revived unmeasured reset metadata");continue;}
  assert(content(tree).includes("预计已重置") && content(tree).includes("待确认"), "elapsed reset was presented as confirmed even for zero or unknown usage");
  assert(tree.props["aria-label"].includes("预计已重置") && tree.props["aria-label"].includes("待确认") && tree.props.title.includes("待确认"), "accessible summary hid elapsed-reset uncertainty");
  assert(find(tree,"progress").props.value === value && content(tree).includes(value + "%"), "elapsed reset zeroed an observed value");
  assert(!content(tree).includes("立即可用") && !content(tree).includes("已恢复"), "elapsed reset claimed current eligibility");
}
`)
}

func TestFrontendUsageColumnBPSPermissionDoesNotInventUsage(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const [allowed,label] of [[true,"BPS 权限可用"],[false,"BPS 权限受限"],[null,""]]) {
  const snapshot = {lastAttempt:{status:"partial"},bps:{observedAt:"2026-10-31T12:00:00Z",allowed},errors:[{component:"usage",code:"permission_denied"}]};
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  assert((label?content(tree).includes(label):!usageClass(tree,"usage-empty").length) && !content(tree).includes("未知") && !content(tree).includes("查询结果不完整") && tree.props["aria-label"].includes("查询结果不完整"), "known BPS permission or the current failed request disappeared, or unknown quota placeholders returned");
  assert(!find(tree,"progress") && !content(tree).includes("0%") && !content(tree).includes("100%") && !content(tree).includes("授权失效"), "BPS permission invented quota or login failure");
}
for (const [snapshot,label] of [[undefined,""],[{},""],[{usage:{limitReached:true,windows:[]}},"额度已用尽"]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  assertEmptyUsageTrack(tree);
  assert(label?content(tree).includes(label):usageClass(tree,"metric-state-label").length===1, "missing windows lost their state explanation or explicit limit evidence");
}
`)
}

func TestFrontendUsageColumnRetainedBPSPermissionIsExplicitlyHistorical(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const allowed of [true,false,null]) {
  for (const attempt of [
    {lastAttempt:{status:"failed"}},
    {stale:true,lastAttempt:{status:"success"}},
    {lastAttempt:{status:"partial",startedAt:"2026-10-31T12:00:00Z"},errors:[{component:"bps",code:"temporary_network_error"}]},
  ]) {
    const snapshot = {...attempt,bps:{observedAt:"2026-10-30T12:00:00Z",allowed}};
    const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
    if(allowed===null&&attempt.stale){assertEmptyUsageTrack(tree);continue;}
    assert(!content(tree).includes("历史权限") && tree.props.className.includes("has-history"), "retained BPS permission was presented with a redundant visible history label");
    const reset=usageClass(tree,"usage-next-reset")[0];
    assert(!reset&&!find(tree,"time")&&!usageClass(tree,"usage-meta").length&&!usageClass(tree,"usage-history-label").length, "permission-only data invented a quota reset or placeholder from its observation date");
    assert(!tree.props["aria-label"].includes("历史权限") && !tree.props.title.includes("历史权限") && !tree.props.title.includes("2026-10-30"), "BPS permission duplicated freshness text or exposed its observation time in the quota summary");
    assert(!content(tree).includes("历史额度") && !find(tree,"progress"), "historical permission was mislabeled as a numeric usage snapshot");
  }
}
const fresh = renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status:"partial",startedAt:"2026-10-31T12:00:00Z"},bps:{observedAt:"2026-10-31T12:00:01Z",allowed:true},errors:[{component:"usage",code:"permission_denied"}]},onOpen(){}});
assert(!content(fresh).includes("历史权限") && !fresh.props.className.includes("has-history"), "independent usage failure marked fresh BPS permission historical");
`)
}

func TestFrontendUsageColumnKeepsAttemptAndPersistenceOutcomesAccessible(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const [status,label] of [["queued","查询已排队"],["running","正在查询"],["failed","本次查询失败"],["partial","查询结果不完整"],["skipped","本次查询已跳过"],["canceled","查询已取消或中断"]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status}},onOpen(){}});
  if(["skipped","canceled"].includes(status))assertEmptyUsageTrack(tree);
  else assert(tree.props["aria-label"].includes(label), "active or failed request lost accessible details: " + status);
  // Empty observations share the same fixed footer as measured observations.
  // Terminal notices remain accessible without adding a process row.
  if (["queued","running"].includes(status)) {
    assert(usageClass(tree,"usage-query-slot").map(content).join()===(status==="queued"?"排队中":"查询中")&&tree.props["aria-busy"]===(status==="running"), "active account lost visible queue/execution feedback");
    assert(usageClass(usageClass(tree,"usage-footer-line")[0],"usage-query-slot").length===1&&!usageClass(tree,"usage-empty").length&&!content(tree).includes("未知"), "active query restored quota placeholders");
  } else assert(!content(tree).includes(label), "terminal process notice was duplicated in the compact row: " + status);
  assert(!tree?.props.title.includes(label), "hover repeated the visible process notice: " + status);
  assert(!find(tree,"progress") && !content(tree).includes("已获取") && !content(tree).includes("查询成功"), "attempt without data claimed acquired quota");
  const retained=renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:37}]}},onOpen(){}});
  assert(find(retained,"progress").props.value===37&&!usageClass(retained,"usage-next-reset").length&&!usageNodes(retained,"time").length, "process feedback changed an existing quota or invented a reset from its observation time");
}
for (const snapshot of [{persisted:false,lastAttempt:{status:"failed"}},{persisted:false,lastAttempt:{status:"partial"},usage:{observedAt:"2026-10-31T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:27}]}}]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  assert(!content(tree).includes("记录保存未确认") && tree.props["aria-label"].includes("保存未确认"), "unconfirmed durability disappeared from accessible details or remained a process row");
  if (snapshot.usage) assert(find(tree,"progress").props.value === 27, "persistence uncertainty discarded the observed result");
}
const never = renderComponent("AccountInsightSummary",{snapshot:{persisted:false},onOpen(){}});
assert(!content(never).includes("保存未确认"), "unqueried placeholder claimed a failed write");
`)
}

func TestFrontendUsageColumnMissingResetKeepsFooterWithoutObservationFallback(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for(const [observation,source,history] of [
  [{usage:{windows:[{windowSeconds:18000,usedPercent:38}]}},"额度观察","历史额度"],
  [{usage:{windows:[{windowSeconds:18000,usedPercent:38}]},bps:{allowed:false,observedAt:"2026-11-02T12:00:00Z"}},"额度观察","历史额度"],
  [{usage:{windows:[]}},"额度观察","历史额度"],
  [{bps:{allowed:false}},"权限观察","历史权限"],
  [{bps:{allowed:false},subscription:{observedAt:"2026-11-02T12:00:00Z"}},"权限观察","历史权限"],
  [{subscription:{}},"订阅观察","订阅观察"],
]) {
  for(const status of ["success","running","failed","cancelled"]) {
    const snapshot={...observation,lastAttempt:{status}},tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
    const footer=usageClass(tree,"usage-footer")[0],reset=usageClass(tree,"usage-next-reset")[0];
    const isHistorical=["failed","cancelled"].includes(status)&&(observation.usage||observation.bps);
    const hasMeaning=Boolean(observation.usage?.windows?.length||observation.bps||["running","failed"].includes(status));
    assert(!reset&&!find(tree,"time")&&footer,"missing reset rendered text or the fixed footer was lost: "+source+"/"+status);
    if(!hasMeaning)assertEmptyUsageTrack(tree);
    assert(!tree?.props.title.includes("重置时间未知")&&!tree?.props.title.includes("观察"),"unknown reset was mislabeled as an observation time");
    if(isHistorical&&tree){const attemptLabel=status==="failed"?"本次查询失败":"查询已取消或中断";assert(tree.props["aria-label"].includes(attemptLabel)&&!tree.props["aria-label"].includes(history),"retained observation lost current attempt context or repeated its historical label");}
    assert(!usageClass(tree,"usage-meta").length&&!usageClass(tree,"usage-history-label").length,"missing date created a state-dependent history line");
    if(observation.usage?.windows?.length)assert(find(tree,"progress").props.value===38,"unknown observation date altered reported usage");
    if(observation.bps&&!observation.usage)assert(content(tree).includes("BPS 权限受限")&&!find(tree,"progress"),"unknown permission date replaced access evidence with quota");
  }
}
assert(requests.length===0&&timers.size===0,"missing date presentation fetched replacement data");
`)
}

func TestFrontendUsageColumnOpensDetailsWithoutStartingRequests(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
let opens = 0;
const tree = renderComponent("AccountInsightSummary",{accountLabel:"long.synthetic@example.invalid",snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:10}]}},onOpen(){opens++;}});
assert(tree.type === "button" && tree.props.type === "button" && !tree.props.disabled, "details entry lost native keyboard-button semantics");
assert(tree.props["aria-label"].includes("long.synthetic@example.invalid") && tree.props["aria-label"].includes("查看用量与状态"), "details entry lacks account-specific accessible purpose");
tree.props.onClick();
assert(opens === 1 && requests.length === 0 && timers.size === 0, "opening summary bypassed the parent read-only details callback");
`)
}

func TestFrontendUsageColumnRowAndEmptyStateKeepEightColumns(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
let opens = 0, refreshes = 0;
const job = {id:"synthetic-usage",email:"synthetic@example.invalid",authorizationMethod:"bps",status:"completed",canDownload:true};
const snapshot = {id:job.id,authorizationMethod:job.authorizationMethod,usage:{windows:[{windowSeconds:18000,usedPercent:19}]}};
const props = {job,token:"synthetic",selectionSupported:true,insightSnapshot:snapshot,onViewInsights(){opens++;},onRefreshInsights(){refreshes++;}};
const tree = RepairUI.JobRow(props), accountRow=usageClass(tree,"job-row")[0],cells = usageNodes(accountRow,"td");
assert(cells.length === 8 && cells[2].props.className === "usage-cell" && cells[cells.length-4].props.className === "step-cell" && cells[cells.length-3].props.className === "activity-time-cell" && cells[cells.length-2].props.className === "operation-type-cell", "usage and final operation columns do not match the eight-column header");
assert(!cells.some(cell=>cell.props.className === "status-cell")&&usageClass(tree,"step-cell").length===1&&tree.type==="tr"&&!usageClass(tree,"job-execution-row").length, "current operation was hidden or moved outside the account row");
const summaries = collectUsageNodes(tree,node => typeof node.type === "function" && node.type.name === "AccountInsightSummary");
assert(summaries.length === 1 && findMatching(cells[2],node => node === summaries[0]), "usage summary was duplicated under the account name or left outside its dedicated cell");
assert(summaries[0].props.snapshot === snapshot && summaries[0].props.accountLabel === job.email, "row changed cached identity or lost its accessible account label");
summaries[0].props.onOpen();
assert(opens === 1 && refreshes === 0 && requests.length === 0, "read-only details entry started an upstream refresh");
for (const filtered of [false,true]) {
  const empty = RepairUI.EmptyState({filtered});
  assert(Number(find(empty,"td").props.colSpan) === 8, "empty or filtered state does not span the complete table");
}
hookCursor = 0;
assert(usageNodes(usageClass(RepairUI.JobRow({...props,onViewInsights:undefined}),"job-row")[0],"td").length === 8, "missing optional details callback changed table structure");
`)
}

func TestFrontendLongOperationNoticeKeepsFullAccessibleDisclosure(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const reason="合成网络诊断："+"连接已中断，请核对当前任务状态。".repeat(40);
const job={id:"synthetic-notice",email:"synthetic@example.invalid",status:"failed",operationError:reason,lastOperationType:"relogin",operationStatus:"failed"};
const row=RepairUI.JobRow({job,token:"synthetic",onError(){}}),cells=usageNodes(row,"td"),execution=cells[cells.length-4];
assert(row.type==="tr"&&execution.props.className==="step-cell"&&!usageClass(row,"job-execution-row").length,"current operation diagnostics moved out of their permanent column");
const element=collectUsageNodes(execution,node=>typeof node.type==="function"&&node.type.name==="JobNotice")[0];
assert(element,"long operation failure lost its row notice");
const notice=element.type(element.props), summary=find(notice,"summary");
assert(notice.type==="details"&&!notice.props.open&&summary,"long failure no longer starts as a native keyboard disclosure");
assert(content(summary).length<reason.length&&summary.props["aria-label"].includes("展开查看完整内容"),"compact notice omitted its accessible full-detail entry");
assert(usageClass(notice,"job-notice-detail-copy").map(content).join()===reason,"compact notice truncated the full diagnostic payload");
assert(requests.length===0,"rendering a long notice retried the account operation");
`)
}

func TestFrontendUsageColumnHeaderAndExpandedLogsStayAligned(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
location.hash = "#accounts";
let tree = render();
const headers = usageNodes(find(tree,"thead"),"th");
assert(headers.length === 8 && headers[2].props.className === "usage-heading" && content(headers[2]) === "已用比例" && headers[headers.length-4].props.className==="current-operation-heading" && headers[headers.length-3].props.className==="activity-time-heading" && headers[headers.length-2].props.className==="operation-type-heading", "usage header or final operation column order is incorrect");
assert(find(tree,"EmptyState"), "empty account page lost its existing placeholder");
const job = {id:"synthetic-usage",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",status:"completed",canDownload:true};
appState.setJobs([job]); appState.setJobSelectionIndex([job]);
tree = render();
const row = find(tree,"JobRow");
row.props.onToggleLogs();
tree = render();
const logRow = findMatching(tree,node => node.type === "tr" && node.props.className === "log-row");
assert(logRow && Number(find(logRow,"td").props.colSpan) === 8 && find(logRow,"JobLogs").props.jobId === job.id, "expanded logs no longer span all eight account columns");
find(tree,"JobRow").props.onViewInsights();
tree = render();
assert(find(tree,"AccountInsightsDialog").props.job.id === job.id && requests.length === 0, "row details callback did not preserve the selected identity or triggered a request before the dialog mounted");
`)
}

func TestFrontendUsageColumnIdentityFilterRejectsOtherAuthorizationSnapshots(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const job = {id:"synthetic-usage",authorizationMethod:"bps"};
const current = {id:job.id,authorizationMethod:"bps",usage:{windows:[{windowSeconds:18000,usedPercent:61}]}};
assert(Browser.insightSnapshotForJob(job,current) === current, "matching usage snapshot was copied or discarded");
for (const candidate of [undefined,null,{...current,id:"other-synthetic"},{...current,authorizationMethod:"chatgpt"},{...current,authorizationMethod:undefined}]) {
  assert(Browser.insightSnapshotForJob(job,candidate) === undefined, "another account or authorization method leaked into the usage column");
}
assert(Browser.insightSnapshotForJob({...job,authorizationMethod:"chatgpt"},current) === undefined, "authorization switch retained the previous identity's usage");
assert(current.usage.windows[0].usedPercent === 61, "filter mutated the cached observation");
`)
}

func TestFrontendUsageColumnMonitorDistinguishesServiceLifecycleFromQueries(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const [state,expected] of [
  [{running:true,enabled:false,automatic:true,active:0,queued:0},"账号查询已关闭"],
  [{running:true,enabled:true,automatic:true,active:0,queued:0},"自动查询已开启"],
  [{running:true,enabled:true,automatic:false,active:0,queued:0},"仅手动查询"],
  [{running:true,enabled:true,automatic:true,active:2,queued:0},"正在查询"],
  [{running:true,enabled:true,automatic:false,active:1,queued:3},"正在查询"],
  [{running:true,enabled:true,automatic:true,active:0,queued:2},"查询已排队"],
]) {
  const tree = renderComponent("AccountInsightsMonitor",{insights:{status:state,statusError:""}});
  const monitorState = usageClass(tree,"insight-monitor-state")[0];
  assert(content(find(monitorState,"strong")) === expected, "service lifecycle was confused with query execution: " + JSON.stringify(state));
  assert(content(monitorState).includes("排队 " + state.queued) && content(monitorState).includes("执行中 " + state.active), "monitor headline disagrees with its queue or active counts");
  if (!state.enabled || !state.automatic) assert(content(monitorState).includes("不安排自动查询"), "disabled or manual mode announced an automatic check");
}
const unavailable = renderComponent("AccountInsightsMonitor",{insights:{status:null,statusError:"合成状态请求失败"}});
const unavailableState = usageClass(unavailable,"insight-monitor-state")[0];
assert(content(find(unavailableState,"strong")) === "查询状态暂不可读" && content(unavailable).includes("合成状态请求失败"), "failed status read became a confirmed disabled or running state");
assert(content(unavailableState).includes("排队 —") && content(unavailableState).includes("执行中 —") && !content(unavailableState).includes("账号查询已关闭"), "missing status invented zero in-flight work or a disabled state");
assert(requests.length === 0 && timers.size === 0, "monitor presentation started its own query lifecycle");
`)
}

func TestFrontendUsageColumnEmptyAndUnknownObservationsUseStaticTracks(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for(const snapshot of [undefined,{}, {lastAttempt:{status:"never"}}, {lastAttempt:{status:"success"},persisted:true}, {usage:{windows:[]}}, {usage:{windows:[null,{}, {usedPercent:null,resetsAt:"2026-11-07T00:00:00Z"},{usedPercent:"0"},{usedPercent:NaN}]}}, {bps:{allowed:null}}, {bps:{allowed:"true"}}, {subscription:{planType:"plus"}}, {lastAttempt:{status:"canceled"}}, {lastAttempt:{status:"skipped"}}]) {
 const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){throw new Error("unexpected details call");}});
 assertEmptyUsageTrack(tree,snapshot?.lastAttempt?.status==="never"?"尚未查询":undefined);
 assert(JSON.stringify(snapshot)===before,"empty track presentation mutated the saved observation");
}
assert(requests.length===0&&timers.size===0,"empty track triggered collection or retry");
`)
}

func TestFrontendUsageColumnFiltersUnknownWindowsBeforeDisplayAndCounting(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-06T00:00:00Z"),next="2026-11-07T00:00:00Z";
const windows=[{name:"unmeasured-first",usedPercent:null,resetsAt:"2026-11-06T01:00:00Z"},{name:"invalid-second",usedPercent:"0"},{name:"measured-zero",usedPercent:0,resetsAt:next},{name:"measured-next",usedPercent:45},{name:"unmeasured-middle",usedPercent:Infinity},{name:"measured-extra",usedPercent:17}];
const snapshot={usage:{windows}},before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
assert(usageClass(tree,"usage-window-label").map(content).join()==="measured-zero,measured-next"&&usageNodes(tree,"progress").map(node=>node.props.value).join()==="0,45","unknown leading windows displaced measured data or zero percent");
assert(usageClass(tree,"usage-extra-count").map(content).join()==="+1"&&!tree.props.title.includes("unmeasured")&&!tree.props.title.includes("invalid-second"),"unknown windows inflated the count or leaked into the summary");
assert(usageClass(tree,"usage-reset-countdown").length===1&&find(usageClass(tree,"usage-next-reset")[0],"time").props.dateTime===next&&!content(tree).includes("未知"),"unmeasured reset took precedence or missing reset rendered a placeholder");
assert(JSON.stringify(snapshot)===before&&requests.length===0&&timers.size===0,"filtering windows changed data or requested a query");
`)
}

func TestFrontendUsageColumnEmptyTrackAndMenuOnlyOpenDetails(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
var opens=0,refreshes=0;const job={id:"blank-usage",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",status:"completed"};
const tree=RepairUI.JobRow({job,onViewInsights(){opens++;},onRefreshInsights(){refreshes++;}});
const summary=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="AccountInsightSummary");
assert(summary,"account row omitted its usage summary");
const placeholder=summary.type(summary.props);
const stateElement=findMatching(placeholder,node=>typeof node.type==="function"&&node.type.name==="AccountMetricState"),metricView=stateElement?.type(stateElement.props);
assert(metricView&&findMatching(metricView,node=>(node.props?.className||"").split(" ").includes("usage-track-empty"))&&!find(placeholder,"progress")&&!content(metricView).includes("0%")&&content(metricView).includes("等待数据"),"unloaded account lost its explained static empty track");
placeholder.props.onClick();
const menu=findMatching(tree,node=>node.type==="ActionMenu"&&node.props.ariaLabel==="账号操作 "+job.email),details=findButton(menu,"用量与状态");
assert(details&&!details.props.disabled,"empty-track presentation removed the existing details menu entry");details.props.onClick();
assert(opens===2&&refreshes===0&&requests.length===0,"empty track or menu opening initiated a refresh or account request");
`)
}

func TestFrontendUsageColumnErrorOnlyKeepsLatestStatusThenClearsOnSuccess(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const requestsBefore=requests.length,observations=[{source:"subscription",status:"failed",httpStatus:503,startedAt:"2026-11-06T01:00:00Z",finishedAt:"2026-11-06T01:00:01Z"},{source:"models",status:"failed",httpStatus:403,startedAt:"2026-11-06T01:01:00Z",finishedAt:"2026-11-06T01:01:01Z"}];
const snapshot={usage:{windows:[{usedPercent:null}]}};
var tree=renderComponent("AccountInsightSummary",{snapshot,requestObservations:observations,onOpen(){}});
assert(usageClass(tree,"usage-inline-status").map(content).join()==="HTTP 403"&&usageClass(tree,"usage-inline-status").length===1&&!usageClass(tree,"usage-window").length&&!find(tree,"progress"),"error-only account lost its latest status or gained multiple statuses/measured quota");
tree=renderComponent("AccountInsightSummary",{snapshot,requestObservations:[...observations,{source:"models",status:"success",httpStatus:200,startedAt:"2026-11-06T01:02:00Z",finishedAt:"2026-11-06T01:02:01Z"}],onOpen(){}});
assertEmptyUsageTrack(tree);
assert(!usageClass(tree,"usage-inline-status").length&&!content(tree).includes("HTTP")&&requests.length===requestsBefore&&timers.size===0,"newer confirmed success retained the previous failure or retried work");
`)
}

func TestFrontendUsageColumnExplainsEveryUnmeasuredStateWithoutZero(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
let opened=0;
const cases=[
 [undefined,"等待数据"],
 [{},"暂未提供数值"],
 [{lastAttempt:{status:"never"}},"尚未查询"],
 [{refreshState:"queued",lastAttempt:{status:"never"}},"等待用量"],
 [{refreshState:"active",lastAttempt:{status:"running"}},"等待用量"],
 [{lastAttempt:{status:"failed"}},"未取得用量"],
 [{lastAttempt:{status:"success"},usage:{windows:[]}},"暂未提供数值"],
 [{lastAttempt:{status:"success"},usage:{windows:[{usedPercent:null}]}},"暂未提供数值"],
];
for(const [snapshot,label] of cases){
 const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,accountLabel:"quota-state@example.invalid",onOpen(){opened++;}});
 assertEmptyUsageTrack(tree,label);
 assert(tree.type==="button"&&tree.props.type==="button"&&tree.props["aria-label"].includes(label)&&tree.props["aria-label"].includes("quota-state@example.invalid"),"state explanation lost its accessible details purpose");
 assert(!usageClass(tree,"usage-next-reset").length&&!find(tree,"time"),"unmeasured state fabricated reset evidence");
 tree.props.onClick();assert(JSON.stringify(snapshot)===before,"quota state mutated the cached snapshot");
}
assert(opened===cases.length&&requests.length===0&&timers.size===0,"explained quota state started a query or retry");
`)
}
