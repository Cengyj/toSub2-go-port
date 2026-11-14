package console

import "testing"

const frontendPoolHook = `
var poolOptions={token:"synthetic",enabled:true,active:false};
function renderPool(){hookCursor=0;const value=Insights.usePoolMonitor(poolOptions);for(const run of effectQueue.splice(0))run();return value;}
var hook=renderPool();
`

func TestFrontendPoolMonitorTargetAndRulesSaveTogether(t *testing.T) {
	for _, target := range []string{"sub2api", "cpa"} {
		t.Run(target, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
			if err := vm.Set("selectedTarget", target); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,sub2PoolReady);`)
			frontendPollingRun(t, vm, `
hook=renderPool();hook.setDraft(current=>({...current,enabled:false,excludedPlanTypes:["unknown","free"],intervalMinutes:"15",cooldownMinutes:"30"}));hook=renderPool();
hook.setDraft(current=>({...current,target:"cpa"}));hook=renderPool();
assert(hook.draft.excludedPlanTypes.join()==="free,unknown"&&hook.draft.intervalMinutes==="15"&&hook.draft.cooldownMinutes==="30","selecting target replaced shared rules");
hook.setDraft(current=>({...current,target:selectedTarget}));hook=renderPool();
assert(requests.length===1&&hook.status.target==="sub2api","target draft changed running target before save");
hook.check();assert(requests.length===1,"unsaved target/rules allowed a check");
assert(hook.dirty,"shared edit was not tracked");hook.save();hook.save();
assert(requests.length===2&&requests[1].url==="/api/pool-monitor"&&requests[1].options.method==="POST","save misrouted or double-submitted");
const body=JSON.parse(requests[1].options.body);
assert(body.enabled===false&&body.target===selectedTarget&&body.excludedPlanTypes.join()==="free,unknown","save omitted target or shared exclusion policy");
assert(Object.keys(body).sort().join()===["cooldownMinutes","enabled","excludedPlanTypes","intervalMinutes","revision","target"].sort().join(),"monitor save carried connection, credentials, or upload parameters");
assert(body.revision===1&&body.intervalMinutes===15&&body.cooldownMinutes===30,"shared save lost current revision or timing");
replyPool(1,{...poolReady,target:selectedTarget,revision:2,enabled:false,excludedPlanTypes:["free","unknown"],intervalMinutes:15,cooldownMinutes:30});
`)
			frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.dirty&&!hook.saving&&hook.status.enabled===false&&hook.status.target===selectedTarget,"confirmed save did not settle shared monitor draft");unmount();`)
		})
	}
}

func TestFrontendPoolMonitorUnknownSaveRequiresExplicitReadWithoutReplay(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,poolReady);`)
	frontendPollingRun(t, vm, `hook=renderPool();hook.setDraft(current=>({...current,enabled:false}));hook=renderPool();var oldSave=hook.save;oldSave();requests[1].reject(new Error("synthetic lost response"));`)
	frontendPollingRun(t, vm, `
oldSave();hook=renderPool();assert(hook.uncertain&&hook.error.includes("待核实")&&requests.length===2,"lost write was retried or uncertainty hidden");
hook.resetDraft();hook.save();hook.check();assert(requests.length===2,"reset or check bypassed uncertain mutation barrier");
hook.reload();assert(requests[2].url==="/api/pool-monitor"&&!requests[2].options.method,"verification was not read-only");replyPool(2,{...poolReady,revision:2,enabled:false});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.uncertain&&!hook.dirty&&!hook.status.enabled&&requests.length===3,"explicit verification retried or retained old policy");
hook.setDraft(current=>({...current,enabled:true}));hook=renderPool();hook.save();assert(JSON.parse(requests[3].options.body).revision===2,"verified current revision was not used");unmount();
`)
}

func TestFrontendPoolMonitorValidationPreventsUnsafeChecksAndMalformedAdoption(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,poolReady);`)
	frontendPollingRun(t, vm, `
hook=renderPool();
for(const [intervalMinutes,cooldownMinutes] of [["0","5"],["1441","5"],["1.5","5"],["1e2","5"],["5","0"],["5","1441"]]){
 hook.setDraft(current=>({...current,intervalMinutes,cooldownMinutes}));hook=renderPool();hook.save();hook.check();hook=renderPool();
 assert(requests.length===1&&hook.error.includes("整数"),"invalid interval wrote policy or allowed check with unsaved changes");
}
hook.reload();replyPool(1,{...poolReady,revision:2,excludedPlanTypes:["invented-plan"]});
`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(hook.status.revision===1&&hook.status.excludedPlanTypes.join()==="free"&&hook.error.includes("响应不完整"),"malformed read silently removed a real policy: "+JSON.stringify({revision:hook.status.revision,excluded:hook.status.excludedPlanTypes,error:hook.error,readError:hook.readError}));unmount();`)
}

func TestFrontendPoolMonitorPollingPreservesDraftAndRejectsStaleResult(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`poolOptions={...poolOptions,active:true};hook=renderPool();`)
	frontendPollingRun(t, vm, `assert(requests.length===1&&requests[0].url==="/api/pool-monitor","visible pool did not load status");replyPool(0,{...poolReady,running:true});`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(hook.status.running,"active check hidden");fire(5000);`)
	frontendPollingRun(t, vm, `replyPool(1,{...poolReady,running:false});`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.status.running,"completed check stayed running");fire(5000);`)
	frontendPollingRun(t, vm, `
