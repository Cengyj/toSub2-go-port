package console

import (
	"github.com/evanw/esbuild/pkg/api"
	"testing"
)

func TestFrontendAccountQualitySeparatesVerdictAttemptAndHistoricalResult(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const lastResult={model:"synthetic-model",ruleVersion:"reasoning-six-v1",verdict:"normal",score:5,total:6,checkedAt:"2026-11-03T00:00:00Z",durationMs:3200};
for(const state of ["eligible","queued","active","coolingDown","unavailable"]) {
  const snapshot={state,lastResult,lastAttempt:{status:"failed",message:"synthetic timeout"},stale:false,persisted:true};
  const before=JSON.stringify(snapshot),view=Browser.describeQuality(snapshot);
  assert(view.label===(state==="active"?"检测中":state==="queued"?"排队中":"检测失败")&&view.tone!=="success"&&view.result.score===5,"latest attempt was hidden by historical success or old score was erased");
  assert(view.running===(state==="active")&&view.queued===(state==="queued"),"queue falsely claimed execution");
  assert(view.historical&&JSON.stringify(snapshot)===before,"history lost or input mutated");
}
assert(Browser.describeQuality({lastAttempt:{status:"failed",errorCode:"unsupported"}}).label==="暂不支持","unsupported became degraded");
assert(Browser.describeQuality({lastResult,stale:true,lastAttempt:{status:"success"}},"bps").label==="已过期","BPS lost its dated measurement after inference support was added");
assert(Browser.describeQuality({lastAttempt:{status:"never"}},"bps").label==="未检测","untested BPS grant was still labelled unsupported");
assert(Browser.describeQuality({lastAttempt:{status:"failed",errorCode:"timeout"}}).label==="检测失败","network failure became degraded");
assert(Browser.describeQuality({lastAttempt:{status:"unrecognized"}}).label==="结果未知","unknown became a passing score");
assert(Browser.describeQuality({lastAttempt:{status:"failed",errorCode:"invalid_response"}}).label==="结果未知","invalid response falsely became measured zero");
assert(Browser.describeQuality({lastResult:{...lastResult,score:null},lastAttempt:{status:"failed"}}).result===null,"missing score became zero");
assert(Browser.describeQuality({lastResult:{...lastResult,score:0,verdict:"degraded"}}).label==="低于阈值","real zero score became missing");
const stale=Browser.describeQuality({lastResult,stale:true});assert(stale.label==="已过期"&&stale.tone==="muted"&&stale.phase==="历史5/6·已过期","stale observation looked current");
for(const [status,errorCode,label,suffix] of [["failed","timeout","检测失败","本次失败"],["failed","invalid_response","结果未知","本次未知"],["cancelled","","已取消","本次取消"],["skipped","","已跳过","本次跳过"]]) {
  const snapshot={lastResult,lastAttempt:{status,errorCode},state:"eligible",persisted:true};
  const view=Browser.describeQuality(snapshot);
  assert(view.label===label&&view.tone!=="success"&&view.phase==="历史5/6·"+suffix,"latest terminal attempt appeared as a passing upload check");
  assert(view.result===lastResult&&view.timestamp===lastResult.checkedAt&&view.historical,"tightening latest status destroyed historical measurement");
}
const current=Browser.describeQuality({lastResult,lastAttempt:{status:"success"},stale:false});assert(current.label==="通过阈值"&&current.tone==="success"&&!current.historical,"fresh confirmed success regressed");
const notice=Browser.describeQualityReceipt({checkId:"synthetic",accepted:["one"],merged:[],skipped:[]});
assert(notice.tone==="info"&&notice.text.includes("受理不代表检测通过")&&notice.dismissAfterMs===6000,"admission became successful measurement");
`)
}

func TestFrontendAccountQualitySettingsFailClosedAndRemainServerOwned(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const defaults=Browser.DEFAULT_QUALITY_SETTINGS;
assert(!defaults.enabled&&!defaults.beforeUpload&&defaults.blockDegraded&&defaults.blockFailed&&defaults.model==="","unsafe defaults");
const valid={...defaults,enabled:true,model:"synthetic-model"};
for(const patch of [{model:""},{minimumScore:0},{minimumScore:2},{minimumScore:7},{minimumScore:1.5},{timeoutSeconds:301},{cooldownSeconds:-1},{resultTTLSeconds:86401},{reasoningEffort:"invented"},{model:"synthetic\nmodel"},{schemaVersion:2},{revision:0}]) {
  let refused=false;try{Browser.validateQualitySettings({...valid,...patch});}catch{refused=true;}assert(refused,"invalid settings accepted: "+JSON.stringify(patch));
}
const context={settings:valid,settingsLoaded:true,settingsLoading:false,verificationRequired:false,uncertain:false,saving:false,actionBusy:"",status:{settingsAvailable:true,enabled:true,running:true},error:""};
const snapshots={one:{state:"eligible",canCheck:true},two:{state:"active",canCheck:false}};
assert(Browser.qualityCanSubmit(context,snapshots,["one"])&&!Browser.qualityCanSubmit(context,snapshots,["missing"]),"eligibility guessed before server projection");
for(const patch of [{settingsLoaded:false},{settingsLoading:true},{verificationRequired:true},{uncertain:true},{saving:true},{actionBusy:"refresh"},{status:{settingsAvailable:false}},{status:{settingsAvailable:true,enabled:true,running:false}},{status:{settingsAvailable:true,enabled:false,running:true}},{error:"unavailable"}]) assert(!Browser.qualityCanSubmit({...context,...patch},snapshots,["one"]),"unconfirmed state admitted work");
assert(Browser.qualityCanSubmit(context,snapshots,null),"whole-inventory scope was reduced to selected rows");
`)
}

