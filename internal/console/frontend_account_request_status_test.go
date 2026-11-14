package console

import "testing"

func TestFrontendAccountRequestHistoricalFailuresRemainAttributed(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const at="2026-11-06T00:00:00Z",old={source:"subscription",status:"failed",httpStatus:403,finishedAt:at,historical:true};
let view=Browser.describeAccountRequestStatuses({requestObservations:[old]});
assert(view.badges.length===1&&view.badges[0].label==="上次 HTTP 403"&&view.badges[0].title.includes(at),"historical failure lost its time and scope");
view=Browser.describeAccountRequestStatuses({requestObservations:[old,{...old,source:"models",historical:false,finishedAt:"2026-11-06T00:00:01Z"}]});
assert(view.badges[0].label==="HTTP 403"&&view.badges[0].sources.join()==="models"&&!view.badges[0].title.includes("套餐查询"),"current single status merged an older request source");
`)
}

func TestFrontendAccountRequestStatusesSelectLatestAcrossAllSources(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const queryErrors=[{component:"usage",sourceLabel:"Codex 额度",httpStatus:401,code:"authorization_rejected"},{component:"bps",sourceLabel:"BPS 模型权限",httpStatus:503,code:"upstream_error"}];
const sources=["subscription","models","candy","pelican","authentication","refresh","totp","password","logout"];
const requestObservations=sources.map((source,index)=>({source,status:"failed",httpStatus:index===8?429:503-index,finishedAt:"2026-11-06T00:00:0"+index+"Z",message:"安全说明",rawBody:"DO_NOT_RENDER_RAW",cookie:"DO_NOT_RENDER_COOKIE"}));
requestObservations.reverse();
const before=JSON.stringify({queryErrors,requestObservations}),view=Browser.describeAccountRequestStatuses({queryErrors,requestObservations});
assert(view.badges.length===1&&view.badges[0].label==="HTTP 429 限流"&&view.badges[0].sources.join()==="logout","display selected severity or array position instead of the latest request");
assert(view.requestErrors.length===1&&!view.queryErrors.length&&!view.probeStatus&&view.badges[0].tone==="warning","latest selection retained another current error or changed 429 tone");
assert(!JSON.stringify(view).includes("DO_NOT_RENDER")&&JSON.stringify({queryErrors,requestObservations})===before,"selection exposed unknown fields or mutated input");
`)
}

func TestFrontendAccountRequestStatusesSuccessReplacesEarlierFailures(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const old="2026-11-05T00:00:00Z",now="2026-11-06T00:00:00Z",qualitySnapshot={httpObservation:{source:"candy",status:"failed",httpStatus:403,finishedAt:old}};
let view=Browser.describeAccountRequestStatuses({qualitySnapshot,requestObservations:[{source:"models",status:"failed",httpStatus:503,finishedAt:old},{source:"subscription",status:"success",httpStatus:0,finishedAt:now}]});
assert(!view.badges.length&&!view.requestErrors.length&&!view.probeStatus&&view.latestStatus.status==="success","latest success left an older source's error in the current status slot");
view=Browser.describeAccountRequestStatuses({requestObservations:[{source:"models",status:"success",httpStatus:200,startedAt:now,finishedAt:now},{source:"models",status:"failed",httpStatus:503,startedAt:now,finishedAt:now}]});
assert(view.badges.length===1&&view.badges[0].httpStatus===503,"partial success incorrectly made a failed operation successful");
for(const httpStatus of [100,302,403,503]){
 view=Browser.describeAccountRequestStatuses({qualitySnapshot,requestObservations:[{source:"candy",status:"success",httpStatus,finishedAt:now}]});
 assert(view.probeStatus?.httpStatus===403&&view.badges.length===1,"malformed success cleared a valid result");
}
view=Browser.describeAccountRequestStatuses({querySnapshot:{lastAttempt:{status:"success",startedAt:now,finishedAt:now}},qualitySnapshot});
assert(!view.badges.length,"newer confirmed query success did not replace an old probe status");
view=Browser.describeAccountRequestStatuses({querySnapshot:{lastAttempt:{status:"success",startedAt:now,finishedAt:now}},queryErrors:[{component:"usage",origin:"retained",httpStatus:403}]});
assert(!view.badges.length&&view.latestStatus.status==="success","retained source error overrode a dated successful query");
view=Browser.describeAccountRequestStatuses({querySnapshot:{lastAttempt:{status:"success",startedAt:now,finishedAt:now}},queryErrors:[{component:"usage",origin:"reported",httpStatus:403}]});
assert(view.badges.length===1&&view.badges[0].httpStatus===403,"inconsistent success marker concealed a reported current failure");
`)
}

func TestFrontendAccountRequestNewerLegacyProbePreservesBusinessAndNetworkErrors(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const old="2026-11-06T00:00:00Z",newer="2026-11-06T00:00:00.010Z";
for(const [httpStatus,errorCode,label] of [[200,"invalid_response","响应异常"],[200,"unexpected_content_type","响应异常"],[200,"invalid_stream","响应异常"],[200,"invalid_answer_format","响应异常"],[0,"timeout","请求异常"],[0,"network_error","请求异常"],[302,"redirect_rejected","HTTP 302"]]){
 const observation={source:"candy",status:"failed",httpStatus,errorCode,finishedAt:old,message:"安全探针说明"};
 const qualitySnapshot={httpObservation:{...observation,finishedAt:newer,model:"synthetic-model"}};
 const view=Browser.describeAccountRequestStatuses({qualitySnapshot,requestObservations:[observation]});
 assert(view.badges.length===1&&view.badges[0].label===label&&view.probeStatus?.httpStatus===httpStatus&&view.probeStatus?.message===observation.message&&view.probeStatus?.model==="synthetic-model","newer legacy clock dropped a business/network failure or lost its actual response");
}
`)
}

