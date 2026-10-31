package console

import "testing"

func TestFrontendUsagePresentationCompactTimeKeepsSourceAndFullTimestamp(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const old="2026-10-30T12:34:00Z", current="2026-10-31T12:56:00Z";
for(const [snapshot,label,expected] of [
  [{lastAttempt:{status:"partial",startedAt:current},usage:{observedAt:old,windows:[{windowSeconds:18000,usedPercent:27}]},bps:{observedAt:current,allowed:true},subscription:{observedAt:current}},"额度观察",old],
  [{lastAttempt:{status:"partial",startedAt:current},bps:{observedAt:old,allowed:true},subscription:{observedAt:current}},"权限观察",old],
  [{lastAttempt:{status:"success",startedAt:current},subscription:{observedAt:current}},"订阅观察",current],
]) {
  const before=JSON.stringify(snapshot), tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  const observed=usageClass(tree,"usage-observed")[0], time=find(observed,"time");
  assert(observed&&content(observed).includes(label)&&time?.props.dateTime===expected,"another successful source replaced the displayed observation timestamp or its identity");
  assert(/^\d{2}-\d{2} \d{2}:\d{2}$/.test(content(time)),"compact timestamp no longer uses the readable month-day hour-minute display");
  const full="2026-"+content(time);
  assert(observed.props.title===label+"："+full&&tree.props["aria-label"].includes(full)&&tree.props.title.includes(full),"compact date removed the year from full hover or accessible details");
  assert(JSON.stringify(snapshot)===before,"observation presentation rewrote another source or its date");
}
const normal=renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status:"success",startedAt:current},usage:{observedAt:current,windows:[{windowSeconds:18000,usedPercent:12,resetsAt:"2099-01-01T00:00:00Z"}]}},onOpen(){}});
assert(usageClass(normal,"usage-caption").length===0&&find(normal,"time")&&normal.props["aria-label"].includes("查看用量与状态"),"normal readings retained redundant state rows or lost the read-only details entry");
assert(requests.length===0&&timers.size===0,"compact source time started new data collection");
`)
}

func TestFrontendUsagePresentationBPSCardFollowsMethodOrExistingObservation(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-insight-details.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z"), job={id:"synthetic-detail",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",canDownload:true};
const observedAt="2026-10-31T12:00:00Z";
const chatgpt=renderComponent("InsightDataSections",{job,snapshot:{usage:{observedAt,windows:[{windowSeconds:18000,usedPercent:45}]},lastAttempt:{status:"success",startedAt:observedAt}},now});
assert(usageClass(chatgpt,"insight-bps-block").length===0&&!content(chatgpt).includes("尚未取得 BPS 权限信息"),"ChatGPT detail displayed an irrelevant empty BPS permission card");
const sections=chatgpt.props.children.flat(Infinity).filter(Boolean), secondary=usageClass(chatgpt,"insight-secondary-grid")[0];
assert(sections[0]===usageClass(chatgpt,"insight-usage-block")[0]&&sections[1]===secondary&&sections[2]===usageClass(chatgpt,"insight-remote-block")[0],"detail layout did not prioritize quota before supporting observations and independent Sub2 status");
assert(usageClass(secondary,"insight-block").length===2,"ChatGPT supporting cards retained an irrelevant BPS grid slot");
assert(usageNodes(chatgpt,"progress").length===1&&find(chatgpt,"progress").props.value===45,"conditional BPS layout changed valid Codex usage");
for(const [method,bps,expected] of [["bps",undefined,"尚未取得 BPS 权限信息"],["chatgpt",{observedAt,allowed:true},"权限接口允许访问"],["bps",{observedAt,allowed:false},"权限接口报告受限"]]) {
  const snapshot={bps,lastAttempt:{status:"partial",startedAt:observedAt}}, before=JSON.stringify(snapshot);
  const tree=renderComponent("InsightDataSections",{job:{...job,authorizationMethod:method},snapshot,now}), blocks=usageClass(tree,"insight-bps-block");
  assert(blocks.length===1&&content(blocks[0]).includes(expected)&&content(blocks[0]).includes("独立来源"),"selected BPS method or existing BPS data lost its independent permission card");
  assert(usageClass(usageClass(tree,"insight-secondary-grid")[0],"insight-block").length===3,"BPS supporting layout dropped credits, permission or subscription information");
  assert(!find(tree,"progress")&&!content(tree).includes("0%")&&!content(tree).includes("100%")&&content(tree).includes("暂无可用的额度结果"),"BPS permission-only data invented a numeric usage reading");
  assert(JSON.stringify(snapshot)===before,"conditional detail view mutated a saved BPS observation");
}
assert(requests.length===0&&timers.size===0,"conditional source cards triggered a query or recovery side effect");
`)
}

