package console

import (
	"fmt"
	"testing"
)

func TestFrontendProbeHTTPUsesOnlyConfirmedObservationWithoutInventingCodes(t *testing.T) {
	vm := frontendBrowserVM(t, "accountHTTP")
	frontendPollingRun(t, vm, `
const base={source:"candy",status:"failed",finishedAt:"2026-11-05T00:00:00Z",model:"selected-model"};
for(const source of ["candy","pelican"])for(const status of [401,402,403,429,503]){
 const observation={...base,source,httpStatus:status,errorCode:status===429?"rate_limited":"http_error"},snapshot={httpObservation:observation},before=JSON.stringify(snapshot),view=Browser.describeProbeHTTPStatus(snapshot);
 assert(view&&view.httpStatus===status&&view.source===source&&view.sourceLabel===(source==="candy"?"糖果探针":"鹈鹕绘图")&&view.title.includes(view.sourceLabel)&&view.label.includes("HTTP "+status),"confirmed HTTP status lost its code or independent probe source");
 assert(view.tone===(status===429?"warning":"danger")&&view.limited===(status===429)&&view.label.includes("限流")===(status===429),"429 was not distinguished from a rejected request");
 assert(!/封号|已失效|授权失效|已欠费|已过期/.test(view.label+view.title+view.message)&&JSON.stringify(snapshot)===before,"HTTP classification invented an account diagnosis or modified the observation");
}
for(const [errorCode,label,tone] of [["forbidden","权限受限","danger"],["unauthorized","授权异常","danger"],["rate_limited","限流","warning"]]){
 const view=Browser.describeProbeHTTPStatus({httpObservation:{...base,errorCode,httpStatus:0}});
 assert(view&&view.httpStatus===0&&view.label===label&&view.tone===tone&&!/HTTP \d/.test(view.title+view.label),"legacy error classification fabricated an HTTP status");
}
for(const observation of [undefined,null,{}, {...base,status:"success",httpStatus:200}, {...base,status:"success",httpStatus:403}, {...base,status:"running",httpStatus:403}, {...base,source:"query",httpStatus:403}, {...base,finishedAt:"invalid",httpStatus:403}, {...base,httpStatus:200}, {...base,httpStatus:999}, {...base,httpStatus:"403",errorCode:"forbidden"}]){
 assert(Browser.describeProbeHTTPStatus({httpObservation:observation,lastAttempt:{status:"failed",errorCode:"forbidden",httpStatus:403}})===null,"successful, malformed or absent observation revived a historical HTTP failure");
}
assert(Browser.httpStatusTone(429,"forbidden")==="warning"&&Browser.httpStatusTone(401,"rate_limited")==="danger"&&Browser.httpStatusTone(0,"rate_limited")==="warning","a stale classification overruled the actual HTTP response");
`)
}

