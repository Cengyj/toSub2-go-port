package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func frontendBrowserVM(t *testing.T, module string) *sobek.Runtime {
	t.Helper()
	entry, err := filepath.Abs("../../web/src/lib/" + module + ".js")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{EntryPoints: []string{entry}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "Browser", Platform: api.PlatformNeutral})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("browser module bundle: %v", built.Errors)
	}
	vm := sobek.New()
	frontendPollingRun(t, vm, `var Intl = {}; function assert(value, message) { if (!value) throw new Error(message); }`)
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	return vm
}

func TestFrontendStorageReportsFailuresAndIncludesEveryPreference(t *testing.T) {
	vm := frontendBrowserVM(t, "console")
	frontendPollingRun(t, vm, `
var stored = new Map(), fail = false;
var window = {localStorage: {
  setItem(key, value) { if (fail) throw new Error("synthetic quota failure"); stored.set(key, value); },
  getItem(key) { return stored.get(key) || null; }
}};
assert(Browser.writeLocalTextSetting(Browser.ACCOUNT_PROXY_STORAGE_KEY, "http://synthetic.example:8000"), "text save unconfirmed");
assert(Browser.readLocalTextSetting(Browser.ACCOUNT_PROXY_STORAGE_KEY) === "http://synthetic.example:8000", "text save did not persist");
assert(Browser.writeLocalJson(Browser.SMS_PROVIDER_SETTINGS_KEY, {selectedProviderId: "synthetic", configs: {}}), "JSON save unconfirmed");
assert(Browser.readSmsProviderSettings().selectedProviderId === "synthetic", "JSON preference did not reopen");
fail = true;
assert(!Browser.writeLocalTextSetting(Browser.ACCOUNT_PROXY_STORAGE_KEY, "replacement"), "failed text save claimed success");
assert(!Browser.writeLocalJson(Browser.SMS_PROVIDER_SETTINGS_KEY, {}), "failed JSON save claimed success");
assert(Browser.readLocalTextSetting(Browser.ACCOUNT_PROXY_STORAGE_KEY) === "http://synthetic.example:8000", "failed write destroyed old value");
for (const key of ["tosub2.console.theme", "tosub2.authorization-method-v1", "tosub2.relogin-workflow-preferences-v1", "tosub2.totp-workflow-preferences-v1"]) {
  assert(Browser.BROWSER_SETTINGS_KEYS.includes(key), "clear browser settings omits " + key);
}
assert(new Set(Browser.BROWSER_SETTINGS_KEYS).size === Browser.BROWSER_SETTINGS_KEYS.length, "duplicate preference registration");
`)
}

func TestFrontendHTTPResponseUsesSafeAndActionableErrors(t *testing.T) {
	vm := frontendBrowserVM(t, "console")
	frontendPollingRun(t, vm, `
var outcomes = [];
const response = (status, body, broken = false) => ({ status, ok: status >= 200 && status < 300, json: async () => { if (broken) throw new Error("synthetic-private-body"); return body; } });
for (const item of [response(204), response(502, null, true), response(200, null, true), response(403, {error: {message: "操作被拒绝"}}), response(200, null), response(200, {jobs: []})]) {
  Browser.readResponse(item).then(value => outcomes.push({value}), error => outcomes.push({error: error.message, status: error.status}));
}
`)
	frontendPollingRun(t, vm, `
assert(outcomes.length === 6, "response did not settle");
assert(outcomes.filter(item => item.value).length === 2, "successful response rejected");
assert(outcomes.some(item => item.error?.includes("HTTP 502")), "missing HTTP status for invalid JSON");
assert(outcomes.some(item => item.error === "操作被拒绝" && item.status === 403), "business refusal lost");
assert(!JSON.stringify(outcomes).includes("synthetic-private-body"), "raw response was exposed");
`)
}

