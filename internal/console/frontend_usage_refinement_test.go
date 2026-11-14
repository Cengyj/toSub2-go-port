package console

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFrontendUsageRefinementEmphasizesReportedNumberWithoutChangingUnits(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const observedAt="2026-11-04T01:02:03Z",resetsAt="2026-11-05T01:02:03Z";
for(const value of [0,12.5,61.25,33.333333333333336,100,125,-8]) {
  const snapshot={usage:{observedAt,windows:[{name:"synthetic-window",windowSeconds:18000,usedPercent:value,resetsAt}]},lastAttempt:{status:"success"},persisted:true};
  const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now:Date.parse(observedAt)});
  const number=usageClass(tree,"usage-percent-number"),unit=usageClass(tree,"usage-percent-unit");
  assert(number.length===1&&content(number[0])===String(value)&&unit.length===1&&content(unit[0])==="%","numeric hierarchy rounded or changed the reported used ratio");
  const percent=usageClass(tree,"usage-percent")[0];
  assert(!usageClass(number[0],"usage-percent-unit").length&&usageClass(percent,"usage-percent-unit")[0]===unit[0]&&percent.props.title===value+"%","clipped numeric text lost its separately preserved unit or exact hover value");
  assert(content(usageClass(tree,"usage-percent")[0])===value+"%"&&find(tree,"progress").props["aria-label"]==="5 小时窗口已用 "+value+"%","visual unit split changed the accessible measured value");
  assert(usageClass(tree,"usage-window-label").map(content).join()==="5 小时","localized window label lost its reported duration");
  const meter=usageClass(tree,"usage-window")[0],line=usageClass(meter,"usage-window-line")[0],context=usageClass(meter,"usage-meter-context")[0];
  assert(!find(line,"progress")&&usageClass(line,"usage-percent-number")[0]===number[0]&&find(context,"progress")===find(tree,"progress"),"compact window mixed its reading row with the track or detached the track from its window");
  assert(content(usageClass(context,"usage-reset-countdown")[0])==="1天后重置"&&usageClass(context,"usage-reset-countdown")[0].props.title.includes("5 小时窗口"),"single-window track lost its own Chinese reset explanation");
  assert(usageClass(meter,"usage-reset-row")[0].props.className.includes("usage-hidden")&&meter.props.title.includes("预计重置")&&meter.props["aria-label"].includes("5 小时窗口"),"compact reset lost its window-specific full disclosure");
  assert(!content(tree).includes("详情")&&!content(tree).includes("额度观察")&&!tree.props.title.includes("额度观察")&&tree.props.title.includes("5 小时窗口")&&tree.props.title.includes("预计重置"),"compact affordance retained observation time or hid the reset window");
  assert(JSON.stringify(snapshot)===before,"presentation mutated a measured window");
}

