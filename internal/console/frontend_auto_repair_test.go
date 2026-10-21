package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func TestFrontendAutomaticReloginPlanSkipIsTerminalWithoutSuccess(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const job = {status:"completed",lastOperationType:"automatic_relogin",operationStatus:"skipped",planType:"free",canDownload:true,autoRepairSkippedReason:"本地套餐 Free 已被排除，自动重登未执行"};
const result = Browser.describeJob(job);
assert(result.terminal && result.status.code === "automatic_relogin_skipped", "policy-skipped queue was still active or shown as completed");
assert(result.status.label === "自动重登已跳过" && result.status.tone !== "success" && result.status.tone !== "danger", "plan exclusion became an operation success or failure");
assert(result.summary.includes("未执行") && result.summary.includes("已有授权"), "skip lost unexecuted semantics or preserved authorization");
assert(result.summary.includes("本地套餐 Free"), "readable exclusion reason missing");
assert(!Browser.uploadBlockedJob(job), "plan exclusion incorrectly blocked manual synchronization");
assert(Browser.uploadBlockedJob({...job,totpRecoveryPending:true}), "plan skip bypassed credential recovery safety gate");
const canceled = Browser.describeJob({...job,status:"canceled",operationError:"synthetic skip persistence error",autoRepairLastError:"synthetic older monitor failure"});
assert(canceled.status.code === "automatic_relogin_skipped" && !canceled.summary.includes("取消"), "queue cleanup status hid plan exclusion");
assert(canceled.notices.some(notice => notice.text.includes("skip persistence error") && notice.tone === "warning"), "skip persistence failure warning was hidden");
assert(!canceled.notices.some(notice => notice.text.includes("older monitor failure")), "old failure was presented as a current exclusion error");
`)
}

func TestFrontendAutomaticReloginGenericSkipPreservesReasonAndStorageWarnings(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
for (const reason of ["监控已关闭", "监控后端已切换", "服务重启，旧自动队列不再执行", ""]) {
  const result = Browser.describeJob({status:"canceled",lastOperationType:"automatic_relogin",operationStatus:"skipped",canDownload:true,autoRepairSkippedReason:reason,operationError:"合成跳过状态落盘失败",credentialWarning:"合成安全存储不可用"});
  assert(result.status.label === "自动重登已跳过" && result.status.tone === "neutral" && result.terminal, "generic skip was described as execution failure");
  assert(result.summary.includes("未执行") && result.summary.includes("已有授权") && (!reason || result.summary.includes(reason)), "generic skip lost its real reason or preserved grant");
  assert(!result.summary.includes("命中排除规则") && !result.status.label.includes("套餐"), "non-plan skip was mislabeled as a plan exclusion");
  for (const warning of ["合成跳过状态落盘失败", "合成安全存储不可用"]) assert(result.notices.some(notice => notice.text === warning && notice.tone === "warning"), "skip hid safety warning: " + warning);
}
`)
}

