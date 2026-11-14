package console

import "testing"

const frontendPoolReady = `
const poolReady={target:"cpa",revision:1,cpaRevision:1,targets:{sub2api:{configured:true},cpa:{configured:true}},enabled:true,configured:true,running:false,intervalMinutes:5,cooldownMinutes:10,excludedPlanTypes:["free"],lastCheckAt:"2026-11-13T00:00:00Z",lastResult:{checked:3,started:1,excludedPlan:1,ineligible:1}};
const sub2PoolReady={...poolReady,target:"sub2api",baseUrl:"https://synthetic.invalid",groupIds:[],openaiRequestTimezone:"UTC",lastResult:{checked:9,updated:3}};
function replyPool(index,data,status=200){requests[index].resolve({ok:status<400,status,json:async()=>data});}
`

func TestFrontendMonitorTargetSelectionUsesSharedDraftWithoutChangingUpload(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendPoolReady+`
location.hash="#monitor";render();appState.setToken("synthetic-console-token");
appState.setFeatures({cpaUpload:true,sub2apiUpload:true,sub2apiMonitor:true,bulkActions:true});
var monitor=find(render(),"MonitorPage");monitor.props.policy.reload();replyPool(0,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
monitor=find(render(),"MonitorPage");monitor.props.policy.setDraft(current=>({...current,excludedPlanTypes:["free","unknown"],intervalMinutes:"15",cooldownMinutes:"20"}));
monitor=find(render(),"MonitorPage");monitor.props.onTargetChange("cpa");monitor=find(render(),"MonitorPage");
assert(monitor.props.target==="cpa"&&monitor.props.status.target==="sub2api"&&monitor.props.status.lastResult.updated===3,"draft target changed active backend or hid current counters");
assert(monitor.props.policy.draft.excludedPlanTypes.join()==="free,unknown"&&monitor.props.policy.draft.intervalMinutes==="15"&&monitor.props.policy.draft.cooldownMinutes==="20","target selection replaced shared rules");
assert(!findButton(render(),"继续编辑")&&requests.length===1&&!storage.has("tosub2.upload-target-v1"),"selecting draft target prompted discard, wrote backend or changed upload preference");
monitor.props.onCheck();assert(requests.length===1,"unsaved target permitted immediate check");
monitor.props.policy.save();assert(requests[1].url==="/api/pool-monitor"&&JSON.parse(requests[1].options.body).target==="cpa","shared target was not saved together with rules");
replyPool(1,{...poolReady,revision:2,excludedPlanTypes:["free","unknown"],intervalMinutes:15,cooldownMinutes:20});
`)
	frontendPollingRun(t, vm, `
monitor=find(render(),"MonitorPage");assert(monitor.props.status.target==="cpa"&&!monitor.props.policy.dirty,"confirmed target did not become active");
monitor.props.onCheck();assert(requests[2].url==="/api/pool-monitor/check"&&requests[2].options.method==="POST"&&JSON.parse(requests[2].options.body).revision===2,"shared check was not bound to saved target revision");
replyPool(2,{...poolReady,revision:2,excludedPlanTypes:["free","unknown"],intervalMinutes:15,cooldownMinutes:20,lastResult:{checked:8,started:2}});
`)
	frontendPollingRun(t, vm, `
assert(find(render(),"MonitorPage").props.status.lastResult.started===2,"CPA completion was not displayed");
find(render(),"MonitorPage").props.onConfigure();
assert(find(render(),"ConfigurationPage").props.section==="cpa"&&!find(render(),"Sub2ApiSettingsDialog"),"CPA service link opened another service");
`)
}

func TestFrontendMonitorTargetSwitchBackPreservesSharedRulesAndUploadTarget(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendPoolReady+`
location.hash="#monitor";render();appState.setToken("synthetic-console-token");appState.setUploadTarget("cpa");
appState.setFeatures({cpaUpload:true,sub2apiUpload:true,sub2apiMonitor:true,bulkActions:true});
const job={id:"synthetic-monitor",email:"monitor@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setSelectedJobIds(new Set([job.id]));
find(render(),"MonitorPage").props.policy.reload();replyPool(0,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
var monitor=find(render(),"MonitorPage");monitor.props.onTargetChange("cpa");monitor=find(render(),"MonitorPage");monitor.props.onTargetChange("sub2api");
monitor=find(render(),"MonitorPage");assert(monitor.props.target==="sub2api"&&!monitor.props.policy.dirty&&requests.length===1,"switching back did not restore the one unchanged policy");
assert(findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="上传 CPA"),"monitor switch changed independent upload target");
monitor.props.onCheck();assert(requests[1].url==="/api/pool-monitor/check"&&JSON.parse(requests[1].options.body).revision===1,"switching draft back broke shared check");replyPool(1,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
find(render(),"MonitorPage").props.onConfigure();assert(find(render(),"Sub2ApiSettingsDialog"),"Sub2 service link retained CPA editor");
assert(requests.filter(request=>request.options.method==="POST").length===1,"target selection performed an extra mutation");
`)
}