func TestFrontendUsagePresentationOldResetCreditsRemainIndependentlyHistorical(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const old="2026-10-30T12:00:00Z", current="2026-10-31T12:00:00Z";
const snapshot={lastAttempt:{status:"success",startedAt:current},usage:{observedAt:current,windows:[{windowSeconds:18000,usedPercent:12}],resetCredits:{observedAt:old,availableCount:2}},bps:{observedAt:current,allowed:true},subscription:{observedAt:current},errors:[]};
const before=JSON.stringify(snapshot);
assert(Browser.insightSourceIsHistorical(snapshot,"resetCredits"),"turning off credit detail silently refreshed retained reset-credit observations");
for(const source of ["usage","bps","subscription"]) assert(!Browser.insightSourceIsHistorical(snapshot,source),"old optional credits contaminated a refreshed independent source: "+source);
const refreshed={...snapshot,usage:{...snapshot.usage,resetCredits:{...snapshot.usage.resetCredits,observedAt:current}}};
assert(!Browser.insightSourceIsHistorical(refreshed,"resetCredits"),"fresh reset-credit observation remained historical");
assert(JSON.stringify(snapshot)===before,"source freshness projection modified saved data");
`)
}

func TestFrontendUsagePresentationExhaustionUsesObservationFreshness(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z");
const current="2026-10-31T12:00:00Z", old="2026-10-30T12:00:00Z";
const base={lastAttempt:{status:"success",startedAt:current},usage:{observedAt:current,limitReached:true,windows:[{windowSeconds:18000,usedPercent:100,resetsAt:"2026-10-31T13:00:00Z"}]}};
for(const snapshot of [
  {...base,stale:true},
  {...base,usage:{...base.usage,observedAt:old}},
  {...base,usage:{...base.usage,windows:[{...base.usage.windows[0],resetsAt:"2026-10-31T11:00:00Z"}]}},
]) {
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  assert(content(tree).includes("上次已用尽")&&usageClass(tree,"usage-inline-status").every(node=>content(node)!=="额度已用尽"),"historical or reset-passed quota falsely asserted a current exhausted state");
  assert(find(tree,"progress").props.value===100&&content(tree).includes("100%"),"uncertain exhaustion invented new quota capacity");
}
const fresh=renderComponent("AccountInsightSummary",{snapshot:base,onOpen(){},now});
assert(usageClass(fresh,"usage-inline-status").some(node=>content(node)==="额度已用尽")&&!content(fresh).includes("上次已用尽")&&find(fresh,"progress").props.value===100,"fresh explicit upstream exhaustion lost its current meaning");
assert(requests.length===0&&timers.size===0,"freshness rendering attempted a quota refresh");
`)
}