// The compact history is one read-only details entry, never a probe trigger.
func TestFrontendAccountQualityCellHasCompactReadOnlyDetails(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
let opened=0;const observed="2026-11-03T00:00:00Z";
const result={model:"synthetic-long-model-name",ruleVersion:"reasoning-six-v1",verdict:"normal",score:5,total:6,checkedAt:observed,durationMs:3200};
for(const [state,status] of [["eligible","never"],["queued","queued"],["active","running"],["eligible","success"],["unavailable","failed"],["eligible","cancelled"]]){
 const snapshot={state,lastAttempt:{status,message:"synthetic long failure ".repeat(20),httpStatus:status==="failed"?503:undefined,startedAt:status==="failed"?"2026-11-03T00:59:00Z":undefined,finishedAt:status==="failed"?"2026-11-03T01:00:00Z":undefined},lastResult:status==="never"?undefined:result,persisted:true};
 const tree=renderComponent("AccountQualitySummary",{snapshot,onOpen(){opened++;},accountLabel:"synthetic@example.invalid"});
 if(status==="never"){assert(tree===null,"unmeasured probe invented an empty action or placeholder");continue;}
 assert(tree.type==="button"&&tree.props.type==="button"&&usageNodes(tree,"button").length===1&&!tree.props.disabled,"history lost its single native read-only button");
 assert(tree.props["aria-busy"]===(state==="active"),"queue falsely announced execution");
 const cells=usageClass(tree,"quality-history-cell");
 assert(cells.length===1&&cells[0].props["data-tone"]===(status==="failed"?"danger":"success"),"legacy snapshot fabricated multiple history records or failure became pass");
 assert(!usageNodes(tree,"time").length&&!usageNodes(tree,"progress").length&&!usageClass(tree,"quality-meta-line").length&&!content(tree).includes(result.model)&&!content(tree).includes("100%"),"row repeated metadata or invented progress");
 assert(tree.props["aria-label"].includes("synthetic@example.invalid")&&tree.props.title.includes("查看推理探针详情")&&!tree.props.title.includes("synthetic long failure"),"read-only entry lost account context or exposed diagnostic prose");
 if(status==="success")assert(cells[0].props.title.includes("5/6 题正确")&&cells[0].props.title.includes(result.model),"history tooltip lost the exact measured score or model");
 tree.props.onClick();
}
assert(opened===5&&requests.length===0&&timers.size===0,"opening history scheduled probe work");
`)
}

const qualityHookSetup = `
function renderQuality(options){hookCursor=0;const value=Insights.useAccountQuality(options);for(const run of effectQueue.splice(0))run();return value;}
const qualityConfig={schemaVersion:1,revision:1,enabled:true,beforeUpload:false,blockDegraded:true,blockFailed:true,model:"synthetic-model",reasoningEffort:"high",minimumScore:1,timeoutSeconds:90,cooldownSeconds:60,resultTTLSeconds:900};
const qualityStatus={enabled:true,beforeUpload:false,running:true,queued:0,active:0,settingsAvailable:true,revision:1,settingsRevision:1};
const qualitySnapshot=id=>({id,state:"eligible",canCheck:true,stale:false,persisted:true,revision:1,lastAttempt:{checkId:"",source:"",status:"never"}});
var qualityOptions={token:"synthetic-token",enabled:true,active:true,context:"accounts",ids:["one"],selectedIds:[]};
function qualityList(ids=["one"]){return {snapshots:Object.fromEntries(ids.map(id=>[id,qualitySnapshot(id)])),status:qualityStatus};}
`

func TestFrontendAccountQualityHookUsesScopedReadsAndNoImplicitProbe(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `
assert(requests.length===2&&requests[0].url==="/api/account-quality/settings"&&requests[1].url==="/api/account-quality?ids=one","initial reads not scoped");
ok(requests[0],qualityConfig);ok(requests[1],qualityList());
`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(quality.canSubmit(["one"]),"confirmed account not eligible");
qualityOptions={...qualityOptions,ids:["two"],selectedIds:["one"],detailId:"three"};renderQuality(qualityOptions);
`)
	frontendPollingRun(t, vm, `
assert(requests.at(-1).url==="/api/account-quality?ids=one%2Cthree%2Ctwo","selection or dialog scope omitted");
const pending=requests.at(-1);qualityOptions={...qualityOptions,active:false,context:"settings"};quality=renderQuality(qualityOptions);
assert(pending.options.signal.aborted&&!quality.canSubmit(null),"leaving accounts did not cancel list or prohibit submissions");
ok(pending,qualityList(["stale"]));
`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(!quality.snapshots.stale,"late view response replaced cache");
assert(requests.every(request=>!request.options.method||request.options.method==="GET"),"cache read triggered active model content");unmount();
assert(timers.size===0&&[...events.values()].every(value=>value.size===0),"unmount leaked timers or listeners");
`)
}

func TestFrontendAccountQualityMutationSingleFlightUnknownAndAllScope(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);ok(requests[1],qualityList());`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);let outcomes=[];quality.submitAll().catch(error=>outcomes.push(error.message));quality.submitAll();
assert(requests.length===3&&JSON.stringify(JSON.parse(requests[2].options.body))==="{\"all\":true}","all scope filtered or duplicate write admitted");
requests[2].reject(new Error("synthetic response lost"));
`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(quality.verificationRequired&&!quality.canSubmit(null),"lost response enabled another probe");
assert(requests.length===4&&requests[3].options.method!=="POST","unknown request automatically repeated mutation");
quality.submitAll();assert(requests.length===4,"verification barrier allowed duplicate");
ok(requests[3],{snapshots:{one:{...qualitySnapshot("one"),state:"active",canCheck:false,lastAttempt:{status:"running",checkId:"synthetic"}}},status:{...qualityStatus,active:1}});
`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(!quality.verificationRequired&&!quality.canSubmit(["one"])&&quality.snapshots.one.state==="active","fresh read failed to settle actual execution");
assert(outcomes.length===1&&requests.filter(request=>request.options.method==="POST").length===1,"failure was swallowed or retried");unmount();
`)
}

func TestFrontendAccountQualityUnknownSettingsRequireReadbackAndExplicitSave(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `qualityOptions={...qualityOptions,active:false,context:"settings"};var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);quality.setDraft({...quality.draft,beforeUpload:true});quality=renderQuality(qualityOptions);quality.saveSettings();quality.saveSettings();
assert(requests.length===2&&requests[1].options.method==="PUT","settings duplicate admitted");requests[1].reject(new Error("synthetic lost save"));
`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(quality.uncertain&&quality.settingsVerificationRequired&&quality.draft.beforeUpload&&!quality.settings.beforeUpload,"unknown settings discarded draft or pretended success");
quality.saveSettings();assert(requests.length===2,"unknown write replayed");quality.reloadSettings();
`)
	frontendPollingRun(t, vm, `ok(requests[2],{...qualityConfig,revision:2,beforeUpload:true});`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);assert(quality.uncertain&&!quality.settingsVerificationRequired&&!quality.dirty,"readback falsely proved durable persistence");quality.saveSettings();