func TestFrontendMonitorCPAPresentationDoesNotInventUploadResults(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, frontendPoolReady+`
var changed="";
const props={target:"cpa",status:{...poolReady,lastResult:{checked:19,started:2,excludedPlan:3,cooldown:4,busy:5,ineligible:6,missing:7,failed:8}},policy:{loaded:true,draft:{target:"cpa",enabled:true,excludedPlanTypes:["free"],intervalMinutes:"5",cooldownMinutes:"10"},setDraft(){},save(){},reload(){}},onTargetChange(value){changed=value;},onConfigure(){},onCheck(){},onRefresh(){},checking:false,available:true};
var tree=RepairUI.MonitorPage(props);
const policyForm=findMatching(tree,node=>node.type==="form"&&node.props["aria-labelledby"]==="monitor-policy-title");
const targetChoice=findMatching(policyForm,node=>node.type==="select"&&node.props["aria-label"]==="巡检对象");
assert(targetChoice?.props.value==="cpa"&&!targetChoice.props.disabled,"shared policy form lost the selected editable inspection target");
assert(!findMatching(tree,node=>node.props?.role==="tab"&&["Sub2API","CPA"].includes(content(node).trim())||node.type==="input"&&node.props.type==="radio"&&node.props.name==="pool-monitor-target"),"monitor target still renders as separate tabs or radio panels");
targetChoice.props.onChange({target:{value:"sub2api"}});assert(changed==="sub2api","target dropdown did not change shared target draft");
assert(!content(tree).includes("已更新账号"),"CPA presented unsupported Sub2 upload count");
assert(content(tree).includes("已排队")&&!content(tree).includes("已上传 2"),"queued repairs presented as completed uploads");
for(const number of [19,2,3,4,5,6,7,8]) assert(findMatching(tree,node=>node.type==="strong"&&content(node)===String(number)),"CPA lost an actual counter: "+number);
tree=RepairUI.MonitorPage({...props,status:{...poolReady,lastCheckAt:null,lastResult:{checked:0,started:0,excludedPlan:0,cooldown:0,busy:0,ineligible:0,missing:0,failed:0}}});
assert(content(tree).includes("暂无巡检记录"),"zero counters became a completed inspection");
assert(!findMatching(tree,node=>node.props?.["aria-label"]==="最近一次巡检统计"),"unexecuted monitor displayed result metrics");
`)
}

func TestFrontendMonitorTargetComesFromServerInsteadOfBrowserPreference(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendPoolReady+`
storage.set("tosub2.monitor-target-v1","cpa");location.hash="#monitor";render();appState.setToken("synthetic");appState.setFeatures({cpaUpload:true,sub2apiMonitor:true});
find(render(),"MonitorPage").props.policy.reload();replyPool(0,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
var monitor=find(render(),"MonitorPage");assert(monitor.props.target==="sub2api","stale browser preference replaced server target");
failStorage=true;monitor.props.onTargetChange("cpa");monitor=find(render(),"MonitorPage");
assert(monitor.props.target==="cpa"&&monitor.props.policy.dirty&&requests.length===1,"unavailable browser storage blocked shared draft edit or caused write");
assert(storage.get("tosub2.monitor-target-v1")==="cpa","draft target modified obsolete browser preference");
`)
}

func TestFrontendMonitorTargetEditKeepsRulesWhilePageNavigationGuardsDraft(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendPoolReady+`
location.hash="#monitor";render();appState.setToken("synthetic");appState.setFeatures({cpaUpload:true,sub2apiMonitor:true});
find(render(),"MonitorPage").props.policy.reload();replyPool(0,sub2PoolReady);
`)
	frontendPollingRun(t, vm, `
var monitor=find(render(),"MonitorPage");monitor.props.policy.setDraft(current=>({...current,excludedPlanTypes:["free","unknown"]}));
monitor=find(render(),"MonitorPage");monitor.props.onTargetChange("cpa");monitor=find(render(),"MonitorPage");
assert(monitor.props.target==="cpa"&&monitor.props.policy.draft.excludedPlanTypes.includes("unknown")&&!findButton(render(),"继续编辑"),"target edit discarded or prompted to discard shared rules");
monitor.props.onConfigure();assert(findButton(render(),"放弃更改并离开"),"service navigation bypassed unsaved shared policy");
findButton(render(),"继续编辑").props.onClick();assert(find(render(),"MonitorPage").props.policy.draft.excludedPlanTypes.includes("unknown"),"continue editing lost shared exclusions");
find(render(),"MonitorPage").props.onConfigure();findButton(render(),"放弃更改并离开").props.onClick();
assert(find(render(),"ConfigurationPage").props.section==="cpa"&&!find(render(),"Sub2ApiSettingsDialog"),"discard lost draft target service destination");
assert(requests.every(request=>!request.options.method||request.options.method==="GET"),"discard/navigation submitted a shared policy");
`)
}