// These tests exercise the production presentation through the Go/Sobek
// boundary. CSS layout, native keyboard focus and responsive sizing still
// require the separate browser fixture verification.
func TestFrontendUsagePresentationMeterVariantsKeepNumericAndColorBoundaries(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-usage.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z");
for (const [value,tone,barValue] of [[-8,"normal",0],[0,"normal",0],[74.99,"normal",74.99],[75,"warning",75],[89.99,"warning",89.99],[90,"danger",90],[100,"danger",100],[125,"danger",100]]) {
  const window=Object.freeze({name:"secondary",windowSeconds:604800,usedPercent:value,resetsAt:"2026-11-01T12:00:00Z"});
  const variants=[true,false].map(compact=>renderComponent("UsageMeter",{window,now,compact}));
  for(const tree of variants) {
    const meters=usageClass(tree,"usage-window"), bars=usageNodes(tree,"progress");
    assert(meters.length===1&&meters[0].props.className.split(" ").includes("is-"+tone),"compact/detail meter disagree at the usage warning threshold: "+value);
    assert(bars.length===1&&Number(bars[0].props.max)===100&&bars[0].props.value===barValue,"meter did not visually bound a reported value: "+value);
    assert(content(tree).includes(value+"%")&&bars[0].props["aria-label"].includes("7 天窗口已用 "+value+"%"),"clamping or visual redesign changed the reported value or window identity");
    assert(!content(tree).includes("额度已恢复")&&!content(tree).includes("授权失效"),"usage color implied an authentication or recovery outcome");
  }
  assert(content(usageClass(variants[0],"usage-window-label")[0])==="7d"&&content(variants[1]).includes("7 天窗口"),"compact and detail labels do not describe the same reported duration");
}
assert(requests.length===0&&timers.size===0,"pure meter render started background work");
`)
}

func TestFrontendUsagePresentationMeterVariantsKeepUnknownAndElapsedReset(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-usage.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z");
for(const value of [null,undefined,"0",false,NaN,Infinity]) {
  for(const compact of [true,false]) {
    const window=Object.freeze({windowSeconds:18000,usedPercent:value,resetsAt:"2026-10-31T11:00:00Z"});
    const tree=renderComponent("UsageMeter",{window,now,compact});
    assert(!find(tree,"progress")&&content(tree).includes("未知")&&!content(tree).includes("0%"),"unknown usage was rendered as empty capacity");
    assert(usageClass(tree,"usage-track-unknown").length===1,"unknown usage lost its distinct visual placeholder");
    assert(content(tree).includes(compact ? "待确认" : "待更新确认")&&!content(tree).includes("已恢复"),"elapsed reset asserted fresh capacity without another observation");
  }
}
for(const compact of [true,false]) {
  const tree=renderComponent("UsageMeter",{window:Object.freeze({windowSeconds:18000,usedPercent:93,resetsAt:"2026-10-31T11:00:00Z"}),now,compact});
  assert(find(tree,"progress").props.value===93&&content(tree).includes("93%")&&content(tree).includes(compact ? "待确认" : "待更新确认"),"elapsed reset erased a known historical reading");
}
assert(requests.length===0&&timers.size===0,"reset formatting created a hidden query or timer");
`)
}

func TestFrontendUsagePresentationSummaryAndDetailUseTheSameReportedMeter(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z");
const quotaWindow=Object.freeze({name:"primary",windowSeconds:604800,usedPercent:92,resetsAt:"2026-10-31T11:00:00Z"});
const snapshot=Object.freeze({lastAttempt:{status:"partial",startedAt:"2026-10-31T12:00:00Z"},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[quotaWindow]},bps:{observedAt:"2026-10-31T12:00:01Z",allowed:true},errors:[{component:"usage",code:"permission_denied",httpStatus:403,message:"合成额度来源读取受限"}]});
const before=JSON.stringify(snapshot);
const summary=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
const detail=renderComponent("InsightWindow",{window:quotaWindow,now});
for(const tree of [summary,detail]) {
  const bars=usageNodes(tree,"progress");
  assert(bars.length===1&&bars[0].props.value===92&&bars[0].props["aria-label"]==="7 天窗口已用 92%","summary and detail disagree about the same observation");
  assert(usageClass(tree,"usage-window")[0].props.className.includes("is-danger")&&content(tree).includes("预计已重置")&&/待(?:更新)?确认/.test(content(tree)),"detail lost the shared threshold or unconfirmed reset state");
}
assert(content(summary).includes("历史额度")&&!content(summary).includes("已更新")&&summary.props["aria-label"].includes("BPS 模型权限已更新；Codex 额度未更新")&&content(summary).includes("HTTP 403"),"fresh BPS permission hid retained usage or its failed source, or compact view repeated the source process notice");
assert(JSON.stringify(snapshot)===before&&requests.length===0&&timers.size===0,"shared presentation mutated observations or started work");
`)
}

func TestFrontendUsagePresentationLongWindowsRetainOrderAndAccessibleDetails(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const custom="合成自定义额度窗口标识".repeat(20);
const windows=[{name:custom,usedPercent:18.5},{windowSeconds:86400,usedPercent:null},{windowSeconds:18000,usedPercent:99},{windowSeconds:604800,usedPercent:0}];
const tree=renderComponent("AccountInsightSummary",{snapshot:{usage:{windows}},onOpen(){},accountLabel:"synthetic.long.email@example.invalid"});
assert(usageClass(tree,"usage-window").length===2&&content(tree).includes("另有 2 个窗口"),"compact layout hid omitted windows or expanded every window into the row");
assert(usageClass(tree,"usage-window-label").map(content).join("|")===custom+"|1d","display changed reported order or dropped a custom label");
assert(tree.props["aria-label"].includes(custom)&&tree.props["aria-label"].includes("18.5%")&&tree.props["aria-label"].includes("synthetic.long.email@example.invalid"),"visual truncation removed the full accessible account or window identity");
assert(usageNodes(tree,"progress").length===1&&find(tree,"progress").props.value===18.5&&content(tree).includes("未知"),"mixed known and unknown windows were collapsed or reordered");
assert(requests.length===0&&timers.size===0,"overflow summary started a background read");
`)
}

