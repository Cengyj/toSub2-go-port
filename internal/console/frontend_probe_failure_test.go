package console

import (
	"fmt"
	"testing"
)

func TestFrontendProbeFailureShowsFullReasonCodeAndHTTPWithoutDisclosure(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-failure.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const fullReason="上游拒绝："+"完整失败信息应直接可见。".repeat(80);
for(const [message,code,httpStatus,title,tone,expected,expectedTitle=title,expectedTone=tone] of [
 [fullReason,"forbidden",403,"检测失败","danger",fullReason],
 ["","rate_limited",429,"检测失败","danger","上游请求受限，请稍后再试。","请求限流","warning"],
 ["","rate_limited",0,"检测失败","danger","上游请求受限，请稍后再试。","请求限流","warning"],
 ["  ","invalid_response",200,"结果未知","warning","未取得符合要求的完整回答，本次没有评分。"],
 ["","unexpected_content_type",200,"结果未知","warning","上游返回的内容类型不符合要求，未取得有效结果。"],
 ["","invalid_stream",200,"结果未知","warning","响应数据流格式异常，未取得完整有效结果。"],
 ["","invalid_answer_format",200,"结果未知","warning","模型答案格式不符合要求，本次没有评分。"],
 [null,"synthetic_unknown_error",0,"检测失败","danger","服务未返回具体原因，请核对账号授权、模型与网络后再试。"],
]){
 const tree=renderComponent("ProbeFailure",{message,code,httpStatus,title,tone});
 assert(tree.props.role==="status"&&tree.props["aria-label"]==="检测错误详情"&&tree.props.className.includes("is-"+expectedTone)&&!(expectedTone==="warning"&&tree.props.className.includes("is-danger")),"failure lost its accessible status or rate limiting gained a danger tone");
 assert(content(usageNodes(tree,"strong")[0])===expectedTitle&&content(usageNodes(tree,"p")[0])===expected&&content(usageNodes(tree,"code")[0])===code,"full reason, result title or error code was hidden or shortened");
 assert(content(tree).includes("响应 HTTP "+httpStatus)===(httpStatus>0),"upstream HTTP status was lost or invented");
 assert(!usageNodes(tree,"details").length&&!usageNodes(tree,"summary").length&&!usageNodes(tree,"button").length&&!findMatching(tree,node=>node.props?.hidden||node.props?.dangerouslySetInnerHTML),"failure reason required disclosure or entered active HTML");
}
for(const httpStatus of [undefined,null,0,99,600,"403",NaN]){
 const tree=renderComponent("ProbeFailure",{code:"timeout",httpStatus});
 assert(!usageClass(tree,"probe-http-status").length&&content(tree).includes("检测超时"),"invalid HTTP status obscured the actual transport failure");
}
assert(requests.length===0&&timers.size===0,"reading a failure scheduled model work");
`)
}

func TestFrontendAccountQualityFailureRemainsInsideResultCardAheadOfRetainedSuccess(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const oldResult={model:"old-success-model",ruleVersion:"candy-v1",verdict:"normal",score:1,total:1,checkedAt:"2026-11-03T00:00:00Z"};
const finishedAt="2026-11-05T12:34:00Z";
let checks=0,cancels=0;
for(const authorizationMethod of ["chatgpt","bps"])for(const retained of [false,true])for(const [errorCode,httpStatus,message,expected,title] of [
 ["forbidden",403,"账号暂不能访问该模型，请核对权限。","账号暂不能访问该模型，请核对权限。","检测失败"],
 ["timeout",0,"","检测超时，未取得完整结果。","检测失败"],
 ["rate_limited",429,"","上游请求受限，请稍后再试。","请求限流"],
 ["invalid_response",200,"","未取得符合要求的完整回答，本次没有评分。","结果未知"],
 ["unexpected_content_type",200,"","上游返回的内容类型不符合要求，未取得有效结果。","结果未知"],
 ["invalid_stream",200,"","响应数据流格式异常，未取得完整有效结果。","结果未知"],
 ["invalid_answer_format",200,"","模型答案格式不符合要求，本次没有评分。","结果未知"],
]){
 const job={id:"failed-probe",email:"failure@synthetic.invalid",authorizationMethod};
 const snapshot={id:job.id,state:"eligible",persisted:true,lastAttempt:{status:"failed",errorCode,httpStatus,message,finishedAt},lastResult:retained?oldResult:undefined};
 const before=JSON.stringify(snapshot);
 const quality={settings:{enabled:true,model:"selected-model",reasoningEffort:"high"},settingsLoaded:true,snapshots:{[job.id]:snapshot},modelsAPI:async()=>({models:[]}),canSubmit(){return false;},cancel(){cancels++;}};
 const dialog=renderComponent("AccountQualityDialog",{job,quality,onClose(){},onCheck(){checks++;}});
 const card=usageClass(dialog,"probe-result-area")[0],failure=usageClass(card,"probe-failure")[0],retainedResult=usageClass(card,"probe-retained-result");
 assert(failure&&usageClass(dialog,"probe-failure").length===1&&content(failure).includes(expected)&&content(failure).includes(errorCode)&&content(usageNodes(failure,"strong")[0])===title,"latest failure was hidden outside the result card or replaced by retained passing evidence");
 assert(content(failure).includes("响应 HTTP "+httpStatus)===(httpStatus>0)&&!usageNodes(card,"details").length,"HTTP response or reason required opening another disclosure");
 assert(!usageClass(card,"probe-result-content").length&&!content(card).includes("选择题目与模型，开始一次检测")&&!content(card).includes("答案符合糖果题判定规则")&&!card.props.className.includes("is-success"),"failed recheck displayed empty guidance or a current passing result");
 if(httpStatus===429)assert(card.props.className.includes("is-warning")&&failure.props.className.includes("is-warning")&&!card.props.className.includes("is-danger")&&!/封号|已失效|授权失效/.test(content(card)),"429 was promoted from rate limiting to an account failure");
 assert(retainedResult.length===(retained?1:0)&&(!retained||content(retainedResult[0]).includes("上次有效测量：1/1")&&content(retainedResult[0]).includes(oldResult.model)),"retained success was discarded or misrepresented as the current attempt");
 const when=usageNodes(usageClass(card,"probe-result-heading")[0],"time");
 assert(when.length===1&&when[0].props.dateTime===finishedAt,"failure was stamped with the retained successful measurement's time");
 assert(JSON.stringify(snapshot)===before&&!findButton(dialog,"取消")&&findButton(dialog,"开始检测").props.disabled,"reading the failure changed state or bypassed eligibility");
}
assert(checks===0&&cancels===0&&requests.length===0&&[...timers.values()].every(timer=>timer.delay===20000),"failure rendering retried inference or cancelled accepted work");
unmount();assert(timers.size===0,"failure detail retained a catalog request deadline");
`)
}