func TestFrontendMonitorCPAConnectionRevisionRefreshesServiceEditor(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup+frontendPoolReady)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `
appState.setFeatures({cpaUpload:true,sub2apiMonitor:true});find(render(),"AppShell").props.onNavigate("monitor");
find(render(),"MonitorPage").props.policy.reload();replyPool(1,{...poolReady,cpaRevision:2});
`)
	frontendPollingRun(t, vm, `
find(render(),"MonitorPage").props.onConfigure();
assert(requests.length===3&&requests[2].url==="/api/cpa/settings"&&!requests[2].options.method,"known newer CPA connection revision did not refresh service editor");respondCPA(2,{...cpaReady,revision:2});
`)
	frontendPollingRun(t, vm, `
cpaState().setDraft(current=>({...current,priority:"8"}));cpaState().save();
assert(requests[3].url==="/api/cpa/settings"&&JSON.parse(requests[3].options.body).revision===2,"service save reused stale CPA connection revision");
`)
}

func TestFrontendMonitorPendingTargetKeepsActualStatusAndSharedTimingControls(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, frontendPoolReady+`
const props={target:"cpa",status:sub2PoolReady,policy:{loaded:true,dirty:true,draft:{target:"cpa",enabled:true,excludedPlanTypes:["free"],intervalMinutes:"19",cooldownMinutes:"27"},setDraft(){},save(){},reload(){}},onTargetChange(){},onConfigure(){},onCheck(){},checking:false,available:true};
var tree=RepairUI.MonitorPage(props);
const liveSummary=findMatching(tree,node=>node.type==="aside"&&node.props.className==="monitor-live-summary"&&node.props["aria-label"]==="当前巡检状态");
const activeTarget=liveSummary&&findMatching(liveSummary,node=>node.props.className==="monitor-active-target");
assert(content(tree).includes("保存后切换至 CPA")&&activeTarget&&content(activeTarget)==="Sub2API","draft target mislabeled the currently active monitor");
assert(content(tree).includes("已更新账号")&&!content(tree).includes("已检查授权文件"),"pending CPA target relabeled actual Sub2 result counters");
for(const [label,value] of [["巡检间隔（分钟）","19"],["重登冷却（分钟）","27"]]) {
 const field=findMatching(tree,node=>node.type==="input"&&node.props["aria-label"]===label);
 assert(field&&field.props.value===value&&!field.props.disabled,"shared editable timing control missing for draft CPA target: "+label);
}
assert(findButton(tree,"立即检查").props.disabled,"pending target exposed enabled immediate check");
tree=RepairUI.MonitorPage({...props,target:"sub2api",policy:{...props.policy,draft:{...props.policy.draft,target:"sub2api"}}});
assert(findMatching(tree,node=>node.type==="input"&&node.props["aria-label"]==="巡检间隔（分钟）").props.value==="19","Sub2 rendered a separate fixed schedule instead of shared draft");
`)
}

func TestFrontendMonitorRepairRequiredEnablesUnchangedRulesSave(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, frontendPoolReady+`
const props={target:"sub2api",status:{...sub2PoolReady,uncertain:true,configurationError:"持久化未确认，请重新保存"},policy:{loaded:true,dirty:false,repairRequired:true,uncertain:false,draft:{target:"sub2api",enabled:true,excludedPlanTypes:["free"],intervalMinutes:"5",cooldownMinutes:"10"},save(){},reload(){}},available:true,checking:false};
var tree=RepairUI.MonitorPage(props);
assert(content(tree).includes("待重新保存")&&content(tree).includes("核对后重新保存以恢复巡检"),"server repair state was labeled already saved");
assert(!findButton(tree,"保存巡检规则").props.disabled,"unchanged verified rules could not be explicitly re-saved");
assert(findButton(tree,"立即检查").props.disabled,"server repair state exposed immediate check");
tree=RepairUI.MonitorPage({...props,status:{...props.status,configurationError:""}});
assert(findButton(tree,"立即检查").props.disabled&&content(tree).includes("配置待核实"),"server uncertainty without error text failed to show paused status");
tree=RepairUI.MonitorPage({...props,status:{...props.status,configured:false,enabled:false,uncertain:false,targets:{sub2api:{configured:false},cpa:{configured:false}}},policy:{...props.policy,draft:{...props.policy.draft,enabled:false}}});
assert(!findButton(tree,"保存巡检规则").props.disabled&&content(tree).includes("待重新保存"),"malformed config safe fallback could not be repaired without configuring a connection");
tree=RepairUI.MonitorPage({...props,policy:{...props.policy,uncertain:true}});
assert(findButton(tree,"保存巡检规则").props.disabled&&content(tree).includes("结果待核实"),"server repair path bypassed local unknown-write readback barrier");
`)
}
