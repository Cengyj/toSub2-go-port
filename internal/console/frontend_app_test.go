package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// Execute production App handlers with synthetic hooks and component boundaries.
// This is not a DOM renderer or a substitute for browser testing.
func frontendAppVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	dir, err := filepath.Abs("../../web/src")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(dir, "main.jsx"))
	if err != nil {
		t.Fatal(err)
	}
	app, _, ok := strings.Cut(string(source), "const appRoot =")
	if !ok {
		t.Fatal("missing App mount boundary")
	}
	// Expose existing setters only to seed synthetic server data, without
	// depending on hook positions or replacing any production event handler.
	const controlsAnchor = "  const canDownloadSelected ="
	if strings.Count(app, controlsAnchor) != 1 {
		t.Fatal("missing App state controls boundary")
	}
	app = strings.Replace(app, controlsAnchor, "  globalThis.appState = {setJobs, setJobSelectionIndex, setSelectedJobIds, setFeatures};\n"+controlsAnchor, 1)
	built := api.Build(api.BuildOptions{
		Stdin:  &api.StdinOptions{Contents: app + "\nexport { App };", ResolveDir: dir, Sourcefile: "main.jsx", Loader: api.LoaderJSX},
		Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "AppHarness", Platform: api.PlatformNeutral,
		Plugins: []api.Plugin{{Name: "synthetic-component-boundaries", Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `(^react$|^react-dom($|/)|^lucide-react$|/components/|\.css$)`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			})
		}}},
	})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("App bundle: %v", built.Errors)
	}
	vm := sobek.New()
	frontendPollingRun(t, vm, `
var Intl = {}, location = {hash: "#settings"}, requests = [], storage = new Map(), failStorage = false, confirmAccepted = false;
var timers = new Map(), nextTimer = 0;
var window = {
  localStorage: {
    getItem(key) {return storage.get(key) || null;},
    setItem(key, value) {if (failStorage) throw new Error("synthetic quota"); storage.set(key, value);},
    removeItem(key) {storage.delete(key);}
  },
  setTimeout(fn, delay) {const id = ++nextTimer; timers.set(id, {fn, delay}); return id;},
  clearTimeout(id) {timers.delete(id);}, scrollTo() {}
};
var AbortController = class {constructor() {this.signal = {aborted: false};} abort() {this.signal.aborted = true;}};
var fetch = (url, options) => new Promise((resolve, reject) => requests.push({url, options, resolve, reject}));
var hookSlots = [], hookCursor = 0;
function state(initial) {
  const id = hookCursor++;
  if (!hookSlots[id]) hookSlots[id] = {value: typeof initial === "function" ? initial() : initial};
  return [hookSlots[id].value, value => {hookSlots[id].value = typeof value === "function" ? value(hookSlots[id].value) : value;}];
}
function memo(factory, deps) {
  const id = hookCursor++, slot = hookSlots[id];
  if (!slot || deps.some((value, index) => !Object.is(value, slot.deps[index]))) hookSlots[id] = {value: factory(), deps};
  return hookSlots[id].value;
}
var React = {useState: state, useRef: value => memo(() => ({current: value}), []), useMemo: memo, useEffect() {},
  Fragment: "Fragment", createElement(type, props, ...children) {return {type, props: {...props, children}};}};
function require(name) {
  if (name === "react") return React;
  const exports = name.endsWith("shell.jsx") ? {useTheme: () => ["light", () => {}, ""]}
    : name.endsWith("ui.jsx") ? {useConfirmation: () => async () => confirmAccepted}
    : name.endsWith("authorization-method.jsx") ? {normalizeAuthorizationMethod: value => value === "bps" ? "bps" : "chatgpt", authorizationMethodLabel: value => value === "bps" ? "BPS" : "ChatGPT"} : {};
  return new Proxy(exports, {get(target, key) {return key === "__esModule" ? false : target[key] || key;}});
}
function assert(value, message) {if (!value) throw new Error(message);}
function render() {hookCursor = 0; return AppHarness.App();}
function find(tree, type) {
  return findMatching(tree, node => node.type === type);
}
function findMatching(tree, predicate) {
  if (!tree || typeof tree !== "object") return null;
  if (predicate(tree)) return tree;
  for (const child of (Array.isArray(tree) ? tree : tree.props?.children || [])) {
    const match = findMatching(child, predicate); if (match) return match;
  }
  return null;
}
function content(tree) {
  if (typeof tree === "string" || typeof tree === "number") return String(tree);
  return (Array.isArray(tree) ? tree : tree?.props?.children || []).map(content).join("");
}
function findButton(tree, label) {return findMatching(tree, node => node.type === "button" && content(node).trim() === label);}
function prepareBatch() {
  location.hash = "#accounts"; render();
  const jobs = ["queued", "failed", "skipped"].map(id => ({id, email: id + "@synthetic.invalid", status: "completed", canForceRelogin: true, canResetTotp: true, canAddPassword: true, canDownload: true}));
  appState.setJobs(jobs); appState.setJobSelectionIndex(jobs); appState.setSelectedJobIds(new Set(jobs.map(job => job.id)));
  appState.setFeatures({bulkActions: true, forceRelogin: true, totpReset: true, passwordAdd: true});
  confirmAccepted = true;
  return render();
}
storage.set("chatgpt-onboarding.sub2api-upload-settings-v1", JSON.stringify({baseUrl: "https://synthetic.invalid", adminApiKey: "synthetic-test-key"}));
`)
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	return vm
}

