package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func TestFrontendInsightsPresentationSeparatesUsageAndAuthorization(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const defaults = Browser.normalizeInsightSettings(null);
assert(!defaults.enabled && defaults.automatic && defaults.intervalMinutes === 15 && !defaults.includeResetCredits, "unsafe defaults");
assert(Browser.normalizeInsightSettings({intervalMinutes:0}).intervalMinutes === 15, "invalid interval accepted");
assert(Browser.normalizeInsightSettings({excludedAccountIds:["one","one",null]}).excludedAccountIds.join() === "one", "exclusion list not normalized");
const usage = {source:"wham",observedAt:"2026-10-31T00:00:00Z",windows:[{windowSeconds:18000,usedPercent:0}]};
const zero = Browser.describeInsightSummary({usage,lastAttempt:{status:"success"}});
assert(zero.label.includes("已用 0%"), "zero usage became unknown");
const empty = Browser.describeInsightSummary({usage:{...usage,windows:[{usedPercent:null}]}});
assert(empty.label === "额度数值未知", "missing usage became zero");
const exhausted = Browser.describeInsightSummary({usage:{...usage,limitReached:true},lastAttempt:{status:"success"}});
assert(exhausted.label === "当前额度已用尽" && !exhausted.label.includes("授权"), "quota exhaustion became authentication failure");
const failed = Browser.describeInsightSummary({usage,lastAttempt:{status:"failed"}});
assert(failed.label === "本次查询失败" && failed.text.includes("历史结果") && failed.observedAt === usage.observedAt, "failure erased or refreshed historic data");
const bps = Browser.describeInsightSummary({bps:{observedAt:usage.observedAt,allowed:true}});
assert(bps.label.includes("BPS 权限") && !bps.label.includes("余额"), "BPS access invented quota");
const admission = Browser.describeInsightSubmission({accepted:["one"],merged:["two"],skipped:[{id:"three",reason:"未授权"}]});
for (const value of ["已受理 1", "已合并在途查询 1", "已跳过 1", "受理不代表查询完成", "未授权"]) assert(admission.text.includes(value), "missing admission distinction: " + value);
assert(Browser.describeInsightSubmission({skipped:{}}).text.includes("未受理"), "malformed response crashed presentation");
assert(!Browser.describeInsightSummary({persisted:false,lastAttempt:{status:"never"}}).text.includes("保存未确认"), "unqueried account became failed persistence");
assert(!Browser.describeInsightSummary({persisted:false,lastAttempt:{status:"failed"}}).text.includes("已获取"), "failed empty query claimed acquisition");
`)
}

func TestFrontendInsightsExpirationUsesVerifiedIdentityAndPreservesManual(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const job = {id:"synthetic",authorizationMethod:"bps",manualExpiresAt:"2026-11-01T00:00:00Z",subscriptionExpiresAt:"2026-12-01T00:00:00Z",expiresAtSource:"subscription",expiresAt:"2026-12-01T00:00:00Z"};
const snapshot = {id:job.id,authorizationMethod:"bps",subscription:{identityVerified:true,reported:true,expiresAt:"2027-01-01T00:00:00Z"}};
const effective = Browser.getInsightExpirationJob(job,snapshot);
assert(effective.manualExpiresAt === job.manualExpiresAt && effective.subscriptionExpiresAt === snapshot.subscription.expiresAt, "manual or queried date lost");
assert(job.subscriptionExpiresAt === "2026-12-01T00:00:00Z", "presentation mutated persisted job");
for (const invalid of [{...snapshot,id:"other"},{...snapshot,authorizationMethod:"chatgpt"},{...snapshot,subscription:{...snapshot.subscription,identityVerified:false}},{...snapshot,subscription:{...snapshot.subscription,expiresAt:"2026-02-30T00:00:00Z"}}]) assert(Browser.getInsightExpirationJob(job,invalid) === job, "untrusted query replaced known metadata");
const unknown = Browser.getInsightExpirationJob({...job,manualExpiresAt:""},{...snapshot,subscription:{...snapshot.subscription,expiresAt:null}});
assert(unknown.subscriptionExpiresAt === "" && unknown.expiresAt === "" && unknown.expiresAtSource === "unknown", "explicit trusted unknown revived old subscription date");
assert(!Browser.insightSnapshotForJob(job,{...snapshot,authorizationMethod:"chatgpt"}), "old authorization summary remained current");
`)
}