func TestFrontendAccountRequestStatusesDoNotInventHTTPFailures(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const base={source:"models",status:"failed",finishedAt:"2026-11-06T00:00:00Z"};
for(const [httpStatus,errorCode,label] of [[0,"forbidden","权限受限"],[200,"forbidden","权限受限"],[204,"invalid_response","响应异常"],[302,"redirect_rejected","HTTP 302"],[0,"network_error","请求异常"]]){
 const view=Browser.describeAccountRequestStatuses({requestObservations:[{...base,httpStatus,errorCode,message:"text HTTP 403 is not structured evidence"}]});
 assert(view.badges.length===1&&view.badges[0].label===label&&view.requestErrors[0].httpStatus===httpStatus,"real response status or safe business-error label lost");
 assert(!view.badges[0].title.includes("HTTP 403"),"message or classification fabricated HTTP 403");
}
for(const item of [null,{}, {...base,source:"unknown",httpStatus:403},{...base,httpStatus:"403"},{...base,httpStatus:999},{...base,finishedAt:"invalid"},{...base,status:"running",httpStatus:403},{...base,status:"success",httpStatus:200}]){
 assert(Browser.describeAccountRequestStatuses({requestObservations:[item]}).badges.length===0,"malformed or successful observation became an error badge");
}
`)
}

func TestFrontendAccountRequestUsageShowsOneLatestStatusAndPreservesQuota(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-06T00:00:03Z"),resetsAt="2026-11-07T00:00:00Z",snapshot={lastAttempt:{status:"partial"},usage:{windows:[{windowSeconds:18000,usedPercent:0,resetsAt},{windowSeconds:604800,usedPercent:92,resetsAt}]},errors:[{component:"usage",httpStatus:401,code:"authorization_rejected"},{component:"bps",httpStatus:503,code:"upstream_error"}]};
const requestObservations=[{source:"subscription",status:"failed",httpStatus:503,message:"ROW_MUST_NOT_SHOW_DETAIL",finishedAt:"2026-11-06T00:00:00Z"},{source:"models",status:"failed",httpStatus:403,finishedAt:"2026-11-06T00:00:02Z"}];
const tree=renderComponent("AccountInsightSummary",{snapshot,requestObservations,onOpen(){},now});
assert(usageClass(tree,"usage-inline-status").map(content).join()==="HTTP 403"&&!usageClass(tree,"usage-request-status").length,"usage displays more than the one latest request status");
assert(usageNodes(tree,"progress").map(node=>node.props.value).join()==="0,92"&&find(usageClass(tree,"usage-next-reset")[0],"time").props.dateTime===resetsAt,"latest status changed quota values or reset time");
assert(!tree.props.title.includes("套餐查询")&&tree.props["aria-label"].includes("模型列表")&&!JSON.stringify(tree).includes("ROW_MUST_NOT_SHOW_DETAIL"),"row includes old sources or raw detail");
assert(requests.length===0&&timers.size===0,"status presentation started a request or retry");
`)
}