func TestFrontendProbeHTTPUsageKeepsPercentagesResetsAndSourceMeaning(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-05T00:00:00Z"),resetsAt="2026-11-06T01:00:00Z";
const snapshot={lastAttempt:{status:"success"},usage:{windows:[{windowSeconds:18000,usedPercent:0,resetsAt},{windowSeconds:604800,usedPercent:92,resetsAt:"2026-11-08T00:00:00Z"}]}};
let opened=0;
for(const source of ["candy","pelican"])for(const httpStatus of [401,402,403,429]){
 const qualitySnapshot={httpObservation:{source,status:"failed",httpStatus,errorCode:httpStatus===429?"rate_limited":"http_error",finishedAt:"2026-11-05T00:00:00Z",message:"SYNTHETIC_DETAIL_MUST_NOT_ENTER_ROW",model:"synthetic-probe-model"}},before=JSON.stringify({snapshot,qualitySnapshot});
 const tree=renderComponent("AccountInsightSummary",{snapshot,qualitySnapshot,onOpen(){opened++;},now}),status=usageClass(tree,"usage-probe-status")[0],sourceLabel=source==="candy"?"糖果探针":"鹈鹕绘图";
 assert(status&&content(status)==="HTTP "+httpStatus+(httpStatus===429?" 限流":"")&&status.props["data-tone"]===(httpStatus===429?"warning":"danger"),"probe HTTP response was hidden from usage or colored incorrectly");
 assert(status.props.title.includes(sourceLabel)&&status.props["aria-label"].includes(sourceLabel)&&tree.props["aria-label"].includes(sourceLabel)&&tree.props.title.includes(sourceLabel),"probe status claimed to be an unattributed quota query failure");
 assert(usageNodes(tree,"progress").map(node=>node.props.value).join()==="0,92"&&usageClass(tree,"usage-window")[0].props.className.includes("is-normal")&&usageClass(tree,"usage-window")[1].props.className.includes("is-danger"),"HTTP state changed actual usage values or their threshold colors");
 assert(find(usageClass(tree,"usage-next-reset")[0],"time").props.dateTime===resetsAt&&content(usageClass(tree,"usage-next-reset")[0]).match(/\d{2}-\d{2} \d{2}:\d{2}/)&&!content(usageClass(tree,"usage-next-reset")[0]).includes("重置"),"probe status replaced the next reset or restored its redundant title");
 assert(usageNodes(tree,"button").length===1&&!content(tree).includes("SYNTHETIC_DETAIL")&&!JSON.stringify(tree).includes("synthetic-probe-model")&&!/封号|已失效|授权失效/.test(content(tree)),"compact HTTP display exposed detail text, invented account failure or added another action");
 tree.props.onClick();assert(JSON.stringify({snapshot,qualitySnapshot})===before,"HTTP presentation changed quota or probe records");
}
const empty=renderComponent("AccountInsightSummary",{qualitySnapshot:{httpObservation:{source:"pelican",status:"failed",httpStatus:429,errorCode:"rate_limited",finishedAt:"2026-11-05T00:00:00Z"}},onOpen(){},now});
assert(content(empty).includes("HTTP 429 限流")&&!find(empty,"progress")&&!content(empty).includes("0%")&&!find(empty,"time"),"probe-only failure fabricated a quota value or reset timestamp");
assert(opened===8&&requests.length===0&&timers.size===0,"displaying probe HTTP performed a query, inference or retry");
`)
}

func TestFrontendProbeHTTPShowsOnlyLatestRequestAcrossQueryAndProbe(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const observation={source:"candy",status:"failed",httpStatus:403,errorCode:"forbidden",finishedAt:"2026-11-05T01:00:00Z"};
const query={lastAttempt:{status:"partial",startedAt:"2026-11-05T00:00:00Z",finishedAt:"2026-11-05T00:00:10Z"},usage:{windows:[{windowSeconds:18000,usedPercent:24}]},errors:[{component:"usage",code:"authorization_rejected",httpStatus:401,message:"synthetic query rejection"}]};
function renderStatus(snapshot,qualitySnapshot){return renderComponent("AccountInsightSummary",{snapshot,qualitySnapshot,onOpen(){}});}
let tree=renderStatus(query,{httpObservation:observation});
assert(usageClass(tree,"usage-inline-status").map(content).join()==="HTTP 403"&&usageClass(tree,"usage-probe-status").length===1&&!tree.props.title.includes("HTTP 401"),"newer probe did not replace the earlier query status");
tree=renderStatus(query,{httpObservation:{...observation,status:"success",httpStatus:200},lastAttempt:{status:"failed",errorCode:"forbidden",httpStatus:403}});
assert(usageClass(tree,"usage-inline-status").length===0&&!usageClass(tree,"usage-probe-status").length,"latest successful probe revived an older failure");
const newerAttempt={status:"success",startedAt:"2026-11-05T02:00:00Z",finishedAt:"2026-11-05T02:00:10Z"};
tree=renderStatus({...query,lastAttempt:newerAttempt,errors:[]},{httpObservation:observation});
assert(usageClass(tree,"usage-inline-status").length===0,"latest successful query left an older probe error on the account row");
tree=renderStatus({...query,lastAttempt:{...newerAttempt,status:"partial"}},{httpObservation:observation});
assert(usageClass(tree,"usage-inline-status").map(content).join()==="HTTP 401"&&!usageClass(tree,"usage-probe-status").length&&tree.props.title.includes("Codex 额度"),"newer query did not replace the prior probe failure");
for(const status of [403,429]){
 const snapshot={...query,errors:[{component:"usage",code:status===429?"rate_limited":"permission_denied",httpStatus:status,message:"synthetic query failure"}]};
 tree=renderStatus(snapshot,{httpObservation:{...observation,httpStatus:status,errorCode:status===429?"rate_limited":"forbidden"}});
 const statuses=usageClass(tree,"usage-inline-status");
 assert(statuses.length===1&&!statuses[0].props.title.includes("Codex 额度")&&statuses[0].props.title.includes("糖果探针")&&statuses[0].props["data-tone"]===(status===429?"warning":"danger"),"latest status merged an older source or lost its proper color");
 assert(find(tree,"progress").props.value===24,"latest-request selection changed the saved quota");
}
assert(requests.length===0&&timers.size===0,"latest status rendering performed upstream work");
`)
}