func TestFrontendManualProbeFailureNeverFallsBackToEmptyGuidanceOrOldImage(t *testing.T) {
	cases := []struct {
		name, message, code, expected string
		status                        int
	}{
		{"upstream", "当前模型访问被拒绝。", "forbidden", "当前模型访问被拒绝。", 403},
		{"transport", "", "timeout", "检测超时，未取得完整结果。", 0},
		{"rate-limited", "", "rate_limited", "上游请求受限，请稍后再试。", 429},
		{"unknown", "", "synthetic_unknown_error", "服务未返回具体原因，请核对账号授权、模型与网络后再试。", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/manual-probe.jsx")
			frontendPollingRun(t, vm, frontendUsageTreeQueries)
			frontendPollingRun(t, vm, `
var manualCalls=[],manualActivity=[],manualJob={id:"manual-failure"},manualOptions={model:"selected-model",reasoningEffort:"high"};
var manualQuality={manualSession:{},settings:{enabled:true},settingsLoaded:true,status:{active:0,queued:0},manualAPI(suffix,options){return new Promise(resolve=>manualCalls.push({suffix,options,resolve}));}};
var panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:value=>manualActivity.push(value)});
`)
			frontendPollingRun(t, vm, fmt.Sprintf(`
assert(manualCalls.length===1&&!manualCalls[0].options.method,"opening a failed drawing replayed inference");
manualCalls[0].resolve({id:manualJob.id,task:"pelican-svg",requestId:"failed-request",state:"failed",message:%q,errorCode:%q,httpStatus:%d,svg:'<svg xmlns="http://www.w3.org/2000/svg"/>',finishedAt:"2026-11-05T12:34:00Z"});
`, tc.message, tc.code, tc.status))
			frontendPollingRun(t, vm, fmt.Sprintf(`
panel=renderComponent("ManualProbePanel",{job:manualJob,quality:manualQuality,options:manualOptions,onActivityChange:value=>manualActivity.push(value)});
const card=usageClass(panel,"probe-result-area")[0],canvas=usageClass(card,"manual-probe-canvas")[0],failure=usageClass(canvas,"probe-failure")[0];
assert(failure&&failure.props.role==="status"&&content(failure).includes(%q)&&content(usageNodes(failure,"code")[0])===%q,"manual failure hid its reason or error code outside the drawing result");
assert(content(failure).includes("响应 HTTP "+%d)===(%d>0),"manual failure lost or invented an HTTP response");
if(%d===429)assert(failure.props.className.includes("is-warning")&&!failure.props.className.includes("is-danger")&&content(usageNodes(failure,"strong")[0])==="请求限流"&&content(usageClass(card,"manual-probe-caption")[0]).includes("请求限流"),"manual 429 title, caption or color misrepresented a rate limit as a danger");
assert(!usageClass(canvas,"manual-probe-placeholder").length&&!usageNodes(card,"img").length&&!usageNodes(card,"details").length&&!content(card).includes("点击开始，查看当前模型的绘制效果"),"manual error fell back to an empty state, disclosure or retained image");
assert(!content(panel).includes("1/1")&&!content(panel).includes("通过阈值")&&manualCalls.length===1&&!manualCalls[0].options.method&&manualActivity.at(-1)===false,"reading manual failure changed an upload score, replayed generation or retained an activity lock");
unmount();assert(timers.size===0,"manual failure unmount retained polling deadlines");
`, tc.expected, tc.code, tc.status, tc.status, tc.status))
		})
	}
}