func frontendInsightsVM(t *testing.T, entry string) *sobek.Runtime {
	t.Helper()
	vm := frontendAppVM(t)
	path, err := filepath.Abs("../../web/src/" + entry)
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{EntryPoints: []string{path}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "Insights", Platform: api.PlatformNeutral,
		Plugins: []api.Plugin{{Name: "synthetic-insight-boundaries", Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `(^react$|^lucide-react$|/ui.jsx$|.css$)`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			})
		}}},
	})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("insights bundle: %v", built.Errors)
	}
	frontendPollingRun(t, vm, `
hookSlots=[]; hookCursor=0; var effectQueue=[], events=new Map();
function addEvent(prefix,name,fn){const key=prefix+name; if(!events.has(key))events.set(key,new Set()); events.get(key).add(fn);}
function removeEvent(prefix,name,fn){events.get(prefix+name)?.delete(fn);}
window.addEventListener=(name,fn)=>addEvent("window:",name,fn); window.removeEventListener=(name,fn)=>removeEvent("window:",name,fn);
var document={hidden:false,addEventListener:(name,fn)=>addEvent("document:",name,fn),removeEventListener:(name,fn)=>removeEvent("document:",name,fn)},navigator={onLine:true};
React.useEffect=(run,deps)=>{const id=hookCursor++,previous=hookSlots[id];if(!previous||deps.some((v,i)=>!Object.is(v,previous.deps[i]))){hookSlots[id]={deps,cleanup:previous?.cleanup};effectQueue.push(()=>{hookSlots[id].cleanup?.();hookSlots[id].cleanup=run();});}};
function renderHook(options){hookCursor=0;const value=Insights.useAccountInsights(options);for(const run of effectQueue.splice(0))run();return value;}
// Expand the actual hook-free presentation components after extraction, while
// keeping stateful React/UI boundaries and every behavioral assertion intact.
function expandInsightPresentation(tree){
  if(Array.isArray(tree))return tree.map(expandInsightPresentation);
  if(!tree||typeof tree!=="object")return tree;
  if(typeof tree.type==="function"&&["UsageMeter","InsightWindow","ObservationTime","InsightErrors","InsightDataSections"].includes(tree.type.name))return expandInsightPresentation(tree.type(tree.props));
  return tree.props?{...tree,props:{...tree.props,children:expandInsightPresentation(tree.props.children)}}:tree;
}
function renderComponent(name,props){hookCursor=0;const value=expandInsightPresentation(Insights[name](props));for(const run of effectQueue.splice(0))run();return value;}
function unmount(){for(const slot of hookSlots)slot?.cleanup?.();}
function fire(delay){const entry=[...timers].find(([,value])=>value.delay===delay);assert(entry,"missing timer "+delay);timers.delete(entry[0]);entry[1].fn();}
function fireEvent(name){for(const fn of [...(events.get(name)||[])])fn();}
function ok(request,data){request.resolve({ok:true,status:200,json:async()=>data});}
var options={token:"synthetic",ids:["one","two","one"],active:true,monitorActive:false};
var config={schemaVersion:1,revision:1,enabled:true,automatic:true,intervalMinutes:15,includeResetCredits:false,excludedAccountIds:[]};
`)
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	return vm
}