assert(requests.length===3,"expected one in-flight passive read");hook.check();assert(JSON.parse(requests[3].options.body).revision===1,"check did not bind to saved shared revision");replyPool(3,{...poolReady,lastResult:{...poolReady.lastResult,started:2}});
`)
	frontendPollingRun(t, vm, `replyPool(2,{...poolReady,running:true,lastResult:{...poolReady.lastResult,started:0}});`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.status.running&&hook.status.lastResult.started===2,"old GET overwrote a newer explicit check receipt");
hook.setDraft(current=>({...current,excludedPlanTypes:["free","unknown"]}));hook=renderPool();fire(5000);
`)
	frontendPollingRun(t, vm, `replyPool(4,{...poolReady,revision:2,excludedPlanTypes:["plus"]});`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.draft.excludedPlanTypes.join()==="free,unknown"&&hook.conflict,"passive policy update erased or silently rebased dirty draft");
hook.save();assert(requests.length===5,"conflicting policy was submitted without readback");
document.hidden=true;fireEvent("document:visibilitychange");assert(timers.size===0,"hidden monitor retained polling timers");
document.hidden=false;fireEvent("document:visibilitychange");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===6&&!requests[5].options.method,"returning page replayed a mutation");replyPool(5,{...poolReady,revision:2});
`)
	frontendPollingRun(t, vm, `
assert(requests.filter(request=>request.options.method==="POST").length===1,"passive polling replayed a check or saved policy");
poolOptions={...poolOptions,active:false};renderPool();unmount();assert(timers.size===0&&[...events.values()].every(listeners=>listeners.size===0),"leaving monitor leaked polling timers or listeners");
`)
}

func TestFrontendPoolMonitorUnknownCheckSurvivesTargetSwitch(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,poolReady);`)
	frontendPollingRun(t, vm, `hook=renderPool();hook.check();requests[1].reject(new Error("synthetic lost check"));`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.uncertain,"check uncertainty missing");hook.setDraft(current=>({...current,target:"sub2api"}));hook=renderPool();
assert(hook.uncertain&&hook.draft.target==="cpa","target edit bypassed unresolved shared mutation");
hook.check();assert(requests.length===2,"returning to target replayed uncertain check");
hook.refresh();replyPool(2,poolReady);
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.uncertain,"passive progress read implicitly verified an unknown mutation");
hook.reload();replyPool(3,poolReady);
`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.uncertain&&requests.filter(request=>request.options.method==="POST").length===1,"explicit verification caused replay");unmount();`)
}

