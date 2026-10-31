package console

import (
	"testing"

	"github.com/grafana/sobek"
)

func frontendInsightErrorVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendInsightsVM(t, "components/account-insights.jsx")
	frontendPollingRun(t, vm, `
function errorNodes(tree,predicate) {
  if (!tree || typeof tree !== "object") return [];
  return (predicate(tree) ? [tree] : []).concat((Array.isArray(tree) ? tree : tree.props?.children || []).flatMap(child => errorNodes(child,predicate)));
}
function errorClass(tree,name) {return errorNodes(tree,node => (node.props?.className || "").split(" ").includes(name));}
var errorJob={id:"synthetic-errors",email:"synthetic-errors@example.invalid",authorizationMethod:"bps",canDownload:true};
var errorMutations=0;
var errorInsights={settings:config,settingsLoaded:true,settingsLoading:false,verificationRequired:false,actionBusy:false,saving:false,revision:0,
  submit:async()=>{errorMutations++;},cancel:async()=>{errorMutations++;},setAutomatic:async()=>{errorMutations++;}};
function errorDialog(snapshot) {return renderComponent("AccountInsightsDialog",{job:errorJob,token:"synthetic",initialSnapshot:{id:errorJob.id,authorizationMethod:errorJob.authorizationMethod,...snapshot},insights:errorInsights,onClose(){}});}
`)
	return vm
}

func TestFrontendInsightErrorsNormalizeOnlySafeDiagnosticFields(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const snapshot={errors:[{component:"usage",code:"authorization_rejected",httpStatus:401,message:"当前授权被查询接口拒绝",upstreamCode:"invalid_api_key",upstreamType:"authentication_error",transportReason:"连接已中断",rawBody:"DO_NOT_RENDER_RAW",accessToken:"DO_NOT_RENDER_TOKEN",upstreamMessage:"DO_NOT_RENDER_FREE_TEXT",requestId:"DO_NOT_RENDER_REQUEST_ID",endpoint:"DO_NOT_RENDER_URL"}]};
const before=JSON.stringify(snapshot), result=Browser.describeInsightErrors(snapshot);
assert(result.length===1&&result[0].sourceLabel==="Codex 额度"&&result[0].httpStatus===401&&result[0].code==="authorization_rejected","public error metadata was omitted");
assert(result[0].upstreamCode==="invalid_api_key"&&result[0].upstreamType==="authentication_error"&&result[0].transportReason==="连接已中断","safe optional diagnostic fields were discarded");
assert(result[0].summary.includes("HTTP 401")&&result[0].summary.includes("连接已中断"),"compact reason omitted HTTP or safe network classification");
const network=Browser.describeInsightErrors({errors:[{component:"usage",code:"network_error",message:"查询网络请求失败（DNS 解析失败）",transportReason:"DNS 解析失败"}]})[0];
assert(network.summary.split("DNS 解析失败").length===2,"summary duplicated a network reason already included in the backend message");
assert(!JSON.stringify(result).includes("DO_NOT_RENDER")&&JSON.stringify(snapshot)===before,"presentation exposed unrecognized response fields or mutated the cache");
const labels=Browser.describeInsightErrors({errors:[{component:"bps"},{component:"subscription"},{component:"resetCredits"},{component:"query"},{component:"future-source"}]}).map(error=>error.sourceLabel);
assert(labels.join()==="BPS 模型权限,账号与订阅,额度重置券,查询任务,其他来源","error sources did not receive readable independent labels");
`)
}

func TestFrontendInsightErrorsRetainedCooldownIsNotANewFailure(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z"), next="2026-10-31T13:00:00Z";
const error={component:"usage",code:"permission_denied",message:"合成额度查询受限",httpStatus:403,retryAfter:"2026-10-31T12:10:00Z"};
const snapshot={lastAttempt:{status:"partial"},errors:[error],components:{usage:{lastError:{...error},nextAllowedAt:next,consecutiveFailures:2}}};
const result=Browser.describeInsightErrors(snapshot,now);
assert(result.length===1&&!result[0].historical&&result[0].originLabel==="来源最近错误","retained component error was duplicated or assigned an unproven current/historical attempt");
assert(result[0].originNote.includes("前次查询")&&result[0].originNote.includes("不代表本次"),"retained cooldown lacked attempt uncertainty");
assert(result[0].cooldownText.includes(Browser.formatInsightDate(next))&&!result[0].cooldownText.includes(Browser.formatInsightDate(error.retryAfter)),"shorter upstream Retry-After bypassed the server component cooldown");
const expired=Browser.describeInsightErrors(snapshot,now+7200000)[0];
assert(expired.cooldownText.includes("冷却时间已过")&&expired.cooldownText.includes("仍需查询确认"),"elapsed cooldown was presented as successful recovery");
const queued=Browser.describeInsightErrors({lastAttempt:{status:"queued"},errors:[error]},now)[0];
assert(queued.historical&&queued.originNote.includes("不代表本次"),"newly queued attempt inherited an old error as its own result");
`)
}