func TestFrontendInsightsPollsOnlyCurrentPageCacheAndReleasesReads(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `
assert(requests.length===2 && requests[0].url.endsWith("/settings") && requests[1].url === "/api/account-insights?ids=one%2Ctwo", "initial reads were not scoped cached GETs");
ok(requests[0],config);ok(requests[1],{snapshots:{one:{id:"one"}},revision:1});
`)
	frontendPollingRun(t, vm, `
hook=renderHook({...options,ids:["one","two"]});assert(hook.settingsLoaded&&hook.settings.intervalMinutes===15,"settings not adopted");
assert(requests.length===2,"same IDs restarted polling on parent render");fire(5000);
`)
	frontendPollingRun(t, vm, `
assert(requests.length===3 && requests[2].url.includes("ids=one%2Ctwo"),"summary interval is not five seconds");
options={...options,ids:["three"]};renderHook(options);assert(requests[2].options.signal.aborted,"page switch did not cancel old read");
`)
	frontendPollingRun(t, vm, `
assert(requests[3].url.endsWith("ids=three"),"page switch kept old accounts");ok(requests[3],{snapshots:{three:{id:"three"}}});ok(requests[2],{snapshots:{one:{id:"obsolete"}}});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.snapshots.three && !hook.snapshots.one,"late response replaced current page");
document.hidden=true;fireEvent("document:visibilitychange");assert(timers.size===0,"hidden page kept polling");
options={...options,active:false};renderHook(options);document.hidden=false;fireEvent("document:visibilitychange");
`)
	frontendPollingRun(t, vm, `
assert(requests.at(-1).url.endsWith("/settings"),"hidden account page restarted summary queries");
assert(requests.every(r=>!r.options.method||r.options.method==="GET"),"polling triggered an upstream admission");unmount();
`)
	frontendPollingRun(t, vm, `assert(timers.size===0 && [...events.values()].every(s=>s.size===0),"unmount leaked reads/timers/listeners");`)
}

func TestFrontendInsightsUnknownSettingsWriteRequiresExplicitVerification(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.setDraft({...hook.draft,intervalMinutes:30});hook=renderHook(options);var outcomes=[];
hook.saveSettings().then(v=>outcomes.push(v));hook.saveSettings().then(v=>outcomes.push(v));
assert(requests.length===2 && requests[1].options.method==="PUT","duplicate configuration write");
requests[1].reject(new Error("synthetic lost response"));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.uncertain&&hook.verificationRequired&&!hook.saving,"unknown save was presented as confirmed");
assert(hook.settings.intervalMinutes===15&&hook.draft.intervalMinutes===30,"unknown save lost confirmed settings or draft");
hook.saveSettings();assert(requests.length===2,"unknown write replayed");hook.reloadSettings();
`)
	frontendPollingRun(t, vm, `ok(requests[2],{...config,revision:2,intervalMinutes:30});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.uncertain&&!hook.verificationRequired&&!hook.dirty,"readback falsely confirmed durability or blocked explicit same-value save");
hook.saveSettings();
`)
	frontendPollingRun(t, vm, `ok(requests[3],{...config,revision:3,intervalMinutes:30});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.uncertain&&!hook.verificationRequired&&hook.settings.revision===3,"explicit confirmed save did not settle");
assert(storage.size===1,"server settings leaked into browser preferences");unmount();
`)
}

func TestFrontendInsightsReadOnlyDialogKeepsActionsAndSourceDistinctions(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, `
var submitted=[],canceled=[],automatic=[],closed=0;
var job={id:"synthetic/insight",email:"long.synthetic@example.invalid",authorizationMethod:"bps",canDownload:true};
var snapshot={id:job.id,authorizationMethod:"bps",canQuery:true,persisted:true,lastAttempt:{status:"partial"},bps:{observedAt:"2026-10-31T00:00:00Z",allowed:true,models:[]},errors:[{component:"usage",message:"额度查询受限"}]};
var insights={settings:config,settingsLoaded:true,settingsLoading:false,settingsError:"",verificationRequired:false,actionBusy:false,saving:false,revision:0,submit:async ids=>{submitted.push(ids);return {accepted:ids,merged:[],skipped:[]};},cancel:async id=>{canceled.push(id);return {cancelled:true};},setAutomatic:async(id,v)=>{automatic.push([id,v]);return true;}};
function view(){return renderComponent("AccountInsightsDialog",{job,token:"synthetic",initialSnapshot:snapshot,insights,onClose(){closed++;}});}
var tree=view();
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-insights/synthetic%2Finsight","details read private credentials or wrong endpoint");ok(requests[0],snapshot);
`)
	frontendPollingRun(t, vm, `
var tree=view();assert(content(tree).includes("查询部分更新")&&content(tree).includes("额度查询受限"),"partial result hidden");
findButton(tree,"立即查询").props.onClick();find(tree,"input").props.onChange({target:{checked:false}});
`)
	frontendPollingRun(t, vm, `
