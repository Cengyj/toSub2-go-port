package console

import "testing"

const frontendCPAMonitorReady = `
const cpaMonitorReady={revision:1,enabled:true,configured:true,running:false,intervalMinutes:5,cooldownMinutes:10,excludedPlanTypes:["free"],lastCheckAt:"2026-11-13T00:00:00Z",lastResult:{checked:3,started:1,excludedPlan:1,ineligible:1}};
`

func TestFrontendCPAServiceSaveDoesNotOverwriteMonitorPolicy(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,{...cpaReady,monitorEnabled:true,excludedPlanTypes:["free"],intervalMinutes:15,cooldownMinutes:30});`)
	frontendPollingRun(t, vm, `
cpaState().setDraft(current=>({...current,priority:"7",weight:"3"}));cpaState().save();
const body=JSON.parse(requests[1].options.body);
assert(body.priority===7&&body.weight===3&&body.baseUrl===cpaReady.baseUrl,"service save lost connection or routing parameters");
for(const key of ["monitorEnabled","enabled","excludedPlanTypes","intervalMinutes","cooldownMinutes"]) assert(!(key in body),"service save overwrote separately managed monitor policy: "+key);
respondCPA(1,{...cpaReady,monitorEnabled:true,excludedPlanTypes:["free"],intervalMinutes:15,cooldownMinutes:30,priority:7,weight:3,revision:2});
`)
	frontendPollingRun(t, vm, `
cpaState().setDraft(current=>({...current,baseUrl:""}));cpaState().save();
assert(JSON.parse(requests[2].options.body).baseUrl===""&&!Object.hasOwn(JSON.parse(requests[2].options.body),"monitorEnabled"),"connection removal resubmitted a stale monitor policy");
`)
}

func TestFrontendCPAServiceFormContainsOnlyConnectionAndUploadSettings(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "cpa-settings")
	frontendPollingRun(t, vm, frontendCPAMonitorReady+`
const cpa={loaded:true,configured:true,draft:{baseUrl:"https://cpa.synthetic.invalid",adminApiKey:"",priority:"3",weight:"2",monitorEnabled:true,excludedPlanTypes:["free"],intervalMinutes:"5",cooldownMinutes:"10"},settings:{monitorEnabled:true},monitor:cpaMonitorReady,save(){},reload(){},setDraft(){}};
const tree=RepairUI.CPASettings({cpa});
for(const text of ["服务连接","管理地址","管理密钥","优先级","权重"]) assert(content(tree).includes(text),"service editor lost upload field: "+text);
for(const text of ["异常重登与上传","自动重登排除套餐","监控状态","巡检间隔","重登冷却","检查并重登异常账号","读取监控状态"]) assert(!content(tree).includes(text),"CPA service editor retained monitoring content: "+text);
assert(!findMatching(tree,node=>node.type==="input"&&node.props.name==="cpa-monitor-enabled"),"CPA monitor switch duplicated outside pool monitor");
`)
}

func TestFrontendCPAConfigurationOverviewRemovesOnlyTrashSummary(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
location.hash="#settings";render();appState.setFeatures({recycleBin:true,cpaUpload:true});
var changed="";
const tree=RepairUI.ConfigurationPage({...find(render(),"ConfigurationPage").props,section:"overview",trashAvailable:true,onSectionChange(value){changed=value;}});
assert(!content(tree).includes("回收站保留策略")&&!findButton(tree,"管理保留策略"),"overview retained unwanted trash policy card");
const tab=findButton(tree,"回收站");assert(tab,"trash configuration tab was removed");tab.props.onClick();assert(changed==="trash","trash configuration tab was disconnected");
`)
}
