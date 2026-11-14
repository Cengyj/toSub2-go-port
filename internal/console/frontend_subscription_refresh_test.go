package console

import (
	"github.com/grafana/sobek"
	"testing"
)

func frontendSubscriptionHookVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendInsightsVM(t, "hooks/accountSubscription.js")
	frontendSubscriptionDisplayIntoVM(t, vm)
	frontendPollingRun(t, vm, `
var subscriptionJob={id:"synthetic-subscription",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"free",canDownload:true,canRefreshSubscription:true};
var updates=[],verifications=0;
var subscriptionOptions={token:"synthetic-token",enabled:true,jobs:[subscriptionJob],onUpdated(job){updates.push(job);},onVerify(){verifications++;}};
function renderSubscription(value=subscriptionOptions){hookCursor=0;const result=Insights.useAccountSubscription(value);for(const run of effectQueue.splice(0))run();return result;}
function subscriptionReply(plan="plus") {
 const source="synthetic-subscription-source",observedAt="2026-11-06T01:02:03Z";
 return {job:{...subscriptionJob,planType:plan,subscriptionPlanCheckedAt:observedAt,subscriptionPlanSource:source,subscriptionPlanStale:false},subscription:{planType:plan,source,observedAt,identityVerified:true,reported:false,scope:"account",accountId:"synthetic-account",expiresAt:null},warnings:[]};
}
`)
	return vm
}

func TestFrontendSubscriptionRefreshIsExplicitAndFailsClosed(t *testing.T) {
	vm := frontendSubscriptionHookVM(t)
	frontendPollingRun(t, vm, `
var hook=renderSubscription(),oldRefresh=hook.refresh;
assert(requests.length===0&&timers.size===0,"mounting controls submitted or polled a query");
for(const overrides of [{enabled:false},{token:""},{jobs:[{...subscriptionJob,canRefreshSubscription:undefined}]},{jobs:[{...subscriptionJob,canRefreshSubscription:false,subscriptionRefreshUnavailableReason:"2FA恢复中"}]}]) {
 subscriptionOptions={...subscriptionOptions,token:"synthetic-token",enabled:true,jobs:[subscriptionJob],...overrides};hook=renderSubscription();oldRefresh(subscriptionJob);
}
assert(requests.length===0,"stale handler bypassed feature, token, or server capability guard");
assert(renderSubscription().states[subscriptionJob.id]?.error==="2FA恢复中","server safety reason was replaced with a login instruction");
unmount();assert(timers.size===0,"disabled controls leaked timers");
`)
}

func TestFrontendSubscriptionRefreshDeduplicatesAndAdoptsVerifiedResult(t *testing.T) {
	vm := frontendSubscriptionHookVM(t)
	frontendPollingRun(t, vm, `
var hook=renderSubscription(),result,duplicate;hook.refresh(subscriptionJob).then(value=>result=value);hook.refresh(subscriptionJob).then(value=>duplicate=value);
assert(requests.length===1&&requests[0].url==="/api/jobs/synthetic-subscription/refresh-subscription","double click submitted twice or used a login/insights endpoint");
assert(requests[0].options.method==="POST"&&requests[0].options.body==="{}","action sent credentials or extra workflow options");
assert(renderSubscription().states[subscriptionJob.id].busy,"query did not expose its in-flight state");ok(requests[0],{...subscriptionReply(),warnings:["合成本地提醒",{component:"subscription",message:"套餐已读取，到期接口暂不可用",code:"upstream_error",privateResponse:"must-not-display"},{code:"expiry_unavailable"}]});
`)
	frontendPollingRun(t, vm, `
hook=renderSubscription();assert(updates.length===1&&updates[0].planType==="plus"&&result.subscription.identityVerified&&duplicate===null,"verified result was lost or duplicated");
assert(!hook.states[subscriptionJob.id].busy&&!hook.states[subscriptionJob.id].error&&hook.states[subscriptionJob.id].warnings[0]==="合成本地提醒","success lost observation or warnings");
assert(hook.states[subscriptionJob.id].warnings.length===3&&hook.states[subscriptionJob.id].warnings[1].includes("到期接口暂不可用")&&hook.states[subscriptionJob.id].warnings[2].includes("expiry_unavailable")&&!JSON.stringify(hook.states[subscriptionJob.id].warnings).includes("must-not-display"),"structured partial-success warning was lost or exposed extra fields");
assert(Browser.subscriptionDisplayPlan(updates[0],hook.states[subscriptionJob.id])==="plus","optional endpoint warning hid a verified subscription");
assert(subscriptionJob.planType==="free"&&requests.length===1&&timers.size===0,"hook mutated caller data, retried, or leaked timer");unmount();
`)
}

