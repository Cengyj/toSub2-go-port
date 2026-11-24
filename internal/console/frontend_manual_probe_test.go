package console

import "testing"

func TestFrontendManualProbeKeepsPreviousArtworkSeparateUntilMatchingReceipt(t *testing.T) {
	for _, response := range []string{"lost", "other-request", "idle"} {
		t.Run(response, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/manual-probe.jsx")
			if err := vm.Set("responseKind", response); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var calls=[],crypto={randomUUID:()=>"new-attempt"},activity=[];
var old={id:"visual-one",task:"pelican-svg",requestId:"old-attempt",state:"success",svg:'<svg xmlns="http://www.w3.org/2000/svg"/>',finishedAt:"2026-11-24T00:00:00Z"};
var quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},reload(){},manualAPI(suffix,options){return new Promise((resolve,reject)=>calls.push({suffix,options,resolve,reject}));}};
var props={job:{id:old.id},quality,options:{model:"synthetic-model",reasoningEffort:"high"},onActivityChange:value=>activity.push(value)};
function renderManual(){return renderComponent("ManualProbePanel",props);}
renderManual();
`)
			frontendPollingRun(t, vm, `calls[0].resolve(old);`)
			frontendPollingRun(t, vm, `
var start=findButton(renderManual(),"开始手动检测");start.props.onClick();
var tree=renderManual(),canvas=findMatching(tree,node=>node.props?.className?.startsWith("manual-probe-canvas"));
assert(!canvas.props.className.includes("is-success")&&!findMatching(canvas,node=>typeof node.type==="function"&&node.type.name==="PelicanPreview"),"new request still presented the old artwork as its current success");
assert(content(tree).includes("上次作品")&&!content(tree).includes("绘制完成"),"previous artifact lost its historical label while admission was pending");
if(responseKind==="lost")calls[1].reject(new Error("synthetic response lost"));
else calls[1].resolve(responseKind==="idle"?{id:old.id,task:old.task,state:"idle"}:old);
`)
			frontendPollingRun(t, vm, `
tree=renderManual();assert(content(tree).includes("待核实")&&findButton(tree,"开始手动检测").props.disabled,"wrong request receipt settled the new attempt");
var read=calls.at(-1);assert(!read.options.method,"uncertain attempt was automatically replayed");read.resolve(old);
`)
			frontendPollingRun(t, vm, `
tree=renderManual();assert(content(tree).includes("待核实")&&content(tree).includes("上次作品")&&findButton(tree,"开始手动检测").props.disabled&&activity.at(-1)===true,"old successful readback released uncertainty or became this attempt's success");
start.props.onClick();assert(calls.filter(call=>call.options.method==="POST").length===1,"old artwork permitted another billable request");
fire(1500);
`)
			frontendPollingRun(t, vm, `calls.at(-1).resolve({id:old.id,task:old.task,requestId:"new-attempt",state:"running"});`)
			frontendPollingRun(t, vm, `
tree=renderManual();assert(findButton(tree,"正在绘制…")?.props.disabled&&!content(tree).includes("待核实")&&!content(tree).includes("上次作品"),"matching readback did not adopt the accepted attempt");
fire(1500);
`)
			frontendPollingRun(t, vm, `calls.at(-1).resolve({...old,requestId:"new-attempt",finishedAt:"2026-11-24T00:01:00Z"});`)
			frontendPollingRun(t, vm, `
tree=renderManual();assert(content(tree).includes("绘制完成")&&!content(tree).includes("上次作品")&&activity.at(-1)===false,"matching completed output was not shown as current");
assert(calls.filter(call=>call.options.method==="POST").length===1,"status verification generated extra work");unmount();assert(timers.size===0,"manual verification leaked polling");
`)
		})
	}
}

func TestFrontendManualProbeExplicitRefusalRemainsRetryableAfterReadback(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var calls=[],crypto={randomUUID:()=>"new-attempt"};
var old={id:"visual-one",task:"pelican-svg",requestId:"old-attempt",state:"success",svg:'<svg xmlns="http://www.w3.org/2000/svg"/>',finishedAt:"2026-11-24T00:00:00Z"};
var quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},reload(){},manualAPI(suffix,options){return new Promise((resolve,reject)=>calls.push({suffix,options,resolve,reject}));}};
var props={job:{id:old.id},quality,options:{model:"synthetic-model",reasoningEffort:"high"}};
renderComponent("ManualProbePanel",props);
`)
	frontendPollingRun(t, vm, `calls[0].resolve(old);`)
	frontendPollingRun(t, vm, `findButton(renderComponent("ManualProbePanel",props),"开始手动检测").props.onClick();calls[1].reject(Object.assign(new Error("another probe is active"),{status:409,data:{error:"another probe is active"}}));`)
	frontendPollingRun(t, vm, `calls.at(-1).resolve(old);`)
	frontendPollingRun(t, vm, `
var tree=renderComponent("ManualProbePanel",props);assert(!findButton(tree,"开始手动检测").props.disabled&&content(tree).includes("本次请求未受理")&&content(tree).includes("上次作品")&&!content(tree).includes("绘制完成"),"explicit admission refusal became current success or permanent uncertainty");
assert(calls.filter(call=>call.options.method==="POST").length===1,"explicit refusal retried without an operator");unmount();assert(timers.size===0,"refused probe retained timers");
`)
}