func TestFrontendAppAllowsCancelingReadOnlySettingsRequest(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
find(render(), "ConfigurationPage").props.onSectionChange("sub2api");
var dialog = find(render(), "Sub2ApiSettingsDialog");
dialog.props.loadSub2ApiOptions();
`)
	frontendPollingRun(t, vm, `
var tree = render();
assert(find(tree, "Sub2ApiSettingsDialog").props.sub2apiGroupsLoading, "synthetic read did not start");
find(tree, "Sub2ApiSettingsDialog").props.setSub2apiSettingsOpen(false);
assert(!find(render(), "Sub2ApiSettingsDialog"), "read-only request incorrectly blocked Cancel");
`)
}

func TestFrontendAppProxyPersistenceConfirmsBeforeApplying(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
var page = find(render(), "ConfigurationPage");
assert(page.props.onProxySave("http://synthetic.invalid:8000") === true, "successful proxy save was reported as failure");
page = find(render(), "ConfigurationPage");
assert(page.props.proxy === "http://synthetic.invalid:8000", "successful proxy save was not applied");
failStorage = true;
assert(page.props.onProxySave("http://rejected.invalid:9000") === false, "failed proxy save was not reported");
page = find(render(), "ConfigurationPage");
assert(page.props.proxy === "http://synthetic.invalid:8000", "failed save replaced active proxy");
assert(storage.get("chatgpt-onboarding.account-proxy-v1") === "http://synthetic.invalid:8000", "failed save lost stored proxy");
`)
}

