package console

import "testing"

func TestFrontendUsageQueryActivityUsesActualAccountAttempt(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
for (const [status,phase,busy] of [["queued","queued",false],["pending","queued",false],["running","running",true],["checking","running",true]]) {
  const snapshot={lastAttempt:{status,queryId:"synthetic-query"},usage:{windows:[{usedPercent:0}]}};
  const before=JSON.stringify(snapshot),activity=Browser.describeInsightQueryActivity(snapshot);
  assert(activity.phase===phase&&activity.active&&activity.busy===busy&&activity.hasUsage,"account activity did not distinguish queued from actual execution: "+status);
  assert(activity.label===(busy?"正在刷新":"等待刷新")&&activity.description.includes("上次用量"),"known zero usage lost its retained-observation presentation");
  assert(JSON.stringify(snapshot)===before,"activity derivation mutated the snapshot");
}
for(const status of ["never","success","partial","failed","skipped","cancelled","canceled","interrupted","synthetic-unknown"]) {
  const activity=Browser.describeInsightQueryActivity({lastAttempt:{status},usage:{windows:[{usedPercent:64}]}});
  assert(!activity.active&&!activity.busy&&activity.phase==="idle","terminal or unknown attempt invented active collection: "+status);
}
`)
}

func TestFrontendUsageQueryActivityPreservesHistoricalMetersAndTimes(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-03T12:00:00Z"),observedAt="2026-11-02T12:00:00Z",resetsAt="2026-11-04T12:00:00Z";
let opens=0;
for(const status of ["queued","pending","running","checking"]) {
  const running=["running","checking"].includes(status),snapshot={lastAttempt:{status,startedAt:running?"2026-11-03T12:00:00Z":undefined},usage:{observedAt,windows:[{windowSeconds:18000,usedPercent:0,resetsAt},{windowSeconds:604800,usedPercent:93,resetsAt}]}};
  const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,now,onOpen(){opens++;},accountLabel:"synthetic@example.invalid"});
  const activity=usageClass(tree,"usage-query-slot")[0],headers=usageClass(tree,"usage-window-line");
  assert(activity&&content(activity) === (running?"刷新中":"排队中"),"account with cached meters has no visible inline activity");
  assert(usageClass(tree,"usage-query-slot").length===1&&usageClass(usageClass(tree,"usage-footer-line")[0],"usage-query-slot")[0]===activity&&headers.every(header=>!usageClass(header,"usage-query-slot").length),"query status left its reserved footer slot or was repeated across windows");
  assert(tree.props["aria-label"].includes("保留上次用量")&&activity.props.className.includes(running?"is-running":"is-queued"),"inline activity is indistinguishable from queueing or hides retention");
  assert(!usageClass(tree,"usage-query-state").length&&!usageClass(tree,"is-query-placeholder").length,"query activity inserted an extra flow row");
  assert(usageNodes(tree,"progress").map(node=>node.props.value).join()==="0,93"&&usageClass(tree,"usage-percent").map(content).join()==="0%,93%","query progress overwrote the quota percentage");
  assert(usageNodes(tree,"time").some(node=>node.props.dateTime===resetsAt)&&!tree.props.title.includes("2026-11-02"),"pending refresh replaced the next quota reset with an observation timestamp");
  assert(tree.props["aria-busy"]===running&&!tree.props.disabled&&tree.type==="button","queued state claimed active work or activity blocked the details button");
  assert(collectUsageNodes(activity,node=>(node.props?.className||"").split(" ").includes("spin")).length===(running?1:0),"queued account animated as though it was already being queried");
  assert(!collectUsageNodes(tree,node=>node.props?.role==="status"||node.props?.["aria-live"]).length,"row polling introduced repeated live announcements");
  tree.props.onClick();assert(JSON.stringify(snapshot)===before,"rendering changed retained usage");
}
assert(opens===4&&requests.length===0&&timers.size===0,"activity display scheduled work or changed the read-only details action");
`)
}