func TestFrontendAccountRequestJobRowPassesObservationsToUsage(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
const accountRequestObservations=[{source:"subscription",status:"failed",httpStatus:403,finishedAt:"2026-11-06T00:00:00Z"}],job={id:"account-status-row",email:"synthetic@example.invalid",authorizationMethod:"bps",status:"completed",canDownload:true,accountRequestObservations};
hookCursor=0;const row=RepairUI.JobRow({job,onViewInsights(){}}),usage=findMatching(row,node=>typeof node.type==="function"&&node.type.name==="AccountInsightSummary");
assert(usage?.props.requestObservations===accountRequestObservations,"job diagnostics never reach the usage display");
assert(requests.length===0&&timers.size===0,"propagation performed account work");
`)
}

func TestFrontendAccountRequestDetailsShowOnlyLatestSafeResult(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const at="2026-11-06T01:02:00Z",job={...errorJob,accountRequestObservations:[{source:"subscription",status:"failed",httpStatus:503,message:"OLD_RESPONSE_MUST_NOT_SHOW",finishedAt:"2026-11-06T01:00:00Z"},{source:"models",status:"failed",httpStatus:200,errorCode:"forbidden",message:"合成返回允许字段为否",finishedAt:at,rawBody:"DO_NOT_RENDER_RAW"}]};
const initialSnapshot={id:job.id,authorizationMethod:job.authorizationMethod,usage:{windows:[{windowSeconds:18000,usedPercent:37}]}},qualitySnapshot={httpObservation:{source:"candy",status:"failed",httpStatus:401,finishedAt:"2026-11-05T00:00:00Z"}};
const tree=renderComponent("AccountInsightsDialog",{job,token:"synthetic",initialSnapshot,qualitySnapshot,insights:errorInsights,onClose(){}}),panel=usageClass(tree,"insight-account-request-errors")[0];
for(const text of ["模型列表","HTTP 200","合成返回允许字段为否","2026-11-06"])assert(content(panel).includes(text),"latest detail omitted metadata: "+text);
assert(usageClass(panel,"insight-error-card").length===1&&!JSON.stringify(panel).includes("DO_NOT_RENDER_RAW")&&!content(tree).includes("HTTP 401")&&!content(tree).includes("OLD_RESPONSE_MUST_NOT_SHOW"),"detail displayed older failures with the latest result");
assert(find(tree,"progress").props.value===37&&errorMutations===0,"details changed quota or initiated account work");
unmount();assert(timers.size===0,"request details leaked polling");
`)
}

func TestFrontendAccountRequestLatestUsesPreciseTimeAndStableResponseOrder(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const make=(source,httpStatus,startedAt,finishedAt=startedAt)=>({source,status:"failed",httpStatus,startedAt,finishedAt});
let records=[make("models",403,"2026-11-06T00:00:00.000000101Z"),make("subscription",503,"2026-11-06T00:00:00.000000100Z")];
let view=Browser.describeAccountRequestStatuses({requestObservations:records});
assert(view.badges.length===1&&view.badges[0].httpStatus===403,"nanosecond request order was lost to millisecond rounding");
records=[make("models",503,"2026-11-06T00:00:00Z"),make("subscription",429,"2026-11-06T00:00:00Z"),make("models",403,"2026-11-06T00:00:00Z")];
view=Browser.describeAccountRequestStatuses({requestObservations:records});
assert(view.badges[0].httpStatus===403&&view.badges[0].sources.join()==="models","equal-time grouping reordered the last received status");
records=[make("models",503,"2026-11-06T00:00:00Z","2026-11-06T00:01:00Z"),make("subscription",403,"2026-11-06T00:00:10Z","2026-11-06T00:00:11Z")];
view=Browser.describeAccountRequestStatuses({requestObservations:records});
assert(view.badges[0].httpStatus===403,"slow completion of an older request displaced the newer request");
`)
}