func TestFrontendAppKeepsSaveAndDirtyNavigationGuards(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
find(render(), "ConfigurationPage").props.onSectionChange("sub2api");
find(render(), "Sub2ApiSettingsDialog").props.saveSub2ApiSettings({preventDefault() {}});
`)
	frontendPollingRun(t, vm, `
var tree = render();
assert(find(tree, "ConfigurationPage").props.saving, "save request did not block navigation");
find(tree, "Sub2ApiSettingsDialog").props.setSub2apiSettingsOpen(false);
assert(find(render(), "Sub2ApiSettingsDialog"), "pending save was dismissed");
requests[0].resolve({ok: true, status: 200, json: async () => ({configured: true})});
`)
	frontendPollingRun(t, vm, `
find(render(), "ConfigurationPage").props.onSectionChange("sub2api");
find(render(), "Sub2ApiSettingsDialog").props.setSub2apiSettingsDraft(value => ({...value, baseUrl: "https://changed.invalid"}));
tree = render();
assert(find(tree, "ConfigurationPage").props.dirty, "draft change was not tracked");
find(tree, "Sub2ApiSettingsDialog").props.setSub2apiSettingsOpen(false);
tree = render();
assert(find(tree, "Sub2ApiSettingsDialog") && find(tree, "DialogFrame"), "dirty editor skipped discard confirmation");
`)
}

func TestFrontendAppBatchPartialSubmissionPreservesFailedSelection(t *testing.T) {
	for _, test := range []struct{ name, button, dialog, route string }{
		{"relogin", "重登授权", "ReloginActionDialog", "/api/jobs/relogin-batch"},
		{"totp", "重置 2FA", "TotpActionDialog", "/api/jobs/setup-2fa-batch"},
		{"password", "批量添加密码", "", "/api/jobs/add-password-batch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := frontendAppVM(t)
			for name, value := range map[string]any{"actionButton": test.button, "actionDialog": test.dialog, "actionRoute": test.route} {
				if err := vm.Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			frontendPollingRun(t, vm, `
var tree = prepareBatch(), handlerErrors = [];
findButton(tree, actionButton).props.onClick();
if (actionDialog) find(render(), actionDialog).props.onSubmit({authorizationMethod:"chatgpt"}).catch(error => handlerErrors.push(error));
`)
			frontendPollingRun(t, vm, `
assert(requests.length === 1 && requests[0].url === actionRoute, "unexpected synthetic submission");
requests[0].resolve({ok:true,status:200,json:async () => ({ok:false,selected:3,queued:1,started:1,skipped:1,failed:1,outcome:"partial",jobs:[{id:"queued"}],failures:[{id:"failed",error:"合成持久化失败"}]})});
`)
			frontendPollingRun(t, vm, `
tree = render();
assert(handlerErrors.length === 0, "partial response escaped the App handler");
assert(content(tree).includes("已排队 1 个") && content(tree).includes("提交失败 1 个") && content(tree).includes("按条件跳过 1 个"), "partial queue result lost counts or was described as completion");
assert(content(tree).includes("合成持久化失败"), "safe per-account failure reason was hidden");
for (const id of ["queued", "failed", "skipped"]) {
  const row = findMatching(tree, node => node.type === "JobRow" && node.props.job.id === id);
  assert(row.props.selected === (id !== "queued"), "selection did not retain only unqueued accounts: " + id);
}
assert(!actionDialog || !find(tree, actionDialog), "partial result left the stale full-batch confirmation open");
assert(requests.length === 1, "partial failure was automatically retried");
`)
		})
	}
}

func TestFrontendAppBatchRefusalAndUnknownTransportKeepRetrySelection(t *testing.T) {
	for _, outcome := range []string{"failed", "skipped", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("responseOutcome", outcome); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var tree = prepareBatch(), handlerErrors = [];
findButton(tree, "重登授权").props.onClick();
find(render(), "ReloginActionDialog").props.onSubmit({authorizationMethod:"chatgpt"}).catch(error => handlerErrors.push(error));
`)
			frontendPollingRun(t, vm, `
if (responseOutcome === "unknown") requests[0].reject(new Error("synthetic lost response"));
else requests[0].resolve({ok:false,status:409,json:async () => ({ok:false,selected:3,queued:0,started:0,skipped:responseOutcome === "skipped" ? 3 : 0,failed:responseOutcome === "failed" ? 3 : 0,outcome:responseOutcome,jobs:[],failures:responseOutcome === "failed" ? [{id:"failed",error:"合成持久化失败"}] : [],error:"没有任务被提交"})});
`)
			frontendPollingRun(t, vm, `
tree = render();
for (const id of ["queued", "failed", "skipped"]) assert(findMatching(tree, node => node.type === "JobRow" && node.props.job.id === id).props.selected, "unqueued or unconfirmed account lost selection");
if (responseOutcome === "unknown") {
  assert(find(tree, "ReloginActionDialog") && handlerErrors.length === 1 && handlerErrors[0].outcome === "unknown", "lost response did not preserve a cautious confirmation error");
} else {
  assert(!find(tree, "ReloginActionDialog") && handlerErrors.length === 0, "structured refusal was not shown by the App");
  assert(content(tree).includes(responseOutcome === "failed" ? "提交失败 3 个" : "按条件跳过 3 个"), "structured HTTP error lost failure/skip details");
}
assert(requests.length === 1, "refusal or unknown response was retried");
`)
		})
	}
}