`)
	frontendPollingRun(t, vm, `ok(requests[3],{...qualityConfig,revision:3,beforeUpload:true});`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(!quality.uncertain&&!quality.settingsVerificationRequired&&quality.settings.revision===3,"explicit save not confirmed");assert(storage.size===1,"server-owned policy leaked into browser preference storage");unmount();`)
}

func TestFrontendAccountQualityTokenABADropsOldResponsesAndCallbacks(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `
const oldSettings=requests[0],oldList=requests[1],oldReload=quality.reloadSettings,oldDraft=quality.setDraft;
qualityOptions={...qualityOptions,token:"synthetic-other"};renderQuality(qualityOptions);
qualityOptions={...qualityOptions,token:"synthetic-token"};renderQuality(qualityOptions);
assert(oldSettings.options.signal.aborted&&oldList.options.signal.aborted,"session changed without aborting old requests");
const count=requests.length;oldReload();oldDraft({...qualityConfig,model:"stale-draft"});assert(requests.length===count,"old callback submitted under new session");
ok(oldSettings,{...qualityConfig,model:"stale-model"});ok(oldList,qualityList(["stale"]));
`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(!quality.settingsLoaded&&!quality.snapshots.stale&&quality.settings.model!=="stale-model"&&quality.draft.model!=="stale-draft","ABA token accepted old response or draft callback");unmount();`)
}

func TestFrontendAccountQualityLeavingPageAbortsMutationWithoutReplaying(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);ok(requests[1],qualityList());`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);var receipts=[];quality.submit(["one"]).then(value=>receipts.push(value));qualityOptions={...qualityOptions,active:false,context:"settings"};renderQuality(qualityOptions);assert(requests[2].options.signal.aborted,"page leave did not abort request");ok(requests[2],{checkId:"late",accepted:["one"],merged:[],skipped:[]});`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(receipts.length===1&&receipts[0]===null&&quality.verificationRequired&&!quality.actionBusy,"late receipt escaped the old page");qualityOptions={...qualityOptions,active:true,context:"accounts"};renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `assert(requests.length===4&&requests[3].options.method!=="POST","returning to page replayed the active probe");ok(requests[3],qualityList());`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(!quality.verificationRequired,"fresh read did not verify current state");unmount();`)
}