func TestFrontendInsightErrorsAttemptFallbackPreservesUnknownReasons(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const fallback=Browser.describeInsightErrors({lastAttempt:{status:"failed",errorCode:"query_timeout",message:"合成查询超时，旧结果保留"}});
assert(fallback.length===1&&fallback[0].component==="query"&&fallback[0].code==="query_timeout"&&fallback[0].message.includes("旧结果保留"),"attempt-only failure disappeared");
const unknown=Browser.describeInsightErrors({lastAttempt:{status:"failed"},errors:[null,"malformed",[]]});
assert(unknown.length===1&&unknown[0].message.includes("原因未知")&&!unknown[0].httpStatus&&!unknown[0].code,"missing backend reason was invented or left blank");
const independent=Browser.describeInsightErrors({lastAttempt:{status:"failed",errorCode:"query_timeout",message:"合成本次任务超时"},components:{usage:{lastError:{component:"usage",code:"permission_denied",message:"合成前次权限拒绝"}}}});
assert(independent.length===2&&independent.some(error=>error.origin==="attempt"&&error.code==="query_timeout")&&independent.some(error=>error.origin==="retained"),"retained component failure hid a distinct current task error");
for (const status of ["failed","cancelled","interrupted"]) {
  const stopped=Browser.describeInsightErrors({lastAttempt:{status,message:"合成服务重启，未完成查询已中断"},components:{usage:{lastError:{component:"usage",code:"permission_denied",message:"合成前次权限拒绝"}}}});
  assert(stopped.length===2&&stopped[0].origin==="attempt"&&stopped[0].message.includes("服务重启"),"message-only task outcome was hidden behind an older source error");
}

const partial=Browser.describeInsightErrors({lastAttempt:{status:"partial",message:"部分信息已更新，请查看各来源结果"},errors:[{component:"usage",code:"permission_denied",message:"合成来源拒绝"}]});
assert(partial.length===1&&partial[0].component==="usage","generic partial-success notice became a second failure");
const duplicate=Browser.describeInsightErrors({lastAttempt:{status:"failed",errorCode:"authorization_rejected",message:"合成拒绝"},errors:[{component:"usage",code:"authorization_rejected",message:"合成拒绝",httpStatus:401}]});
assert(duplicate.length===1,"task fallback duplicated an existing detailed source error");
assert(Browser.describeInsightErrors({lastAttempt:{status:"success",message:"查询结果已更新"}}).length===0,"successful observation became an error card");
const unfamiliar=Browser.describeInsightErrors({errors:[{component:"usage",code:"future_error",message:"合成未知错误",httpStatus:999,retryAfter:"not-a-date"}]})[0];
assert(!unfamiliar.httpStatus&&unfamiliar.cooldownText.includes("未提供重试时间")&&unfamiliar.hint.includes("暂无该错误码"),"unknown code or malformed metadata invented eligibility advice");
`)
}

func TestFrontendInsightCacheFailureDoesNotInventQueryOrPersistenceOutcome(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const snapshot={stale:true,persisted:false,lastAttempt:{status:"failed",errorCode:"cache_unavailable",message:"历史查询缓存无法读取，原文件已保留；可重新查询恢复"}};
const summary=Browser.describeInsightSummary(snapshot), errors=Browser.describeInsightErrors(snapshot);
assert(summary.label==="历史缓存不可读"&&summary.text.includes("原文件已保留"),"cache read failure was hidden as an unqueried account");
assert(!summary.text.includes("本次查询")&&!summary.text.includes("保存未确认")&&!summary.observedAt,"cache read failure invented a new query or write");
assert(errors.length===1&&errors[0].sourceLabel==="本地查询缓存"&&errors[0].hint.includes("原文件"),"local cache failure became an upstream permission/network error");
`)
}