func TestFrontendUsagePresentationSummaryRemainsOneReadOnlyKeyboardAction(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
let opens=0;
for(const status of ["never","queued","running","partial","failed","cancelled"]) {
  const tree=renderComponent("AccountInsightSummary",{snapshot:{persisted:false,lastAttempt:{status},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:0}]},errors:[{component:"usage",code:"upstream_error",httpStatus:402,message:"合成接口拒绝读取"}]},onOpen(){opens++;},accountLabel:"synthetic@example.invalid"});
  assert(tree.type==="button"&&tree.props.type==="button"&&!tree.props.disabled,"state styling blocked the keyboard-accessible read-only details action");
  assert(usageNodes(tree,"button").length===1&&usageNodes(tree,"a").length===0&&usageNodes(tree,"input").length===0,"summary introduced nested interactive controls");
  assert(content(tree).includes("HTTP 402")&&tree.props["aria-label"].includes("查看用量与状态"),"compact design hid its HTTP status or read-only details purpose");
  tree.props.onClick();
}
assert(opens===6&&requests.length===0&&timers.size===0,"opening cached details submitted or retried an upstream query");
`)
}

func TestFrontendUsagePresentationHTTPStaysBesideResetOncePerAccount(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const errors=[
  {component:"subscription",code:"network_error",message:"合成详细错误不进入列表"},
  {component:"usage",code:"authorization_rejected",httpStatus:401,message:"合成授权读取拒绝"},
  {component:"bps",code:"permission_denied",httpStatus:403,message:"合成独立权限拒绝"},
];
for(const windows of [[],[{windowSeconds:18000,usedPercent:0}],[{windowSeconds:18000,usedPercent:21},{windowSeconds:604800,usedPercent:82},{windowSeconds:86400,usedPercent:67}]]) {
  const snapshot={lastAttempt:{status:"partial"},usage:{windows},errors}, before=JSON.stringify(snapshot);
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  const statuses=usageClass(tree,"usage-inline-status"), rows=usageClass(tree,"usage-reset-row");
  assert(statuses.length===1&&content(statuses[0])==="HTTP 401","HTTP status was lost behind a reason without HTTP, or repeated for every window");
  const row=rows.find(node=>usageClass(node,"usage-inline-status").length===1);
  assert(row&&usageClass(row,"usage-reset").length===1&&rows.indexOf(row)===rows.length-1,"HTTP short status is not beside the reset text on the last visible line");
  const children=row.props.children.flat(Infinity).filter(Boolean);
  assert(children.indexOf(usageClass(row,"usage-reset")[0])<children.indexOf(statuses[0]),"HTTP status moved before the reset description");
  assert(!content(tree).includes("部分更新")&&!content(tree).includes("合成")&&!tree.props.title.includes("部分更新")&&!tree.props.title.includes("合成"),"compact content or hover title contains process or detailed error text");
  assert(statuses[0].props.title.includes("Codex 额度")&&tree.props["aria-label"].includes("查询结果不完整"),"compact HTTP source or unverified attempt semantics disappeared");
  if(!windows.length) assert(rows.length===1&&!find(tree,"progress")&&content(tree).includes("额度未知")&&content(row).includes("重置时间未知"),"HTTP-only failure invented quota or lost its reset-line status");
  assert(JSON.stringify(snapshot)===before,"status placement modified cached errors or window order");
}
assert(requests.length===0&&timers.size===0,"compact status rendering requested or retried upstream work");
`)
}

