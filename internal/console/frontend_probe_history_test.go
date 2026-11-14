package console

import "testing"

func TestFrontendProbeHistoryKeepsFiveNewestCompletedObservationsInTimeOrder(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const entries=Array.from({length:8},(_,index)=>({id:"probe-"+index,kind:index%2?"pelican":"candy",status:"success",verdict:"normal",score:1,total:1,model:"synthetic-"+index,finishedAt:"2026-11-06T01:00:00.00000000"+index+"Z"}));
const snapshot={probeHistory:[...entries].reverse()},before=JSON.stringify(snapshot);
const history=Browser.qualityProbeHistory(snapshot);
assert(history.length===5&&history.map(item=>item.id).join()==="probe-3,probe-4,probe-5,probe-6,probe-7","history truncation used input order, HTTP order, or lost nanosecond precision");
assert(history.every(item=>item.title.includes("synthetic-")&&item.title.includes("2026-")),"historical identity or time disappeared from tooltips");
assert(JSON.stringify(snapshot)===before,"history sorting mutated server snapshot");
const duplicate={...entries[7],status:"failed",errorCode:"http_error",httpStatus:503,finishedAt:"2026-11-06T01:00:01Z"};
const combined=Browser.qualityProbeHistory({probeHistory:[duplicate,...entries,{...entries[7],kind:"candy"}]});
assert(combined.length===5&&combined.filter(item=>item.id===duplicate.id&&item.kind===duplicate.kind).length===1,"duplicate attempt consumed multiple history cells");
assert(combined.at(-1).status==="failed"&&combined.at(-1).tone==="danger","older duplicate replaced newer outcome");
assert(combined.some(item=>item.id===duplicate.id&&item.kind==="candy"),"same id in distinct probe kinds was collapsed");
`)
}

func TestFrontendProbeHistoryFiltersKindBeforeChoosingNewestFive(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const candy=Array.from({length:7},(_,index)=>({id:"candy-"+index,kind:"candy",status:"success",verdict:"normal",score:1,total:1,model:"candy-model-"+index,finishedAt:"2026-11-06T01:00:00.00000000"+index+"Z"}));
const drawing=Array.from({length:7},(_,index)=>({id:"drawing-"+index,kind:"pelican",status:"success",model:"drawing-model-"+index,finishedAt:"2026-11-06T01:00:01.00000000"+index+"Z"}));
const snapshot={probeHistory:[...drawing,...candy].reverse()},before=JSON.stringify(snapshot);
const filtered=Browser.qualityProbeHistory(snapshot,"candy");
assert(filtered.length===5&&filtered.map(item=>item.id).join()==="candy-2,candy-3,candy-4,candy-5,candy-6","newer drawing records evicted candy history or kind filtering happened after truncation");
assert(filtered.every(item=>item.kind==="candy"&&item.measured&&item.label==="通过"),"candy filtering fabricated results from completed drawings");
const combined=Browser.qualityProbeHistory(snapshot);
assert(combined.length===5&&combined.map(item=>item.id).join()==="drawing-2,drawing-3,drawing-4,drawing-5,drawing-6","optional kind filter changed the default combined history");
const duplicate={...candy[6],status:"failed",errorCode:"http_error",httpStatus:503,finishedAt:"2026-11-06T01:00:02Z"};
const deduplicated=Browser.qualityProbeHistory({probeHistory:[duplicate,...snapshot.probeHistory,{...duplicate,kind:"pelican"}]},"candy");
assert(deduplicated.length===5&&deduplicated.map(item=>item.id).join()==="candy-2,candy-3,candy-4,candy-5,candy-6"&&deduplicated.at(-1).status==="failed"&&deduplicated.at(-1).tone==="danger","kind filtering lost newest duplicate outcome or mixed probe identity");
assert(Browser.qualityProbeHistory({probeHistory:drawing},"candy").length===0&&Browser.qualityProbeHistory({probeHistory:[]},"candy").length===0,"drawing-only or empty history invented a candy attempt");
const legacy={lastResult:{verdict:"degraded",score:0,total:1,model:"legacy-candy",checkedAt:"2026-11-05T00:00:00Z"},lastAttempt:{status:"success"}};
assert(Browser.qualityProbeHistory(legacy,"candy").length===1&&Browser.qualityProbeHistory(legacy,"candy")[0].label==="答案不符"&&Browser.qualityProbeHistory(legacy,"pelican").length===0,"kind filtering lost the sole real legacy candy result or relabeled it as drawing");
const oldMeasurement={id:"old-six-question-measurement",kind:"candy",status:"success",verdict:"normal",score:6,total:6,model:"legacy-six-question-model",finishedAt:"2026-11-06T03:00:00Z"};
assert(Browser.qualityProbeHistory({probeHistory:[oldMeasurement]},"candy").length===0,"old six-question success was presented as one candy pass");
const oldCombined=Browser.qualityProbeHistory({probeHistory:[oldMeasurement]});
assert(oldCombined.length===1&&oldCombined[0].measured&&oldCombined[0].title.includes("6/6 题正确"),"candy-only filtering erased an existing full-suite result from the combined detail history");
assert(Browser.qualityProbeHistory({lastResult:{verdict:"normal",score:6,total:6,model:"legacy-six-question-model",checkedAt:oldMeasurement.finishedAt},lastAttempt:{status:"success"}},"candy").length===0,"legacy fallback relabeled a six-question measurement as candy-only success");
assert(JSON.stringify(snapshot)===before,"kind filtering mutated server evidence");
`)
}

