package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// Load the actual display selector beside the actual refresh hook. Keeping
// both in one VM checks their contract without duplicating presentation rules.
func frontendSubscriptionDisplayIntoVM(t *testing.T, vm *sobek.Runtime) {
	t.Helper()
	entry, err := filepath.Abs("../../web/src/lib/accountSubscription.js")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{EntryPoints: []string{entry}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "Browser", Platform: api.PlatformNeutral})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("subscription display bundle: %v", built.Errors)
	}
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
}

func frontendSubscriptionComponentVM(t *testing.T, component string) *sobek.Runtime {
	t.Helper()
	vm := frontendInsightsVM(t, "components/"+component+".jsx")
	frontendPollingRun(t, vm, `
function expandSubscriptionBadges(tree) {
 if(Array.isArray(tree))return tree.map(expandSubscriptionBadges);
 if(!tree||typeof tree!=="object")return tree;
 if(typeof tree.type==="function"&&tree.type.name==="PlanBadge")return expandSubscriptionBadges(tree.type(tree.props));
 return tree.props?{...tree,props:{...tree.props,children:expandSubscriptionBadges(tree.props.children)}}:tree;
}
function renderSubscriptionComponent(name,props){return expandSubscriptionBadges(renderComponent(name,props));}
function findPlanBadge(tree){return findMatching(tree,node=>node.props?.className?.split(" ").includes("plan-badge"));}
`)
	return vm
}

func TestFrontendSubscriptionDisplayRestoresDurableFailureAndVerifiedSuccess(t *testing.T) {
	vm := frontendBrowserVM(t, "accountSubscription")
	frontendPollingRun(t, vm, `
const base={id:"synthetic",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",planType:"plus",subscriptionPlanCheckedAt:"2026-11-06T01:00:00Z"};
const failure={source:"subscription",status:"failed",httpStatus:401,errorCode:"unauthorized",startedAt:"2026-11-06T01:01:00Z",finishedAt:"2026-11-06T01:01:03Z"};
const failed={...base,accountRequestObservations:[failure,{...failure,source:"candy",status:"success",httpStatus:200,startedAt:"2026-11-06T01:02:00Z",finishedAt:"2026-11-06T01:02:01Z"}]};
const before=JSON.stringify(failed);
assert(Browser.subscriptionDisplayPlan(JSON.parse(before))==="unknown","page reload lost the durable subscription failure or another source success cleared it");
assert(JSON.stringify(failed)===before&&failed.planType==="plus","Unknown presentation modified the saved verified plan");
for(const httpStatus of [0,200,401,403,429,503]) {
 assert(Browser.subscriptionDisplayPlan({...base,accountRequestObservations:[{...failure,httpStatus}]})==="unknown","failure did not hide the displayed plan for transport/semantic status "+httpStatus);
}
const recovered={...failed,subscriptionPlanCheckedAt:"2026-11-06T01:03:02Z",accountRequestObservations:[failure,{...failure,status:"success",httpStatus:200,startedAt:"2026-11-06T01:03:00Z",finishedAt:"2026-11-06T01:03:03Z"}]};
assert(Browser.subscriptionDisplayPlan(recovered)==="plus","later verified success did not restore the plan after reload");
assert(Browser.subscriptionDisplayPlan({...base,accountRequestObservations:[{...failure,historical:true}]})==="plus","historical operation failure contaminated the current authorization");
for(const source of ["models","candy","pelican","usage","authentication","totp"]) {
 assert(Browser.subscriptionDisplayPlan({...base,accountRequestObservations:[{...failure,source}]})==="plus","unrelated account failure erased subscription display: "+source);
}
assert(Browser.subscriptionDisplayPlan({...base,accountRequestObservations:[{...failure,finishedAt:"invalid"}]})==="plus","invalid diagnostic was treated as a current query");
assert(Browser.subscriptionDisplayPlan({...base,subscriptionPlanCheckedAt:""})==="plus","an unqueried authorization snapshot was discarded");
assert(Browser.subscriptionDisplayPlan({planType:"unrecognized"})==="unknown","an unknown saved plan was presented as known");
`)
}