func TestFrontendManualProbeScopeChangeImmediatelyHidesPrivateArtwork(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var calls=[],quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise((resolve,reject)=>calls.push({suffix,options,resolve,reject}));}};
var props={job:{id:"scope-one"},quality,options:{model:"synthetic-model",reasoningEffort:"high"}};
renderComponent("ManualProbePanel",props);
`)
	frontendPollingRun(t, vm, `calls[0].resolve({id:props.job.id,task:"pelican-svg",requestId:"prior-request",state:"success",svg:'<svg xmlns="http://www.w3.org/2000/svg"/>'});`)
	frontendPollingRun(t, vm, `
assert(findMatching(renderComponent("ManualProbePanel",props),node=>typeof node.type==="function"&&node.type.name==="PelicanPreview"),"fixture artwork missing");
props={...props,quality:{...quality,manualSession:{}}};
var tree=renderComponent("ManualProbePanel",props);assert(!findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="PelicanPreview"),"first render of a new session exposed the prior artwork before effect cleanup");unmount();assert(timers.size===0,"session change leaked polling");
`)
}

func TestFrontendManualProbeUnknownAdmissionNeverRetriesAndOnlyReadsStatus(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var manualCalls=[],manualReloads=0,crypto={randomUUID:()=>"synthetic-request"};
var manualJob={id:"manual-one",authorizationMethod:"bps"};
var manualOptions={model:"selected-catalog-model",reasoningEffort:"medium"},manualActivity=[],reportManualActivity=value=>manualActivity.push(value);
var manualQuality={manualSession:{},settings:{enabled:true,model:"synthetic-model"},settingsLoaded:true,status:{active:0,queued:0},reload(){manualReloads++;},manualAPI(suffix,options){return new Promise((resolve,reject)=>manualCalls.push({suffix,options,resolve,reject}));}};
var panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
assert(manualCalls.length===0,"mount unexpectedly submitted work synchronously");
`)
	frontendPollingRun(t, vm, `
assert(manualCalls.length===1&&!manualCalls[0].options.method,"opening manual tab did not read only");
manualCalls[0].resolve({id:manualJob.id,task:"pelican-svg",state:"idle"});
`)
	frontendPollingRun(t, vm, `
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
var start=findButton(panel,"开始手动检测");assert(start&&!start.props.disabled,"confirmed idle BPS account cannot start manual task");
start.props.onClick();start.props.onClick();
assert(manualCalls.filter(c=>c.options.method==="POST").length===1,"manual double click submitted twice");
var posted=manualCalls.find(c=>c.options.method==="POST");
assert(JSON.stringify(JSON.parse(posted.options.body))===JSON.stringify({id:manualJob.id,task:"pelican-svg",requestId:"synthetic-request",options:manualOptions}),"manual task sent arbitrary text or another identity");
posted.reject(new Error("synthetic response lost"));
`)
	frontendPollingRun(t, vm, `
start.props.onClick();assert(manualCalls.filter(c=>c.options.method==="POST").length===1,"retained start handler replayed unknown admission before render");
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
assert(findButton(panel,"开始手动检测").props.disabled&&content(panel).includes("待核实"),"unknown admission allowed another potentially billable request");
assert(manualCalls.filter(c=>c.options.method==="POST").length===1,"unknown admission was replayed");
assert(manualActivity.at(-1)===true,"unknown admission unlocked the parent topic/model selectors");
var read=manualCalls.at(-1);assert(!read.options.method,"verification retried a mutation");
read.resolve({id:manualJob.id,task:"pelican-svg",requestId:"synthetic-request",state:"running",model:"synthetic-model"});
`)
	frontendPollingRun(t, vm, `
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
assert(findButton(panel,"正在绘制…").props.disabled&&!findButton(panel,"取消").props.disabled,"readback failed to settle accepted manual task");
unmount();
assert(manualCalls.filter(c=>c.options.method==="POST").length===1&&manualReloads===1,"closing manual panel cancelled or repeated accepted work");
assert(manualActivity.at(-1)===true,"unmount reset accepted manual activity before a confirmed terminal read");
`)
}