func TestFrontendProbeHistoryFiltersInvalidRecordsWithoutInventingAttempts(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const valid={id:"safe",kind:"candy",status:"success",verdict:"normal",score:1,total:1,finishedAt:"2026-11-06T00:00:00Z"};
for(const entry of [null,[],{}, {...valid,id:""},{...valid,kind:"unrecognized"},{...valid,status:"running"},{...valid,status:"queued"},{...valid,status:"unknown"},{...valid,finishedAt:undefined},{...valid,finishedAt:"not-a-date"},{...valid,score:null},{...valid,score:-1},{...valid,score:2},{...valid,total:0},{...valid,total:1.5},{...valid,verdict:"unknown"},{...valid,score:0},{...valid,verdict:"degraded"},{...valid,httpStatus:403},{...valid,httpStatus:503},{...valid,kind:"pelican",httpStatus:302}]){
 assert(Browser.qualityProbeHistory({probeHistory:[entry]}).length===0,"unconfirmed input became historical probe: "+JSON.stringify(entry));
}
const secret="SYNTHETIC_PRIVATE_RESPONSE_NOT_FOR_PRESENTATION";
const safe=Browser.qualityProbeHistory({probeHistory:[{...valid,model:" synthetic-model ",message:secret,rawResponse:secret,html:secret,credentials:secret}]});
assert(safe.length===1&&safe[0].title.includes("模型：synthetic-model")&&!JSON.stringify(safe).includes(secret),"history copied private fields into public presentation");
assert(Browser.qualityProbeHistory({probeHistory:[]}).length===0&&Browser.qualityProbeHistory().length===0,"missing history was padded with empty cells");
`)
}

func TestFrontendProbeHistoryRejectsContradictoryScoresInExplicitAndLegacyCandyRecords(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const base={id:"single-candy",kind:"candy",status:"success",total:1,finishedAt:"2026-11-06T00:00:00Z"};
for(const [verdict,score,valid] of [["normal",1,true],["degraded",0,true],["normal",0,false],["degraded",1,false]]){
 const entry={...base,verdict,score},result={verdict,score,total:1,checkedAt:base.finishedAt};
 for(const snapshot of [{probeHistory:[entry]},{lastResult:result,lastAttempt:{status:"success",checkId:base.id}}]){
  for(const kind of ["","candy"]){
   const history=Browser.qualityProbeHistory(snapshot,kind);
   assert(history.length===Number(valid),"contradictory score/verdict became a history cell: "+JSON.stringify({verdict,score,kind}));
   if(valid)assert(history[0].label===(score===1?"通过":"答案不符")&&history[0].tone===(score===1?"success":"warning"),"consistent candy result lost its outcome");
  }
 }
}
const mixed={probeHistory:[{...base,verdict:"normal",score:1},{...base,id:"contradictory",verdict:"normal",score:0,finishedAt:"2026-11-06T00:01:00Z"}]};
assert(Browser.qualityProbeHistory(mixed,"candy").map(item=>item.id).join()===base.id,"contradictory newer record consumed a cell or erased confirmed evidence");
// The previous six-question suite passed at five correct answers; only the
// current single-candy rule equates a normal verdict with a perfect score.
const previousSuite={...base,id:"old-threshold-pass",verdict:"normal",score:5,total:6};
for(const snapshot of [{probeHistory:[previousSuite]},{lastResult:{...previousSuite,checkedAt:base.finishedAt},lastAttempt:{status:"success",checkId:previousSuite.id}}]){
 const combined=Browser.qualityProbeHistory(snapshot);
 assert(combined.length===1&&combined[0].tone==="success"&&combined[0].title.includes("5/6 题正确"),"single-candy consistency validation erased a valid prior suite threshold pass");
 assert(Browser.qualityProbeHistory(snapshot,"candy").length===0,"prior suite threshold pass became a single-candy history cell");
}
`)
}

