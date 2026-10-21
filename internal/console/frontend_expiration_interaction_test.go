package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// Execute the production component handlers with synthetic React hooks. This
// checks request ordering and state transitions, not browser layout or the DOM.
func frontendExpirationComponentVM(t *testing.T, component string) *sobek.Runtime {
	t.Helper()
	vm := frontendAppVM(t)
	entry, err := filepath.Abs("../../web/src/components/" + component + ".jsx")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{
		EntryPoints: []string{entry}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "ExpirationUI", Platform: api.PlatformNeutral,
		Plugins: []api.Plugin{{Name: "synthetic-expiration-boundaries", Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `(^react$|^lucide-react$|/ui.jsx$|/PlanBadge.jsx$|.css$)`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			})
		}}},
	})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("expiration component bundle: %v", built.Errors)
	}
	frontendPollingRun(t, vm, `
hookSlots = []; hookCursor = 0; var effectQueue = [], busyChanges = [], dirtyChanges = [], saved = [], closeCalls = 0;
Intl.DateTimeFormat = function() {return {resolvedOptions() {return {timeZone:"Asia/Shanghai"};}};};
var setTimeout = window.setTimeout, clearTimeout = window.clearTimeout, setInterval = window.setTimeout, clearInterval = window.clearTimeout;
React.useEffect = (run, deps) => {
  const id = hookCursor++, previous = hookSlots[id];
  if (!previous || deps.some((value, index) => !Object.is(value, previous.deps[index]))) {
    hookSlots[id] = {deps, cleanup: previous?.cleanup};
    effectQueue.push(() => {hookSlots[id].cleanup?.(); hookSlots[id].cleanup = run();});
  }
};
var editorJob = {id:"synthetic/expiration",email:"long-synthetic-account@example.invalid",manualExpiresAt:"2026-10-18T08:30:00Z",subscriptionExpiresAt:"2026-12-01T00:00:00Z",expiresAt:"2026-10-18T08:30:00Z",expiresAtSource:"manual"};
var refreshCalls = 0;
const editorCallbacks = {token:"synthetic-token",onSaved(job) {saved.push(job); editorJob = job;},onBusyChange(value) {busyChanges.push(value);},onDirtyChange(value) {dirtyChanges.push(value);},onRefresh() {refreshCalls++;}};
function renderComponent(name, props) {
  hookCursor = 0; const tree = ExpirationUI[name](props);
  for (const run of effectQueue.splice(0)) run();
  return tree;
}
function renderEditor() {return renderComponent("AccountExpirationEditor", {...editorCallbacks, job:editorJob});}
function renderInfo() {return renderComponent("AccountInfoDialog", {job:editorJob,token:"synthetic-token",onClose() {closeCalls++;},onExpirationSaved(job) {saved.push(job);}});}
function changeExpiry(value) {find(renderEditor(), "input").props.onChange({target:{value}}); return renderEditor();}
function submitExpiry(tree = renderEditor()) {tree.props.onSubmit({preventDefault() {}});}
function unmount() {for (const slot of hookSlots) slot?.cleanup?.();}
`)
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	return vm
}

func TestFrontendExpirationEditorSavesOnceAndClearsToSubscription(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-expiration")
	frontendPollingRun(t, vm, `
var tree = changeExpiry("2026-10-18T16:30");
// Select a different value even when the test process happens to use UTC+8.
tree = changeExpiry("2026-10-19T16:30");
submitExpiry(tree); submitExpiry(tree);
assert(requests.length === 1 && saved.length === 0, "duplicate submission or optimistic mutation");
assert(requests[0].url === "/api/jobs/synthetic%2Fexpiration/expiration", "job id was not safely encoded");
const body = JSON.parse(requests[0].options.body);
assert(body.expiresAt === "2026-10-19T16:30" && body.timeZone === "Asia/Shanghai", "local datetime or timezone changed in transit");
assert(busyChanges.at(-1) === true && findButton(renderEditor(), "保存到期时间").props.disabled, "pending write stayed enabled");
requests[0].resolve({ok:true,status:200,json:async () => ({job:{...editorJob,manualExpiresAt:"2026-10-19T08:30:00Z",expiresAt:"2026-10-19T08:30:00Z",expiresAtSource:"manual"}})});
`)
	frontendPollingRun(t, vm, `
tree = renderEditor();
assert(saved.length === 1 && content(tree).includes("到期时间已保存"), "confirmed save missing");
assert(findButton(tree, "保存到期时间").props.disabled && busyChanges.at(-1) === false, "saved editor remained dirty or busy");
findButton(tree, "清除手动时间").props.onClick();
assert(requests.length === 2 && JSON.parse(requests[1].options.body).expiresAt === "", "clear did not send an explicit empty override");
requests[1].resolve({ok:true,status:200,json:async () => ({job:{...editorJob,manualExpiresAt:"",expiresAt:editorJob.subscriptionExpiresAt,expiresAtSource:"subscription"}})});
`)
	frontendPollingRun(t, vm, `
tree = renderEditor();
assert(saved.length === 2 && editorJob.expiresAtSource === "subscription" && find(tree, "input").props.value === "", "clear did not adopt server subscription fallback");
assert(content(tree).includes("已清除手动时间") && findButton(tree, "清除手动时间").props.disabled, "clear result ambiguous");
unmount(); assert(timers.size === 0, "editor leaked interval or request timeout");
`)
}