func TestFrontendInsightErrorsSummaryShowsHTTPAndKeepsReadOnlySingleButton(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
let opens=0;
const snapshot={lastAttempt:{status:"failed"},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:0}]},errors:[{component:"usage",code:"authorization_rejected",httpStatus:401,message:"当前授权被查询接口拒绝"}]};
const tree=renderComponent("AccountInsightSummary",{snapshot,accountLabel:errorJob.email,onOpen(){opens++;}});
for (const label of ["HTTP 401","历史额度","0%"]) assert(content(tree).includes(label),"compact error summary lost its status or observed value: "+label);
assert(!content(tree).includes("当前授权被查询接口拒绝")&&!content(tree).includes("Codex 额度"),"compact row repeated the detailed error source or reason");
assert(tree.props["aria-label"].includes("HTTP 401")&&tree.props["aria-label"].includes("查看用量与状态"),"accessible summary hid the failure status or details entry");
assert(errorNodes(tree,node=>node.type==="button").length===1&&find(tree,"progress").props.value===0,"error entry nested interactive buttons or erased known zero usage");
tree.props.onClick();
assert(opens===1&&requests.length===0&&errorMutations===0&&timers.size===0,"viewing an error started a query, login, or hidden retry");
`)
}

func TestFrontendInsightErrorsLongReasonStaysOutOfCompactTextAndTitles(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const reason="合成很长的查询失败原因".repeat(120)+"<script>synthetic()</script>";
const tree=renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status:"partial"},usage:{windows:[{windowSeconds:18000,usedPercent:null}]},errors:[{component:"usage",code:"upstream_error",httpStatus:402,message:reason}]},onOpen(){}});
assert(errorClass(tree,"usage-error-text").length===0&&!content(tree).includes("部分更新"),"compact row retained the error excerpt or process notice");
assert(!JSON.stringify(tree).includes(reason.slice(0,80)),"detailed error reason leaked into compact text, aria or a hover title");
assert(content(tree).includes("HTTP 402")&&tree.props["aria-label"].includes("查看用量与状态"),"compact status lost its HTTP code or cached details entry");
assert(content(tree).includes("未知")&&!find(tree,"progress"),"error rendering invented unknown usage as zero");
assert(errorNodes(tree,node=>node.type==="script").length===0,"summary treated an error message as executable markup");
`)
}