func TestFrontendUploadUnknownPreservesProgressAndNeverClaimsCompletion(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const partial = Browser.describeUpload({ok:false, outcome:"partial", uncertain:true, error:"synthetic upstream timeout", result:{created:0,updated:1,unknown:1,not_attempted:2}});
assert(partial.tone === "warning" && !partial.spinning, "uncertain upload appears active or successful");
for (const text of ["更新 1 条", "结果未知 1 条", "未执行 2 条", "请先核实远端账号状态"]) assert(partial.text.includes(text), "upload lost progress: " + text);
assert(partial.label.includes("结果待核实"), "unknown result downgraded to ordinary failure");
const rejected = Browser.describeUpload({ok:false, outcome:"failed", result:{failed:1,not_attempted:2}}, {error:"synthetic refusal"});
assert(rejected.tone === "warning" && !rejected.label.includes("已完成") && rejected.text.includes("失败 1 条"), "explicit refusal became success");
const skipped = Browser.describeUpload({skipped:2,result:{created:0,updated:0}});
assert(skipped.tone === "neutral" && skipped.label.includes("未执行同步"), "skip-only upload became success");
const interrupted = Browser.describeUpload({uncertain:true}, {error:"synthetic transport failure"});
assert(interrupted.label === "同步结果待核实" && interrupted.text.includes("避免重复创建"), "lost HTTP response invited replay");
`)
}

func TestFrontendHTTPErrorRetainsStructuredUploadResult(t *testing.T) {
	vm := frontendBrowserVM(t, "console")
	frontendPollingRun(t, vm, `
var caught; const payload = {ok:false, outcome:"partial", uncertain:true, error:"synthetic timeout", result:{updated:1,unknown:1,not_attempted:1}};
Browser.readResponse({ok:false, status:502, json:async () => payload}).catch(error => caught = error);
`)
	frontendPollingRun(t, vm, `assert(caught.status === 502 && caught.data === payload && caught.data.result.unknown === 1, "HTTP failure discarded partial progress");`)
}

func TestFrontendHTTPNeverRetriesMutationsAndMarksLostResponsesUnknown(t *testing.T) {
	vm := frontendBrowserVM(t, "console")
	frontendPollingRun(t, vm, `
var requests = [], failures = [];
var fetch = async (url, options) => { requests.push({url,options}); throw new Error("synthetic private network diagnostic"); };
Browser.apiFetch("synthetic", "/api/jobs/fixture/setup-2fa", {method:"POST",body:"{}"}).catch(error => failures.push(error));
Browser.apiFetch("synthetic", "/api/jobs").catch(error => failures.push(error));
Browser.apiFetch("synthetic", "/api/jobs/query", {method:"POST",readOnly:true,body:"{}"}).catch(error => failures.push(error));
`)
	frontendPollingRun(t, vm, `
assert(requests.length === 3 && failures.length === 3, "network errors were retried or swallowed");
assert(failures[0].outcome === "unknown" && failures[0].message.includes("避免重复提交"), "mutation uncertainty was hidden");
assert(failures[1].outcome === "read_failed" && failures[2].outcome === "read_failed", "read-only POST was treated as a mutation");
assert(!Object.hasOwn(requests[2].options, "readOnly"), "internal request metadata leaked to fetch options");
assert(!failures.some(error => error.message.includes("private network diagnostic")), "transport diagnostic leaked");
`)
}

func TestFrontendDialogFocusKeepsTopModalAndRestoresBody(t *testing.T) {
	vm := frontendBrowserVM(t, "dialogFocus")
	frontendPollingRun(t, vm, `
const listeners = {keydown: new Set(), focusin: new Set()};
const owner = { body: {style: {overflow: "scroll"}}, activeElement: null,
  addEventListener(name, fn) { listeners[name].add(fn); }, removeEventListener(name, fn) { listeners[name].delete(fn); } };
function control(name) { return { name, isConnected: true, closest() {return null;}, getClientRects() {return [1];}, focus() {owner.activeElement = this; for (const fn of [...listeners.focusin]) fn();} }; }
function modal(name) { const value = control(name); value.items = [control(name + "first"), control(name + "last")]; value.contains = element => element === value || value.items.includes(element); value.querySelectorAll = () => value.items; return value; }
function key(key, shiftKey = false) { const e = {key, shiftKey, defaultPrevented: false, preventDefault() {this.defaultPrevented = true;}, stopPropagation() {}}; for (const fn of [...listeners.keydown]) fn(e); return e; }
const opener = control("open"), first = modal("first"), second = modal("second");
opener.focus();
let firstCloses = 0, secondCloses = 0, busy = true;
const detachFirst = Browser.attachDialogFocus(first, {document: owner, onClose() {firstCloses++;}, isBusy: () => false});
assert(owner.activeElement === first.items[0], "initial focus missing");
first.items[1].focus(); assert(key("Tab").defaultPrevented && owner.activeElement === first.items[0], "tab escaped");
assert(key("Tab", true).defaultPrevented && owner.activeElement === first.items[1], "reverse tab escaped");
const detachSecond = Browser.attachDialogFocus(second, {document: owner, onClose() {secondCloses++;}, isBusy: () => busy});
key("Escape"); assert(firstCloses === 0 && secondCloses === 0, "busy modal or background dismissed");
busy = false; key("Escape"); assert(secondCloses === 1 && firstCloses === 0, "escape closed multiple modals");
opener.focus(); assert(owner.activeElement === second.items[0], "background stole focus");
detachSecond(); assert(owner.body.style.overflow === "hidden" && owner.activeElement === first.items[1], "child close unlocked body or lost focus");
detachFirst(); assert(owner.activeElement === opener && owner.body.style.overflow === "scroll", "modal cleanup did not restore caller");
assert(listeners.keydown.size === 0 && listeners.focusin.size === 0, "modal cleanup leaked listeners");
`)
}

func TestFrontendDialogOutOfOrderUnmountKeepsScrollLock(t *testing.T) {
	vm := frontendBrowserVM(t, "dialogFocus")
	frontendPollingRun(t, vm, `
const owner = {body: {style: {overflow: "auto"}}, activeElement: null, addEventListener() {}, removeEventListener() {}};
const container = () => ({querySelectorAll() {return [];}, contains() {return false;}, focus() {}});
const first = Browser.attachDialogFocus(container(), {document: owner, onClose() {}, isBusy() {return false;}});
const second = Browser.attachDialogFocus(container(), {document: owner, onClose() {}, isBusy() {return false;}});
first(); assert(owner.body.style.overflow === "hidden", "background unmount unlocked body");
second(); assert(owner.body.style.overflow === "auto", "last modal failed to restore body");
`)
}

func TestFrontendBatchAdmissionUnknownRequiresVerificationBeforeRetry(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const mixed = Browser.describeBatchSubmission({selected:3,queued:1,failed:1,unknown:1,skipped:0,uncertain:true,outcome:"partial",failures:[{id:"failed",error:"合成确定失败"},{id:"unknown",error:"合成目录同步结果未知",uncertain:true}]}, "重置 2FA");
assert(mixed.tone === "warning" && mixed.label.includes("待核实"), "uncertain admission appeared settled");
for (const detail of ["已排队 1 个", "提交失败 1 个", "提交结果待核实 1 个", "核实账号与任务状态", "避免重复提交"]) assert(mixed.text.includes(detail), "uncertain admission lost distinction: " + detail);
assert(!mixed.text.includes("失败原因：合成确定失败；合成目录同步结果未知"), "unknown admission was called a confirmed failure");
const unknown = Browser.describeBatchSubmission({selected:1,queued:0,failed:0,unknown:1,skipped:0,uncertain:true,outcome:"unknown",failures:[{id:"unknown",error:"合成结果未知",uncertain:true}]}, "ChatGPT 授权");
assert(unknown.label === "提交结果待核实" && !unknown.text.includes("提交失败") && !unknown.text.includes("后重试"), "unknown-only result invited immediate retry");
`)
}

func frontendLatestReadVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendBrowserVM(t, "latestRead")
	frontendPollingRun(t, vm, `
var timers = new Map(), nextTimer = 0, requests = [], values = [], errors = [], loading = [];
var environment = {
  window: {
    setTimeout(fn) { const id = ++nextTimer; timers.set(id, fn); return id; },
    clearTimeout(id) { timers.delete(id); }
  },
  AbortController: class { constructor() { this.signal = {aborted: false}; } abort() { this.signal.aborted = true; } }
};
var reader = Browser.createLatestRead({environment, timeoutMs: 1000, onLoading(value) {loading.push(value);}});
function read(isCurrent) {
  reader.run(signal => new Promise((resolve, reject) => requests.push({signal, resolve, reject})), isCurrent)
    .then(value => values.push(value), error => errors.push(error));
}
`)
	return vm
}

func TestFrontendLatestReadReplacesOldRequestWithoutClearingCurrentLoading(t *testing.T) {
	vm := frontendLatestReadVM(t)
	frontendPollingRun(t, vm, `read();`)
	frontendPollingRun(t, vm, `assert(requests.length === 1, "first read missing"); read();`)
	frontendPollingRun(t, vm, `
assert(requests.length === 2 && requests[0].signal.aborted, "replacement did not cancel predecessor");
assert(errors.length === 1 && errors[0].name === "AbortError", "replacement cancellation was not distinguishable");
assert(loading[loading.length - 1] && timers.size === 1, "old cleanup cleared current loading or timer");
requests[0].resolve("obsolete"); requests[1].resolve("current");
`)
	frontendPollingRun(t, vm, `
assert(values.join() === "current", "old response replaced current options");
assert(!loading[loading.length - 1] && timers.size === 0, "completed read leaked loading or timers");
`)
}