func TestFrontendAccountQualitySettingsRevisionDoesNotDiscardUnsavedDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);ok(requests[1],{...qualityList(),status:{...qualityStatus,revision:30}});`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(!quality.settingsVerificationRequired,"ordinary state revision became configuration conflict");quality.setDraft({...quality.draft,model:"synthetic-unsaved-model"});quality=renderQuality(qualityOptions);quality.reload();`)
	frontendPollingRun(t, vm, `ok(requests[2],{...qualityList(),status:{...qualityStatus,settingsRevision:2,revision:31}});`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(quality.settingsVerificationRequired&&quality.draft.model==="synthetic-unsaved-model"&&!quality.canSubmit(null),"cross-browser settings silently replaced draft or allowed old policy");unmount();`)
}

func TestFrontendAccountQualitySettingsShowsCostAndExplicitServerPolicy(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `
var updates=[],saved=0,modelReads=[],quality={draft:qualityConfig,settings:qualityConfig,settingsLoaded:true,settingsLoading:false,saving:false,dirty:true,settingsError:"",uncertain:false,settingsVerificationRequired:false,manualSession:{},modelsAPI:async(id,options)=>{modelReads.push({id,options});return {models:[]};},setDraft(update){updates.push(update(qualityConfig));},saveSettings:async()=>{saved++;return true;},resetDraft(){},reloadSettings(){}};
const jobs=[{id:"codex-source",email:"codex@synthetic.invalid",authorizationMethod:"chatgpt"},{id:"bps-source",email:"bps@synthetic.invalid",authorizationMethod:"bps"},{id:"unsupported-source",authorizationMethod:"other"}];
const original=JSON.stringify(qualityConfig),tree=renderComponent("AccountQualitySettings",{quality,jobs});
for(const label of ["消耗模型额度","保存在本地服务","仅主动检测或上传前策略","上传前检查探针结果","答案不符时阻止上传","失败或未知时阻止上传","任一账号未通过时整批不上传"])assert(content(tree).includes(label),"missing product contract: "+label);
assert(!content(tree).includes("少量"),"settings promised an unmeasured quota cost");
const model=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect");
assert(model&&model.props.value===qualityConfig.model&&!findMatching(tree,node=>node.type==="input"&&node.props.placeholder==="填写此账号可调用的模型名称"),"default model selection bypassed shared searchable directory picker");
const source=findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="模型目录来源");
assert(source&&content(source).includes("Codex")&&content(source).includes("BPS")&&!content(source).includes("unsupported-source"),"model directory source omitted a supported identity or exposed unsupported one");
model.props.onChange("synthetic-catalog-model");assert(updates[0].model==="synthetic-catalog-model"&&JSON.stringify(qualityConfig)===original,"model choice ignored explicit draft or mutated saved policy");
tree.props.onSubmit({preventDefault(){}});assert(saved===1&&requests.length===0,"settings presentation wrote outside server hook");
quality.settingsVerificationRequired=true;const blocked=renderComponent("AccountQualitySettings",{quality,jobs});assert(findMatching(blocked,node=>node.type==="button"&&node.props.type==="submit").props.disabled,"unconfirmed policy could be saved without reread");
unmount();assert(timers.size===0,"settings model directory timer survived close");
`)
}

func TestFrontendAccountQualityAppRemovesHeaderAndSharesAllRowSelectionActions(t *testing.T) {
	vm := frontendAppVM(t, api.Plugin{Name: "synthetic-quality-app", Setup: func(build api.PluginBuild) {
		build.OnResolve(api.OnResolveOptions{Filter: `/hooks/(accountQuality|accountInsights|serverState)\.js$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			return api.OnResolveResult{Path: args.Path, Namespace: "quality-app"}, nil
		})
		build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "quality-app"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			source := `export function useAccountQuality(){return globalThis.qualityApp;} export function useAccountInsights(){return globalThis.insightApp;} export function useAccountPolling({setConnected,setPagination}){setConnected(true);setPagination({total:1,totalAll:4,totalPages:1,page:1,pageSize:20});return ()=>{};} export function useMonitorPolling(){return "";} export function useLatestRead(){return ()=>{};}`
			return api.OnLoadResult{Contents: &source, Loader: api.LoaderJS}, nil
		})
	}})
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `
var qualityCalls=[],qualityApp={settings:qualityConfig,draft:qualityConfig,settingsLoaded:true,snapshots:{one:qualitySnapshot("one")},status:qualityStatus,canSubmit:ids=>ids===null||ids.length>0,submit:async ids=>{qualityCalls.push({scope:"selected",ids});return {checkId:"fixture",accepted:ids,merged:[],skipped:[]};},submitAll:async()=>{qualityCalls.push({scope:"all"});return {checkId:"fixture-all",accepted:["one"],merged:[],skipped:[]};},resetDraft(){}};
var insightApp={settings:{enabled:true},settingsLoaded:true,snapshots:{},resetDraft(){},refreshSummary:{disabled:false,busy:false,label:"用量查询状态"},selectedRefreshControl:{disabled:false,busy:false,label:"刷新所选用量"}};
location.hash="#accounts";var tree=render();appState.setToken("synthetic");appState.setFeatures({accountQuality:true,accountInsights:true,accountTrash:true,bulkActions:true});appState.setJobs([{id:"one",email:"synthetic@example.invalid",status:"completed",authorizationMethod:"chatgpt"}]);appState.setSelectedJobIds(new Set(["one"]));tree=render();tree=render();
const header=findMatching(tree,node=>node.type==="PageHeader"&&node.props.title==="账号管理");assert(!header&&!find(tree,"AccountsPageActions"),"quality actions recreated the removed page heading or header actions");
assert(findMatching(tree,node=>node.type==="h2"&&content(node)==="账号信息与操作"),"quality feature kept usage-only list title");
appState.setFeatures({accountInsights:true,accountTrash:true,bulkActions:true});tree=render();assert(findMatching(tree,node=>node.type==="h2"&&content(node)==="账号用量与操作"),"without quality the established usage title changed");
appState.setFeatures({bulkActions:true});tree=render();assert(findMatching(tree,node=>node.type==="h2"&&content(node)==="账号列表"),"ordinary account-list title changed");
appState.setFeatures({accountQuality:true,accountInsights:true,accountTrash:true,bulkActions:true});tree=render();
const table=findMatching(tree,node=>node.type==="table"&&node.props.className==="accounts-table");assert(table&&table.props["data-quality"]===true&&content(find(table,"thead")).includes("推理探针"),"enabled quality feature lost its retained list column");
const all=findButton(tree,"全部账号探针"),selected=findButton(tree,"所选账号探针"),row=find(tree,"JobRow");assert(all&&!all.props.disabled&&selected&&!selected.props.disabled&&row.props.qualityCanCheck,"independent scopes not actionable");
all.props.onClick();selected.props.onClick();row.props.onCheckQuality();
`)
	frontendPollingRun(t, vm, `assert(qualityCalls.length===3&&qualityCalls[0].scope==="all"&&qualityCalls[1].ids.join()==="one"&&qualityCalls[2].ids.join()==="one","App changed all or selected scope");tree=render();findButton(tree,"探针设置").props.onClick();tree=render();const configuration=find(tree,"ConfigurationPage");assert(configuration.props.section==="quality"&&find(tree,"AccountQualitySettings"),"settings entry did not open server policy");assert(requests.length===0,"navigation invoked upstream work");`)
}