func TestFrontendManualProbeRejectsOldAccountActionHandler(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var calls=[],crypto={randomUUID:()=>"synthetic"},quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise((resolve,reject)=>calls.push({resolve,reject,suffix,options}));}};
var props={job:{id:"one"},quality,options:{model:"synthetic-model",reasoningEffort:"high"}};
renderComponent("ManualProbePanel",props);
`)
	frontendPollingRun(t, vm, `calls[0].resolve({id:"one",task:"pelican-svg",state:"idle"});`)
	frontendPollingRun(t, vm, `var start=findButton(renderComponent("ManualProbePanel",props),"开始手动检测");props={...props,job:{id:"two"}};renderComponent("ManualProbePanel",props);start.props.onClick();assert(calls.every(call=>call.options.method!=="POST"),"old account handler started a probe in a new scope");unmount();`)
}

func TestFrontendManualProbeUsesDocumentPreviewAndNeverCreatesAutomaticScore(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var manualOptions={model:"selected-catalog-model",reasoningEffort:"high"},manualActivity=[],reportManualActivity=value=>manualActivity.push(value);
var manualCalls=[],manualJob={id:"manual-result"},manualQuality={manualSession:{},settings:{enabled:true,model:"synthetic-model"},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise(resolve=>manualCalls.push({resolve}));}};
var panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
`)
	frontendPollingRun(t, vm, `manualCalls[0].resolve({id:manualJob.id,task:"pelican-svg",requestId:"synthetic-result",state:"success",svg:'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><circle cx="10" cy="10" r="5"/></svg>',finishedAt:"2026-11-05T00:00:00Z"});`)
	frontendPollingRun(t, vm, `
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:reportManualActivity});
var preview=findMatching(panel,node=>typeof node.type==="function"&&node.type.name==="PelicanPreview");
assert(preview&&preview.props.output.startsWith("<svg")&&!findMatching(panel,node=>node.type==="img"||node.props?.dangerouslySetInnerHTML),"legacy drawing did not use the document preview boundary");
assert(content(panel).includes("不影响糖果题判定和上传策略")&&!content(panel).includes("1/1")&&!content(panel).includes("通过阈值"),"manual output was presented as an automatic passing score");
assert(manualCalls.length===1,"reading a completed drawing started another probe");
assert(manualActivity.at(-1)===false&&!findMatching(panel,n=>n.type==="h3"||n.type==="input"||n.type==="select"),"completed manual result retained activity or duplicated parent controls");unmount();
`)
}