tree=view();assert(submitted.length===1&&submitted[0][0]===job.id&&automatic[0][1]===false,"detail callbacks changed business operation");
assert(content(tree).includes("受理不代表查询完成")||content(tree).includes("已排除"),"admission claimed completion");
findButton(tree,"关闭").props.onClick();assert(closed===1&&canceled.length===0,"closing dialog canceled background query");
assert(requests.every(r=>r.url.startsWith("/api/account-insights/")),"dialog read account-info");unmount();
`)
	frontendPollingRun(t, vm, `assert(timers.size===0,"detail leaked cache poll");`)
}

func TestFrontendInsightsPartialResultsRetainIndependentFreshness(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const now="2026-10-31T10:00:00Z", old="2026-10-30T10:00:00Z";
const snapshot={lastAttempt:{status:"partial",startedAt:now},usage:{observedAt:old},bps:{observedAt:now},subscription:{observedAt:now},errors:[{component:"usage",code:"permission_denied"},{component:"subscription",code:"subscription_scope_mismatch"}]};
assert(Browser.insightSourceIsHistorical(snapshot,"usage"),"retained usage became current after BPS success");
assert(!Browser.insightSourceIsHistorical(snapshot,"bps"),"healthy BPS became stale because usage failed");
assert(!Browser.insightSourceIsHistorical(snapshot,"subscription"),"current partial subscription observation was described as old");
assert(Browser.describeInsightSummary(snapshot).text.includes("历史结果"),"summary omitted retained usage age");
assert(Browser.insightPercent(null)===null&&Browser.insightPercent("0")===null&&Browser.insightPercent(0)===0,"unknown and zero collapsed");
`)
}

func TestFrontendInsightsConflictRetainsDraftUntilExplicitReadback(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.setDraft({...hook.draft,intervalMinutes:30});hook=renderHook(options);fire(30000);`)
	frontendPollingRun(t, vm, `ok(requests[1],{...config,revision:2,intervalMinutes:20});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.settings.intervalMinutes===20&&hook.draft.intervalMinutes===30&&hook.draft.revision===1,"periodic read erased unsaved draft or rebased revision");
hook.saveSettings();assert(JSON.parse(requests[2].options.body).revision===1,"write silently overwrote newer settings");
requests[2].resolve({ok:false,status:409,json:async()=>({error:"synthetic conflict"})});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(hook.verificationRequired&&hook.draft.intervalMinutes===30&&!hook.uncertain,"conflict lost draft or became transport uncertainty");
hook.saveSettings();assert(requests.length===3,"conflict automatically replayed");hook.reloadSettings();hook.reloadSettings();assert(requests.length===4,"duplicate verification reads");
`)
	frontendPollingRun(t, vm, `ok(requests[3],{...config,revision:2,intervalMinutes:20});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(!hook.verificationRequired&&!hook.dirty&&hook.draft.intervalMinutes===20,"explicit readback did not settle conflict");unmount();`)
}

func TestFrontendInsightsUnknownDisableReflectsFailClosedServerState(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.setDraft({...hook.draft,enabled:false});hook=renderHook(options);hook.saveSettings();
requests[1].resolve({ok:false,status:503,json:async()=>({error:"synthetic sync unknown",uncertain:true,settings:{...config,enabled:false,persistenceWarning:"durability unconfirmed"}})});
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.settings.enabled&&hook.uncertain&&hook.verificationRequired&&hook.persistenceWarning,"failed disable presented live collection or confirmed persistence");
var errors=[];hook.submit(["one"]).catch(error=>errors.push(error));
`)
	frontendPollingRun(t, vm, `assert(requests.length===2&&errors.length===1,"unknown disabled settings allowed manual query");unmount();`)
}

func TestFrontendInsightsMutationsDoNotReplayAndExclusionsStayServerSide(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);var errors=[];`)
	frontendPollingRun(t, vm, `ok(requests[0],{...config,automatic:false});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);hook.submit(["one","one"]).catch(error=>errors.push(error));hook.submit(["one"]);
