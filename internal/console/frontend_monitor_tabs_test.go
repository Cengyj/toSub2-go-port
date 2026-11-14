package console

import "testing"

func TestFrontendMonitorTabsKeepSharedDraftWithoutSubmittingWork(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, frontendPoolReady+`
hookSlots=[];
var saves=0,checks=0,configures=0;
var draft={target:"cpa",enabled:true,excludedPlanTypes:["free","unknown"],intervalMinutes:"17",cooldownMinutes:"29"};
const before=JSON.stringify(draft);
const props={target:"cpa",status:sub2PoolReady,available:true,policy:{loaded:true,dirty:true,draft,save(){saves++;},reload(){},setDraft(update){draft=update(draft);}},onCheck(){checks++;},onConfigure(){configures++;},queryOverview:{type:"synthetic-query-overview",props:{children:["synthetic separate query summary"]}}};
function renderMonitor(){hookCursor=0;return RepairUI.MonitorPage({...props,policy:{...props.policy,draft}});}
function panel(tree,name){return findMatching(tree,node=>node.props?.role==="tabpanel"&&node.props.id==="pool-monitor-"+name+"-panel");}
function tab(tree,name){return findMatching(tree,node=>node.props?.role==="tab"&&node.props.id==="pool-monitor-"+name+"-tab");}
var tree=renderMonitor();
assert(tab(tree,"config")?.props["aria-selected"]===true&&tab(tree,"records")?.props["aria-selected"]===false,"monitor did not open on its configuration tab");
assert(panel(tree,"config")?.props.hidden===false&&panel(tree,"records")?.props.hidden===true,"monitor tab selection did not control panel visibility");
for(const name of ["config","records"]){
 assert(tab(tree,name).props["aria-controls"]===panel(tree,name).props.id&&panel(tree,name).props["aria-labelledby"]===tab(tree,name).props.id,"monitor tab lost its accessible panel linkage");
}
tab(tree,"records").props.onClick();tree=renderMonitor();
assert(tab(tree,"records").props["aria-selected"]===true&&panel(tree,"config").props.hidden===true&&!panel(tree,"records").props.hidden,"record tab did not switch the visible panel");
assert(findMatching(panel(tree,"config"),node=>node.type==="form"&&node.props["aria-labelledby"]==="monitor-policy-title"),"switching to records unmounted the configuration form");
assert(findMatching(panel(tree,"records"),node=>node.props?.className?.split(" ").includes("monitor-records-card")),"record tab did not contain the merged result card");
assert(find(panel(tree,"records"),"synthetic-query-overview"),"merged record card lost the query overview slot");
tab(tree,"config").props.onClick();tree=renderMonitor();
const target=findMatching(panel(tree,"config"),node=>node.type==="select"&&node.props["aria-label"]==="巡检对象");
assert(target.props.value==="cpa"&&findMatching(panel(tree,"config"),node=>node.props?.["aria-label"]==="巡检间隔（分钟）").props.value==="17","record navigation replaced the unsaved target or time rule");
assert(JSON.stringify(draft)===before&&saves===0&&checks===0&&configures===0&&requests.length===0,"switching monitor tabs saved rules, checked accounts, or discarded the draft");
assert(content(panel(tree,"records")).includes("已更新账号")&&!content(panel(tree,"records")).includes("已检查授权文件"),"record tab relabeled the saved Sub2 results as the pending CPA target");
`)
}

func TestFrontendMonitorPageOwnsTheSingleEmbeddedQueryOverview(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#monitor";
const tree=render(),monitor=find(tree,"MonitorPage");
assert(monitor,"monitor page missing");
assert(monitor.props.queryOverview?.type==="AccountInsightsMonitor"&&monitor.props.queryOverview.props.embedded===true,"query overview was not embedded in the monitor record card");
assert(!find(tree,"AccountInsightsMonitor"),"query overview remained a separate sibling card outside the tabbed monitor");
assert(requests.length===0,"record card composition started an automatic query or account mutation");
`)
}

func TestFrontendMonitorTabsKeyboardMovesSelectionAndFocusWithoutRequests(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
hookSlots=[];
var focused=[],prevented=0;
function renderTabs(){
 hookCursor=0;
 const tree=RepairUI.MonitorPage({available:true,policy:{loaded:true,draft:{enabled:false,excludedPlanTypes:[],intervalMinutes:"5",cooldownMinutes:"5"}},status:{configured:false,enabled:false}});
 for(const name of ["config","records"]){
  findMatching(tree,node=>node.props?.id==="pool-monitor-"+name+"-tab").props.ref({focus(){focused.push(name);}});
 }
 return tree;
}
function getTab(tree,name){return findMatching(tree,node=>node.props?.id==="pool-monitor-"+name+"-tab");}
var tree=renderTabs();
for(const [from,key,to] of [["config","ArrowRight","records"],["records","ArrowRight","config"],["config","ArrowLeft","records"],["records","Home","config"],["config","End","records"]]){
 getTab(tree,from).props.onKeyDown({key,preventDefault(){prevented++;}});tree=renderTabs();
 assert(getTab(tree,to).props["aria-selected"]===true&&getTab(tree,to).props.tabIndex===0&&focused[focused.length-1]===to,"monitor keyboard navigation did not move both selection and focus: "+key);
 assert(getTab(tree,to==="config"?"records":"config").props.tabIndex===-1,"inactive monitor tab remained in normal tab order");
}
getTab(tree,"records").props.onKeyDown({key:"Tab",preventDefault(){prevented++;}});tree=renderTabs();
assert(prevented===5&&focused.length===5&&getTab(tree,"records").props["aria-selected"]===true,"monitor tabs trapped unrelated keys or changed selection without navigation");
assert(requests.length===0,"keyboard tab navigation submitted monitoring or query work");
`)
}

func TestFrontendMonitorEmbeddedQuerySummaryKeepsIndependentCountsAndWarnings(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, `
var configures=0;
const insights={status:{enabled:true,automatic:false,active:2,queued:3,counts:{success:11,partial:7,failed:5,skipped:4,cancelled:1},lastError:"合成查询服务暂不可读"},statusError:""};
var tree=renderComponent("AccountInsightsMonitor",{embedded:true,insights,onConfigure(){configures++;}});
assert(!findMatching(tree,node=>node.props?.className?.split(" ").includes("section-card")),"embedded query summary retained a second card inside the merged record card");
assert(content(tree).includes("账号查询概览")&&content(tree).includes("合成查询服务暂不可读")&&content(tree).includes("保留已保存的历史结果"),"merging query records hid its label or retained-result warning");
const counts=findMatching(tree,node=>node.props?.["aria-label"]==="最近查询批次统计");
assert(counts,"merged query record lost its independently labelled statistics");
for(const [count,label] of [[11,"已完成"],[7,"部分更新"],[5,"查询失败"],[4,"已跳过"],[1,"已取消"]])assert(findMatching(counts,node=>node.type==="div"&&content(node)===String(count)+label),"query statistics were added to inspection metrics or dropped: "+label);
findButton(tree,"查询配置").props.onClick();assert(configures===1&&requests.length===0,"embedded configuration entry changed behavior or started a query");
tree=renderComponent("AccountInsightsMonitor",{embedded:true,insights:{status:null,statusError:"合成状态读取失败"}});
assert(content(tree).includes("查询状态暂不可读")&&!content(tree).includes("账号查询已关闭"),"merged unread query status became a confirmed disabled state");
`)
}
