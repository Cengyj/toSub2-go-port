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
assert(usageClass(tree,"usage-window-label").map(content).join() === "7d,5h", "summary reordered reported windows or guessed 5h/7d from position");
assert(usageNodes(tree,"progress").map(node => node.props.value).join() === "82,0", "summary changed reported window usage");
assert(usageClass(tree,"usage-window").length === 2 && content(tree).includes("另有 2 个窗口"), "summary omitted overflow or exceeded its compact two-window limit");
assert(tree.props["aria-label"].includes("7 天窗口已用 82%") && tree.props["aria-label"].includes("5 小时窗口已用 0%"), "screen-reader summary lost exact window identities");
assert(JSON.stringify(snapshot) === before && requests.length === 0 && timers.size === 0, "render modified or independently refreshed cached usage");
`)
}

func TestFrontendUsageColumnDistinguishesZeroUnknownAndClampedBars(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const value of [null,undefined,"0",false,NaN,Infinity,-Infinity]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:value}]}},onOpen(){}});
  assert(!find(tree,"progress") && usageClass(tree,"usage-percent").map(content).join() === "未知", "unknown percentage became a numeric progress value: " + String(value));
  assert(usageClass(tree,"usage-track-unknown").length === 1 && !content(tree).includes("0%"), "unknown percentage was displayed as empty capacity");
}
for (const [value,barValue] of [[0,0],[12.5,12.5],[125,100],[-8,0]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:value}]}},onOpen(){}});
  const bar = find(tree,"progress");
  assert(bar && Number(bar.props.max) === 100 && bar.props.value === barValue, "valid progress value was absent or escaped visual bounds");
  assert(usageClass(tree,"usage-percent").map(content).join() === value + "%" && bar.props["aria-label"].includes(value + "%"), "visual clamping changed the reported numeric value");
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
assert(!content(tree).includes("部分更新") && tree.props["aria-label"].includes("查询部分更新") && content(tree).includes("历史额度") && tree.props.className.includes("has-history"), "compact partial outcome was repeated visibly or fresh BPS permission made retained usage current");
assert(find(tree,"progress").props.value === 92 && content(tree).includes("92%"), "partial permission failure erased or reset historical usage");
assert(content(tree).includes("待确认") && tree.props.title.includes("历史额度") && tree.props.title.includes("2026-10-30"), "partial result hid expired reset uncertainty or observation age");
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
  assert(content(tree).includes("预计已重置") && content(tree).includes("待确认"), "elapsed reset was presented as confirmed even for zero or unknown usage");
  assert(tree.props["aria-label"].includes("预计已重置") && tree.props["aria-label"].includes("待确认") && tree.props.title.includes("待确认"), "accessible summary hid elapsed-reset uncertainty");
  if (value === null) assert(!find(tree,"progress") && content(tree).includes("未知"), "elapsed unknown reset invented capacity");
  else assert(find(tree,"progress").props.value === value && content(tree).includes(value + "%"), "elapsed reset zeroed an observed value");
  assert(!content(tree).includes("立即可用") && !content(tree).includes("已恢复"), "elapsed reset claimed current eligibility");
}
`)
}