func TestFrontendExpirationEditorUnknownDoesNotApplyOrReplay(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-expiration")
	frontendPollingRun(t, vm, `
submitExpiry(changeExpiry("2026-10-19T16:30"));
requests[0].reject(new Error("synthetic connection loss"));
`)
	frontendPollingRun(t, vm, `
const tree = renderEditor();
assert(saved.length === 0 && editorJob.manualExpiresAt === "2026-10-18T08:30:00Z", "unknown mutation replaced confirmed data");
assert(content(tree).includes("请先刷新账号信息核实"), "unknown result invited immediate retry");
assert(findButton(tree, "保存到期时间").props.disabled && find(tree, "input").props.disabled, "unverified mutation was replayable");
submitExpiry(tree); assert(requests.length === 1, "unknown mutation was automatically or manually replayed");
findButton(tree, "重新读取到期时间").props.onClick();
assert(refreshCalls === 1, "unknown result had no in-place verification action");
unmount(); assert(timers.size === 0, "failed editor leaked timer");
`)
}

func TestFrontendExpirationEditorValidationFailurePreservesEditableDraft(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-expiration")
	frontendPollingRun(t, vm, `
submitExpiry(changeExpiry("2026-10-19T16:30"));
requests[0].resolve({ok:false,status:400,json:async () => ({error:"合成日期校验失败"})});
`)
	frontendPollingRun(t, vm, `
const tree = renderEditor();
assert(saved.length === 0 && content(tree).includes("合成日期校验失败"), "validation refusal was swallowed or applied");
assert(!find(tree, "input").props.disabled && find(tree, "input").props.value === "2026-10-19T16:30", "validation refusal lost editable draft");
unmount();
`)
}

func TestFrontendExpirationInfoProtectsDirtyDraftAndPendingWrite(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-info-dialog")
	frontendPollingRun(t, vm, `
renderInfo(); assert(requests.length === 1 && requests[0].url === "/api/jobs/synthetic%2Fexpiration", "metadata did not load before private account info");
requests[0].resolve({ok:true,status:200,json:async () => editorJob});
`)
	frontendPollingRun(t, vm, `
assert(requests.length === 2 && requests[1].url.endsWith("/account-info"), "private info did not retain its separate endpoint");
requests[1].resolve({ok:true,status:200,json:async () => ({...editorJob,password:"synthetic-only",totpSecret:"synthetic-only"})});
`)
	frontendPollingRun(t, vm, `
var tree = renderInfo();
var editor = findMatching(tree, node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
assert(editor, "expiration editor missing from account info");
editor.props.onDirtyChange(true);
findButton(renderInfo(), "关闭").props.onClick();
`)
	frontendPollingRun(t, vm, `
assert(closeCalls === 0, "unsaved expiry closed without accepted discard");
editor.props.onBusyChange(true);
tree = renderInfo();
assert(find(tree, "DialogFrame").props.busy && findButton(tree, "关闭").props.disabled, "pending mutation did not block keyboard/backdrop close");
findButton(tree, "关闭").props.onClick(); findButton(tree, "刷新").props.onClick();
assert(requests.length === 2 && closeCalls === 0, "pending save allowed close or competing refresh");
editor.props.onBusyChange(false); confirmAccepted = true; findButton(renderInfo(), "关闭").props.onClick();
`)
	frontendPollingRun(t, vm, `assert(closeCalls === 1, "accepted discard could not close dialog"); unmount(); assert(timers.size === 0, "info dialog leaked read deadline");`)
}

func TestFrontendExpirationInfoRemainsEditableWhenCredentialsCannotLoad(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-info-dialog")
	frontendPollingRun(t, vm, `
renderInfo();
requests[0].resolve({ok:true,status:200,json:async () => editorJob});
`)
	frontendPollingRun(t, vm, `requests[1].resolve({ok:false,status:503,json:async () => ({error:"合成凭据存储不可用"})});`)
	frontendPollingRun(t, vm, `
var tree = renderInfo();
var editor = findMatching(tree, node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
assert(editor && !editor.props.refreshing, "private credential failure hid or disabled independent metadata editor");
assert(content(tree).includes("合成凭据存储不可用"), "private information read failure was hidden");
assert(!findMatching(tree, node => typeof node.type === "function" && node.type.name === "AccountInfoField"), "failed private information was displayed");
editor.props.onSaved({...editorJob,manualExpiresAt:"",expiresAt:editorJob.subscriptionExpiresAt,expiresAtSource:"subscription"});
editor = findMatching(renderInfo(), node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
assert(editor.props.job.manualExpiresAt === "" && editor.props.job.expiresAtSource === "subscription", "metadata save depended on readable credentials");
assert(saved.at(-1).manualExpiresAt === "", "metadata change did not update account list");
unmount();
`)
}

