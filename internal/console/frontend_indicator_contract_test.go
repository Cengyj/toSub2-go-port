package console

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Shared semantic classes do not require pill surfaces. Check the quiet inline
// hierarchy and readable text here; native browser screenshots separately
// verify the cascade, geometry, hit targets, and contrast.
func TestFrontendIndicatorsUseReadableFlatHierarchy(t *testing.T) {
	readStyle := func(path, selector string) map[string]string {
		t.Helper()
		raw, err := os.ReadFile("../../web/src/" + path)
		if err != nil {
			t.Fatal(err)
		}
		matches := regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(selector)+`\s*\{([^}]*)\}`).FindAllStringSubmatch(string(raw), -1)
		if len(matches) == 0 {
			t.Fatalf("missing inline indicator style %s in %s", selector, path)
		}
		style := map[string]string{}
		for _, match := range matches {
			for _, declaration := range strings.Split(match[1], ";") {
				key, value, ok := strings.Cut(declaration, ":")
				if ok {
					style[strings.TrimSpace(key)] = strings.TrimSpace(value)
				}
			}
		}
		return style
	}
	base := readStyle("components/account-indicators.css", ".account-pill")
	for _, item := range []struct{ path, target, layout string }{
		{"styles.css", ".operation-status", ".operation-summary"},
		{"components/account-usage.css", ".account-usage.insight-summary", ".account-usage.insight-summary"},
	} {
		target, layout := readStyle(item.path, item.target), readStyle(item.path, item.layout)
		if target["min-height"] != "68px" || target["padding"] != "0" || layout["align-content"] != "start" {
			t.Fatalf("%s lost the shared first-row baseline or accessible hit area", item.target)
		}
	}
	for _, item := range []struct {
		name, path, selector string
		inherits             bool
	}{
		{"operation result", "styles.css", ".account-pill.operation-status-pill", true},
		{"saved authorization method", "styles.css", ".operation-method", false},
		{"quota window identity", "components/account-usage.css", ".usage-meter.is-compact .usage-window-label", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			style := map[string]string{}
			if item.inherits {
				for key, value := range base {
					style[key] = value
				}
			}
			for key, value := range readStyle(item.path, item.selector) {
				style[key] = value
			}
			if style["border"] != "0" {
				t.Fatal("inline indicator regained a competing capsule outline")
			}
			if radius := style["border-radius"]; radius != "" && radius != "0" {
				t.Fatal("inline indicator regained a rounded capsule silhouette")
			}
			if style["background"] != "transparent" || !strings.Contains(style["color"], "var(--") {
				t.Fatal("inline indicator lost its transparent surface or theme-aware text")
			}
			if !regexp.MustCompile(`^(?:1[1-9]|[2-9][0-9])(?:\.[0-9]+)?px$`).MatchString(style["font-size"]) {
				t.Fatal("inline indicator text fell below the readable 11px baseline")
			}
		})
	}
	grid := readStyle("components/account-quality.css", ".quality-history-grid")
	if grid["display"] != "grid" || grid["grid-template-columns"] != "repeat(5, 20px)" {
		t.Fatal("probe history lost its five-cell grid")
	}
}

// Shared indicators are presentation inside an existing native button. This
// synthetic tree check cannot measure CSS size or substitute for browser QA.
const frontendIndicatorContractQueries = `
function indicatorNodes(tree,predicate){
 if(!tree||typeof tree!=="object")return [];
 return (predicate(tree)?[tree]:[]).concat((Array.isArray(tree)?tree:tree.props?.children||[]).flatMap(child=>indicatorNodes(child,predicate)));
}
function indicatorClass(tree,name){return indicatorNodes(tree,node=>(node.props?.className||"").split(" ").includes(name));}
function indicatorContract(tree,titleTerms,ariaTerms,{requirePill=true}={}){
 const buttons=indicatorNodes(tree,node=>node.type==="button");
 assert(buttons.length===1,"indicator must have exactly one native hit target");
 const button=buttons[0];
 assert(button.props.type==="button"&&!button.props.disabled&&typeof button.props.onClick==="function","read-only indicator lost its native action");
 for(const term of titleTerms)assert(button.props.title?.includes(term),"full hover meaning was lost: "+term);
 for(const term of ariaTerms)assert(button.props["aria-label"]?.includes(term),"full accessible meaning was lost: "+term);
 const pills=indicatorClass(button,"account-pill");
 if(requirePill)assert(pills.length>0,"indicator lost its shared inline presentation contract");
 for(const pill of pills)assert(pill.type==="span","inline indicator became an independent control");
 for(const node of indicatorNodes(button,node=>node!==button)){
  const props=node.props||{};
  assert(!["button","a","input","select","textarea","details","summary"].includes(node.type),"indicator contains a nested interactive element");
  assert(!["button","link"].includes(props.role)&&!(props.tabIndex!==undefined&&Number(props.tabIndex)>=0),"inline indicator introduced a second keyboard target");
  assert(!Object.keys(props).some(key=>/^on[A-Z]/.test(key)&&typeof props[key]==="function"),"indicator child installed an independent event handler");
 }
 return button;
}
`