func TestFrontendSubscriptionRefreshFailureAndTimeoutNeverRelogin(t *testing.T) {
	for _, scenario := range []string{"unauthorized", "timeout", "network", "unmount"} {
		t.Run(scenario, func(t *testing.T) {
			vm := frontendSubscriptionHookVM(t)
			if err := vm.Set("scenario", scenario); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var hook=renderSubscription();hook.refresh(subscriptionJob);
if(scenario==="unauthorized")requests[0].resolve({ok:false,status:401,json:async()=>({error:"已保存授权不可用；未重新登录"})});
if(scenario==="timeout")fire(30000);
if(scenario==="network")requests[0].reject(new Error("synthetic connection loss"));
if(scenario==="unmount")unmount();
`)
			frontendPollingRun(t, vm, `
hook=renderSubscription();assert(updates.length===0&&subscriptionJob.planType==="free"&&requests.length===1,"failure changed known plan or started automatic authentication/retry");
if(scenario!=="unmount"){assert(!hook.states[subscriptionJob.id].busy&&hook.states[subscriptionJob.id].error,"failure left an endless spinner or hid the error");assert(Browser.subscriptionDisplayPlan(subscriptionJob,hook.states[subscriptionJob.id])==="unknown","failed query continued to present the saved plan as current");}
if(scenario==="timeout"||scenario==="unmount")assert(requests[0].options.signal.aborted,"query was not aborted");ok(requests[0],subscriptionReply());
`)
			frontendPollingRun(t, vm, `assert(updates.length===0&&requests.length===1&&timers.size===0,"late canceled result was adopted or retried");unmount();`)
		})
	}
}

func TestFrontendSubscriptionRefreshRejectsUnverifiedOrMismatchingResponses(t *testing.T) {
	for _, scenario := range []string{"account", "email", "method", "unverified", "unknown-plan", "conflicting-plan", "time", "source"} {
		t.Run(scenario, func(t *testing.T) {
			vm := frontendSubscriptionHookVM(t)
			if err := vm.Set("scenario", scenario); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var hook=renderSubscription();hook.refresh(subscriptionJob);var result=subscriptionReply();
if(scenario==="account")result.job.id="different-account";
if(scenario==="email")result.job.email="different@example.invalid";
if(scenario==="method")result.job.authorizationMethod="bps";
if(scenario==="unverified")result.subscription.identityVerified=false;
if(scenario==="unknown-plan")result.subscription.planType="unrecognized";
if(scenario==="conflicting-plan")result.job.planType="free";
if(scenario==="time")result.job.subscriptionPlanCheckedAt="2026-11-01T00:00:00Z";
if(scenario==="source")result.job.subscriptionPlanSource="unrelated-source";ok(requests[0],result);
`)
			frontendPollingRun(t, vm, `assert(updates.length===0&&renderSubscription().states[subscriptionJob.id].error&&requests.length===1,"untrusted response replaced known plan or retried");assert(subscriptionJob.planType==="free"&&Browser.subscriptionDisplayPlan(subscriptionJob,renderSubscription().states[subscriptionJob.id])==="unknown","HTTP 200 semantic failure changed saved metadata or retained the displayed plan");unmount();`)
		})
	}
}