func TestFrontendProbeHistoryColorsSeparateScoresDrawingFailuresAndUnknownResponses(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const base={id:"synthetic",kind:"candy",status:"failed",finishedAt:"2026-11-06T00:00:00Z"};
for(const [patch,tone,label,measured] of [
 [{status:"success",verdict:"normal",score:1,total:1},"success","通过",true],
 [{status:"success",verdict:"degraded",score:0,total:1},"warning","答案不符",true],
 [{kind:"pelican",status:"success"},"success","绘图完成",false],
 [{httpStatus:403,errorCode:"forbidden"},"danger","检测失败",false],
 [{kind:"pelican",httpStatus:503,errorCode:"http_error"},"danger","检测失败",false],
 [{httpStatus:429,errorCode:"http_error"},"warning","请求限流",false],
 [{errorCode:"rate_limited"},"warning","请求限流",false],
 [{httpStatus:403,errorCode:"rate_limited"},"danger","检测失败",false],
 [{httpStatus:200,errorCode:"invalid_response"},"warning","结果未确认",false],
 [{httpStatus:200,errorCode:"unexpected_content_type"},"warning","结果未确认",false],
 [{httpStatus:200,errorCode:"invalid_stream"},"warning","结果未确认",false],
 [{httpStatus:200,errorCode:"invalid_answer_format"},"warning","结果未确认",false],
 [{errorCode:"persistence_failed"},"warning","结果未确认",false],
 [{status:"cancelled"},"neutral","已取消",false],
]){
 const [item]=Browser.qualityProbeHistory({probeHistory:[{...base,...patch}]});
 assert(item&&item.tone===tone&&item.label===label&&item.measured===measured,"probe outcome colors or evidence were conflated: "+JSON.stringify(patch));
 assert(!item.title.includes("0/0")&&(!measured||item.title.includes(patch.score+"/"+patch.total+" 题正确")),"real zero disappeared or unmeasured failure gained score");
 if(patch.kind==="pelican")assert(item.title.includes("鹈鹕绘图")&&!item.title.includes("题正确"),"drawing completion became candy score");
}
`)
}

func TestFrontendProbeHistoryLegacyCompatibilityDoesNotReconstructFiveRuns(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const result={verdict:"degraded",score:0,total:1,model:"legacy-model",checkedAt:"2026-11-05T00:00:00Z"};
const known={lastResult:result,lastAttempt:{checkId:"legacy-result",status:"success"}};
const [measured]=Browser.qualityProbeHistory(known);
assert(measured&&measured.id==="legacy-result"&&measured.tone==="warning"&&measured.title.includes("0/1 题正确"),"legacy measured zero was replaced with empty or fabricated history");
assert(Browser.qualityProbeHistory({...known,probeHistory:[]}).length===0,"authoritative empty history was repopulated from unrelated legacy fields");
const failed={...known,lastAttempt:{checkId:"legacy-failure",status:"failed",errorCode:"timeout",httpStatus:503,startedAt:"2026-11-05T23:59:00Z",finishedAt:"2026-11-06T00:00:00Z"}};
const records=Browser.qualityProbeHistory(failed);
assert(records.length===1&&records[0].id==="legacy-failure"&&records[0].tone==="danger"&&!records[0].measured,"legacy failure was hidden behind old score or falsely expanded into multi-run history");
assert(Browser.qualityProbeHistory({lastAttempt:{status:"failed",errorCode:"timeout"}}).length===0,"undated failure fabricated a completed run");
for(const patch of [{startedAt:undefined},{startedAt:"2026-11-07T00:00:00Z"},{httpStatus:undefined},{httpStatus:0},{httpStatus:99},{httpStatus:600}]){
 const attempt={...failed.lastAttempt,...patch};
 assert(Browser.qualityProbeHistory({lastAttempt:attempt}).length===0,"unproven legacy attempt fabricated a completed request");
 const fallback=Browser.qualityProbeHistory({...known,lastAttempt:attempt});
 assert(fallback.length===1&&fallback[0].measured&&fallback[0].tone==="warning","unproven failure hid the sole actual legacy measurement");
}
assert(Browser.qualityProbeHistory({lastAttempt:{status:"success",finishedAt:"2026-11-06T00:00:00Z"}}).length===0,"bare successful transport fabricated a measured candy result");
assert(Browser.qualityProbeHistory({...known,state:"active",lastAttempt:{status:"running"}}).length===1,"pending work erased confirmed history or generated another completed run");
`)
}