func TestFrontendSubscriptionDisplayKeepsVerifiedPlanWithAuxiliaryWarning(t *testing.T) {
	vm := frontendBrowserVM(t, "accountSubscription")
	frontendPollingRun(t, vm, `
const startedAt="2026-11-06T01:02:03.123456701Z",checkedAt="2026-11-06T01:02:03.123456702Z",finishedAt="2026-11-06T01:02:03.123456703Z";
const warning={source:"subscription",status:"failed",httpStatus:503,errorCode:"expiry_unavailable",startedAt,finishedAt};
const job={planType:"plus",subscriptionPlanCheckedAt:checkedAt,accountRequestObservations:[warning]};
assert(Browser.subscriptionDisplayPlan(job)==="plus","same-query optional endpoint warning replaced the verified plan");
assert(Browser.subscriptionDisplayPlan({...job,subscriptionPlanCheckedAt:startedAt})==="plus","equal-time verified result lost to the same query warning");
assert(Browser.subscriptionDisplayPlan({...job,accountRequestObservations:[{...warning,startedAt:finishedAt,finishedAt:"2026-11-06T01:02:03.123456704Z"}]})==="unknown","sub-millisecond newer failure was mistaken for the verified query warning");
assert(Browser.subscriptionDisplayPlan({...job,subscriptionPlanCheckedAt:"2026-11-06T01:02:03.123456700Z"})==="unknown","new failed request reused an older verified plan");
assert(Browser.subscriptionDisplayPlan({...job,accountRequestObservations:[{...warning,startedAt:undefined}]})==="unknown","undated legacy failure was silently treated as the verified query");
`)
}

func TestFrontendSubscriptionDisplayBindsLocalFailureToAccountAndOperation(t *testing.T) {
	vm := frontendBrowserVM(t, "accountSubscription")
	frontendPollingRun(t, vm, `
const job={id:"synthetic",email:"synthetic@example.invalid",authorizationMethod:"chatgpt",createdAt:"2026-11-01T01:00:00Z",attempt:1,lastOperationAt:"2026-11-02T01:00:00Z",planType:"plus",subscriptionPlanCheckedAt:"2026-11-06T01:00:00.123456701Z"};
const state={requestJob:{...job},checkedAt:job.subscriptionPlanCheckedAt,failed:true,error:"synthetic timeout"};
assert(Browser.subscriptionDisplayPlan(job,state)==="unknown","current local query failure did not show Unknown immediately");
for(const change of [{id:"replacement"},{email:"replacement@example.invalid"},{authorizationMethod:"bps"},{createdAt:"2026-11-03T01:00:00Z"},{attempt:2},{lastOperationAt:"2026-11-04T01:00:00Z"}]) {
 assert(Browser.subscriptionDisplayPlan({...job,...change},state)==="plus","old local failure contaminated changed account/operation: "+JSON.stringify(change));
}
assert(Browser.subscriptionDisplayPlan({...job,email:" SYNTHETIC@EXAMPLE.INVALID "},state)==="unknown","email normalization detached an otherwise identical request");
assert(Browser.subscriptionDisplayPlan(job,{...state,failed:false})==="plus","a capability guard or superseded response marked a real query failure");
assert(Browser.subscriptionDisplayPlan({...job,subscriptionPlanCheckedAt:"2026-11-06T01:00:00.123456702Z"},state)==="plus","new verified observation did not clear the old local failure");
assert(Browser.subscriptionDisplayPlan(job,{requestJob:state.requestJob,checkedAt:state.checkedAt,busy:true,error:""})==="plus","starting a first query erased a known snapshot");
`)
}