func TestFrontendSubscriptionRefreshPersistenceUncertainReconcilesWithoutReplay(t *testing.T) {
	for _, matching := range []bool{true, false} {
		vm := frontendSubscriptionHookVM(t)
		if err := vm.Set("matching", matching); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `
var hook=renderSubscription();hook.refresh(subscriptionJob);var pending=subscriptionReply().job;if(!matching)pending.id="different-account";
requests[0].resolve({ok:false,status:503,json:async()=>({error:"synthetic fsync failure",code:"persistence_uncertain",outcome:"unknown",job:pending})});
`)
		frontendPollingRun(t, vm, `
hook=renderSubscription();const subscriptionState=hook.states[subscriptionJob.id];
assert(subscriptionState.uncertain&&!subscriptionState.busy&&subscriptionState.error.includes("套餐已查询，本地保存待核实"),"uncertain persistence was presented as unchanged or confirmed success");
assert(matching?updates.length===1&&updates[0].planType==="plus"&&verifications===0:updates.length===0&&verifications===1,"uncertain result neither adopted matching job nor requested safe local verification");
assert(Browser.subscriptionDisplayPlan(matching?updates[0]:subscriptionJob,subscriptionState)==="unknown"&&subscriptionJob.planType==="free","uncertain save was presented as a confirmed plan or mutated caller data");
assert(requests.length===1&&timers.size===0,"uncertain persistence repeated the POST");unmount();
`)
	}
}

func TestFrontendSubscriptionRowKeepsColumnsAndUsesIndependentAction(t *testing.T) {
	vm := frontendSubscriptionComponentVM(t, "accounts")
	frontendPollingRun(t, vm, `
var refreshes=0,relogins=0,insightRefreshes=0;
var job={id:"synthetic-subscription",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"free",status:"completed",canDownload:true,canRefreshSubscription:true};
var props={job,token:"synthetic",subscriptionAvailable:true,insightEnabled:false,onRefreshSubscription(){refreshes++;},onRelogin(){relogins++;},onRefreshInsights(){insightRefreshes++;}};
var tree=renderSubscriptionComponent("JobRow",props),button=findPlanBadge(tree);
assert(button?.type==="button"&&!button.props.disabled&&button.props["aria-label"].includes(job.email)&&content(button)==="Free","plan capsule depends on insights enablement or lost its plan/account label");
assert(tree.type==="tr"&&tree.props.children.filter(child=>child?.type==="td").length===8,"subscription entry added a table column");
assert(!findButton(tree,"刷新套餐")&&!findMatching(tree,node=>node.props?.className==="account-plan-observation"),"row retained an independent refresh button or query-time line");button.props.onClick();
assert(refreshes===1&&relogins===0&&insightRefreshes===0&&requests.length===0,"plan action reused login or broad insight query");
for(const extra of [{subscriptionAvailable:false},{onRefreshSubscription:undefined},{job:{...job,canRefreshSubscription:undefined}},{job:{...job,canRefreshSubscription:false,subscriptionRefreshUnavailableReason:"2FA恢复中"}},{subscriptionRefreshState:{busy:true}}]) {
 tree=renderSubscriptionComponent("JobRow",{...props,...extra});button=findPlanBadge(tree);
 const staticBadge=extra.subscriptionAvailable===false||Object.hasOwn(extra,"onRefreshSubscription");
 assert(staticBadge?button?.type==="span"&&!button.props.onClick:button?.type==="button"&&button.props.disabled&&!button.props.onClick,"feature-off, legacy, blocked, or busy account exposed actionable refresh");
 assert(content(button)==="Free"&&!findButton(tree,"刷新套餐"),"availability state replaced the plan text or restored an independent action");
}
tree=renderSubscriptionComponent("JobRow",{...props,job:{...job,planType:"plus",subscriptionPlanCheckedAt:"2026-11-06T01:02:03Z",subscriptionPlanSource:"synthetic",subscriptionPlanStale:true},subscriptionRefreshState:{error:"合成查询失败",warnings:["合成长文辅助提醒"]}});
assert(content(findPlanBadge(tree))==="Unknown"&&!content(tree).includes("上次查询")&&!content(tree).includes("2026-11-06"),"failed query lost Unknown or restored a visible last-query timestamp");
assert(!content(tree).includes("合成查询失败")&&!content(tree).includes("合成长文辅助提醒")&&!findMatching(tree,node=>node.props?.className?.includes("account-plan-feedback")),"plan row repeated a long query error or warning");
assert(refreshes===1&&relogins===0&&insightRefreshes===0&&requests.length===0,"rerendering capsules launched account work");unmount();
`)
}