func TestFrontendInsightErrorsDetailsAreGroupedAfterControlsBeforeObservations(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const tree=errorDialog({lastAttempt:{status:"partial"},errors:[
  {component:"usage",code:"authorization_rejected",httpStatus:401,message:"合成额度读取被拒绝"},
  {component:"bps",code:"upstream_error",httpStatus:402,message:"查询接口返回 HTTP 402，未取得该来源数据"},
  {component:"usage",code:"network_error",message:"合成后续网络错误"},
]});
const dialog=findMatching(tree,node=>node.props?.role==="dialog");
const children=dialog.props.children.flat(Infinity).filter(Boolean);
const errorIndex=children.findIndex(node=>node.props?.className==="insight-errors"), controlsIndex=children.findIndex(node=>node.props?.className==="insight-controls"), observationsIndex=children.findIndex(node=>node.props?.className==="insight-detail-grid");
assert(controlsIndex>=0&&errorIndex>controlsIndex&&errorIndex<observationsIndex,"detail layout lost its controls, directly visible errors, then observed data order");
const panel=children[errorIndex], groups=errorClass(panel,"insight-error-source");
assert(groups.length===2&&content(find(groups[0],"h4"))==="Codex 额度"&&content(find(groups[1],"h4"))==="BPS 模型权限","source errors were not grouped under Chinese labels");
assert(errorClass(groups[0],"insight-error-card").length===2&&errorClass(groups[1],"insight-error-card").length===1,"grouping dropped independent errors from one source");
for (const text of ["HTTP 401","HTTP 402","authorization_rejected","upstream_error","合成额度读取被拒绝","下一步"]) assert(content(panel).includes(text),"top error card lost metadata: "+text);
assert(content(panel).includes("不能确认欠费或过期")&&!content(panel).includes("Token 已过期")&&!content(panel).includes("账号已欠费"),"status code was overinterpreted as credential expiry or billing failure");
assert(!content(tree).includes("各来源查询说明"),"obsolete lower error block duplicated the new cards");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-insights/synthetic-errors"&&(!requests[0].options.method||requests[0].options.method==="GET")&&errorMutations===0,"opening error details triggered a side effect instead of one local cache read: "+JSON.stringify({requests:requests.map(request=>({url:request.url,method:request.options.method})),mutations:errorMutations}));
unmount();
assert(timers.size===0,"error details leaked polling after close");
`)
}

func TestFrontendInsightErrorsDetailsShowLegacyFallbackAndHistoricalCooldown(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const retained={component:"usage",code:"authorization_rejected",httpStatus:401,message:"合成旧额度读取拒绝"};
const tree=errorDialog({lastAttempt:{status:"failed",errorCode:"query_timeout",message:"合成任务超时，历史结果保留"},components:{usage:{lastError:retained,nextAllowedAt:"2099-01-01T00:00:00Z"}}});
const panel=errorClass(tree,"insight-errors")[0];
for (const text of ["来源最近错误","前次查询","不代表本次再次请求","冷却至","查询任务","query_timeout","合成任务超时","旧缓存中未保存的内容无法恢复"]) assert(content(panel).includes(text),"error fallback or retained state lost clarity: "+text);
assert(errorClass(panel,"insight-error-card").length===2&&!content(panel).includes("上游错误码")&&!content(panel).includes("上游类型")&&!content(panel).includes("网络原因"),"old cache invented new diagnostic fields or lost its separate task failure");
unmount();
`)
}

