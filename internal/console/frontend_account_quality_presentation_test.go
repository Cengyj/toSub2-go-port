package console

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFrontendAccountQualityPresentationShowsOnlyActualCompletedHistory(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const entries=[
 {id:"pass",kind:"candy",status:"success",verdict:"normal",score:1,total:1,model:"synthetic-pass",finishedAt:"2026-11-04T00:00:01Z"},
 {id:"low",kind:"candy",status:"success",verdict:"degraded",score:0,total:1,model:"synthetic-low",finishedAt:"2026-11-04T00:00:02Z"},
 {id:"draw",kind:"pelican",status:"success",model:"synthetic-drawing",finishedAt:"2026-11-04T00:00:03Z"},
 {id:"limit",kind:"candy",status:"failed",errorCode:"rate_limited",httpStatus:429,finishedAt:"2026-11-04T00:00:04Z"},
 {id:"fail",kind:"pelican",status:"failed",httpStatus:503,finishedAt:"2026-11-04T00:00:05Z"},
];
let opens=0;
for(const count of [1,2,3,4,5]){
 const snapshot={state:"eligible",probeHistory:entries.slice(0,count),persisted:true},before=JSON.stringify(snapshot);
 const tree=renderComponent("AccountQualitySummary",{snapshot,onOpen(){opens++;},accountLabel:"synthetic@example.invalid"});
 const cells=usageClass(tree,"quality-history-cell");
 assert(cells.length===count&&usageClass(tree,"quality-history-grid").length===1,"history was padded or actual records were removed");
 assert(cells.map(cell=>cell.props["data-tone"]).join()===['success','warning','success','warning','danger'].slice(0,count).join(),"completed attempts lost their individual verdict colors");
 assert(cells.map(cell=>cell.props["data-kind"]).join()===entries.slice(0,count).map(item=>item.kind).join(),"mixed probe kinds were merged or reordered");
 assert(cells.every(cell=>cell.type==="span"&&cell.props.role==="img"&&cell.props.title===cell.props["aria-label"]&&cell.props.title.includes("2026-")),"history cell lost its non-color accessible explanation");
 assert(cells.every(cell=>collectUsageNodes(cell,node=>["Check","CircleAlert","X","Ban","svg"].includes(node.type)).length===0),"plain color cells recreated outcome icons");
 assert(tree.type==="button"&&tree.props.type==="button"&&usageNodes(tree,"button").length===1&&!tree.props.disabled,"history is not a single read-only native button");
 assert(tree.props["aria-label"].includes("synthetic@example.invalid")&&tree.props.title.includes("左旧右新")&&tree.props.title.includes("查看推理探针详情")&&tree.props.title.includes("不代表标准 IQ"),"history lacks account, chronology or disclosure context");
 assert(!content(tree)&&!usageNodes(tree,"progress").length&&!usageClass(tree,"quality-meta-line").length,"compact grid recreated visible score/date/status placeholder text");
 tree.props.onClick();assert(JSON.stringify(snapshot)===before,"rendering or opening history mutated measurements");
}
assert(opens===5&&requests.length===0&&timers.size===0,"history presentation invoked model work");
`)
}

func TestFrontendAccountQualityPresentationActivityDoesNotOccupyHistory(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const entries=Array.from({length:5},(_,i)=>({id:"saved-"+i,kind:"candy",status:"success",verdict:"normal",score:1,total:1,finishedAt:"2026-11-04T00:00:0"+i+"Z"}));
for(const [state,label,icon] of [["queued","排队中","Clock3"],["active","检测中","LoaderCircle"]])for(const probeHistory of [[],entries]){
 const snapshot={state,lastAttempt:{status:state==="active"?"running":"queued"},probeHistory,persisted:false},before=JSON.stringify(snapshot);
 const tree=renderComponent("AccountQualitySummary",{snapshot,onOpen(){}}),activity=usageClass(tree,"quality-history-activity");
 assert(usageClass(tree,"quality-history-cell").length===probeHistory.length&&activity.length===1&&content(activity[0])===label,"unfinished work replaced or invented completed history");
 const icons=usageNodes(activity[0],icon);assert(icons.length===1&&String(icons[0].props["aria-hidden"])==="true"&&Boolean(icons[0].props.className?.includes("spin"))===(state==="active"),"queued work claimed execution or activity icon disappeared");
 assert(tree.props["aria-busy"]===(state==="active")&&JSON.stringify(snapshot)===before,"activity announcement or original snapshot changed");
}
assert(requests.length===0&&timers.size===0,"activity display scheduled probes");
`)
}