func TestFrontendSubscriptionDetailsPreferNewObservedJobOverOldInsight(t *testing.T) {
	vm := frontendSubscriptionComponentVM(t, "account-insight-details")
	frontendPollingRun(t, vm, `
var refreshed=0,job={id:"synthetic",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"plus",canDownload:true,canRefreshSubscription:true,subscriptionPlanCheckedAt:"2026-11-06T01:02:03Z",subscriptionPlanSource:"fresh-source"};
var snapshot={id:job.id,authorizationMethod:job.authorizationMethod,subscription:{planType:"free",observedAt:"2026-11-01T00:00:00Z",identityVerified:true,reported:false,expiresAt:null}};
var tree=renderSubscriptionComponent("InsightDataSections",{job,snapshot,now:Date.now(),subscriptionAvailable:true,onRefreshSubscription(){refreshed++;}});
var block=findMatching(tree,node=>node.props?.className?.includes("insight-subscription-block"));
assert(content(findPlanBadge(block))==="Plus"&&!content(block).includes("Free")&&content(block).includes("fresh-source"),"older insight snapshot overrode freshly observed plan");
assert(!content(block).includes("上次查询")&&!findButton(block,"刷新套餐")&&!findMatching(block,node=>node.props?.className==="subscription-refresh-actions"),"details retained the last-query line or independent refresh button");
findPlanBadge(block).props.onClick();assert(refreshed===1&&requests.length===0,"subscription details bypassed explicit callback");
assert(content(block).includes("无需开启用量查询")&&content(block).includes("不会重新登录"),"independent safe scope was hidden");
tree=renderSubscriptionComponent("InsightDataSections",{job,snapshot,subscriptionAvailable:true,subscriptionRefreshState:{uncertain:true,error:"套餐已查询，本地保存待核实"}});
block=findMatching(tree,node=>node.props?.className?.includes("insight-subscription-block"));
assert(content(block).includes("Unknown")&&!content(block).includes("Plus")&&!content(block).includes("本地保存待核实")&&!content(block).includes("上次套餐已保留"),"uncertain persistence failed to show Unknown or repeated the error below the plan");
for(const extra of [{subscriptionAvailable:false},{job:{...job,canRefreshSubscription:false}},{subscriptionRefreshState:{busy:true}}]) {
 tree=renderSubscriptionComponent("InsightDataSections",{job,snapshot,subscriptionAvailable:true,onRefreshSubscription(){refreshed++;},...extra});
 block=findMatching(tree,node=>node.props?.className?.includes("insight-subscription-block"));const badge=findPlanBadge(block);
 assert(extra.subscriptionAvailable===false?badge.type==="span"&&!badge.props.onClick:badge.type==="button"&&badge.props.disabled&&!badge.props.onClick,"details capsule bypassed feature, capability, or busy restriction");
}
assert(refreshed===1&&requests.length===0,"details rerender repeated a query");unmount();
`)
}