assert(requests.length===2&&requests[1].url.endsWith("/refresh")&&JSON.parse(requests[1].options.body).ids.length===1,"duplicate refresh admission");
requests[1].reject(new Error("synthetic lost admission"));
`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(errors.length===1&&errors[0].outcome==="unknown"&&!hook.actionBusy&&requests.length===2,"unknown admission retried or claimed success");
hook.cancel("one");hook.cancel("one");assert(requests.length===3&&requests[2].url==="/api/account-insights/queries/one/cancel"&&requests[2].options.method==="POST","cancel duplicated or used wrong route");ok(requests[2],{cancelled:true});
`)
	frontendPollingRun(t, vm, `hook=renderHook(options);hook.setAutomatic("one",false);assert(requests[3].options.method==="PUT"&&JSON.parse(requests[3].options.body).excludedAccountIds.join()==="one","account exclusion was not saved server-side");ok(requests[3],{...config,automatic:false,revision:2,excludedAccountIds:["one"]});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.settings.excludedAccountIds.join()==="one"&&!hook.settings.automatic&&storage.size===1,"exclusion lost manual mode or wrote browser preferences");unmount();`)
}

func TestFrontendInsightsRejectsMalformedSettingsWithoutEnablingWrites(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],{...config,intervalMinutes:0});`)
	frontendPollingRun(t, vm, `
hook=renderHook(options);assert(!hook.settingsLoaded&&!hook.settings.enabled&&hook.settingsError,"invalid server config became a trusted default");hook.saveSettings();assert(requests.length===1,"invalid settings allowed mutation");unmount();
`)
}

func TestFrontendInsightsComponentsExposeUnknownAndExpiredResetStates(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, `
var tree=renderComponent("InsightWindow",{window:{windowSeconds:18000,usedPercent:null,resetsAt:"2026-10-30T00:00:00Z"},now:Date.parse("2026-10-31T00:00:00Z")});
assert(content(tree).includes("用量未知")&&content(tree).includes("预计已重置")&&content(tree).includes("待更新确认")&&!find(tree,"progress"),"past reset invented a zero balance");
tree=renderComponent("InsightWindow",{window:{windowSeconds:18000,usedPercent:0}});assert(find(tree,"progress").props.value===0&&content(tree).includes("已用 0%"),"known zero was hidden");
hookSlots=[];tree=renderComponent("AccountInsightsMonitor",{insights:{status:null,statusError:""}});assert(content(tree).includes("正在读取")&&!content(tree).includes("已关闭"),"unread status became disabled");
tree=renderComponent("AccountInsightsMonitor",{insights:{status:{enabled:true,automatic:false,counts:{partial:2,failed:1,skipped:3}},statusError:""}});assert(content(tree).includes("仅手动查询")&&content(tree).includes("部分更新")&&content(tree).includes("已跳过"),"monitor collapsed partial/skipped into success");
hookSlots=[];var changed={},saves=0;var settingsView={draft:config,settingsLoaded:false,settingsLoading:true,saving:false,dirty:false,setDraft:fn=>{changed=fn(config);},saveSettings:async()=>{saves++;return false;}};
tree=renderComponent("AccountInsightsSettings",{insights:settingsView});assert(findButton(tree,"保存查询配置").props.disabled&&find(tree,"input").props.disabled,"unread settings editable");
settingsView={...settingsView,settingsLoaded:true,settingsLoading:false,dirty:true};tree=renderComponent("AccountInsightsSettings",{insights:settingsView});find(tree,"form").props.onSubmit({preventDefault(){}});
`)
	frontendPollingRun(t, vm, `tree=renderComponent("AccountInsightsSettings",{insights:settingsView});assert(saves===1&&!content(tree).includes("账号查询配置已保存"),"failed save displayed saved confirmation");unmount();`)
}

func TestFrontendInsightsConfigurationHeaderKeepsUnconfirmedSaveVisible(t *testing.T) {
	vm := frontendInsightsVM(t, "components/shell.jsx")
	frontendPollingRun(t, vm, `
var props={section:"insights",dirty:false,saving:false,unconfirmed:true};
var tree=renderComponent("ConfigurationPage",props);
assert(content(tree).includes("保存待核实")&&!content(tree).includes("无待保存更改"),"configuration header contradicted unresolved persistence");
tree=renderComponent("ConfigurationPage",{...props,unconfirmed:false});
assert(content(tree).includes("无待保存更改")&&!content(tree).includes("保存待核实"),"confirmed configuration remained unresolved");
unmount();
`)
}