func TestFrontendSubscriptionUnknownBadgeUsesExactEnglishLabel(t *testing.T) {
	vm := frontendInsightsVM(t, "components/PlanBadge.jsx")
	frontendPollingRun(t, vm, `
for(const value of ["unknown","unrecognized",undefined]) {
 const tree=renderComponent("PlanBadge",{value,short:true});
 assert(content(tree)==="Unknown"&&tree.props["aria-label"]==="套餐：Unknown"&&tree.props["data-plan"]==="unknown","unknown plan badge did not use the exact English label");
 assert(tree.type==="span"&&!tree.props.onClick&&!tree.props.disabled&&!tree.props.className.includes("is-interactive"),"ordinary static badges acquired refresh behavior");
}
assert(content(renderComponent("PlanBadge",{value:"plus"}))==="Plus","Unknown presentation changed a known plan label");unmount();
`)
}

func TestFrontendSubscriptionInteractiveBadgePreservesLabelAndBlocksBusyClicks(t *testing.T) {
	vm := frontendInsightsVM(t, "components/PlanBadge.jsx")
	frontendPollingRun(t, vm, `
var clicks=0;const props={value:"plus",accountLabel:"synthetic@example.invalid",onRefresh(){clicks++;}};
var tree=renderComponent("PlanBadge",props);
assert(tree.type==="button"&&tree.props.type==="button"&&tree.props.className.includes("is-interactive")&&!tree.props.disabled,"refresh capsule was not an enabled native button");
assert(content(tree)==="Plus"&&tree.props["aria-label"].includes(props.accountLabel)&&tree.props.title.includes("不会重新登录"),"interactive badge lost its plan, account label, or request scope");
tree.props.onClick();assert(clicks===1&&requests.length===0&&timers.size===0,"capsule did not call the supplied callback exactly once or started its own request");
tree=renderComponent("PlanBadge",{...props,refreshing:true});
assert(tree.props.disabled&&tree.props.onClick===undefined&&tree.props["aria-busy"]===true&&content(tree)==="Plus","busy capsule allowed another click or replaced the plan text");
assert(findMatching(tree,node=>node.props?.className==="plan-badge-refresh-icon spin"),"busy capsule omitted its rotating refresh indicator");
tree=renderComponent("PlanBadge",{...props,refreshDisabled:true,refreshHint:"2FA恢复中"});
assert(tree.props.disabled&&tree.props.onClick===undefined&&tree.props.title==="2FA恢复中"&&!tree.props["aria-busy"],"blocked capsule lost its server restriction or exposed a click handler");
tree=renderComponent("PlanBadge",{...props,value:"unknown"});
assert(content(tree)==="Unknown"&&!tree.props.disabled&&typeof tree.props.onClick==="function","failed-plan capsule cannot be explicitly refreshed");
for(const value of ["free","plus","pro","team","unknown"]) {
 tree=renderComponent("PlanBadge",{value,short:true});
 assert(tree.type==="span"&&!tree.props.onClick&&!findMatching(tree,node=>node.props?.className?.includes("plan-badge-refresh-icon")),"static plan usage acquired a button or refresh icon");
}
assert(clicks===1&&requests.length===0&&timers.size===0,"rendering a busy, blocked, or static badge triggered background work");unmount();
`)
}