func TestFrontendMonitorAutoUploadPauseKeepsProgressAndRecoveryInstructions(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
for (const data of [{checked:0,autoUploadPaused:1}, {checked:3,autoUploadPaused:1,excludedPlan:2}, {checked:4,started:1,updated:2,autoUploadPaused:1}]) {
  const result = Browser.describeMonitorResult(data);
  assert(result.code === "upload_paused" && result.tone === "warning" && !result.spinning, "pending saved authorization was presented as success or running");
  for (const text of ["自动回传暂停 1 条", "新授权与待处理记录已保留", "手动同步", "取消排除", "下次巡检"]) assert(result.text.includes(text), "pause lost recovery instruction: " + text);
  assert(!result.text.includes("未启动自动重登"), "saved authorization was described as never started");
  if (data.updated) assert(result.text.includes("已更新账号 2 条"), "pause hid completed update count");
  if (data.started) assert(result.text.includes("已排队自动重登 1 条"), "pause hid queued count");
  const monitor = Browser.describeMonitor({configured:true,enabled:true,lastResult:data});
  assert(JSON.stringify(monitor.resultPresentation) === JSON.stringify(result), "refresh changed persisted result presentation");
}
const failure = Browser.describeMonitorResult({autoUploadPaused:2,error:"合成后续巡检失败"});
assert(failure.code === "error" && failure.text.includes("自动回传暂停 2 条") && failure.text.includes("合成后续巡检失败"), "pause hid a separate inspection error");
const free = Browser.describeJob({status:"completed",lastOperationType:"automatic_relogin",operationStatus:"succeeded",planType:"free",canDownload:true});
assert(free.status.code === "operation_succeeded", "display inferred an exclusion without server evidence");
`)
}

// Bundle the production components with the same synthetic React boundaries as
// the App tests. Request callbacks are exercised; this does not emulate a DOM.
func frontendAutoRepairComponentVM(t *testing.T, component string) *sobek.Runtime {
	t.Helper()
	vm := frontendAppVM(t)
	entry, err := filepath.Abs("../../web/src/components/" + component + ".jsx")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{
		EntryPoints: []string{entry}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "RepairUI", Platform: api.PlatformNeutral,
		Plugins: []api.Plugin{{Name: "synthetic-auto-repair-boundaries", Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `(^react$|^lucide-react$|/ui.jsx$|/PlanBadge.jsx$|\.css$)`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			})
		}}},
	})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("auto repair component bundle: %v", built.Errors)
	}
	frontendPollingRun(t, vm, `Intl.DateTimeFormat = function() {return {format() {return "合成时间";},resolvedOptions() {return {timeZone:"Asia/Shanghai"};}};};`)
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	return vm
}

func TestFrontendPausedAutoRepairRowKeepsManualUploadAndAuthorizationActions(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
var uploads = 0, relogins = 0;
const job = {id:"synthetic-paused",email:"synthetic@example.invalid",status:"completed",lastOperationType:"automatic_relogin",operationStatus:"succeeded",canDownload:true,canForceRelogin:true,autoRepairPendingAccountIds:[7],autoRepairSkippedReason:"合成套餐排除，自动回传暂停"};
const props = {job,token:"synthetic",selectionSupported:true,sub2apiUploadAvailable:true,forceReloginAvailable:true,onUpload() {uploads++;},onRelogin() {relogins++;}};
var tree = RepairUI.JobRow(props);
const upload = findButton(tree, "上传"), relogin = findButton(tree, "重登授权");
assert(upload && !upload.props.disabled && relogin && !relogin.props.disabled, "automatic policy pause blocked manual recovery actions");
upload.props.onClick(); relogin.props.onClick();
assert(uploads === 1 && relogins === 1 && requests.length === 0, "manual recovery did not use existing action entry points");
const badge = findMatching(tree, node => typeof node.type === "function" && node.type.name === "StatusBadge");
assert(badge.props.presentation.code === "automatic_upload_paused" && content(tree).includes("新授权已保存"), "row summary and status disagree");
hookCursor = 0; tree = RepairUI.JobRow({...props,job:{...job,status:"canceled",operationStatus:"skipped"}});
assert(!findButton(tree, "上传").props.disabled && !findButton(tree, "取消任务"), "skipped queue still required cancellation before manual sync");
hookCursor = 0; tree = RepairUI.JobRow({...props,job:{...job,totpRecoveryPending:true}});
assert(findButton(tree, "上传").props.disabled, "manual action bypassed real credential recovery protection");
`)
}