func TestFrontendSubscriptionAppMergesReturnedJobImmediately(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#accounts";render();
var job={id:"synthetic-subscription",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"free",canDownload:true,canRefreshSubscription:true,status:"completed"};
appState.setFeatures({accountSubscription:true});appState.setToken("synthetic");appState.setJobs([job]);appState.setJobSelectionIndex([job]);
var tree=render(),row=find(tree,"JobRow");assert(row.props.subscriptionAvailable&&typeof row.props.onRefreshSubscription==="function","App did not wire independent row action");
row.props.onRefreshSubscription();row.props.onRefreshSubscription();
assert(requests.length===1&&requests[0].url.endsWith("/refresh-subscription"),"App wired duplicate or unrelated requests");
var observedAt="2026-11-06T01:02:03Z",source="synthetic-source",updated={...job,planType:"plus",subscriptionPlanCheckedAt:observedAt,subscriptionPlanSource:source};
requests[0].resolve({ok:true,status:200,json:async()=>({job:updated,subscription:{planType:"plus",source,observedAt,identityVerified:true,accountId:"synthetic-account",scope:"account",reported:false,expiresAt:null},warnings:[]})});
`)
	frontendPollingRun(t, vm, `
var tree=render(),row=find(tree,"JobRow");assert(row.props.job.planType==="plus"&&row.props.job.subscriptionPlanCheckedAt===observedAt&&!row.props.subscriptionRefreshState.busy,"successful query waited for polling before updating badge");
assert(requests.filter(request=>request.options.method==="POST").length===1,"result merge triggered another mutation");
row.props.onViewInsights();tree=render();assert(find(tree,"AccountInsightsDialog").props.job.planType==="plus"&&find(tree,"AccountInsightsDialog").props.subscriptionAvailable,"open details retained old job snapshot");
`)
}

func TestFrontendSubscriptionNewObservationFencesOldExpirationCache(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
var job={id:"synthetic",authorizationMethod:"chatgpt",planType:"plus",subscriptionPlanCheckedAt:"2026-11-06T01:02:03Z",subscriptionPlanStale:false,subscriptionExpiresAt:"",manualExpiresAt:"2027-01-01T00:00:00Z"};
var snapshot={id:job.id,authorizationMethod:job.authorizationMethod,subscription:{identityVerified:true,reported:true,observedAt:"2026-11-01T00:00:00Z",expiresAt:"2026-12-01T00:00:00Z"}};
for(const observedAt of [snapshot.subscription.observedAt,job.subscriptionPlanCheckedAt,undefined]) {
 const historical={...snapshot,subscription:{...snapshot.subscription,observedAt}};
 assert(Browser.getInsightExpirationJob(job,historical)===job,"older or undated insights resurrected expiry cleared by newer subscription query");
 const dated={...job,subscriptionExpiresAt:"2027-02-01T00:00:00Z"};
 assert(Browser.getInsightExpirationJob(dated,historical)===dated,"older insights replaced newly verified expiry");
}
var newer={...snapshot,subscription:{...snapshot.subscription,observedAt:"2026-11-07T00:00:00Z"}};
assert(Browser.getInsightExpirationJob(job,newer).subscriptionExpiresAt===snapshot.subscription.expiresAt,"fence blocked a newer independently verified subscription observation");
assert(Browser.getInsightExpirationJob(job,newer).manualExpiresAt===job.manualExpiresAt,"subscription freshness overwrote manual expiration");
var replaced={...job,subscriptionPlanCheckedAt:"",subscriptionPlanSource:"",subscriptionPlanStale:true,updatedAt:"2026-11-06T01:02:03Z"};
assert(Browser.getInsightExpirationJob(replaced,snapshot)===replaced,"old same-method snapshot revived expiry after grant replacement");
assert(Browser.getInsightExpirationJob(replaced,newer).subscriptionExpiresAt===snapshot.subscription.expiresAt,"grant replacement fence rejected a newer trusted observation");
`)
}