func TestFrontendProbeHTTPManualFailureDoesNotReplaceAutomaticCandyScore(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const result={model:"automatic-model",ruleVersion:"candy-v1",score:1,total:1,verdict:"normal",checkedAt:"2026-11-05T00:00:00Z"};
const snapshot={state:"eligible",persisted:true,lastAttempt:{status:"success"},lastResult:result,httpObservation:{source:"pelican",status:"failed",httpStatus:429,errorCode:"rate_limited",finishedAt:"2026-11-05T01:00:00Z"}},before=JSON.stringify(snapshot);
const tree=renderComponent("AccountQualitySummary",{snapshot,onOpen(){}});
const cells=usageClass(tree,"quality-history-cell");
assert(cells.length===1&&cells[0].props["data-tone"]==="success"&&cells[0].props.title.includes("1/1")&&cells[0].props["data-kind"]==="candy"&&!content(tree).includes("限流"),"manual drawing error overwrote the recorded candy result or invented an unrecorded history entry");
assert(JSON.stringify(snapshot)===before&&requests.length===0&&timers.size===0,"manual observation changed the automatic score or triggered another inference");
`)
}

func TestFrontendProbeHTTPJobRowPassesOnlyAvailableQualityObservationToUsage(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
const job={id:"probe-http-row",email:"synthetic@example.invalid",authorizationMethod:"bps",status:"completed",canDownload:true};
const qualitySnapshot={id:job.id,httpObservation:{source:"pelican",status:"failed",httpStatus:403,finishedAt:"2026-11-05T00:00:00Z"}},insightSnapshot={id:job.id};
for(const qualityAvailable of [true,false]){
 hookCursor=0;
 const row=RepairUI.JobRow({job,qualityAvailable,qualitySnapshot,insightSnapshot,onViewInsights(){},onViewQuality(){}}),usage=findMatching(row,node=>typeof node.type==="function"&&node.type.name==="AccountInsightSummary");
 assert(usage&&usage.props.snapshot===insightSnapshot&&usage.props.qualitySnapshot===(qualityAvailable?qualitySnapshot:undefined),"account row dropped the probe observation or exposed it when the feature was unavailable");
}
assert(requests.length===0&&timers.size===0,"propagating probe status invoked account work");
`)
}