func TestFrontendIndicatorOperationPreservesLongOutcomesAndHistory(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "account-operation")
	frontendPollingRun(t, vm, frontendIndicatorContractQueries)
	frontendPollingRun(t, vm, `
const base={id:"synthetic-indicator",email:"synthetic.long.operation@example.invalid",authorizationMethod:"chatgpt",status:"completed",operationStatus:"succeeded",canDownload:true,lastOperationType:"relogin",uploadAfterRelogin:true};
let opened=0;
for(const [patch,label,history] of [
 [{reloginUploadStatus:"unknown"},"授权成功 · 同步待核实","新授权文件可下载"],
 [{reloginUploadStatus:"skipped"},"授权成功 · 同步未执行","新授权文件可下载"],
 [{reloginUploadStatus:"failed"},"授权成功 · 同步失败","新授权文件可下载"],
 [{lastOperationType:"automatic_relogin",autoRepairPendingAccountIds:[17],autoRepairSkippedReason:"Synthetic excluded plan"},"授权已保存 · 自动回传暂停","待处理记录保留"],
 [{lastOperationType:"automatic_relogin",operationStatus:"skipped",autoRepairSkippedReason:"Synthetic excluded plan"},"自动重登已跳过","已有授权文件仍可下载或手动同步"],
 [{operationStatus:"interrupted"},"本次已中断","已有授权文件仍可下载"],
 [{totpRecoveryPending:true},"2FA 需人工处理","需人工处理"],
]){
 const job=Object.freeze({...base,...patch}),before=JSON.stringify(job);
 const tree=RepairUI.AccountOperationSummary({job,onToggleLogs(){opened++;}});
 const button=indicatorContract(tree,[label,history,"查看执行日志"],[label,history,job.email,"查看执行日志"]);
 const visibleLabel=({"授权已保存 · 自动回传暂停":"授权已存 · 回传暂停","本次已中断":"已中断"})[label]||label;
 assert(content(indicatorClass(button,"operation-status-pill")[0])===visibleLabel,"visual capsule lost the decisive outcome: "+label);
 assert(indicatorClass(button,"account-pill").length===1&&!indicatorClass(indicatorClass(button,"operation-method")[0],"account-pill").length,"saved identity competes with the decisive status as a second capsule");
 const heading=indicatorClass(button,"operation-summary-heading")[0],metadata=indicatorClass(button,"operation-summary-meta")[0],pill=indicatorClass(metadata,"operation-status-pill")[0],method=indicatorClass(heading,"operation-method")[0];
 assert(heading&&metadata&&pill&&method&&!indicatorClass(heading,"operation-status-pill").length&&!indicatorClass(metadata,"operation-method").length&&indicatorClass(heading,"operation-name").length===1,"long outcome or historical warning displaced the saved identity or escaped its dedicated result row");
 const prior=opened;button.props.onClick();assert(opened===prior+1,"operation capsule did not delegate exactly once");
 assert(JSON.stringify(job)===before,"operation capsule mutated authorization history");
}
assert(opened===7&&requests.length===0&&timers.size===0,"operation indicator performed work beyond delegated log disclosure");
`)
}