func TestFrontendUsagePresentationExhaustionStatusTracksVisibleLimitedWindow(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-10-31T12:00:00Z"), observedAt="2026-10-31T12:00:00Z";
for(const [values,expectedIndex] of [[[100,12],0],[[12,125],1],[[null,18],1],[[NaN,100],1]]) {
  const snapshot={lastAttempt:{status:"success",startedAt:observedAt},usage:{observedAt,limitReached:true,windows:values.map((usedPercent,index)=>({windowSeconds:index?604800:18000,usedPercent,resetsAt:"2026-10-31T13:00:00Z"}))}};
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now}), windows=usageClass(tree,"usage-window"), statuses=usageClass(tree,"usage-inline-status");
  assert(statuses.length===1&&content(statuses[0])==="额度已用尽","explicit fresh quota exhaustion was missing or repeated across windows");
  assert(usageClass(windows[expectedIndex],"usage-inline-status").length===1&&usageClass(usageClass(windows[expectedIndex],"usage-reset-row")[0],"usage-reset").length===1,"exhaustion label was attached to a different visible window instead of the reported full one");
}
const withoutFlag=renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:100}]}},onOpen(){},now});
assert(usageClass(withoutFlag,"usage-inline-status").length===0&&find(withoutFlag,"progress").props.value===100,"percentage styling invented an explicit upstream exhaustion result");
assert(requests.length===0&&timers.size===0,"exhaustion placement started a query");
`)
}

func TestFrontendUsagePresentationErrorAndHistoricalMeterCoexistInDetails(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const tree=errorDialog({lastAttempt:{status:"failed"},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:24}]},errors:[{component:"usage",code:"authorization_rejected",httpStatus:401,message:"合成来源拒绝读取；此前额度保留"},{component:"bps",code:"upstream_error",httpStatus:402,message:"合成BPS来源返回402"}]});
const panel=usageClass(tree,"insight-errors")[0], cards=usageClass(tree,"insight-error-card");
assert(panel&&panel.props.hidden!==true&&cards.length===2&&content(panel).includes("HTTP 401")&&content(panel).includes("HTTP 402"),"detail redesign hid independent source errors");
assert(content(panel).includes("不能确认欠费或过期")&&!content(panel).includes("账号已欠费"),"visual error status invented a billing conclusion");
const bars=usageNodes(tree,"progress");
assert(bars.length===1&&bars[0].props.value===24&&content(tree).includes("历史观察"),"error cards replaced or refreshed the last known usage");
const ordered=collectUsageNodes(tree,node=>Boolean(node.props?.className));
assert(ordered.indexOf(panel)<ordered.indexOf(usageClass(tree,"insight-detail-grid")[0]),"failure details returned to the bottom of the dialog");
assert(errorMutations===0,"rendering query failures triggered a recovery operation");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-insights/synthetic-errors"&&(!requests[0].options.method||requests[0].options.method==="GET"),"detail page changed the scoped cached read contract");
unmount();
assert(timers.size===0,"redesigned detail leaked its existing cache polling after close");
`)
}

func TestFrontendUsagePresentationDetailsRetainAttemptAndDurabilitySemantics(t *testing.T) {
	for _, tc := range []struct{ status, label string }{
		{"queued", "查询已排队"}, {"running", "正在查询"},
		{"failed", "本次查询失败"}, {"partial", "查询结果不完整"},
		{"skipped", "本次查询已跳过"}, {"canceled", "查询已取消或中断"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			vm := frontendInsightErrorVM(t)
			if err := vm.Set("attemptStatus", tc.status); err != nil {
				t.Fatal(err)
			}
			if err := vm.Set("attemptLabel", tc.label); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
const tree=errorDialog({persisted:false,lastAttempt:{status:attemptStatus},usage:{observedAt:"2026-10-30T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:17}]}});
const result=errorClass(tree,"insight-result")[0];
assert(result&&result.props.role==="status"&&content(result).includes(attemptLabel),"detail layout removed the actual query lifecycle outcome: "+attemptStatus);
assert(content(result).includes("保存未确认")&&find(tree,"progress").props.value===17,"unconfirmed durability erased observed usage or was presented as confirmed");
if(["queued","running"].includes(attemptStatus)) assert(findButton(tree,"立即查询").props.disabled&&!findButton(tree,"取消查询").props.disabled,"in-flight detail state allowed duplicate submission or lost cancellation");
assert(content(tree).includes("已受理的后台查询继续执行")&&errorMutations===0,"detail presentation changed close/cancel semantics or automatically submitted work");
`)
			frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-insights/synthetic-errors"&&(!requests[0].options.method||requests[0].options.method==="GET"),"lifecycle presentation added a request beyond its scoped cache read");
unmount();assert(timers.size===0,"detail state left polling active after close");
`)
		})
	}
}