func TestFrontendUsageQueryActivityPrefersAuthoritativeFlightProjection(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for(const status of ["never","success","failed","cancelled"]) {
  const now=Date.parse("2026-11-03T12:00:00Z"),resetsAt="2026-11-04T12:00:00Z";
  const snapshot={refreshState:"active",lastAttempt:{status},lastSuccessAt:"2026-11-03T12:00:00Z",usage:{observedAt:"2026-11-02T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:42,resetsAt}]}};
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  assert(usageClass(tree,"usage-query-slot")[0]?.props.className.includes("is-running")&&tree.props["aria-busy"]===true,"lastAttempt hid a real active flight or pending cleanup: "+status);
  assert(tree.props["aria-label"].includes("正在查询")&&!tree.props["aria-label"].includes("本次查询失败")&&!tree.props["aria-label"].includes("查询已取消或中断"),"accessible state contradicted the active flight");
  assert(find(tree,"progress").props.value===42&&usageNodes(tree,"time")[0].props.dateTime===resetsAt,"query or success metadata replaced the quota reset time");
}
const queued=renderComponent("AccountInsightSummary",{snapshot:{refreshState:"queued",lastAttempt:{status:"never"}},onOpen(){}});
assert(usageClass(queued,"usage-query-slot").map(content).join()==="排队中"&&queued.props["aria-busy"]===false,"new-grant queued flight looked idle or active");
for(const refreshState of ["eligible","coolingDown","unavailable","synthetic-unknown","",null]) {
  const snapshot={refreshState,lastAttempt:{status:"running"},usage:{windows:[{usedPercent:12}]}};
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
  const slot=usageClass(tree,"usage-query-slot")[0];
  assert(slot&&!content(slot)&&tree.props["aria-busy"]===false&&!tree.props["aria-label"].includes("正在查询"),"explicit live state removed its reserved slot or fell back to obsolete running metadata: "+String(refreshState));
}
const idle=renderComponent("AccountInsightSummary",{snapshot:{refreshState:"eligible",lastAttempt:{status:"success"},usage:{windows:[{usedPercent:9}]}},onOpen(){}});
assert(!collectUsageNodes(idle,node=>(node.props?.className||"").split(" ").includes("spin")).length&&requests.length===0&&timers.size===0,"ordinary cache polling was presented as an upstream query");
`)
}

func TestFrontendUsageQueryActivityWithoutMetersWaitsWithoutInventingProgress(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for(const usage of [undefined,{windows:[]},{observedAt:"2026-11-02T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:null},{windowSeconds:604800,usedPercent:"0"}]}]) {
  for(const status of ["queued","running"]) {
    const snapshot={lastAttempt:{status},usage},tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){}});
    const activity=usageClass(tree,"usage-query-slot")[0];
    const expected=status==="queued"?"排队中":"查询中";
    assert(activity&&content(activity)===expected,"account without usable meters still shows an idle unknown state");
    assert(tree.props["aria-label"].includes("尚无可用用量")&&!tree.props["aria-label"].includes("上次用量"),"unknown usage was described as a retained numeric observation");
    assert(!usageClass(tree,"is-query-placeholder").length&&!usageClass(tree,"usage-query-state").length,"unknown usage added a query placeholder beside existing content");
    assert(!usageClass(tree,"usage-empty").length&&usageClass(tree,"usage-footer-line").length===1&&!usageClass(tree,"usage-next-reset").length,"active query restored unknown quota or reset placeholders");
    assertEmptyUsageTrack(tree);
    assert(!content(tree).includes("100%"),"waiting state fabricated full capacity");
    assert(!usageClass(tree,"usage-window").length&&!usageClass(tree,"usage-percent").length&&!content(tree).includes("未知"),"active state rendered unmeasured windows");
  }
}
const bps=renderComponent("AccountInsightSummary",{snapshot:{lastAttempt:{status:"running"},bps:{allowed:false,observedAt:"2026-11-02T12:00:00Z"}},onOpen(){}});
assert(content(bps).includes("BPS 权限受限")&&!content(bps).includes("授权失效")&&!find(bps,"progress"),"empty activity erased known BPS permission or invented quota");
assert(content(find(usageClass(bps,"usage-empty")[0],"strong"))==="BPS 权限受限"&&!usageNodes(usageClass(bps,"usage-empty")[0],"small").length&&usageClass(bps,"usage-query-slot").map(content).join()==="查询中"&&!usageClass(bps,"usage-empty-status").length,"BPS permission/activity was lost or unknown quota placeholders returned");
assert(requests.length===0&&timers.size===0,"empty activity started its own polling");
`)
}

func TestFrontendUsageQueryActivityStopsWithoutClearingFailedOrCanceledResults(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
for(const status of ["success","partial","failed","cancelled","canceled","interrupted"]) {
  const now=Date.parse("2026-11-03T12:00:00Z"),resetsAt="2026-11-04T12:00:00Z";
  const snapshot={lastAttempt:{status},usage:{observedAt:"2026-11-02T12:00:00Z",windows:[{windowSeconds:18000,usedPercent:46,resetsAt}]},errors:[{component:"usage",code:"permission_denied",httpStatus:403,message:"synthetic retained failure"}]};
  const tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
  assert(usageClass(tree,"usage-query-slot").length===1&&!content(usageClass(tree,"usage-query-slot")[0])&&!usageClass(tree,"usage-query-state").length&&!usageClass(tree,"is-query-placeholder").length&&tree.props["aria-busy"]===false&&!collectUsageNodes(tree,node=>(node.props?.className||"").split(" ").includes("spin")).length,"terminal state removed its reserved slot or kept refresh activity");
  assert(find(tree,"progress").props.value===46&&content(tree).includes("HTTP 403")&&usageNodes(tree,"time")[0].props.dateTime===resetsAt&&!tree.props.title.includes("2026-11-02"),"terminal state cleared the last quota result, error or next reset time");
}
assert(requests.length===0&&timers.size===0,"terminal presentation retried failed work");
`)
}

func TestFrontendUsageQueryActivityKeepsStructuralRowsStable(t *testing.T) {
	vm := frontendUsageColumnVM(t)
	frontendPollingRun(t, vm, `
const observedAt="2026-11-02T12:00:00Z",now=Date.parse("2026-11-03T12:00:00Z");
const containers=["usage-windows","usage-window","usage-window-line","usage-reset-row","usage-empty","usage-empty-copy","usage-empty-status","usage-footer","usage-meta","usage-next-reset"];
const signature=tree=>JSON.stringify(collectUsageNodes(tree,node=>containers.some(name=>(node.props?.className||"").split(" ").includes(name))).map(node=>({type:node.type,classes:containers.filter(name=>(node.props?.className||"").split(" ").includes(name))})));
const cases=[
  {},
  {usage:{observedAt,windows:[{windowSeconds:18000,usedPercent:0},{windowSeconds:604800,usedPercent:93}]}},
  {usage:{windows:[{windowSeconds:18000,usedPercent:42}]}},
  {usage:{windows:[{windowSeconds:18000,usedPercent:42}]},bps:{allowed:true,observedAt}},
  {usage:{observedAt,windows:[{windowSeconds:18000,usedPercent:null},{windowSeconds:604800,usedPercent:"0"}]}},
  {usage:{observedAt,windows:[{usedPercent:31},{usedPercent:62},{usedPercent:44}]}},
  {usage:{windows:[]}},
  {bps:{allowed:false,observedAt}},
  {bps:{allowed:true}},
  {bps:{allowed:false},subscription:{observedAt}},
  {subscription:{}},
];
const phases=[["eligible","success"],["queued","never"],["active","running"],["active","cancelled"],["coolingDown","success"],["unavailable","failed"],["eligible","partial"],["eligible","cancelled"]];
for(const base of cases) {
  let initial;
  const measured=(base.usage?.windows||[]).filter(window=>typeof window.usedPercent==="number"&&Number.isFinite(window.usedPercent)).length;
  const hasData=measured>0||typeof base.bps?.allowed==="boolean";
  for(const [refreshState,status] of phases) {
    const snapshot={...base,refreshState,lastAttempt:{status,startedAt:status==="running"?"2026-11-03T12:00:00Z":undefined}};
    const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){},now});
    const meaningful=hasData||["queued","active"].includes(refreshState)||["failed","partial"].includes(status);
    if(!meaningful){assertEmptyUsageTrack(tree);assert(JSON.stringify(snapshot)===before,"empty idle snapshot mutated data");}
    assert(tree!==null,"measured data or meaningful query state disappeared");
    const current=signature(tree);if(hasData){if(initial===undefined)initial=current;assert(current===initial,"refresh-only transition changed the measured observation structure");}
    assert(!usageClass(tree,"usage-query-state").length&&!usageClass(tree,"is-query-placeholder").length&&!usageClass(tree,"usage-history-label").length,"refresh or historical state inserted a separate flow row");
    assert(usageClass(tree,"usage-meta").length===(measured>2?1:0),"history or unknown windows inflated the omitted-window count");
    assert(usageClass(tree,"usage-footer").length===1&&!usageClass(tree,"usage-next-reset").length,"missing reset timestamps rendered placeholder metadata");
    assert(usageClass(tree,"usage-query-slot").length===1&&usageClass(tree,"usage-footer-line").length===1,"compact footer did not keep exactly one activity slot and explanation line");
    if(!measured)assert(usageClass(tree,"usage-empty").length===(typeof base.bps?.allowed==="boolean"?1:0)&&!usageClass(tree,"usage-empty-status").length&&!usageClass(tree,"usage-empty-icon").length,"empty observation added unknown placeholders or lost explicit permission");
    assert(JSON.stringify(snapshot)===before,"structural presentation rewrote an observation");
  }
}
assert(requests.length===0&&timers.size===0,"stable activity presentation scheduled work");
`)
}