func TestFrontendSub2ServiceSavePreservesSeparatelyManagedMonitorPolicy(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
var dialog=find(render(),"Sub2ApiSettingsDialog");
dialog.props.setSub2apiSettingsDraft(current=>({...current,monitorEnabled:false,excludedPlanTypes:["unknown"],priority:"6"}));
dialog=find(render(),"Sub2ApiSettingsDialog");dialog.props.saveSub2ApiSettings({preventDefault(){}});
const body=JSON.parse(requests[0].options.body);
assert(Number(body.config.priority)===6&&body.config.baseUrl==="https://synthetic.invalid","service save lost real upload configuration");
assert(!("enabled" in body)&&!("excludedPlanTypes" in body)&&!("monitorEnabled" in body.config)&&!("excludedPlanTypes" in body.config),"Sub2 service save overwrote hidden stale monitor rules");
`)
}

func TestFrontendMonitorWritingBlocksNavigationBeforeNextRender(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendPoolReady+`
location.hash="#monitor";render();appState.setToken("synthetic");appState.setFeatures({sub2apiMonitor:true,cpaUpload:true});
find(render(),"MonitorPage").props.policy.reload();replyPool(0,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
find(render(),"MonitorPage").props.policy.setDraft(current=>({...current,enabled:false}));
var tree=render(),monitor=find(tree,"MonitorPage"),navigate=find(tree,"AppShell").props.onNavigate;
monitor.props.policy.save();monitor.props.onTargetChange("cpa");navigate("accounts");
assert(find(render(),"MonitorPage")?.props.target==="sub2api"&&!findButton(render(),"放弃更改并离开"),"retained navigation handler escaped an active monitor save");
assert(requests.length===2&&requests[1].url==="/api/pool-monitor","navigation duplicated a policy write");replyPool(1,{...sub2PoolReady,enabled:false});
`)
	frontendPollingRun(t, vm, `find(render(),"MonitorPage").props.onTargetChange("cpa");assert(find(render(),"MonitorPage").props.target==="cpa","completed monitor write permanently blocked target navigation");`)
}

func TestFrontendPoolMonitorReadPreservesSharedDraftTargetAndRules(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,sub2PoolReady);`)
	frontendPollingRun(t, vm, `
hook=renderPool();hook.setDraft(current=>({...current,target:"cpa",intervalMinutes:"17",excludedPlanTypes:["plus"]}));hook=renderPool();
hook.refresh();assert(renderPool().loading,"explicit monitor read did not enter loading state");
replyPool(1,{...sub2PoolReady,lastResult:{checked:20,updated:7}});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.loading&&hook.status.target==="sub2api"&&hook.status.lastResult.updated===7,"shared progress read failed");
assert(hook.draft.target==="cpa"&&hook.draft.intervalMinutes==="17"&&hook.draft.excludedPlanTypes.join()==="plus"&&!hook.conflict,"progress read erased or conflicted with shared target draft");
hook.resetDraft();hook=renderPool();assert(hook.draft.target==="sub2api"&&hook.draft.intervalMinutes==="5"&&!hook.dirty,"discard did not restore one saved shared configuration");unmount();
`)
}

func TestFrontendPoolMonitorTargetRequiresItsOwnConnectionBeforeEnabling(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,{...sub2PoolReady,targets:{sub2api:{configured:true},cpa:{configured:false}}});`)
	frontendPollingRun(t, vm, `
hook=renderPool();hook.setDraft(current=>({...current,target:"cpa"}));hook=renderPool();hook.save();hook=renderPool();
assert(requests.length===1&&hook.error.includes("所选目标"),"configured Sub2 connection incorrectly authorized an unconfigured CPA target");
hook.setDraft(current=>({...current,enabled:false}));hook=renderPool();hook.save();
assert(requests.length===2&&JSON.parse(requests[1].options.body).target==="cpa"&&JSON.parse(requests[1].options.body).enabled===false,"unconfigured target could not save disabled shared policy");
replyPool(1,{...poolReady,revision:2,enabled:false,configured:false,targets:{sub2api:{configured:true},cpa:{configured:false}}});
`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(hook.draft.target==="cpa"&&hook.draft.excludedPlanTypes.join()==="free"&&hook.draft.cooldownMinutes==="10"&&!hook.dirty,"disabled target save lost shared rules");hook.check();assert(requests.length===2,"disabled target ran a check");unmount();`)
}

func TestFrontendPoolMonitorStaleCheckRevisionRequiresReadWithoutReplay(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,sub2PoolReady);`)
	frontendPollingRun(t, vm, `hook=renderPool();hook.check();replyPool(1,{error:"target updated elsewhere",outcome:"failed",uncertain:false},409);`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.conflict&&!hook.uncertain,"confirmed revision rejection was treated as success or unknown");
hook.setDraft(current=>({...current,target:"cpa"}));hook.check();hook.save();assert(requests.length===2,"stale target check was replayed or allowed policy edit before readback");
hook.reload();replyPool(2,{...poolReady,revision:2});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.conflict&&hook.status.target==="cpa"&&hook.draft.target==="cpa"&&!hook.dirty,"readback failed to adopt concurrently selected target");
hook.check();assert(requests.length===4&&JSON.parse(requests[3].options.body).revision===2,"subsequent user check reused stale revision");unmount();
`)
}