func TestFrontendIndicatorProbePreservesHistoricalAndUnknownMeaning(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendIndicatorContractQueries)
	frontendPollingRun(t, vm, `
const accountLabel="synthetic.history@example.invalid";let opened=0;
for(const snapshot of [undefined,{}, {lastAttempt:{status:"never"}}, {lastAttempt:{status:"failed",errorCode:"invalid_response"}}, {probeHistory:[]}]){
 assert(renderComponent("AccountQualitySummary",{snapshot,accountLabel,onOpen(){opened++;}})===null,"unqueried or unrecorded probe rendered an unknown placeholder");
}
const result={model:"synthetic-model",ruleVersion:"reasoning-six-v1",score:5,total:6,verdict:"normal",checkedAt:"2026-11-04T00:00:00Z"};
for(const state of ["eligible","active","queued"]){
 const snapshot={state,stale:true,staleReason:"expired",lastAttempt:{status:state==="eligible"?"success":state==="active"?"running":"queued"},lastResult:result},before=JSON.stringify(snapshot);
 const tree=renderComponent("AccountQualitySummary",{snapshot,accountLabel,onOpen(){opened++;}});
 const button=indicatorContract(tree,["5/6","已过期"],[accountLabel,"查看推理探针详情"],{requirePill:false});
 assert(indicatorClass(button,"quality-history-cell").length===1&&!content(button).includes("未知"),"history count invented attempts or restored unknown text");
 assert(button.props["aria-busy"]===(state==="active")&&indicatorClass(button,"quality-history-activity").length===(state==="eligible"?0:1),"live probe activity replaced or expanded completed history");
 button.props.onClick();assert(JSON.stringify(snapshot)===before,"history viewing modified stored results");
}
assert(opened===3&&requests.length===0&&timers.size===0,"history presentation submitted model work");unmount();
`)
}

func TestFrontendIndicatorProbeResultPreservesIndependentMeasuredScoreAndVerdict(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, frontendIndicatorContractQueries)
	frontendPollingRun(t, vm, `
const accountLabel="synthetic.aggregate@example.invalid",baseResult={model:"synthetic-model",ruleVersion:"reasoning-six-v1",total:6,checkedAt:"2026-11-04T00:00:00Z",durationMs:3200};let opened=0;
for(let score=0;score<=6;score++){
 const snapshot={state:"eligible",persisted:true,lastAttempt:{status:"success"},lastResult:{...baseResult,score,verdict:score>=5?"normal":"degraded"}},before=JSON.stringify(snapshot);
 const tree=renderComponent("AccountQualitySummary",{snapshot,accountLabel,onOpen(){opened++;}});
 const button=indicatorContract(tree,[score+"/6"],[score+"/6",accountLabel,"查看推理探针详情"],{requirePill:false});
 const cells=indicatorClass(button,"quality-history-cell");
 assert(cells.length===1&&cells[0].props["data-tone"]===(score>=5?"success":"warning"),"historical score changed its measured outcome or expanded into multiple attempts");
 assert(cells[0].props.role==="img"&&cells[0].props["aria-label"].includes(score+"/6"),"grid cell relied on color alone or lost its true denominator");
 assert(!indicatorNodes(button,node=>node.type==="progress"||node.props?.role==="progressbar").length&&!indicatorClass(button,"quality-score").length,"probe grid was treated as quota progress or retained the old text display");
 button.props.onClick();assert(JSON.stringify(snapshot)===before,"history interaction changed source data");
}
assert(opened===7&&requests.length===0&&timers.size===0,"history grid triggered model calls");unmount();
`)
}