func TestFrontendMonitorPageDisplaysPausedCountAndConfigurationEntry(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
var configureCalls = 0;
const tree = RepairUI.MonitorPage({status:{configured:true,enabled:true,excludedPlanTypes:["free"],lastResult:{checked:3,excludedPlan:1,autoUploadPaused:2}},onConfigure() {configureCalls++;},onCheck() {},checking:false,available:true});
const metric = findMatching(tree, node => node.type === "div" && content(node) === "2自动回传暂停");
assert(metric, "paused pending count was absent from monitor metrics");
const notice = findMatching(tree, node => typeof node.type === "function" && node.type.name === "MonitorResultNotice");
assert(notice.props.presentation.code === "upload_paused", "monitor notice and pending metric disagree");
for (const text of ["任一本地或远端已知套餐", "执行前再次检查", "未知不等于 Free", "手动同步"]) assert(content(tree).includes(text), "monitor lost policy explanation: " + text);
findButton(tree, "监控配置").props.onClick();
assert(configureCalls === 1, "pause lost existing configuration recovery entry");
`)
}

func TestFrontendPlanExclusionSettingsDistinguishUnknownFromEmptySelection(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "SettingsDialogs")
	frontendPollingRun(t, vm, `
var draft = {baseUrl:"https://synthetic.invalid",adminApiKey:"synthetic-only",excludedPlanTypes:["free"],groupIds:[]};
const props = {inline:true,features:{sub2apiMonitor:true},sub2apiGroups:[],sub2apiProxies:[],sub2apiMonitorStatus:{},setSub2apiSettingsDraft(update) {draft = typeof update === "function" ? update(draft) : update;}};
function renderSettings() {return RepairUI.Sub2ApiSettingsDialog({...props,sub2apiSettingsDraft:draft});}
var tree = renderSettings();
const help = findMatching(tree, node => node.props?.id === "sub2api-plan-help");
for (const detail of ["任一本地或远端已知套餐", "执行前再次检查", "暂停自动回传", "未选择任何套餐表示不排除", "未知不等于 Free"]) assert(content(help).includes(detail), "settings lost policy explanation: " + detail);
const free = findMatching(tree, node => node.type === "input" && node.props["aria-label"] === "Free");
const unknown = findMatching(tree, node => node.type === "input" && node.props["aria-label"] === "未知套餐");
assert(free && unknown && free.props.checked && !unknown.props.checked, "Free exclusion implicitly selected unknown or selection controls are missing");
unknown.props.onChange({target:{checked:true}});
assert(draft.excludedPlanTypes.join() === "free,unknown", "unknown selection overwrote the Free rule");
tree = renderSettings();
const fieldset = findMatching(tree, node => node.type === "fieldset" && node.props["aria-describedby"] === "sub2api-plan-help");
findButton(fieldset, "清空").props.onClick();
assert(draft.excludedPlanTypes.length === 0 && requests.length === 0, "empty draft became unknown or immediately saved a side effect");
`)
}

func TestFrontendAutomaticReloginSavedGrantPauseIsPartialAndActionable(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const job = {status:"completed",lastOperationType:"automatic_relogin",operationStatus:"succeeded",planType:"free",canDownload:true,resultSaved:true,autoRepairPendingAccountIds:[7,8],autoRepairSkippedReason:"新授权套餐 Free 已被排除，自动回传暂停"};
const result = Browser.describeJob(job);
assert(result.terminal && result.status.code === "automatic_upload_paused", "saved authorization pause lost its distinct outcome");
assert(result.status.label === "授权已保存 · 自动回传暂停" && result.status.tone === "warning", "paused automatic upload became full success");
for (const part of ["新授权已保存", "手动同步", "取消排除", "下次巡检"]) assert(result.summary.includes(part), "pause lost recovery instruction: " + part);
assert(result.notices.some(notice => notice.text.includes("新授权套餐 Free")), "pause lost its reason");
assert(result.authorization.available && !Browser.uploadBlockedJob(job), "pause disabled safely saved authorization or manual upload");
const manual = Browser.describeJob({...job,lastOperationType:"relogin"});
assert(manual.status.code === "operation_succeeded", "stale monitor reason overwrote a later manual operation");
const clean = Browser.describeJob({...job,autoRepairSkippedReason:""});
assert(clean.status.code === "operation_succeeded", "ordinary automatic authorization changed without a policy pause");
const synchronized = Browser.describeJob({...job,autoRepairPendingAccountIds:[]});
assert(synchronized.status.code === "operation_succeeded" && !synchronized.notices.some(notice => notice.key === "monitor-plan-skip"), "old reason kept a confirmed manual synchronization paused");
const recovering = Browser.describeJob({...job,totpRecoveryPending:true});
assert(recovering.status.code === "recovery_required", "plan pause hid credential recovery requirements");
`)
}