func TestFrontendLatestReadRejectsChangedDraftAndTimesOutWithoutRetry(t *testing.T) {
	vm := frontendLatestReadVM(t)
	frontendPollingRun(t, vm, `var current = true; read(() => current);`)
	frontendPollingRun(t, vm, `current = false; requests[0].resolve("old draft");`)
	frontendPollingRun(t, vm, `
assert(values.length === 0 && errors[0].name === "AbortError", "changed draft accepted old options");
read();
`)
	frontendPollingRun(t, vm, `[...timers.values()][0]();`)
	frontendPollingRun(t, vm, `
assert(requests[1].signal.aborted && errors[1].message.includes("超时"), "deadline did not abort and report failure");
assert(requests.length === 2 && timers.size === 0 && !loading[loading.length - 1], "deadline retried or leaked state");
requests[1].resolve("late timeout"); read();
`)
	frontendPollingRun(t, vm, `reader.cancel();`)
	frontendPollingRun(t, vm, `
assert(requests[2].signal.aborted && errors[2].name === "AbortError", "close did not cancel pending options");
assert(values.length === 0 && timers.size === 0 && !loading[loading.length - 1], "canceled read retained options or resources");
`)
}

func TestFrontendLatestReadAllowsCompleteBackendReadBudgets(t *testing.T) {
	for _, test := range []struct {
		name     string
		constant string
		budgetMS int
		slowMS   int
	}{
		{"sub2-options", "SUB2_OPTIONS_TIMEOUT_MS", 245000, 65000},
		{"sms-options", "SMS_OPTIONS_TIMEOUT_MS", 25000, 20000},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := frontendBrowserVM(t, "latestRead")
			for name, value := range map[string]any{"timeoutConstant": test.constant, "expectedBudget": test.budgetMS, "slowResponseTime": test.slowMS} {
				if err := vm.Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			frontendPollingRun(t, vm, `
var now = 0, timers = new Map(), nextTimer = 0, requests = [], results = [], errors = [];
var environment = {
  window: {
    setTimeout(fn, delay) { const id = ++nextTimer; timers.set(id, {fn, at: now + delay}); return id; },
    clearTimeout(id) { timers.delete(id); }
  },
  AbortController: class { constructor() { this.signal = {aborted: false}; } abort() { this.signal.aborted = true; } }
};
function advance(ms) {
  now += ms;
  for (const [id, timer] of [...timers]) {
    if (timer.at <= now) { timers.delete(id); timer.fn(); }
  }
}
assert(Browser[timeoutConstant] === expectedBudget, "read budget no longer covers backend timeout");
var reader = Browser.createLatestRead({environment, timeoutMs: Browser[timeoutConstant], onLoading() {}});
function read() {
  reader.run(signal => new Promise(resolve => requests.push({signal, resolve})))
    .then(value => results.push(value), error => errors.push(error));
}
read();
`)
			frontendPollingRun(t, vm, `
advance(slowResponseTime);
assert(!requests[0].signal.aborted && errors.length === 0, "supported slow read was canceled early");
requests[0].resolve("slow options");
`)
			frontendPollingRun(t, vm, `
assert(results.join() === "slow options" && timers.size === 0, "supported slow result was discarded");
read();
`)
			frontendPollingRun(t, vm, `
advance(expectedBudget - 1);
assert(!requests[1].signal.aborted, "read did not receive its complete budget");
advance(1);
`)
			frontendPollingRun(t, vm, `
assert(requests[1].signal.aborted && errors.length === 1 && errors[0].message.includes("超时"), "budget did not bound stalled read");
assert(requests.length === 2 && timers.size === 0, "longer read budget introduced retry or leaked timers");
`)
		})
	}
}