func TestFrontendSubscriptionRefreshLaterSuccessClearsLocalFailure(t *testing.T) {
	vm := frontendSubscriptionHookVM(t)
	frontendPollingRun(t, vm, `
var hook=renderSubscription();hook.refresh(subscriptionJob);
requests[0].resolve({ok:false,status:403,json:async()=>({error:"synthetic rejection"})});
`)
	frontendPollingRun(t, vm, `
hook=renderSubscription();assert(Browser.subscriptionDisplayPlan(subscriptionJob,hook.states[subscriptionJob.id])==="unknown"&&requests.length===1,"failed query did not remain Unknown until explicit continuation");
hook.refresh(subscriptionJob);assert(requests.length===2,"second user click did not submit exactly one new query");
assert(Browser.subscriptionDisplayPlan(subscriptionJob,renderSubscription().states[subscriptionJob.id])==="unknown","retry spinner prematurely restored the stale plan");ok(requests[1],subscriptionReply());
`)
	frontendPollingRun(t, vm, `
hook=renderSubscription();assert(updates.length===1&&Browser.subscriptionDisplayPlan(updates[0],hook.states[subscriptionJob.id])==="plus","verified retry did not restore the known plan");
assert(subscriptionJob.planType==="free"&&requests.length===2&&timers.size===0&&!hook.states[subscriptionJob.id].failed,"retry changed saved input, replayed automatically, or retained its local failure");unmount();
`)
}

func TestFrontendSubscriptionRefreshOldFailureCannotHideNewAccountPlan(t *testing.T) {
	for _, scenario := range []string{"email", "authorization", "createdAt", "attempt", "operation", "new-observation"} {
		t.Run(scenario, func(t *testing.T) {
			vm := frontendSubscriptionHookVM(t)
			if err := vm.Set("scenario", scenario); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
subscriptionJob={...subscriptionJob,createdAt:"2026-11-01T00:00:00Z",attempt:1,lastOperationAt:"2026-11-02T00:00:00Z",subscriptionPlanCheckedAt:"2026-11-03T00:00:00Z"};
subscriptionOptions={...subscriptionOptions,jobs:[subscriptionJob]};var hook=renderSubscription();hook.refresh(subscriptionJob);
var latest={...subscriptionJob,planType:"plus"};
if(scenario==="email")latest.email="replacement@example.invalid";
if(scenario==="authorization")latest.authorizationMethod="bps";
if(scenario==="createdAt")latest.createdAt="2026-11-04T00:00:00Z";
if(scenario==="attempt")latest.attempt=2;
if(scenario==="operation")latest.lastOperationAt="2026-11-04T00:00:00Z";
if(scenario==="new-observation")latest.subscriptionPlanCheckedAt="2026-11-04T00:00:00Z";
subscriptionOptions={...subscriptionOptions,jobs:[latest]};renderSubscription();requests[0].reject(new Error("synthetic old connection failure"));
`)
			frontendPollingRun(t, vm, `
hook=renderSubscription();assert(Browser.subscriptionDisplayPlan(latest,hook.states[subscriptionJob.id])==="plus","late old failure hid the new account/operation plan");
assert(updates.length===0&&requests.length===1&&latest.planType==="plus"&&subscriptionJob.planType==="free"&&timers.size===0,"old failure modified current data or started another request");unmount();
`)
		})
	}
}

func TestFrontendSubscriptionRefreshNewOwnerDoesNotInheritPriorFailedState(t *testing.T) {
	vm := frontendSubscriptionHookVM(t)
	frontendPollingRun(t, vm, `
var hook=renderSubscription();hook.refresh(subscriptionJob);requests[0].reject(new Error("synthetic prior authorization failure"));
`)
	frontendPollingRun(t, vm, `
hook=renderSubscription();assert(Browser.subscriptionDisplayPlan(subscriptionJob,hook.states[subscriptionJob.id])==="unknown","fixture did not record the original failed query");
var replaced={...subscriptionJob,authorizationMethod:"bps",planType:"plus"};subscriptionOptions={...subscriptionOptions,jobs:[replaced]};
hook=renderSubscription();assert(Browser.subscriptionDisplayPlan(replaced,hook.states[subscriptionJob.id])==="plus","old authorization error polluted the replacement before querying");
hook.refresh(replaced);hook=renderSubscription();
assert(hook.states[replaced.id].busy&&Browser.subscriptionDisplayPlan(replaced,hook.states[replaced.id])==="plus","query start rebound prior failed state onto a new authorization");
assert(requests.length===2&&updates.length===0,"replacement query made an unexpected extra request or result update");unmount();
`)
}