func TestFrontendAccountQualityPresentationUnknownCellsRemainEmpty(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
let opens=0;
for(const snapshot of [undefined,null,{}, {state:"eligible",lastAttempt:{status:"never"}}, {state:"unavailable",unavailableReason:"synthetic reason"}, {lastAttempt:{status:"failed",errorCode:"timeout"}}, {lastAttempt:{status:"unrecognized"}}, {probeHistory:[]}])for(const authorizationMethod of ["chatgpt","bps"]){
 const tree=renderComponent("AccountQualitySummary",{snapshot,authorizationMethod,accountLabel:"synthetic@example.invalid",onOpen(){opens++;}});
 assert(tree===null,"missing or unconfirmed history created placeholder text, a fake score, or an empty action");
}
assert(opens===0&&requests.length===0&&timers.size===0,"empty presentation made network requests");
`)
}

func TestFrontendAccountQualityPresentationKeepsFullDiagnosticsInReadOnlyDetails(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
const result={model:"synthetic-diagnostic-model",ruleVersion:"reasoning-six-v1",verdict:"normal",score:5,total:6,checkedAt:"2026-11-04T00:00:00Z",durationMs:3200};
const reason="SYNTHETIC-DIAGNOSTIC: "+"完整合成失败信息需要保留在详情。".repeat(60);
let opens=0,closes=0,checks=0,cancels=0;
for(const historical of [false,true]){
 for(const [errorCode,label] of [["timeout","检测失败"],["invalid_response","结果未知"],["persistence_failed","检测失败"]]){
  const job={id:"synthetic-diagnostic",email:"synthetic-diagnostic@example.invalid",authorizationMethod:"chatgpt"};
  const snapshot={state:"eligible",persisted:errorCode!=="persistence_failed",lastAttempt:{status:"failed",errorCode,message:reason,httpStatus:503,startedAt:"2026-11-04T00:59:00Z",finishedAt:"2026-11-04T01:00:00Z"},lastResult:historical?result:undefined},before=JSON.stringify(snapshot);
  const summary=renderComponent("AccountQualitySummary",{snapshot,onOpen(){opens++;},accountLabel:job.email});
  const cells=usageClass(summary,"quality-history-cell");
  assert(cells.length===1&&cells[0].props["data-tone"]===(errorCode==="timeout"?"danger":"warning")&&!content(summary),"latest completed legacy failure was hidden by an older passing measurement or visible status text");
  assert(!summary.props.title.includes("SYNTHETIC-DIAGNOSTIC")&&!summary.props["aria-label"].includes("SYNTHETIC-DIAGNOSTIC")&&summary.props.title.includes("查看推理探针详情")&&summary.props.title.includes("不代表标准 IQ"),"grid tooltip exposed diagnostic paragraphs or lost its read-only disclosure");
  summary.props.onClick();
  const quality={snapshots:{[job.id]:snapshot},settings:{enabled:true,minimumScore:1,model:"synthetic-dialog-model",reasoningEffort:"high"},modelsAPI:async()=>({models:[]}),settingsLoaded:true,actionBusy:"",error:"",verificationRequired:false,canSubmit(){return false;},async cancel(){cancels++;}};
  const dialog=renderComponent("AccountQualityDialog",{job,quality,onClose(){closes++;},onCheck(){checks++;}}),diagnostic=usageClass(dialog,"quality-diagnostic")[0];
  assert(diagnostic&&content(usageNodes(diagnostic,"p")[0])===reason&&content(diagnostic).includes(errorCode),"read-only detail lost, shortened or misclassified the full latest diagnostic");
  assert(findButton(dialog,"开始检测").props.disabled&&!findButton(dialog,"取消"),"reading a terminal error gained an eligible invocation or cancellation");
  findMatching(dialog,node=>node.type==="button"&&node.props["aria-label"]==="关闭推理探针").props.onClick();
  assert(JSON.stringify(snapshot)===before,"moving diagnostics into details changed the retained observation");
 }
}
assert(opens===6&&closes===6&&checks===0&&cancels===0&&requests.length===0&&[...timers.values()].every(timer=>timer.delay===20000),"reading full diagnostic details invoked, cancelled or retried model content");unmount();assert(timers.size===0,"diagnostic details retained model metadata deadlines");
`)
}

