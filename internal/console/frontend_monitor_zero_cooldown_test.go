package console

import "testing"

func TestFrontendMonitorZeroCooldownRoundTrip(t *testing.T) {
	for _, target := range []string{"sub2api", "cpa"} {
		t.Run(target, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
			if err := vm.Set("selectedTarget", target); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`
assert(hook.draft.intervalMinutes==="5"&&hook.draft.cooldownMinutes==="5","default timing changed");
hook.reload();replyPool(0,{...poolReady,target:selectedTarget,cooldownMinutes:5});
`)
			frontendPollingRun(t, vm, `
hook=renderPool();hook.setDraft(current=>({...current,cooldownMinutes:"0"}));hook=renderPool();
assert(hook.dirty&&hook.draft.cooldownMinutes==="0","zero was replaced in the draft");
hook.save();hook.save();assert(requests.length===2,"zero cooldown was rejected or save duplicated");
const body=JSON.parse(requests[1].options.body);
assert(body.cooldownMinutes===0&&body.intervalMinutes===5&&body.target===selectedTarget&&body.revision===1,"zero save changed another policy or revision");
replyPool(1,{...poolReady,target:selectedTarget,revision:2,cooldownMinutes:0});
`)
			frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.status.cooldownMinutes===0&&hook.draft.cooldownMinutes==="0"&&!hook.dirty&&!hook.uncertain&&!hook.error,"zero save response was rejected or defaulted");
hook.reload();replyPool(2,{...poolReady,target:selectedTarget,revision:2,cooldownMinutes:0});
`)
			frontendPollingRun(t, vm, `
hook=renderPool();assert(hook.status.cooldownMinutes===0&&hook.draft.cooldownMinutes==="0"&&!hook.error,"zero did not survive explicit readback");
hook.check();assert(requests.length===4&&requests[3].url==="/api/pool-monitor/check"&&JSON.parse(requests[3].options.body).revision===2,"verified zero policy could not run a normal check");
replyPool(3,{...poolReady,target:selectedTarget,revision:2,cooldownMinutes:0});
`)
			frontendPollingRun(t, vm, `hook=renderPool();assert(!hook.error&&!hook.uncertain&&hook.draft.cooldownMinutes==="0","check response lost zero");unmount();`)
		})
	}
}

func TestFrontendMonitorZeroCooldownPreservesTimingValidation(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/poolMonitor.js")
	frontendPollingRun(t, vm, frontendPoolReady+frontendPoolHook+`
assert(Insights.validatePoolMonitor({...poolReady,intervalMinutes:1,cooldownMinutes:0}).cooldownMinutes===0,"zero cooldown response was rejected");
assert(Insights.validatePoolMonitor({...poolReady,intervalMinutes:1440,cooldownMinutes:1440}).intervalMinutes===1440,"upper timing boundary was changed");
for(const [key,values] of [["intervalMinutes",[0,-1,1441,1.5,"1",null,undefined,NaN,Infinity]],["cooldownMinutes",[-1,1441,0.5,"0",null,undefined,NaN,Infinity]]]){
 for(const value of values){let rejected=false;try{Insights.validatePoolMonitor({...poolReady,[key]:value});}catch{rejected=true;}assert(rejected,"malformed timing response accepted: "+key+"="+String(value));}
}
hook.reload();replyPool(0,poolReady);
`)
	frontendPollingRun(t, vm, `
hook=renderPool();
for(const [intervalMinutes,cooldownMinutes] of [["0","0"],["1441","0"],["1.5","0"],["","0"],["5","-1"],["5","1441"],["5","0.5"],["5","1e2"],["5",""]]){
 hook.setDraft(current=>({...current,intervalMinutes,cooldownMinutes}));hook=renderPool();hook.save();hook.check();hook=renderPool();
 assert(requests.length===1&&hook.error.includes("整数"),"invalid timing wrote policy or triggered a check");
}
hook.setDraft(current=>({...current,intervalMinutes:"1440",cooldownMinutes:"1440"}));hook=renderPool();hook.save();
assert(requests.length===2&&JSON.parse(requests[1].options.body).cooldownMinutes===1440,"valid maximum cooldown was rejected");unmount();
`)
}

func TestFrontendMonitorZeroCooldownCPASettingsCompatibility(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/cpaSettings.js")
	frontendPollingRun(t, vm, `
const ready={baseUrl:"https://cpa.synthetic.invalid",configured:true,hasApiKey:true,credentialPersistence:"secure",revision:1};
const defaults=Insights.validateCPASettings(ready);assert(defaults.intervalMinutes===5&&defaults.cooldownMinutes===5,"omitted CPA timing lost historical defaults");
assert(Insights.validateCPASettings({...ready,intervalMinutes:1,cooldownMinutes:0}).cooldownMinutes===0,"CPA zero response was rejected");
for(const [key,values] of [["intervalMinutes",[0,-1,1441,1.5,"1",null,NaN,Infinity]],["cooldownMinutes",[-1,1441,0.5,"0",null,NaN,Infinity]]]){
 for(const value of values){let rejected=false;try{Insights.validateCPASettings({...ready,[key]:value});}catch{rejected=true;}assert(rejected,"malformed CPA timing response accepted: "+key+"="+String(value));}
}
`)
	app := frontendAppVM(t)
	frontendPollingRun(t, app, frontendCPASetup+`cpaState().reload();respondCPA(0,{...cpaReady,intervalMinutes:5,cooldownMinutes:0});`)
	frontendPollingRun(t, app, `
assert(cpaState().loaded&&cpaState().settings.cooldownMinutes===0&&!cpaState().error,"CPA editor rejected saved zero policy");
cpaState().setDraft(current=>({...current,priority:"7"}));cpaState().save();
const body=JSON.parse(requests[1].options.body);assert(body.priority===7&&!("cooldownMinutes" in body)&&!("intervalMinutes" in body),"connection save changed shared timing");
respondCPA(1,{...cpaReady,revision:2,priority:7,intervalMinutes:5,cooldownMinutes:0});
`)
	frontendPollingRun(t, app, `assert(cpaState().settings.cooldownMinutes===0&&!cpaState().error,"CPA connection receipt lost zero cooldown");`)
}

func TestFrontendMonitorZeroCooldownFormPreservesIntervalMinimum(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
var draft={target:"sub2api",enabled:true,excludedPlanTypes:[],intervalMinutes:"5",cooldownMinutes:"5"},saves=0;
const props={target:"sub2api",status:{configured:true,enabled:true},available:true,policy:{loaded:true,setDraft(update){draft=typeof update==="function"?update(draft):update;},save(){saves++;},reload(){}}};
function renderTiming(){hookCursor=0;return RepairUI.MonitorPage({...props,policy:{...props.policy,draft}});}
var tree=renderTiming();
const interval=findMatching(tree,node=>node.type==="input"&&node.props.name==="pool-monitor-interval");
const cooldown=findMatching(tree,node=>node.type==="input"&&node.props.name==="pool-monitor-cooldown");
assert(interval.props.min==="1"&&cooldown.props.min==="0"&&interval.props.max==="1440"&&cooldown.props.max==="1440","form incorrectly coupled interval and cooldown bounds");
assert(interval.props.value==="5"&&cooldown.props.value==="5","form timing defaults changed");
assert(content(tree).includes("0 表示不限制")&&content(tree).includes("1–1440"),"form omitted zero meaning or interval bounds");
cooldown.props.onChange({target:{value:"0"}});tree=renderTiming();
assert(draft.cooldownMinutes==="0"&&findMatching(tree,node=>node.type==="input"&&node.props.name==="pool-monitor-cooldown").props.value==="0"&&saves===0&&requests.length===0,"editing zero coerced it to default or submitted policy");
`)
}