func TestFrontendExpirationUnknownRefreshReadsMetadataWithoutDiscardGate(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-info-dialog")
	frontendPollingRun(t, vm, `
renderInfo(); requests[0].resolve({ok:true,status:200,json:async () => editorJob});
`)
	frontendPollingRun(t, vm, `requests[1].resolve({ok:false,status:409,json:async () => ({error:"合成2FA恢复期间暂不可读取凭据"})});`)
	frontendPollingRun(t, vm, `
var editor = findMatching(renderInfo(), node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
editor.props.onDirtyChange(true); editor.props.onUncertainChange(true);
var previousEditorKey = editor.props.key;
editor = findMatching(renderInfo(), node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
confirmAccepted = false; editor.props.onRefresh();
`)
	frontendPollingRun(t, vm, `
var tree = renderInfo();
assert(requests.length === 3 && requests[2].url === "/api/jobs/synthetic%2Fexpiration", "unknown write verification was blocked by unsaved draft or private-info gate");
assert(content(tree).includes("不代表已确认磁盘保存成功"), "verification promised unproven durability");
requests[2].resolve({ok:true,status:200,json:async () => ({...editorJob,manualExpiresAt:"2026-10-19T08:30:00Z",expiresAt:"2026-10-19T08:30:00Z"})});
`)
	frontendPollingRun(t, vm, `
editor = findMatching(renderInfo(), node => typeof node.type === "function" && node.type.name === "AccountExpirationEditor");
assert(editor.props.key !== previousEditorKey && editor.props.job.manualExpiresAt === "2026-10-19T08:30:00Z", "successful public reread did not reset unknown editor using current metadata");
assert(editor.props.allowUnchangedSave && !editor.props.refreshing, "verification prevented explicit same-value save");
assert(content(renderInfo()).includes("持久化结果尚未确认"), "metadata reread erased durability warning");
requests[3].resolve({ok:false,status:409,json:async () => ({error:"合成恢复仍需处理"})});
`)
	frontendPollingRun(t, vm, `
assert(content(renderInfo()).includes("合成恢复仍需处理"), "public reread bypassed private information safety gate");
assert(requests.every(request => !request.options.method || request.options.method === "GET"), "verification replayed a mutation");
unmount(); assert(timers.size === 0, "verification left a deadline timer");
`)
}

func TestFrontendExpirationExplicitUnchangedSaveAfterVerification(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-expiration")
	frontendPollingRun(t, vm, `
editorCallbacks.allowUnchangedSave = true;
const tree = renderEditor();
assert(!findButton(tree, "保存到期时间").props.disabled, "explicit retry could not confirm unchanged metadata");
submitExpiry(tree);
assert(requests.length === 1, "explicit verified retry was not submitted");
const body = JSON.parse(requests[0].options.body);
assert(body.expiresAt === find(tree, "input").props.value && body.timeZone === "Asia/Shanghai", "same-value retry changed the visible metadata");
unmount();
requests[0].reject(new Error("synthetic abort"));
`)
	frontendPollingRun(t, vm, `assert(timers.size === 0 && saved.length === 0, "unmounted editor applied late response or leaked deadline");`)
}

func TestFrontendExpirationBatchImportSendsTimezoneWithoutStartingLogin(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
Intl.DateTimeFormat = function() {return {resolvedOptions() {return {timeZone:"Asia/Shanghai"};}};};
location.hash = "#accounts";
findButton(render(), "批量导入").props.onClick();
var tree = render();
const help = content(tree);
for (const text of ["16.30", "7-18 08:00", "7-18 8:00", "导入当年", "不顺延到明天或明年", "完整日期时间", "真实套餐到期时间", "邮箱收码 API", "保留原手动值"]) assert(help.includes(text), "import help lost contract: " + text);
const raw = "synthetic@example.invalid----synthetic-password----JBSWY3DPEHPK3PXP----16.30";
find(tree, "textarea").props.onChange({target:{value:raw}});
tree = render();
findMatching(tree, node => node.type === "form" && node.props["aria-labelledby"] === "batch-title").props.onSubmit({preventDefault() {}});
assert(requests.length === 1 && requests[0].url === "/api/jobs/batch", "import triggered an authorization side effect");
const body = JSON.parse(requests[0].options.body);
assert(body.timeZone === "Asia/Shanghai" && body.text === raw, "import guessed expiry or changed raw source before backend parsing");
`)
}