for(const name of ["custom","month","standard"]){
  const tree=renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{name,usedPercent:12}]}},onOpen(){}});
  assert(content(usageClass(tree,"usage-window-label")[0])===name&&tree.props["aria-label"].includes(name),"unit localization rewrote a reported window name without a known duration");
}
for(const value of [null,undefined,"0",false,NaN,Infinity]) {
  const tree=renderComponent("AccountInsightSummary",{snapshot:{usage:{windows:[{windowSeconds:18000,usedPercent:value}]}},onOpen(){}});
  assert(!usageClass(tree,"usage-percent-number").length&&!usageClass(tree,"usage-percent-unit").length&&!find(tree,"progress"),"unknown value acquired numeric emphasis or a measured capacity bar");
  assertEmptyUsageTrack(tree);
}
assert(requests.length===0&&timers.size===0,"number rendering scheduled upstream collection");
`)
}

func TestFrontendUsageRefinementKeepsUnconfirmedObservationWarningInExistingFooter(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const observedAt="2026-11-03T01:02:03Z",now=Date.parse("2026-11-04T01:02:03Z");
const base={usage:{observedAt,windows:[{windowSeconds:18000,usedPercent:0,resetsAt:"2026-11-05T00:00:00Z"},{windowSeconds:604800,usedPercent:92}]},lastAttempt:{status:"partial"},errors:[{component:"usage",code:"permission_denied",httpStatus:403,message:"Synthetic detailed reason stays in the details dialog."}]};
const stableClasses=["usage-window","usage-window-line","usage-meter-context","usage-query-slot","usage-reset-row","usage-footer","usage-footer-line","usage-meta","usage-next-reset"];
const shape=tree=>JSON.stringify(collectUsageNodes(tree,node=>stableClasses.some(name=>(node.props?.className||"").split(" " ).includes(name))).map(node=>[node.type,stableClasses.filter(name=>(node.props?.className||"").split(" " ).includes(name))]));
let initial;
for(const persisted of [true,false]) {
  const snapshot={...base,persisted},before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  const persistence=usageClass(tree,"usage-persistence")[0],reset=usageClass(tree,"usage-next-reset")[0];
  assert(content(persistence)===(persisted?"":"待核实")&&persistence.props.className.includes("needs-verification")===!persisted,"unconfirmed persistence lacked a lightweight visible warning");
  assert(!usageClass(tree,"usage-open").length&&!content(tree).includes("›"),"compact usage retained a separate trailing disclosure arrow");
  assert(usageClass(usageClass(tree,"usage-footer-line")[0],"usage-query-slot").length===1,"query feedback no longer shares the reserved compact footer");
  const meters=usageClass(tree,"usage-window");
  assert(meters.every((meter,index)=>usageClass(meter,"usage-meter-context").length===1&&usageClass(meter,"usage-reset-countdown").length===Number(Boolean(base.usage.windows[index].resetsAt)))&&usageClass(tree,"usage-footer-line").length===1&&!usageClass(tree,"usage-context-line").length&&!usageClass(tree,"usage-reset-summary").length,"persistence or HTTP failure hid a known reset or invented an unknown one");
  assert(usageClass(tree,"usage-inline-status").map(content).join()==="HTTP 403","persistence warning replaced the independently reported HTTP failure");
  assert(!content(reset).includes("历史额度")&&find(reset,"time").props.dateTime===base.usage.windows[0].resetsAt&&!usageClass(tree,"usage-observed").length,"warning erased the next reset time or retained observation-time presentation");
  assert(usageNodes(tree,"progress").map(node=>node.props.value).join()==="0,92","warning reset valid usage");
  if(initial===undefined)initial=shape(tree);else assert(shape(tree)===initial,"warning introduced a new flow row");
  assert(!content(tree).includes("Synthetic")&&!content(tree).includes("记录保存未确认"),"compact warning duplicated a detailed process explanation");
  if(!persisted)assert(tree.props["aria-label"].includes("记录保存未确认")&&tree.props.title.includes("记录保存未确认"),"short warning lost its complete accessible meaning");
  assert(JSON.stringify(snapshot)===before,"warning altered persisted source data");
}
assert(requests.length===0&&timers.size===0,"warning retried an unsuccessful query");
`)
}