func TestFrontendIndicatorUsagePreservesWindowsAndReadOnlyDetails(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-usage.jsx")
	frontendPollingRun(t, vm, frontendIndicatorContractQueries)
	frontendPollingRun(t, vm, `
const now=Date.parse("2026-11-04T12:00:00Z"),observedAt="2026-11-03T12:00:00Z",accountLabel="synthetic.long.usage@example.invalid";let opened=0;
for(const refreshState of ["idle","queued","active"]){
 const snapshot={refreshState,persisted:true,lastAttempt:{status:"failed",startedAt:"2026-11-04T12:00:00Z"},usage:{observedAt,windows:[{name:"primary",windowSeconds:18000,usedPercent:0,resetsAt:"2026-11-04T13:00:00Z"},{name:"secondary",windowSeconds:604800,usedPercent:null}]},errors:[{component:"usage",code:"http_error",httpStatus:401,message:"Synthetic diagnostic remains in detail"}]};
 const before=JSON.stringify(snapshot),tree=renderComponent("AccountInsightSummary",{snapshot,onOpen(){opened++;},accountLabel,now});
 const meaning=["5 小时窗口已用 0%","HTTP 401","11-04","查看用量与状态"];
 const button=indicatorContract(tree,meaning,[...meaning,accountLabel],{requirePill:false});
 const time=indicatorNodes(indicatorClass(button,"usage-next-reset")[0],node=>node.type==="time")[0];
 assert(content(button).includes("0%")&&!content(button).includes("未知")&&content(button).includes("HTTP 401")&&time?.props.dateTime==="2026-11-04T13:00:00Z","known quota, HTTP or reset disappeared with unknown window");
 const bars=indicatorNodes(button,node=>node.type==="progress"),windows=indicatorClass(button,"usage-window");
 assert(bars.length===1&&bars[0].props.value===0&&windows.length===1&&indicatorClass(windows[0],"usage-window-label")[0].props.title==="5 小时窗口","missing usage became zero or still occupied a window");
 assert(!content(button).includes("7 天")&&!indicatorClass(button,"usage-extra-count").length,"unknown window counted toward visible history");
 assert(button.props["aria-busy"]===(refreshState==="active"),"queued state was treated as active execution");
 if(refreshState!=="idle")assert(button.props["aria-label"].includes(refreshState==="active"?"本次查询尚未结束，保留上次用量":"本次查询尚未开始，保留上次用量"),"live state lost retained-data meaning");
 button.props.onClick();assert(JSON.stringify(snapshot)===before,"usage presentation mutated observations");
}
assert(opened===3&&requests.length===0&&timers.size===0,"usage presentation queried upstream");unmount();
`)
}

func TestFrontendIndicatorUsageEmptyStatesDoNotInventWindowsOrZero(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-usage.jsx")
	frontendPollingRun(t, vm, frontendIndicatorContractQueries)
	frontendPollingRun(t, vm, `
const accountLabel="synthetic.unmeasured@example.invalid";let opened=0;
for(const snapshot of [undefined,{}, {bps:{allowed:null}}, {usage:{windows:[{usedPercent:null,windowSeconds:18000}]}}]){
 const tree=renderComponent("AccountInsightSummary",{snapshot,accountLabel,onOpen(){opened++;}});
 const empty=indicatorClass(tree,"usage-placeholder"),track=indicatorClass(tree,"usage-track-empty");
 assert(empty.length===1&&track.length===1&&indicatorClass(empty[0],"metric-state-label").length===1&&content(indicatorClass(empty[0],"metric-state-label")[0]).trim(),"unmeasured usage lost its explicit description or static empty track");
 assert(!indicatorNodes(tree,node=>node.type==="progress"||node.props?.["aria-valuenow"]!==undefined).length&&!content(tree).includes("0%")&&!content(tree).includes("未知"),"empty track fabricated a numeric or unknown value");
 tree.props.onClick();
}
for(const [allowed,label] of [[true,"BPS 权限可用"],[false,"BPS 权限受限"]]){
 const tree=renderComponent("AccountInsightSummary",{snapshot:{bps:{allowed}},accountLabel,onOpen(){opened++;}});
 const button=indicatorContract(tree,[label],[label,accountLabel,"查看用量与状态"],{requirePill:false});
 assert(!indicatorClass(button,"usage-window").length&&!indicatorNodes(button,node=>node.type==="progress").length&&!content(button).includes("0%")&&!content(button).includes("未知"),"known permission invented quota or unknown placeholder");
 assert(!indicatorClass(button,"usage-next-reset").length&&!indicatorNodes(button,node=>node.type==="time").length,"permission-only record invented a quota reset");button.props.onClick();
}
assert(opened===6&&requests.length===0&&timers.size===0,"empty/permission presentation submitted a query");unmount();
`)
}