func TestFrontendPoolMonitorUnknownSaveCanRepairServerPauseAfterReadback(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,sub2PoolReady);`)
	frontendPollingRun(t, vm, `
hook=renderPool();hook.setDraft(current=>({...current,excludedPlanTypes:["free","unknown"]}));hook=renderPool();hook.save();
requests[1].reject(new Error("synthetic committed save lost its response"));
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.uncertain,"lost save did not require readback");
hook.save();hook.check();hook.resetDraft();hook.save();assert(requests.length===2,"unverified save was replayed before explicit readback");
hook.reload();replyPool(2,{...sub2PoolReady,revision:2,excludedPlanTypes:["free","unknown"],uncertain:true,configurationError:"配置持久化尚未确认，自动巡检已暂停，请核实后重新保存"});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.uncertain&&hook.repairRequired&&!hook.dirty,"explicit read did not separate client unknown result from server repair requirement");
assert(hook.status.uncertain&&hook.error.includes("重新保存"),"server suspension was hidden after explicit read");
hook.check();assert(requests.length===3,"server-paused monitor allowed immediate check");
hook.save();assert(requests.length===4&&requests[3].url==="/api/pool-monitor","unchanged verified rules could not be explicitly re-saved");
const body=JSON.parse(requests[3].options.body);
assert(body.revision===2&&body.target==="sub2api"&&body.enabled===true&&body.excludedPlanTypes.join()==="free,unknown"&&body.intervalMinutes===5&&body.cooldownMinutes===10,"repair save changed verified rules or reused stale revision");
replyPool(3,{...sub2PoolReady,revision:3,excludedPlanTypes:["free","unknown"],uncertain:false,configurationError:""});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.uncertain&&!hook.repairRequired&&!hook.dirty&&!hook.error&&hook.status.revision===3,"confirmed repair save retained suspension or uncertainty");
assert(requests.filter(request=>request.options.method==="POST").length===2,"readback automatically replayed a save");unmount();
`)
}

func TestFrontendPoolMonitorMalformedStoredPolicyCanResaveDisabledDefaults(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`
hook.reload();replyPool(0,{...sub2PoolReady,revision:0,enabled:false,configured:false,targets:{sub2api:{configured:false},cpa:{configured:false}},excludedPlanTypes:[],intervalMinutes:5,cooldownMinutes:5,uncertain:false,configurationError:"巡检配置文件格式无效，已停止自动巡检，请重新保存配置"});
`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(!hook.uncertain&&hook.repairRequired&&!hook.dirty&&!hook.draft.enabled,"malformed stored config did not expose safe defaults and repair state");
hook.check();assert(requests.length===1,"malformed stored config permitted a check");
hook.save();assert(requests.length===2,"unchanged disabled fallback config could not replace malformed stored policy");
const body=JSON.parse(requests[1].options.body);assert(body.revision===0&&body.target==="sub2api"&&body.enabled===false&&body.excludedPlanTypes.length===0&&body.intervalMinutes===5&&body.cooldownMinutes===5,"repair of malformed file invented rules or required an unrelated connection");
replyPool(1,{...sub2PoolReady,revision:1,enabled:false,configured:false,targets:{sub2api:{configured:false},cpa:{configured:false}},excludedPlanTypes:[],intervalMinutes:5,cooldownMinutes:5,uncertain:false,configurationError:""});
`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.repairRequired&&!hook.dirty&&!hook.error,"confirmed fallback policy save did not clear repair state");unmount();`)
}

func TestFrontendPoolMonitorServerUncertaintyAloneRequiresRepairBeforeCheck(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`hook.reload();replyPool(0,{...sub2PoolReady,uncertain:true,configurationError:""});`)
	frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.repairRequired&&!hook.uncertain&&!hook.dirty,"server uncertainty flag was not retained as repair requirement");
hook.check();assert(requests.length===1,"server uncertainty without an error message permitted an immediate check");
hook.save();assert(requests.length===2&&JSON.parse(requests[1].options.body).revision===1,"server uncertainty without an error message prevented explicit unchanged repair save");
replyPool(1,{...sub2PoolReady,revision:2,uncertain:false,configurationError:""});
`)
	frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.repairRequired&&!hook.error&&!hook.dirty,"confirmed uncertainty repair did not settle");unmount();`)
}