func TestFrontendUsageRefinementKeepsAllQueryPhasesOneReadOnlyAction(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
let opened=0;const observedAt="2026-11-03T01:02:03Z",now=Date.parse("2026-11-04T01:02:03Z");
for(const [refreshState,status] of [["eligible","success"],["queued","queued"],["active","running"],["unavailable","failed"],["eligible","cancelled"],["eligible","skipped"],["eligible","interrupted"]]) {
  const tree=renderComponent("AccountInsightSummary",{snapshot:{refreshState,persisted:true,lastAttempt:{status},usage:{observedAt,limitReached:true,windows:[{windowSeconds:18000,usedPercent:100,resetsAt:"2026-11-05T00:00:00Z"},{windowSeconds:604800,usedPercent:12}]}},onOpen(){opened++;},now});
  assert(tree.type==="button"&&tree.props.type==="button"&&!tree.props.disabled&&usageNodes(tree,"button").length===1,"simplified visuals introduced nested actions or blocked cached details");
  assert(usageClass(tree,"usage-query-slot").length===1&&usageClass(tree,"usage-window").length===2&&usageClass(tree,"usage-reset-row").length===2,"query activity or error added another meter row");
  assert(usageClass(tree,"usage-percent-number").map(content).join()==="100,12"&&usageNodes(tree,"time")[0].props.dateTime==="2026-11-05T00:00:00Z","phase styling changed usage or substituted the observation timestamp for next reset");
  assert(tree.props["aria-busy"]===(refreshState==="active"),"queue/terminal phase pretended to execute");
  const footer=usageClass(tree,"usage-footer-line")[0],meters=usageClass(tree,"usage-window");
  assert(footer&&usageClass(tree,"usage-footer-line").length===1&&!usageClass(tree,"usage-context-line").length&&meters.every((meter,index)=>usageClass(meter,"usage-meter-context").length===1&&usageClass(meter,"usage-reset-countdown").length===(index===0?1:0)),"query phase hid a known reset or invented a missing one");
  const attemptLabel={cancelled:"已取消",skipped:"已跳过",interrupted:"已中断"}[status];
  if(attemptLabel)assert(content(usageClass(footer,"usage-attempt-state")[0])===attemptLabel,"terminal query outcome disappeared from the visible footer");
  if(status==="failed")assert(tree.props["aria-label"].includes("本次查询失败"),"retained data hid the failed latest query");
  assert(!usageNodes(tree,"input").length&&!usageNodes(tree,"a").length&&!collectUsageNodes(tree,node=>node.props?.role==="status"||node.props?.["aria-live"]).length,"row gained extra controls or noisy live announcements");
  tree.props.onClick();
}
assert(opened===7&&requests.length===0&&timers.size===0,"read-only summary submitted or replayed work");
`)
}

func TestFrontendUsageRefinementUsesIndependentMetersAndOneStatusLine(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-usage.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const observedAt="2026-11-04T01:02:03Z",now=Date.parse(observedAt),resetsAt="2026-11-05T01:02:03Z";
for(const [extra,expected,tone] of [
  [{},"",""],
  [{errors:[{component:"usage",httpStatus:403,code:"permission_denied",message:"Synthetic detail remains private to the dialog."}]},"HTTP 403","danger"],
  [{limitReached:true},"额度已用尽","danger"],
  [{limitReached:true,lastAttempt:{status:"failed"}},"上次已用尽","warning"],
]) {
  const snapshot={usage:{observedAt,limitReached:extra.limitReached,windows:[{windowSeconds:18000,usedPercent:61.25,resetsAt},{windowSeconds:604800,usedPercent:100,resetsAt}]},lastAttempt:extra.lastAttempt||{status:"success"},errors:extra.errors,persisted:true};
  const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  const labels=usageClass(tree,"usage-window-label"),status=usageClass(tree,"usage-inline-status");
  assert(labels.map(content).join()==="5 小时,7 天"&&labels.every(label=>!label.props.onClick&&label.props.tabIndex===undefined),"window capsules lost their localized identity or introduced separate controls");
  assert(status.map(content).join()===expected&&(!expected||(status[0].props["data-tone"]===tone&&usageClass(usageClass(tree,"usage-footer-line")[0],"usage-inline-status")[0]===status[0])),"plain status obscured the complete short failure or moved it from its shared footer");
  if(extra.limitReached)assert(status[0].props["data-window-index"]===1&&status[0].props["aria-label"].includes("7 天窗口"),"shared exhaustion status lost its association with the reported full window");
  assert(usageClass(tree,"usage-percent").map(content).join()==="61.25%,100%"&&usageNodes(tree,"progress").map(node=>node.props.value).join()==="61.25,100","micro-pills changed the exact measured ratios");
  assert(usageClass(tree,"usage-query-slot").length===1&&usageClass(tree,"usage-reset-row").length===2&&usageNodes(tree,"button").length===1,"micro-pills added a flow row or another action");
  assert(JSON.stringify(snapshot)===before&&!content(tree).includes("Synthetic"),"micro-pills mutated data or exposed detailed failure text");
}
const empty=renderComponent("AccountInsightSummary",{snapshot:{bps:{allowed:false,observedAt},errors:[{component:"bps",httpStatus:403,code:"permission_denied"}]},onOpen(){},now});
assert(content(empty).includes("BPS 权限受限")&&!find(empty,"progress")&&!usageClass(empty,"account-pill").length&&usageClass(empty,"usage-inline-status").map(content).join()==="HTTP 403"&&!usageClass(empty,"usage-empty-icon").length&&!usageClass(empty,"usage-reset-row").length,"empty permission observation lost its short error, added redundant rows or fabricated quota");
const detailed=renderComponent("UsageMeter",{window:{windowSeconds:18000,usedPercent:61.25,resetsAt},now,extraWindows:3});
assert(!usageClass(detailed,"account-pill").length&&content(detailed).includes("61.25%")&&!usageClass(detailed,"usage-extra-count").length&&!usageClass(detailed,"usage-window-heading").length,"compact styling or omitted-window count leaked into the detailed meter");
assert(requests.length===0&&timers.size===0,"micro-pills submitted or retried work");
`)
}