func TestFrontendAccountQualityHookSendsOneOffOptionsWithoutSavingGlobalPolicy(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `var quality=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);ok(requests[1],qualityList());`)
	frontendPollingRun(t, vm, `
quality=renderQuality(qualityOptions);var original=JSON.stringify(quality.settings),runOptions={model:"selected-catalog-model",reasoningEffort:"medium"};
quality.submit(["one"],runOptions);quality.submit(["one"],runOptions);
var payload=JSON.parse(requests[2].options.body);
assert(requests.length===3&&requests[2].url==="/api/account-quality/refresh"&&requests[2].options.method==="POST"&&payload.ids.join()==="one"&&JSON.stringify(payload.options)===JSON.stringify(runOptions),"one-off probe lost explicit model parameters or duplicated admission");
assert(JSON.stringify(quality.settings)===original&&JSON.stringify(quality.draft)===original&&!requests.some(request=>request.options.method==="PUT"),"one-off probe saved its selection as global upload policy");
ok(requests[2],{checkId:"one-off",accepted:["one"],merged:[],skipped:[]});
`)
	frontendPollingRun(t, vm, `quality=renderQuality(qualityOptions);assert(requests.length===4&&!requests[3].options.method,"admission verification replayed generation");unmount();`)
}

func TestFrontendAccountQualityOneOffMeasurementIsNotExpiredOrDefaultPolicyApproval(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var result={model:"one-off-model",ruleVersion:"candy-v1",score:1,total:1,verdict:"normal",checkedAt:"2026-11-05T00:00:00Z",durationMs:2100};
var snapshot={id:"independent-measurement",state:"eligible",canCheck:true,stale:true,staleReason:"options_changed",persisted:true,lastAttempt:{status:"success"},lastResult:result};
var summary=renderComponent("AccountQualitySummary",{snapshot,authorizationMethod:"bps",onOpen(){}});
assert(summary.props.title.includes("参数不同")&&!summary.props.title.includes("已过期")&&!content(summary),"historical one-off measurement claimed expiry or showed current upload-policy approval");
var quality={settings:{enabled:true,model:"default-model",reasoningEffort:"high"},settingsLoaded:true,snapshots:{[snapshot.id]:snapshot},modelsAPI:async()=>({models:[]}),canSubmit(){return false;}};
var tree=renderComponent("AccountQualityDialog",{job:{id:snapshot.id,email:"independent@synthetic.invalid",authorizationMethod:"bps"},quality,onCheck(){},onClose(){}});
assert(content(tree).includes("糖果题通过")&&content(tree).includes("本次参数与默认配置不同，不用于默认上传策略")&&!content(tree).includes("已过期"),"detail erased passing single-run evidence or presented it as default upload approval");
unmount();
`)
}

func TestFrontendAccountQualityRowKeepsStatusColumnMenuAndHTTP(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const job={id:"synthetic-probe-menu",email:"probe-menu@example.invalid",authorizationMethod:"chatgpt",status:"completed"};
const qualitySnapshot={id:job.id,httpObservation:{source:"candy",status:"failed",httpStatus:429,finishedAt:"2026-11-06T01:00:00Z"}};
let viewed=0,checked=0;
for(const qualityAvailable of [false,true]){
 hookSlots=[];hookCursor=0;
 const row=RepairUI.JobRow({job,qualityAvailable,qualitySnapshot,qualityCanCheck:true,onViewInsights(){},onViewQuality(){viewed++;},onCheckQuality(){checked++;}});
 const cells=usageNodes(row,"td"),summary=collectUsageNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountInsightSummary")[0];
 const count=qualityAvailable?9:8;
 assert(cells.length===count&&cells[2].props.className==="usage-cell"&&cells[count-5].props.className==="expiration-cell","probe feature changed the retained account column structure");
 const probeCells=usageClass(row,"quality-cell");
 const statuses=collectUsageNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountQualityStatus");
 assert(probeCells.length===Number(qualityAvailable)&&statuses.length===Number(qualityAvailable)&&!usageClass(row,"quality-history-cell").length&&!collectUsageNodes(row,node=>typeof node.type==="function"&&node.type.name==="AccountQualitySummary").length,"probe column lost its readable status or restored color blocks");
 if(qualityAvailable){assert(statuses[0].props.snapshot===qualitySnapshot&&statuses[0].props.accountLabel===job.email,"probe state lost its snapshot or account identity");const stateView=statuses[0].type(statuses[0].props);assert(stateView.type==="button"&&stateView.props.type==="button","probe state lost native details-button semantics");stateView.props.onClick();}
 assert(summary&&summary.props.qualitySnapshot===(qualityAvailable?qualitySnapshot:undefined),"emptying probe cells dropped their HTTP evidence from usage aggregation");
 const menu=findMatching(row,node=>node.type==="ActionMenu"&&node.props.ariaLabel==="账号操作 "+job.email);
 const details=findButton(menu,"推理探针详情"),run=findButton(menu,"发起推理探针");
 assert(Boolean(details)===qualityAvailable&&Boolean(run)===qualityAvailable,"probe menu entries no longer follow their feature gate");
 if(qualityAvailable){assert(!run.props.disabled,"available menu probe became disabled");details.props.onClick();run.props.onClick();}
 for(const filtered of [false,true])assert(Number(find(RepairUI.EmptyState({filtered,columnCount:count}),"td").props.colSpan)===count,"empty or filtered table failed to span the retained columns");
}
assert(viewed===2&&checked===1&&requests.length===0&&timers.size===0,"status/menu callbacks changed scope or table rendering invoked upstream work");
`)
}