func TestFrontendInsightErrorsDiagnosticsStayTextAndExcludeRawResponseFields(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const text="<img src=x onerror=synthetic()><script>synthetic()</script> & 合成安全文本";
const tree=errorDialog({lastAttempt:{status:"failed"},errors:[{component:"usage",code:"network_error",message:text,upstreamCode:"invalid_api_key",upstreamType:"authentication_error",transportReason:"DNS 解析失败",rawBody:"DO_NOT_RENDER_RAW",accessToken:"DO_NOT_RENDER_TOKEN",upstreamMessage:"DO_NOT_RENDER_FREE_TEXT",requestId:"DO_NOT_RENDER_REQUEST_ID",endpoint:"DO_NOT_RENDER_ENDPOINT"}]});
const panel=errorClass(tree,"insight-errors")[0], message=errorClass(panel,"insight-error-message")[0];
assert(message.props.children.length===1&&message.props.children[0]===text,"message bypassed React text-child escaping");
assert(errorNodes(panel,node=>node.type==="script"||node.type==="img"||node.props?.dangerouslySetInnerHTML||node.props?.innerHTML).length===0,"safe DTO text was rendered as HTML");
for (const label of ["上游错误码","invalid_api_key","上游类型","authentication_error","网络原因","DNS 解析失败"]) assert(content(panel).includes(label),"optional safe diagnostic field was hidden: "+label);
assert(!JSON.stringify(tree).includes("DO_NOT_RENDER"),"raw response or unrecognized diagnostics escaped the display allowlist");
unmount();
`)
}

func TestFrontendInsightErrorsNativeDisclosureKeepsPrimaryReasonsVisible(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const tree=errorDialog({lastAttempt:{status:"partial"},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:41}]},errors:[
  {component:"usage",code:"authorization_rejected",httpStatus:401,message:"合成额度授权读取拒绝",upstreamCode:"invalid_api_key",upstreamType:"authentication_error",retryAfter:"2099-01-01T00:00:00Z"},
  {component:"bps",code:"upstream_error",httpStatus:402,message:"合成BPS来源拒绝",transportReason:"响应读取中断"},
]});
const panel=errorClass(tree,"insight-errors")[0], cards=errorClass(panel,"insight-error-card");
assert(cards.length===2,"disclosure grouping dropped one independent source error");
for(const [index,card] of cards.entries()) {
  const details=find(card,"details"), summary=find(details,"summary");
  assert(details&&details.props.open!==true&&summary&&Number(summary.props.tabIndex)>=0,"diagnostics lost their initially closed native keyboard disclosure");
  assert(details.props.children.flat(Infinity).filter(Boolean)[0]===summary&&summary.props["aria-label"].includes(index?"BPS 模型权限":"Codex 额度"),"native summary is not the first disclosure child or lacks its source-specific accessible label");
  assert(!details.props.onToggle&&!summary.props.onClick&&!summary.props.onKeyDown,"opening safe cached diagnostics attached a hidden request or custom keyboard handler");
  assert(errorClass(card,"insight-error-message").length===1&&errorClass(details,"insight-error-message").length===0&&errorClass(details,"insight-http-status").length===0,"primary reason or HTTP status became hidden inside the collapsed disclosure");
  for(const label of ["错误分类","下一步"]) assert(content(details).includes(label),"expanded diagnosis lost its classification or safe recovery guidance");
}
for(const label of ["HTTP 401","HTTP 402","authorization_rejected","upstream_error","invalid_api_key","authentication_error","响应读取中断","冷却至","不能确认欠费或过期"]) assert(content(panel).includes(label),"disclosure discarded safe source diagnostic metadata: "+label);
assert(find(tree,"progress").props.value===41&&content(tree).includes("历史观察"),"disclosure reset or hid historical numeric data");
assert(errorMutations===0,"opening detail diagnostics submitted work");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-insights/synthetic-errors"&&(!requests[0].options.method||requests[0].options.method==="GET"),"diagnostic presentation added upstream or credential reads");
unmount();assert(timers.size===0,"native disclosure retained cache polling after close");
`)
}

func TestFrontendInsightErrorsMissingReasonRemainsExplicitlyUnknown(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const tree=errorDialog({lastAttempt:{status:"failed"}}), panel=errorClass(tree,"insight-errors")[0];
for (const text of ["原因未知","HTTP 状态未提供","错误码未提供","未提供重试时间"]) assert(content(panel).includes(text),"missing error data was concealed or invented: "+text);
assert(errorMutations===0,"unknown failure triggered recovery automatically");
unmount();
`)
}

func TestFrontendInsightErrorsTransportOnlyDiagnosticsDoNotClaimMissingDetails(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const tree=errorDialog({lastAttempt:{status:"failed"},errors:[{component:"usage",code:"upstream_error",httpStatus:503,message:"合成上游读取失败",transportReason:"响应读取中断"}]});
const panel=errorClass(tree,"insight-errors")[0];
assert(content(panel).includes("HTTP 503")&&content(panel).includes("网络原因")&&content(panel).includes("响应读取中断"),"HTTP response failure lost its safe transport classification");
assert(!content(panel).includes("未包含进一步的安全诊断"),"transport-only diagnostics were simultaneously described as missing");
unmount();
`)
}