func TestFrontendAccountQualityPresentationDialogRetainsExplicitActionsAndEligibility(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendUsageTreeQueries)
	frontendPollingRun(t, vm, `
renderComponent("AccountQualityDialog",{job:{id:"synthetic-probe",authorizationMethod:"chatgpt"},quality:{settings:{model:"synthetic-dialog-model",reasoningEffort:"high"},snapshots:{},modelsAPI:async()=>({models:[]}),canSubmit(){return false;}},onCheck(){throw new Error("directory lookup started detection");},onClose(){}});
`)
	frontendPollingRun(t, vm, `
let checks=0,cancels=0,closes=0;
const nextAllowedAt="2026-11-04T12:00:00Z";
for(const [authorizationMethod,state,eligible,busy] of [["chatgpt","eligible",true,""],["chatgpt","coolingDown",false,""],["bps","eligible",true,""],["chatgpt","active",false,""],["chatgpt","active",false,"cancel"]]){
  const job={id:"synthetic-probe",email:"synthetic@example.invalid",authorizationMethod};
  const quality={snapshots:{[job.id]:{state,lastAttempt:{status:state==="active"?"running":"never"},nextAllowedAt,unavailableReason:state==="coolingDown"?"合成账号仍在冷却":""}},settings:{enabled:true,minimumScore:1,model:"synthetic-dialog-model",reasoningEffort:"high"},modelsAPI:async()=>({models:[]}),settingsLoaded:true,actionBusy:busy,error:"",verificationRequired:false,canSubmit(ids){assert(ids.length===1&&ids[0]===job.id,"dialog checked a different account");return eligible;},async cancel(ids){assert(ids.length===1&&ids[0]===job.id,"dialog cancelled a different account");cancels++;}};
  const tree=renderComponent("AccountQualityDialog",{job,quality,onClose(){closes++;},onCheck(){checks++;}});
  assert(requests.length===0&&[...timers.values()].every(timer=>timer.delay===20000),"opening details invoked model content or started probe polling");
  const trigger=findButton(tree,state==="active"?"检测中":"开始检测"),cancel=findButton(tree,"取消");
  assert(trigger&&trigger.props.disabled===!eligible,"dialog invocation bypassed qualification or BPS exclusion");
  assert(Boolean(cancel)===(state==="active"),"dialog removed or invented cancellation");
  if(cancel)assert(cancel.props.disabled===Boolean(busy),"dialog admitted duplicate cancellation");
  assert(content(tree).includes("下次可检测")&&content(tree).includes("题目与判定")&&content(tree).includes("结果仅供参考"),"details lost cooldown or score interpretation");
  if(state==="coolingDown")assert(content(tree).includes("合成账号仍在冷却"),"cooldown reason disappeared");
  if(state==="eligible"&&authorizationMethod==="chatgpt")assert(content(tree).includes("消耗模型额度"),"explicit invocation lost cost disclosure");
  if(!trigger.props.disabled)trigger.props.onClick();
  if(cancel&&!cancel.props.disabled)cancel.props.onClick();
  const before=cancels;findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="关闭推理探针").props.onClick();assert(cancels===before,"closing details cancelled accepted work");
}
assert(checks===2&&cancels===1&&closes===5&&requests.length===0&&[...timers.values()].every(timer=>timer.delay===20000),"dialog actions did not remain explicit and scoped");unmount();assert(timers.size===0,"model directory deadline survived close");
`)
}

// Browser hit targets, focus and visual fit are also checked with synthetic data.
func TestFrontendAccountQualityPresentationHistoryGridRemainsCompactAndAccessible(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/components/account-quality.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	row := regexp.MustCompile(`(?s)[.]account-quality[[:space:]]*[{]([^}]*)[}]`).FindStringSubmatch(css)
	if len(row) != 2 || regexp.MustCompile(`(?:^|;)[[:space:]]*(?:min-)?height[[:space:]]*:`).MatchString(row[1]) || strings.Contains(row[1], "grid-template-rows") {
		t.Fatal("empty probe rows reserve artificial placeholder lines")
	}
	for _, required := range []string{".quality-history-grid", "repeat(5, 20px)", ".quality-history-cell", "width: 20px", "height: 20px", ".quality-history-summary:focus-visible", ".quality-history-activity", "prefers-reduced-motion", `data-tone="success"`, `data-tone="warning"`, `data-tone="danger"`} {
		if !strings.Contains(css, required) {
			t.Fatalf("missing grid affordance %q", required)
		}
	}
	for _, removed := range []string{".quality-verdict", ".quality-score", ".quality-result-pill", ".quality-meta-line", ".quality-label"} {
		if strings.Contains(css, removed) {
			t.Fatalf("obsolete single-result style remains: %s", removed)
		}
	}
}