func TestFrontendUsageRefinementChineseResetKeepsWindowIdentityAndUncertainty(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-04T01:00:00Z"),at=offset=>new Date(now+offset).toISOString();
for(const [resets,expected] of [
  [[at(71*3600000),at(79*60000)],"2天23小时后重置,1小时19分后重置"],
  [[at(167*3600000),at(299*60000)],"6天23小时后重置,4小时59分后重置"],
  [[at(1),at(60000)],"1分钟后重置,1分钟后重置"],
  [[at(0),at(-60000)],"重置待确认,重置待确认"],
  [[undefined,"synthetic-invalid-date"],""],
]) {
  const snapshot={lastAttempt:{status:"success"},persisted:true,usage:{observedAt:at(0),windows:[{windowSeconds:604800,usedPercent:61.25,resetsAt:resets[0]},{windowSeconds:18000,usedPercent:0,resetsAt:resets[1]}]}};
  for(const count of [1,2]) {
    const visibleSnapshot={...snapshot,usage:{...snapshot.usage,windows:snapshot.usage.windows.slice(0,count)}};
    const before=JSON.stringify(visibleSnapshot),tree=renderComponent("AccountInsightSummary",{snapshot:visibleSnapshot,now,onOpen(){}}),meters=usageClass(tree,"usage-window");
    assert(meters.length===count&&usageClass(tree,"usage-window-label").map(content).join()===["7 天","5 小时"].slice(0,count).join(),"single or dual window layout reordered the reported independent windows");
    assert(usageClass(tree,"usage-window-heading").length===count&&!usageClass(tree,"usage-extra-count").length,"fully displayed windows invented an omitted-window count");
    assert(usageClass(tree,"usage-reset-countdown").map(content).join()===expected.split(",").slice(0,count).join(),"Chinese reset wording changed rounding, unknown or pending meaning");
    assert(usageClass(tree,"usage-footer-line").length===1&&!usageClass(tree,"usage-context-line").length&&!usageClass(tree,"usage-reset-summary").length&&!usageClass(tree,"usage-reset-window").length&&!usageClass(tree,"usage-history").length,"normal windows regained detached reset metadata, repeated window labels or a redundant history marker");
    for(let index=0;index<meters.length;index++) {
      const meter=meters[index],context=usageClass(meter,"usage-meter-context")[0],item=usageClass(context,"usage-reset-countdown")[0],metadata=usageClass(meter,"usage-reset-row")[0];
      const label=index?"5 小时窗口":"7 天窗口",text=expected.split(",")[index]||"";
      if(!text){assert(!item&&!meter.props.title.includes("未知")&&find(context,"progress")===find(meter,"progress"),"missing reset rendered a placeholder or hid measured usage");continue;}
      assert(item&&content(item)===text&&item.props.hidden!==true&&!item.props.className.includes("usage-hidden")&&find(context,"progress")===find(meter,"progress")&&!find(usageClass(meter,"usage-window-line")[0],"progress"),"reset or track escaped its owning window context");
      assert(item.props.title.includes(label)&&meter.props["aria-label"].includes(label)&&metadata.props.className.includes("usage-hidden")&&metadata.props["aria-hidden"]===true,"Chinese reset lost its window-specific full disclosure");
      assert(item.props.className.includes("is-pending")===text.includes("待确认")&&meter.props.title.includes(text.includes("待确认")?"待确认":text.includes("未知")?"重置时间未知":"预计重置"),"compact reset changed uncertainty or omitted its full interpretation");
    }
    assert(usageClass(tree,"usage-percent").map(content).join()===["61.25%","0%"].slice(0,count).join()&&JSON.stringify(visibleSnapshot)===before,"reset simplification changed measured quota or source data");
  }
  const extended={...snapshot,usage:{...snapshot.usage,windows:[...snapshot.usage.windows,{windowSeconds:3600,usedPercent:8,resetsAt:at(60000)}]}},overflow=renderComponent("AccountInsightSummary",{snapshot:extended,now,onOpen(){}});
  const overflowMeters=usageClass(overflow,"usage-window"),extra=usageClass(overflow,"usage-extra-count"),lastHeading=usageClass(overflowMeters[1],"usage-window-heading")[0];
  assert(usageClass(overflow,"usage-reset-countdown").map(content).join()===expected&&extra.length===1&&content(extra[0])==="+1"&&usageClass(lastHeading,"usage-extra-count")[0]===extra[0]&&!usageClass(overflowMeters[0],"usage-extra-count").length&&!usageClass(usageClass(overflow,"usage-footer-line")[0],"usage-extra-count").length,"extra-window count changed meaning, repeated or escaped the last visible window heading");
  assert(extra[0].props.title.includes("另有 1 个窗口")&&!extra[0].props.onClick&&extra[0].props.tabIndex===undefined,"extra-window disclosure lost its full meaning or became another action");
  assert(!usageClass(overflow,"usage-context-line").length&&usageClass(overflow,"usage-footer-line").length===1&&usageClass(overflow,"usage-meter-context").length===2&&overflow.props["aria-label"].includes("另有 1 个窗口"),"overflow changed per-window context or hid the undisplayed-window disclosure");
}
assert(requests.length===0&&timers.size===0,"per-window reset rendering queried upstream state");
`)
}

func TestFrontendUsageRefinementCombinesHistoryActivityFailureAndPersistence(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-04T01:00:00Z"),observedAt="2026-11-03T01:00:00Z";
let opened=0;
for(const refreshState of ["eligible","queued","active"]) {
  const snapshot={refreshState,persisted:false,stale:true,lastAttempt:{status:"failed"},usage:{observedAt,limitReached:true,windows:[{windowSeconds:18000,usedPercent:100,resetsAt:"2026-11-05T01:00:00Z"},{windowSeconds:604800,usedPercent:61.25},{windowSeconds:86400,usedPercent:17}]},errors:[{component:"usage",code:"permission_denied",httpStatus:403,message:"Synthetic full reason belongs in details."}]};
  const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,now,onOpen(){opened++;}}),footer=usageClass(tree,"usage-footer-line")[0];
  const meters=usageClass(tree,"usage-window");
  assert(usageClass(tree,"usage-footer-line").length===1&&!usageClass(tree,"usage-context-line").length&&meters.every((meter,index)=>usageClass(meter,"usage-meter-context").length===1&&usageClass(meter,"usage-reset-countdown").length===Number(Boolean(snapshot.usage.windows[index].resetsAt))),"combined status hid a known reset or invented a missing one");
  const extra=usageClass(tree,"usage-extra-count"),heading=usageClass(meters[1],"usage-window-heading")[0];
  assert(meters.length===2&&extra.length===1&&content(extra[0])==="+1"&&usageClass(heading,"usage-extra-count")[0]===extra[0]&&!usageClass(meters[0],"usage-extra-count").length&&!usageClass(footer,"usage-extra-count").length&&tree.props.title.includes("另有 1 个窗口")&&tree.props["aria-label"].includes("另有 1 个窗口"),"dense combined state repeated, hid or displaced the third-window disclosure");
  assert(content(usageClass(heading,"usage-window-label")[0])==="7 天"&&content(usageClass(usageClass(meters[1],"usage-window-line")[0],"usage-percent")[0])==="61.25%"&&!usageClass(heading,"usage-inline-status").length&&!extra[0].props.onClick&&extra[0].props.tabIndex===undefined,"overflow metadata changed the last window identity, reading or read-only action boundary");
  assert(!usageClass(footer,"usage-history").length&&usageClass(footer,"usage-inline-status").map(content).join()==="HTTP 403"&&usageClass(footer,"usage-persistence").map(content).join()==="待核实","combined status added redundant history text or hid the HTTP/persistence state");
  assert(usageClass(footer,"usage-limit-state").map(content).join()==="用尽"&&usageClass(footer,"usage-limit-state")[0].props.title==="上次已用尽","HTTP priority hid explicit historical exhaustion or claimed a current limit");
  const activity=usageClass(footer,"usage-query-slot")[0];
  assert(content(activity)===(refreshState==="active"?"刷新中":refreshState==="queued"?"排队中":"")&&tree.props["aria-busy"]===(refreshState==="active"),"combined status confused queued, running or finished work");
  const reset=usageClass(tree,"usage-next-reset")[0];
  assert(!reset.props.className.includes("usage-hidden")&&usageClass(footer,"usage-next-reset")[0]===reset&&footer.props.className.includes("is-dense")&&find(reset,"time").props.dateTime===snapshot.usage.windows[0].resetsAt&&!tree.props["aria-label"].includes("历史额度")&&!tree.props.title.includes("历史额度")&&tree.props["aria-label"].includes("记录保存未确认")&&tree.props["aria-label"].includes(reset.props.title)&&tree.props.title.includes(reset.props.title),"dense status erased the next reset and owning window or repeated the freshness label");
  assert(!usageClass(tree,"usage-observed").length&&!tree.props.title.includes("额度观察"),"dense state retained the observation timestamp");
  assert(usageClass(tree,"usage-percent").map(content).join()==="100%,61.25%"&&usageNodes(tree,"button").length===1&&!usageClass(tree,"usage-open").length&&!usageClass(tree,"account-pill").length,"status density changed measured data or added a separate action");
  tree.props.onClick();assert(JSON.stringify(snapshot)===before&&!content(tree).includes("Synthetic"),"combined presentation changed source data or exposed private detail");
}
assert(opened===3&&requests.length===0&&timers.size===0,"combined status submitted a request or retried failed work");
`)
}

