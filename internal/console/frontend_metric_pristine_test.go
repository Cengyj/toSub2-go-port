package console

import "testing"

func TestFrontendMetricPristineUsageRequiresExplicitNeverEvidence(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const base={id:"synthetic",lastAttempt:{queryId:"",status:"never"},refreshState:"eligible",canQuery:true,persisted:false,stale:true,errors:[]};
for(const patch of [{},{canQuery:false,refreshState:"unavailable",unavailableReason:"账号查询功能未启用"},{automatic:true,nextCheckAt:"2026-11-07T01:00:00Z"},{persisted:true,components:{}},{lastAttempt:{status:"never",message:"当前授权尚未查询"}},{usage:null,bps:null,subscription:null,errors:null}]){
 const snapshot={...base,...patch},before=JSON.stringify(snapshot);Object.freeze(snapshot.lastAttempt);Object.freeze(snapshot);
 assert(Browser.insightNeverQueried(snapshot)===true,"valid never snapshot was rejected: "+before);
 assert(JSON.stringify(snapshot)===before,"never-query classification mutated its input");
}
for(const snapshot of [undefined,null,false,0,"never",[],{}, {lastAttempt:[]},{lastAttempt:{}}, {lastAttempt:{status:""}}]) assert(Browser.insightNeverQueried(snapshot)===false,"missing or malformed state became a never-query claim");
for(const status of ["queued","pending","running","checking","success","partial","failed","cancelled","canceled","skipped","interrupted","unrecognized"]) assert(!Browser.insightNeverQueried({...base,lastAttempt:{status}}),"attempt was relabeled never: "+status);
for(const patch of [
 {refreshState:"active"},{refreshState:"queued"},{refreshState:"coolingDown"},{refreshState:"loading"},{refreshState:"future-state"},{state:"loading"},
 {lastAttempt:{status:"never",queryId:"accepted"}},{lastAttempt:{status:"never",startedAt:"not-a-date"}},{lastAttempt:{status:"never",finishedAt:"2026-11-07T00:00:00Z"}},
 {lastAttempt:{status:"never",errorCode:"cache_unavailable"}},{lastAttempt:{status:"never",message:"合成缓存无法读取"}},{lastAttempt:{status:"never",httpStatus:502}},
 {usage:{windows:[{usedPercent:0}]}},{usage:{}},{bps:{allowed:false}},{subscription:{}},{httpObservation:{source:"query",status:"success",httpStatus:200}},
 {lastSuccessAt:"2026-11-07T00:00:00Z"},{nextAllowedAt:"2026-11-07T00:00:00Z"},{errors:[{code:"timeout"}]},{errors:{}},{attemptedComponents:["usage"]},{attemptedComponents:"usage"},
 {components:{usage:{consecutiveFailures:0}}},{components:[]},
 {loading:true},{isLoading:true},{uncertain:true},{verificationRequired:true},{recoveryPending:true},{persistenceUnconfirmed:true},
 {error:"unavailable"},{lastError:"unavailable"},{persistenceWarning:"unverified"}
]){
 const snapshot={...base,...patch},before=JSON.stringify(snapshot);
 assert(Browser.insightNeverQueried(snapshot)===false,"observation, activity or uncertainty became never-query: "+JSON.stringify(patch));
 assert(JSON.stringify(snapshot)===before,"query classifier mutated retained evidence");
}
`)
}

func TestFrontendMetricPristineProbeRejectsHistoryAndManualObservations(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
const base={id:"synthetic",lastAttempt:{checkId:"",source:"",status:"never"},state:"eligible",canCheck:true,persisted:false,stale:true};
for(const patch of [{},{canCheck:false,state:"unavailable",unavailableReason:"模型检测未启用"},{persisted:true,probeHistory:[]},{lastAttempt:{status:"never",source:"manual",httpStatus:0}},{lastResult:null,httpObservation:null,probeHistory:null}]){
 const snapshot={...base,...patch},before=JSON.stringify(snapshot);Object.freeze(snapshot.lastAttempt);Object.freeze(snapshot);
 assert(Browser.qualityNeverChecked(snapshot)===true,"valid never probe was rejected: "+before);
 assert(JSON.stringify(snapshot)===before,"never-probe classification mutated input");
}
for(const snapshot of [undefined,null,false,0,"never",[],{}, {lastAttempt:[]},{lastAttempt:{}}, {lastAttempt:{status:""}}]) assert(Browser.qualityNeverChecked(snapshot)===false,"missing or malformed state became a never-probe claim");
for(const status of ["queued","running","success","failed","cancelled","canceled","skipped","interrupted","unrecognized"]) assert(!Browser.qualityNeverChecked({...base,lastAttempt:{status}}),"probe attempt was relabeled never: "+status);
for(const patch of [
 {state:"active"},{state:"queued"},{state:"coolingDown"},{state:"loading"},{state:"future-state"},{refreshState:"active"},
 {lastAttempt:{status:"never",checkId:"accepted"}},{lastAttempt:{status:"never",startedAt:"not-a-date"}},{lastAttempt:{status:"never",finishedAt:"2026-11-07T00:00:00Z"}},
 {lastAttempt:{status:"never",errorCode:"cache_unavailable"}},{lastAttempt:{status:"never",message:"合成保存结果无法核实"}},{lastAttempt:{status:"never",httpStatus:200}},
 {lastResult:{score:0,total:1,verdict:"degraded"}},{lastResult:{}},{probeHistory:[{kind:"pelican",status:"success"}]},{probeHistory:[null]},{probeHistory:{}},
 {httpObservation:{source:"pelican",status:"success",httpStatus:200}},{httpObservation:{source:"pelican",status:"failed",httpStatus:429}},{httpObservation:{}},
 {nextAllowedAt:"2026-11-07T00:00:00Z"},{staleReason:"authorization_changed"},{errors:[{code:"timeout"}]},{errors:{}},
 {loading:true},{isLoading:true},{uncertain:true},{verificationRequired:true},{recoveryPending:true},{persistenceUnconfirmed:true},
 {error:"unavailable"},{lastError:"unavailable"},{persistenceWarning:"unverified"}
]){
 const snapshot={...base,...patch},before=JSON.stringify(snapshot);
 assert(Browser.qualityNeverChecked(snapshot)===false,"probe evidence, activity or uncertainty became never-check: "+JSON.stringify(patch));
 assert(JSON.stringify(snapshot)===before,"probe classifier mutated retained evidence");
}
`)
}