func TestFrontendUsageColumnBPSPermissionDoesNotInventUsage(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for (const [allowed,label] of [[true,"BPS 权限可用"],[false,"BPS 权限受限"],[null,"BPS 权限未知"]]) {
  const snapshot = {lastAttempt:{status:"partial"},bps:{observedAt:"2026-10-31T12:00:00Z",allowed},errors:[{component:"usage",code:"permission_denied"}]};
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  assert(content(tree).includes(label) && content(tree).includes("额度数值未知") && !content(tree).includes("部分更新") && tree.props["aria-label"].includes("查询部分更新"), "BPS permission state lost its independent meaning or compact view repeated the partial process notice");
  assert(!find(tree,"progress") && !content(tree).includes("0%") && !content(tree).includes("100%") && !content(tree).includes("授权失效"), "BPS permission invented quota or login failure");
}
for (const [snapshot,label] of [[undefined,"额度未知"],[{},"额度未知"],[{usage:{limitReached:true,windows:[]}},"额度已用尽"]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  assert(content(tree).includes(label) && !find(tree,"progress"), "missing windows invented a numeric quota or lost explicit limit evidence");
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
    assert(content(tree).includes("历史权限") && tree.props.className.includes("has-history"), "retained BPS permission was presented as a current result");
    assert(tree.props["aria-label"].includes("历史权限") && tree.props.title.includes("历史权限"), "historical BPS qualifier was missing from accessible details");
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
for (const [status,label] of [["queued","查询已排队"],["running","正在查询"],["failed","本次查询失败"],["partial","查询部分更新"],["skipped","本次查询已跳过"],["canceled","查询已取消或中断"]]) {
  const tree = renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status}},onOpen(){}});
  assert(tree.props["aria-label"].includes(label), "attempt state was collapsed into completion or removed from accessible details: " + status);
  assert(!content(tree).includes(label) && !tree.props.title.includes(label), "compact row or hover repeated a process notice: " + status);
  assert(!find(tree,"progress") && !content(tree).includes("已获取") && !content(tree).includes("查询成功"), "attempt without data claimed acquired quota");
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

func TestFrontendUsageColumnRowAndEmptyStateKeepNineColumns(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
let opens = 0, refreshes = 0;
const job = {id:"synthetic-usage",email:"synthetic@example.invalid",authorizationMethod:"bps",status:"completed",canDownload:true};
const snapshot = {id:job.id,authorizationMethod:job.authorizationMethod,usage:{windows:[{windowSeconds:18000,usedPercent:19}]}};
const props = {job,token:"synthetic",selectionSupported:true,insightSnapshot:snapshot,onViewInsights(){opens++;},onRefreshInsights(){refreshes++;}};
const tree = RepairUI.JobRow(props), cells = usageNodes(tree,"td");
assert(cells.length === 9 && cells[2].props.className === "usage-cell", "dedicated usage column is missing or row columns do not match the header");
const summaries = collectUsageNodes(tree,node => typeof node.type === "function" && node.type.name === "AccountInsightSummary");
assert(summaries.length === 1 && findMatching(cells[2],node => node === summaries[0]), "usage summary was duplicated under the account name or left outside its dedicated cell");
assert(summaries[0].props.snapshot === snapshot && summaries[0].props.accountLabel === job.email, "row changed cached identity or lost its accessible account label");
summaries[0].props.onOpen();
assert(opens === 1 && refreshes === 0 && requests.length === 0, "read-only details entry started an upstream refresh");
for (const filtered of [false,true]) {
  const empty = RepairUI.EmptyState({filtered});
  assert(Number(find(empty,"td").props.colSpan) === 9, "empty or filtered state does not span the complete table");
}
hookCursor = 0;
assert(usageNodes(RepairUI.JobRow({...props,onViewInsights:undefined}),"td").length === 9, "missing optional details callback changed table structure");
`)
}

func TestFrontendUsageColumnHeaderAndExpandedLogsStayAligned(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
location.hash = "#accounts";
let tree = render();
const headers = usageNodes(find(tree,"thead"),"th");
assert(headers.length === 9 && headers[2].props.className === "usage-heading" && content(headers[2]).includes("已用比例"), "usage header is missing, misaligned or ambiguous about used versus remaining");
assert(find(tree,"EmptyState"), "empty account page lost its existing placeholder");
const job = {id:"synthetic-usage",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",status:"completed",canDownload:true};
appState.setJobs([job]); appState.setJobSelectionIndex([job]);
tree = render();
const row = find(tree,"JobRow");
row.props.onToggleLogs();
tree = render();
const logRow = findMatching(tree,node => node.type === "tr" && node.props.className === "log-row");
assert(logRow && Number(find(logRow,"td").props.colSpan) === 9 && find(logRow,"JobLogs").props.jobId === job.id, "expanded logs no longer span all nine account columns");
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