func TestFrontendSubscriptionRefreshCannotOverwriteNewerAccountState(t *testing.T) {
	for _, scenario := range []string{"newer-security-state", "different-method", "removed-account", "missing-version"} {
		t.Run(scenario, func(t *testing.T) {
			vm := frontendSubscriptionHookVM(t)
			if err := vm.Set("scenario", scenario); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
subscriptionJob={...subscriptionJob,updatedAt:"2026-11-06T01:00:00Z"};subscriptionOptions={...subscriptionOptions,jobs:[subscriptionJob]};
var hook=renderSubscription();hook.refresh(subscriptionJob);var result=subscriptionReply();result.job.updatedAt="2026-11-06T01:02:03Z";
var latest={...subscriptionJob,updatedAt:"2026-11-06T01:03:00Z",canRefreshSubscription:false,totpRecoveryPending:true};
if(scenario==="different-method")latest={...subscriptionJob,authorizationMethod:"bps"};
if(scenario==="missing-version"){latest={...latest,updatedAt:undefined};result.job.updatedAt=undefined;}
subscriptionOptions={...subscriptionOptions,jobs:scenario==="removed-account"?[]:[latest]};renderSubscription();ok(requests[0],result);
`)
			frontendPollingRun(t, vm, `
assert(updates.length===0&&verifications===1&&requests.length===1,"old response replaced newer account identity/security state or replayed query");
assert(!renderSubscription().states[subscriptionJob.id].busy&&renderSubscription().states[subscriptionJob.id].error.includes("账号记录已变化"),"stale response was shown as successful current state");unmount();
`)
		})
	}
}

func TestFrontendSubscriptionProvenanceDistinguishesSnapshotAndHistoricalQuery(t *testing.T) {
	vm := frontendBrowserVM(t, "accountSubscription")
	frontendPollingRun(t, vm, `
const replaced=Browser.describeSubscriptionObservation({planType:"free",subscriptionPlanCheckedAt:"",subscriptionPlanSource:"",subscriptionPlanStale:true});
assert(replaced.text.includes("授权快照")&&!replaced.text.includes("上次查询"),"new grant claims inherited prior live observation label");
const history=Browser.describeSubscriptionObservation({planType:"plus",subscriptionPlanCheckedAt:"2026-11-06T01:02:03Z",subscriptionPlanSource:"synthetic",subscriptionPlanStale:true});
assert(history.text.includes("上次查询")&&history.text.includes("历史结果")&&history.source==="synthetic","missing grant erased provenance of retained historical observation");
for(const [source,label] of [["chatgpt_accounts","账号订阅信息"],["chatgpt_subscriptions","订阅详情"]]) {
 const observation=Browser.describeSubscriptionObservation({subscriptionPlanSource:source});
 assert(observation.source===source&&observation.sourceLabel===label,"readable source label changed raw provenance or omitted its label");
}
`)
}

func TestFrontendSubscriptionAppFunctionalMergePreservesUnrenderedNewerState(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#accounts";render();
var job={id:"synthetic-subscription",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"free",canDownload:true,canRefreshSubscription:true,status:"completed",updatedAt:"2026-11-06T01:00:00Z"};
appState.setFeatures({accountSubscription:true});appState.setToken("synthetic");appState.setJobs([job]);appState.setJobSelectionIndex([job]);
var row=find(render(),"JobRow");row.props.onRefreshSubscription();
var newer={...job,updatedAt:"2026-11-06T01:04:00Z",totpRecoveryPending:true,canRefreshSubscription:false};
// Simulate a React update already queued before another render can refresh
// the hook's authority ref. Only the functional setter sees this newer row.
appState.setJobs([newer]);appState.setJobSelectionIndex([newer]);
var observedAt="2026-11-06T01:02:03Z",source="synthetic",updated={...job,updatedAt:observedAt,planType:"plus",subscriptionPlanCheckedAt:observedAt,subscriptionPlanSource:source};
requests[0].resolve({ok:true,status:200,json:async()=>({job:updated,subscription:{planType:"plus",source,observedAt,identityVerified:true,accountId:"synthetic-account"},warnings:[]})});
`)
	frontendPollingRun(t, vm, `
var row=find(render(),"JobRow");assert(row.props.job.updatedAt===newer.updatedAt&&row.props.job.totpRecoveryPending===true&&row.props.job.canRefreshSubscription===false&&row.props.job.planType==="free","functional merge overwrote unrendered newer account/security state");
assert(requests.filter(request=>request.options.method==="POST").length===1,"stale merge retried the query");
`)
}