func TestFrontendProbeHTTPLimitsUseWarningInQueryDiagnosticsAndProbeDetails(t *testing.T) {
	t.Run("classifications", func(t *testing.T) {
		vm := frontendBrowserVM(t, "accountInsights")
		frontendPollingRun(t, vm, `
for(const [httpStatus,code,tone] of [[429,"upstream_error","warning"],[0,"rate_limited","warning"],[401,"rate_limited","danger"],[403,"permission_denied","danger"]]){
 const error=Browser.describeInsightErrors({errors:[{component:"usage",httpStatus,code,message:"synthetic source response"}]})[0];
 assert(error.tone===tone,"query diagnostic did not preserve warning-only rate limits or the actual rejection status");
}
`)
	})
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed-%v", mixed), func(t *testing.T) {
			vm := frontendInsightErrorVM(t)
			frontendPollingRun(t, vm, frontendUsageTreeQueries)
			frontendPollingRun(t, vm, fmt.Sprintf(`
const mixed=%v,errors=[{component:"usage",httpStatus:429,code:"rate_limited",message:"本次查询受限流，请稍后再试。"}];
if(mixed)errors.push({component:"bps",httpStatus:403,code:"permission_denied",message:"另一来源权限受限。"});
const initialSnapshot={id:errorJob.id,authorizationMethod:errorJob.authorizationMethod,lastAttempt:{status:"partial",startedAt:"2026-11-05T02:00:00Z",finishedAt:"2026-11-05T02:00:10Z"},errors,usage:{windows:[{windowSeconds:18000,usedPercent:37}]}};
const qualitySnapshot={httpObservation:{source:"pelican",status:"failed",httpStatus:429,errorCode:"rate_limited",finishedAt:"2026-11-05T01:02:00Z",model:"manual-selected-model"}};
const tree=renderComponent("AccountInsightsDialog",{job:errorJob,token:"synthetic",initialSnapshot,qualitySnapshot,insights:errorInsights,onClose(){}});
const panel=usageClass(tree,"insight-errors")[0],badges=usageClass(panel,"insight-http-status"),notice=usageClass(tree,"insight-probe-notice")[0];
assert(panel.props["data-tone"]===(mixed?"danger":"warning")&&badges.length===1&&badges[0].props["data-tone"]===(mixed?"danger":"warning")&&content(badges[0]).includes(mixed?"HTTP 403":"HTTP 429"),"latest query response lost its single badge or HTTP-specific tone");
assert(mixed?!content(panel).includes("HTTP 429"):content(badges[0]).includes("限流"),"an earlier query failure remained beside the latest result");
assert(notice&&notice.props.hidden===true&&!usageClass(notice,"probe-failure").length,"older probe failure remained beside the latest query");
assert(!content(notice).includes("manual-selected-model"),"latest query retained detail from an older probe");
assert(find(tree,"progress").props.value===37&&errorMutations===0,"reading separate probe and query failures changed quota or triggered account work");
unmount();assert(timers.size===0,"error detail unmount retained polling deadlines");
`, mixed))
		})
	}
}

func TestFrontendProbeHTTPLatestProbeDetailKeepsWarningAndSafeMetadata(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const initialSnapshot={id:errorJob.id,authorizationMethod:errorJob.authorizationMethod,lastAttempt:{status:"failed",startedAt:"2026-11-05T00:00:00Z",finishedAt:"2026-11-05T00:00:10Z"},errors:[{component:"usage",httpStatus:403,code:"permission_denied",message:"older query rejection"}],usage:{windows:[{windowSeconds:18000,usedPercent:37}]}};
const qualitySnapshot={httpObservation:{source:"pelican",status:"failed",httpStatus:429,errorCode:"rate_limited",finishedAt:"2026-11-05T01:02:00Z",model:"manual-selected-model"}};
const tree=renderComponent("AccountInsightsDialog",{job:errorJob,token:"synthetic",initialSnapshot,qualitySnapshot,insights:errorInsights,onClose(){}}),notice=usageClass(tree,"insight-probe-notice")[0],failure=usageClass(notice,"probe-failure")[0];
assert(!usageClass(tree,"insight-errors").length&&notice.props.hidden===false&&failure.props.className.includes("is-warning")&&!failure.props.className.includes("is-danger")&&content(failure).includes("鹈鹕绘图")&&content(failure).includes("请求限流")&&content(failure).includes("HTTP 429"),"latest probe status was lost, duplicated with an older query, or colored as danger");
assert(content(notice).includes("manual-selected-model")&&content(notice).includes("2026-11-05")&&!usageNodes(notice,"details").length&&!/封号|已失效|授权失效/.test(content(notice)),"probe notice hid source/time/model or invented an account failure");
assert(find(tree,"progress").props.value===37&&errorMutations===0,"reading the latest probe changed quota or triggered account work");
unmount();assert(timers.size===0,"probe detail unmount retained polling deadlines");
`)
}