func TestFrontendManualPreviewValidatesHTMLAndPrefersItOverLegacySVG(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var html='<!doctype html><html><body><svg viewBox="0 0 640 480"></svg><script>document.body.dataset.animation="running";</script></body></html>';
var good={id:"animation",task:"pelican-svg",requestId:"animation-request",state:"success",html};
assert(Insights.validateManualProbe(good,"animation").html===html,"complete HTML animation rejected");
for(const changes of [{html:"<html><body>truncated"},{html:"<html><body>"+"鹈".repeat(720000)+"</body></html>"},{html:"not html",svg:'<svg xmlns="http://www.w3.org/2000/svg"/>'}]) {
 let rejected=false;try{Insights.validateManualProbe({...good,...changes},"animation");}catch{rejected=true;}
 assert(rejected,"invalid or oversized HTML bypassed validation through legacy SVG");
}
`)
}

func TestFrontendCandyProbeUsesSingleQuestionVerdictForBothAuthorizations(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
for (const authorizationMethod of ["chatgpt","bps"]) for(const [score,verdict,label] of [[1,"normal","糖果通过"],[0,"degraded","答案不符"]]) {
 const snapshot={state:"eligible",persisted:true,lastAttempt:{status:"success"},lastResult:{model:"synthetic",ruleVersion:"candy-v1",total:1,score,verdict,checkedAt:"2026-11-05T00:00:00Z"}};
 const tree=renderComponent("AccountQualitySummary",{snapshot,authorizationMethod,onOpen(){}});
 const cells=usageClass(tree,"quality-history-cell");
 assert(cells.length===1&&cells[0].props["data-tone"]===(verdict==="normal"?"success":"warning")&&!content(tree)&&!findMatching(tree,n=>n.type==="progress"),"single candy result gained visible text, a redundant score chart, or lost its historical verdict");
 assert(tree.props["aria-label"].includes(score+"/1 题正确"),"accessible description lost the measured result");
}
assert(requests.length===0&&timers.size===0,"rendering candy result invoked inference");
`)
}

func TestFrontendManualProbeRequiresSelectedOptionsWithoutFallingBackToGlobalModel(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var manualCalls=[],manualJob={id:"manual-explicit-options"},manualQuality={manualSession:{},settings:{enabled:true,model:"global-model-must-not-be-used"},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise(resolve=>manualCalls.push({resolve}));}};
var panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality});
`)
	frontendPollingRun(t, vm, `manualCalls[0].resolve({id:manualJob.id,task:"pelican-svg",state:"idle"});`)
	frontendPollingRun(t, vm, `
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality});
assert(findButton(panel,"开始手动检测").props.disabled&&content(panel).includes("请选择模型与推理强度"),"missing selected model silently used global/default options");
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:{model:"catalog-model",reasoningEffort:"high"}});
assert(!findButton(panel,"开始手动检测").props.disabled&&!content(panel).includes("global-model-must-not-be-used"),"explicit catalog selection could not enable manual task");unmount();
`)
}

func TestFrontendManualProbeTimeoutCooldownExpiresAndAcceptsExtendedEfforts(t *testing.T) {
	vm := frontendInsightsVM(t, "components/manual-probe.jsx")
	frontendPollingRun(t, vm, `
var now=Date.parse("2026-11-05T03:07:30Z");Date.now=()=>now;
var calls=[],job={id:"timeout-account"},quality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise(resolve=>calls.push({resolve,options}));}};
var props={job,quality,options:{model:"catalog-model",reasoningEffort:"max"}},tree=renderComponent("ManualProbePanel",props);
`)
	frontendPollingRun(t, vm, `
calls[0].resolve({id:job.id,task:"pelican-svg",requestId:"timeout-request",state:"failed",errorCode:"timeout",nextAllowedAt:"2026-11-05T03:08:12Z"});
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("ManualProbePanel",props);
assert(findButton(tree,"开始手动检测").props.disabled&&content(tree).includes("冷却中，还需 42 秒"),"timeout cooldown did not explain the remaining wait");
now+=42000;fire(1500);
`)
	frontendPollingRun(t, vm, `calls[1].resolve({id:job.id,task:"pelican-svg",requestId:"timeout-request",state:"failed",errorCode:"timeout",nextAllowedAt:"2026-11-05T03:08:12Z"});`)
	frontendPollingRun(t, vm, `
tree=renderComponent("ManualProbePanel",props);
assert(!findButton(tree,"开始手动检测").props.disabled&&!content(tree).includes("冷却中"),"elapsed cooldown kept the manual probe blocked");
props.options={model:"catalog-model",reasoningEffort:"ultra"};tree=renderComponent("ManualProbePanel",props);
assert(!findButton(tree,"开始手动检测").props.disabled,"Ultra effort unexpectedly disabled manual probe");
quality.status={active:0,queued:1};tree=renderComponent("ManualProbePanel",props);
assert(findButton(tree,"开始手动检测").props.disabled&&content(tree).includes("其他探针正在执行或排队"),"queued probe disabled the button without an explanation");
assert(calls.length===2&&calls.every(call=>!call.options.method),"cooldown expiry automatically started a probe");unmount();assert(timers.size===0,"manual probe polling survived unmount");
`)
}