func TestFrontendAccountQualityStatusCombinesCurrentStateWithRecordedHistory(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const result={model:"synthetic-state",ruleVersion:"candy-v1",score:1,total:1,verdict:"normal",checkedAt:"2026-11-06T01:00:00Z"};
const previous={...result,id:"previous-pass",kind:"candy",status:"success",finishedAt:result.checkedAt};
const cases=[
 [undefined,"等待数据","unknown"],
 [{},"结果待确认","unknown"],
 [{state:"eligible",lastAttempt:{status:"never"}},"尚未检测","unknown"],
 [{state:"queued",lastAttempt:{status:"queued"}},"排队中","queued"],
 [{state:"active",lastAttempt:{status:"running"}},"检测中","running"],
 [{lastAttempt:{status:"failed",errorCode:"timeout"}},"检测失败","failed"],
 [{lastAttempt:{status:"failed",errorCode:"invalid_response"}},"结果待确认","unknown"],
 [{lastAttempt:{status:"cancelled"}},"已取消","unknown"],
 [{lastAttempt:{status:"success"},lastResult:result,persisted:true},"通过","passed"],
 [{lastAttempt:{status:"success"},lastResult:{...result,score:0,verdict:"degraded"},persisted:true},"答案不符","incorrect"],
 [{lastAttempt:{status:"success"},lastResult:result,persisted:true,stale:true},"历史记录","unknown"],
 [{lastAttempt:{status:"success"},lastResult:result,persisted:false},"保存待核实","unknown"],
 [{lastAttempt:{status:"failed",errorCode:"invalid_response"},lastResult:result,persisted:true},"结果待确认","unknown"],
];
for(const [httpStatus,errorCode,label,outcome] of [[502,"upstream_error","检测失败","failed"],[429,"rate_limited","请求限流","unknown"],[200,"invalid_response","结果待确认","unknown"]]){
 const finishedAt="2026-11-06T02:00:00Z";
 cases.push([{
  lastResult:result,stale:true,persisted:true,
  lastAttempt:{status:"failed",errorCode,httpStatus,startedAt:"2026-11-06T01:59:00Z",finishedAt},
  probeHistory:[previous,{id:"new-failure-"+httpStatus,kind:"candy",status:"failed",errorCode,httpStatus,finishedAt}],
 },label,outcome]);
}
// A rounded/equal timestamp is not permission for an old pass to override a
// failed current attempt. The result icon represents current state, not history.
for(const [errorCode,label,outcome] of [["timeout","检测失败","failed"],["invalid_response","结果待确认","unknown"]]){
 cases.push([{lastResult:result,persisted:true,lastAttempt:{checkId:"new-attempt",status:"failed",errorCode,finishedAt:result.checkedAt},probeHistory:[previous]},label,outcome]);
}
for(const [errorCode,label] of [["invalid_response","结果待确认"],["unsupported_authorization","暂不支持"]]){
 cases.push([{lastResult:result,persisted:true,lastAttempt:{checkId:previous.id,status:"success",errorCode,finishedAt:result.checkedAt},probeHistory:[previous]},label,"unknown"]);
}
let opened=0;
function assertState(tree,outcome){
 assert(tree.props["data-current-outcome"]===outcome,"current status was replaced by historical results: "+outcome);
 const bars=usageClass(tree,"quality-run-bar");
 assert(bars.length<=5&&bars.every(bar=>bar.type==="span"&&bar.props.role==="img"&&bar.props["data-probe-id"]&&bar.props["aria-label"]&&bar.props["data-caption"]),"record bars lost identity or accessible per-attempt descriptions");
 const empty=usageClass(tree,"quality-run-placeholder");
 assert(bars.length+empty.length===5&&empty.every(slot=>slot.props["aria-hidden"]==="true"&&!slot.props["data-probe-id"]&&!slot.props["data-outcome"]&&slot.props["data-caption"].includes("暂无可显示记录")),"fixed slots fabricated probe evidence or lost their empty-state explanation");
 assert(!usageClass(tree,"quality-pulse").length&&!usageClass(tree,"quality-signal").length,"history reverted to a decorative multi-stroke result");
 assert(usageNodes(tree,"button").length===1&&!usageNodes(tree,"progress").length&&!collectUsageNodes(tree,node=>node.props?.["aria-valuenow"]!==undefined).length,"history invented numeric strength or nested controls");
}
for(const [snapshot,label,outcome] of cases){
 const before=JSON.stringify(snapshot),tree=renderComponent("AccountQualityStatus",{snapshot,accountLabel:"state@example.invalid",onOpen(){opened++;}});
 assert(tree.props["data-state-label"]===label,"signal current-state label was misclassified: expected "+label+", got "+tree.props["data-state-label"]);
 assert(tree.type==="button"&&tree.props.type==="button"&&tree.props["aria-label"].includes(label)&&tree.props["aria-label"].includes(label)&&tree.props["aria-label"].includes("state@example.invalid"),"signal entry lost native semantics or account-specific explanation");
 assert(tree.props["aria-busy"]===(snapshot?.state==="active"),"queued or terminal state was announced as executing");
 assertState(tree,outcome);
 tree.props.onClick();assert(JSON.stringify(snapshot)===before,"signal presentation modified saved evidence");
}
for(const snapshot of [undefined,{state:"eligible",lastAttempt:{status:"never"}},{lastAttempt:{status:"success"},lastResult:result,persisted:true}]){
 const tree=renderComponent("AccountQualityStatus",{snapshot,stateUncertain:true,onOpen(){}});
 assert(tree.props["data-state-label"]==="状态待同步"&&tree.props["aria-label"].includes("状态待同步"),"unsynchronized state still claimed a current pass or confirmed pristine account");
 assertState(tree,"unknown");
 if(snapshot?.lastResult)assert(tree.props["aria-label"].includes("保留已取得的历史结果")&&tree.props["aria-label"].includes("1/1 题正确"),"sync uncertainty discarded prior evidence rather than keeping it in details");
}
const history=[
 {...previous,model:"candy-old",finishedAt:"2026-11-06T01:00:00Z"},
 {id:"drawing",kind:"pelican",status:"success",model:"drawing-not-candy",finishedAt:"2026-11-06T01:00:01Z"},
 {...previous,id:"candy-new",model:"candy-new",finishedAt:"2026-11-06T01:00:02Z"},
];
const mixed=renderComponent("AccountQualityStatus",{snapshot:{state:"active",lastAttempt:{status:"running"},probeHistory:[...history].reverse()},onOpen(){}});
assertState(mixed,"running");
assert(mixed.props["aria-label"].includes("candy-old")&&mixed.props["aria-label"].includes("candy-new")&&mixed.props["aria-label"].indexOf("candy-old")<mixed.props["aria-label"].lastIndexOf("candy-new")&&!mixed.props["aria-label"].includes("drawing-not-candy")&&!mixed.props["aria-label"].includes("鹈鹕绘图"),"tooltip lost candy chronology or included drawing history");
const drawingOnly=renderComponent("AccountQualityStatus",{snapshot:{probeHistory:[history[1]]},onOpen(){}});
assertState(drawingOnly,"unknown");
assert(!drawingOnly.props["aria-label"].includes("绘图完成")&&!drawingOnly.props["aria-label"].includes("通过"),"drawing completion fabricated a candy pass");
const oldSuite=renderComponent("AccountQualityStatus",{snapshot:{lastResult:{...result,score:6,total:6,ruleVersion:"reasoning-six-v1"},lastAttempt:{status:"success"},persisted:true},onOpen(){}});
assertState(oldSuite,"unknown");
assert(!oldSuite.props["aria-label"].includes("糖果题当前状态：通过"),"old six-question measurement became a candy pass");
// Each recorded attempt, rather than each decorative stroke, occupies one bar.
const at=(second)=>"2026-11-06T01:00:"+String(second).padStart(2,"0")+"Z";
const pass={...previous,id:"one-pass",finishedAt:at(1)};
const wrong={...previous,id:"two-wrong",verdict:"degraded",score:0,finishedAt:at(2)};
const fail={id:"three-failed",kind:"candy",status:"failed",httpStatus:503,errorCode:"http_error",finishedAt:at(3)};
const unknown={...fail,id:"four-unknown",httpStatus:200,errorCode:"invalid_response",finishedAt:at(4)};
const recent={...pass,id:"five-pass",finishedAt:at(5)};
const draw={id:"drawing",kind:"pelican",status:"success",finishedAt:at(6)};
function assertRecords(snapshot,ids,outcomes,extra={}){
 const before=JSON.stringify(snapshot),tree=renderComponent("AccountQualityStatus",{snapshot,onOpen(){},...extra});
 const bars=usageClass(tree,"quality-run-bar");
 assert(bars.map(bar=>bar.props["data-probe-id"]).join()===ids.join(),"one record did not correspond to exactly one ordered bar");
 assert(bars.map(bar=>bar.props["data-outcome"]).join()===outcomes.join(),"individual history outcomes were recolored as the latest result");
 const empty=usageClass(tree,"quality-run-placeholder");
 assert(empty.length===5-ids.length&&empty.every(slot=>!slot.props["data-probe-id"]&&!slot.props["data-outcome"]),"empty display slots were counted as completed probes");
 const slots=collectUsageNodes(tree,node=>["quality-run-bar","quality-run-placeholder"].includes(node.props?.className));
 assert(slots.length===5&&slots.slice(0,empty.length).every(slot=>slot.props.className==="quality-run-placeholder")&&slots.slice(empty.length).map(slot=>slot.props["data-probe-id"]).join()===ids.join(),"real probe history was not right-aligned after empty display slots");
 assert(bars.filter(bar=>bar.props["data-latest"]).map(bar=>bar.props["data-probe-id"]).join()===ids.slice(-1).join(),"latest marker did not identify the last actual record");
 assert(bars.every(bar=>bar.props["aria-label"].includes("2026-")&&bar.props["aria-label"].includes("糖果探针")),"per-attempt time or meaning missing");
 assert(JSON.stringify(snapshot)===before,"rendering changed stored records");
 return tree;
}
for(const probeHistory of [[],[draw],[{...previous,total:6,score:6}], [{...pass,status:"queued"}],[{...pass,status:"running"}]])assertRecords({probeHistory},[],[]);
assertRecords({probeHistory:[pass]},[pass.id],["passed"]);
assertRecords({probeHistory:[fail,pass,wrong]},[pass.id,wrong.id,fail.id],["passed","incorrect","failed"]);
assertRecords({probeHistory:[draw,recent,pass,unknown,fail,wrong,pass]},[pass.id,wrong.id,fail.id,unknown.id,recent.id],["passed","incorrect","failed","unknown","passed"]);
for(const state of ["active","queued"]){
 const tree=assertRecords({state,lastAttempt:{status:state==="active"?"running":"queued"},probeHistory:[pass,fail]},[pass.id,fail.id],["passed","failed"]);
 assert(usageClass(tree,"quality-run-activity").length===1,"in-flight attempt was not distinguished from completed history");
}
const unsynced=assertRecords({probeHistory:[pass,fail]},[pass.id,fail.id],["passed","failed"],{stateUncertain:true});
assert(usageClass(unsynced,"quality-run-track")[0].props["data-unconfirmed"]===true&&unsynced.props["aria-label"].includes("状态待同步"),"unconfirmed current state was hidden behind historical passes");
assertRecords({lastResult:result,lastAttempt:{status:"success"},persisted:true,stale:true},["legacy:"+result.checkedAt],["passed"]);
assert(opened===cases.length&&requests.length===0&&timers.size===0,"history details opening invoked a probe, query or retry");
`)
}