func TestFrontendUsageRefinementKeepsFullWidthStackedWindowsAndFlatEmptyContext(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const observedAt="2026-11-04T01:02:03Z";let opens=0;
for(const snapshot of [undefined,{lastAttempt:{status:"never"}},{refreshState:"active",lastAttempt:{status:"running"}},{bps:{allowed:false,observedAt},lastAttempt:{status:"failed"},errors:[{component:"bps",httpStatus:403,code:"permission_denied"}]}]){
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){opens++;}}),empty=usageClass(tree,"usage-empty")[0],footer=usageClass(tree,"usage-footer-line")[0];
  if(!snapshot||snapshot.lastAttempt?.status==="never")assertEmptyUsageTrack(tree,snapshot?.lastAttempt?.status==="never"?"尚未查询":undefined);
  assert(footer&&!usageClass(tree,"usage-context-line").length&&usageClass(tree,"usage-footer-line").length===1&&(snapshot?.bps?content(empty)==="BPS 权限受限":!empty),"meaningful permission/query status disappeared or an empty quota label returned");
  assert(!usageClass(tree,"usage-empty-status").length&&!content(tree).includes("未知"),"permission/activity restored unknown quota placeholders");
  if(snapshot?.bps)assert(content(footer).includes("HTTP 403")&&!tree.props.title.includes("权限观察"),"permission failure lost its error or retained the permission-observation timestamp");
  assert(!usageClass(footer,"usage-next-reset").length&&!find(footer,"time")&&!usageClass(tree,"usage-record-empty").length&&!usageClass(tree,"usage-observed").length,"unmeasured quota invented a reset time or retained observation metadata");
  assert(usageNodes(tree,"button").length===1&&!find(tree,"progress")&&!usageClass(tree,"usage-percent-number").length&&!content(tree).includes("0%")&&!tree.props.title.includes("重置时间未知"),"empty context invented a zero usage value or another action");
  tree.props.onClick();
}
assert(opens===4&&requests.length===0&&timers.size===0,"empty track/context opening submitted or retried a query");
`)
	raw, err := os.ReadFile("../../web/src/components/account-usage.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	declarations := func(pattern string) string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindStringSubmatch(css)
		if len(match) != 2 {
			t.Fatalf("missing usage style boundary %q", pattern)
		}
		return match[1]
	}
	hover := declarations(`(?s)\.account-usage\.insight-summary:hover\s*\{([^}]*)\}`)
	if !strings.Contains(hover, "background: transparent") || !strings.Contains(css, ".account-usage.insight-summary:hover .usage-window-label") {
		t.Fatal("usage hover paints the whole column or loses feedback on the window identity")
	}
	footer := declarations(`(?s)\.account-usage \.usage-footer\s*\{([^}]*)\}`)
	if strings.Contains(footer, "grid-template-rows: 18px;") || strings.Contains(css, ".usage-context-line") || strings.Contains(css, ".usage-reset-summary") {
		t.Fatal("usage footer reserves detached reset or duplicate metadata rows")
	}
	footerLine := declarations(`(?s)\.account-usage \.usage-footer-line\s*\{([^}]*)\}`)
	if !strings.Contains(footerLine, "flex-wrap: wrap") || !strings.Contains(footerLine, "min-height: 18px") || strings.Contains(footerLine, " height: 18px") {
		t.Fatal("usage footer cannot wrap complete reset times and status into readable 18px lines")
	}
	meter := declarations(`(?s)\.usage-meter\.usage-window\.is-compact\s*\{([^}]*)\}`)
	lanes := declarations(`(?s)\.account-usage \.usage-windows\s*\{([^}]*)\}`)
	wrappers := regexp.MustCompile(`(?s)\.usage-meter\.is-compact \.usage-window-line,\s*\.usage-meter\.is-compact \.usage-meter-context\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if !strings.Contains(lanes, "grid-template-columns: minmax(0, 1fr)") || !strings.Contains(lanes, "gap: 5px") || !strings.Contains(meter, "grid-template-columns: max-content minmax(0, 1fr) max-content") || !strings.Contains(meter, "grid-template-rows: 16px 3px") || len(wrappers) != 2 || !strings.Contains(wrappers[1], "display: contents") {
		t.Fatal("quota windows lost their full-width stacked lanes, horizontal track, reading, and independent reset hierarchy")
	}
	if strings.Contains(css, ".account-usage .usage-windows > :only-child") || strings.Contains(css, ".account-usage .usage-windows > :nth-child(2)") {
		t.Fatal("single and paired observations regained different track widths or a side-by-side divider")
	}
	for _, item := range []struct {
		selector, column, row string
	}{
		{".usage-meter.is-compact .usage-window-heading", "grid-column: 1", "grid-row: 1"},
		{".usage-meter.is-compact .usage-value", "grid-column: 3", "grid-row: 1"},
	} {
		declaration := declarations(`(?s)` + regexp.QuoteMeta(item.selector) + `\s*\{([^}]*)\}`)
		if !strings.Contains(declaration, item.column) || !strings.Contains(declaration, item.row) {
			t.Fatalf("compact quota %s detached from its first-row lane", item.selector)
		}
	}
	// Window identity must retain its own lane when an exact, unrounded ratio
	// is wider than the available space. Only the numeric reading may elide;
	// its separate unit and complete title remain protected by the tree test.
	number := declarations(`(?s)\.usage-meter\.is-compact \.usage-percent-number\s*\{([^}]*)\}`)
	unit := declarations(`(?s)\.usage-meter\.is-compact \.usage-percent-unit\s*\{([^}]*)\}`)
	if !strings.Contains(number, "flex: 0 1 auto") || !strings.Contains(number, "min-width: 0") || !strings.Contains(number, "text-overflow: ellipsis") || !strings.Contains(unit, "flex: 0 0 auto") {
		t.Fatal("long ratios may displace the quota identity or hide their percent unit")
	}
	track := declarations(`(?s)\.usage-meter\.usage-window\.is-compact progress\s*\{([^}]*)\}`)
	if !strings.Contains(track, "grid-column: 1 / -1") || !strings.Contains(track, "grid-row: 2") || !strings.Contains(track, "width: 100%") || !strings.Contains(track, "height: 3px") {
		t.Fatal("measured tracks lost their full horizontal lane or compact dimensions")
	}
	if strings.Contains(css, ".usage-meter.usage-window.is-compact progress { writing-mode: vertical-lr; direction: rtl; }") {
		t.Fatal("native quota progress retained the obsolete vertical writing mode")
	}
	reset := declarations(`(?s)\.usage-meter\.is-compact \.usage-reset-countdown\s*\{([^}]*)\}`)
	if !strings.Contains(reset, "grid-column: 2;") || !strings.Contains(reset, "grid-row: 1;") {
		t.Fatal("each quota window lost its independent reset explanation beside its reading")
	}
	label := ""
	for _, rule := range regexp.MustCompile(`(?m)^\.usage-meter\.is-compact \.usage-window-label\s*\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		label += rule[1]
	}
	if !strings.Contains(label, "color: var(--text-secondary)") || !strings.Contains(label, "font-size: 11px") || !strings.Contains(label, "line-height: 16px") || !strings.Contains(label, "border: 0") || !strings.Contains(label, "background: transparent") || !strings.Contains(label, "padding: 0") {
		t.Fatal("window identity lost its readable neutral inline hierarchy")
	}
	dense := declarations(`(?s)\.account-usage \.usage-footer-line\.is-dense\s*\{([^}]*)\}`)
	if !strings.Contains(dense, "column-gap: 3.5px") {
		t.Fatal("dense status footer lost its compact spacing")
	}
	resetTime := declarations(`(?s)\.account-usage \.usage-next-reset\s*\{([^}]*)\}`)
	if !strings.Contains(resetTime, "display: inline-flex") || !strings.Contains(resetTime, "font-size: 11px") || !strings.Contains(resetTime, "flex: 0 0 auto") || strings.Contains(resetTime, "overflow: hidden") || strings.Contains(css, ".usage-observed") || strings.Contains(css, ".usage-record-empty") {
		t.Fatal("next reset time can shrink or truncate, or obsolete observation-time styles remain")
	}
}
