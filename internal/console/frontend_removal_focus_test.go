package console

import (
	"fmt"
	"testing"
)

func TestFrontendTrashFailedActionRestoresRetainedTriggerAfterBusy(t *testing.T) {
	for _, userMoved := range []bool{false, true} {
		t.Run(fmt.Sprint(userMoved), func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/account-trash.jsx")
			frontendPollingRun(t, vm, frontendRemovalFocusHarness)
			if err := vm.Set("userMoved", userMoved); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
document=owner;var mutationResolve;
var entry={id:"archive-one",email:"focus@example.invalid",recordCount:1,canRestore:true,state:"trashed",jobs:[]};
var trash={loaded:true,entries:[entry],settings:{retentionDays:0},mutate:()=>new Promise(resolve=>{mutationResolve=resolve;}),reloadList(){}};
var props={token:"synthetic",available:true,trash,onBack(){},onConfigure(){}};
var tree=renderComponent("AccountTrashPage",props);findMatching(tree,node=>node.type==="h2"&&content(node)==="回收记录").props.ref.current=fallback;
confirmAccepted=true;findButton(tree,"彻底删除").props.onClick();
`)
			frontendPollingRun(t, vm, `
trigger.disabled=true;owner.activeElement=owner.body;trash={...trash,actionBusy:"purge"};tree=renderComponent("AccountTrashPage",{...props,trash});
assert(findButton(tree,"彻底删除").props.disabled,"in-flight action was not disabled");
if(userMoved)emitFocus("pointerdown",unrelated);
mutationResolve({count:0,outcome:"failed"});
`)
			frontendPollingRun(t, vm, `
trash={...trash,actionBusy:"",verificationRequired:true};tree=renderComponent("AccountTrashPage",{...props,trash});
assert(triggerFocused===0&&focused===0,"failed receipt restored focus while button remained disabled");
trigger.disabled=false;trash={...trash,verificationRequired:false,entries:[{...entry}]};tree=renderComponent("AccountTrashPage",{...props,trash});
assert(triggerFocused===(userMoved?0:1)&&focused===0&&listeners()===0,"failed action did not restore retained trigger or stole subsequent user focus");
assert(owner.activeElement===(userMoved?owner.body:trigger),"failed action chose wrong focus target");unmount();
`)
		})
	}
}

func TestFrontendTrashUnknownReceiptWaitsForVerifiedRemoval(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/account-trash.jsx")
			frontendPollingRun(t, vm, frontendRemovalFocusHarness)
			if err := vm.Set("removed", removed); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
document=owner;var mutationResolve;
var entry={id:"archive-one",email:"focus@example.invalid",recordCount:1,canRestore:true,state:"trashed",jobs:[]};
var trash={loaded:true,entries:[entry],settings:{retentionDays:0},mutate:()=>new Promise(resolve=>{mutationResolve=resolve;}),reloadList(){}};
var props={token:"synthetic",available:true,trash,onBack(){},onConfigure(){}};
var tree=renderComponent("AccountTrashPage",props);findMatching(tree,node=>node.type==="h2"&&content(node)==="回收记录").props.ref.current=fallback;
confirmAccepted=true;findButton(tree,"彻底删除").props.onClick();
`)
			frontendPollingRun(t, vm, `mutationResolve({count:0,outcome:"unknown"});`)
			frontendPollingRun(t, vm, `
assert(listeners()===3&&focused===0,"unknown receipt discarded verification guard");
if(removed){trigger.isConnected=false;owner.activeElement=owner.body;}
trash={...trash,entries:removed?[]:[{...entry}]};tree=renderComponent("AccountTrashPage",{...props,trash});
assert(focused===(removed?1:0)&&listeners()===0,"verified unknown outcome did not settle against actual row presence");unmount();
`)
		})
	}
}

const frontendRemovalFocusHarness = `
var focused=0,triggerFocused=0;
var focusListeners=new Map();
var owner={body:{isConnected:true},documentElement:{isConnected:true},addEventListener(name,fn){if(!focusListeners.has(name))focusListeners.set(name,new Set());focusListeners.get(name).add(fn);},removeEventListener(name,fn){focusListeners.get(name)?.delete(fn);}};
function emitFocus(name,target){if(name==="focusin")owner.activeElement=target;for(const fn of [...(focusListeners.get(name)||[])])fn({target});}
function listeners(){return [...focusListeners.values()].reduce((count,set)=>count+set.size,0);}
var trigger={isConnected:true,ownerDocument:owner,disabled:false,getClientRects:()=>[{}],focus(){triggerFocused++;owner.activeElement=this;emitFocus("focusin",this);}};
var fallback={isConnected:true,disabled:false,getClientRects:()=>[{}],focus(){focused++;owner.activeElement=this;emitFocus("focusin",this);}};
var unrelated={isConnected:true};
owner.activeElement=trigger;
`

func TestFrontendRemovedRowFocusOnlyRepairsLostFocus(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/removalFocus.js")
	frontendPollingRun(t, vm, frontendRemovalFocusHarness)
	frontendPollingRun(t, vm, `
var guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);
assert(!guard.check()&&focused===0,"connected trigger stole focus");
trigger.isConnected=false;owner.activeElement=owner.body;
assert(guard.check()&&focused===1&&owner.activeElement===fallback,"removed trigger did not restore stable list focus");
assert(listeners()===0,"completed removal left global listeners");
guard.check();assert(focused===1,"focus fallback repeated");
`)
}

func TestFrontendRemovedRowFocusYieldsToUserAndUnavailableFallback(t *testing.T) {
	for _, event := range []string{"focusin", "pointerdown", "keydown"} {
		t.Run(event, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/removalFocus.js")
			frontendPollingRun(t, vm, frontendRemovalFocusHarness)
			if err := vm.Set("interaction", event); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);
emitFocus(interaction,unrelated);trigger.isConnected=false;owner.activeElement=owner.body;
guard.check();assert(focused===0&&listeners()===0,"later completion stole focus after user interaction");
`)
		})
	}
	for _, state := range []string{"hidden", "disconnected", "disabled"} {
		t.Run(state, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/removalFocus.js")
			frontendPollingRun(t, vm, frontendRemovalFocusHarness)
			if err := vm.Set("unavailable", state); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
if(unavailable==="hidden")fallback.getClientRects=()=>[];
if(unavailable==="disconnected")fallback.isConnected=false;
if(unavailable==="disabled")fallback.disabled=true;
var guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);trigger.isConnected=false;owner.activeElement=owner.body;
guard.check();assert(focused===0&&listeners()===0,"invisible fallback received focus or leaked listeners");
`)
		})
	}
}

func TestFrontendRemovedRowFocusAllowsDialogReturnAndCancelsRetainedTrigger(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/removalFocus.js")
	frontendPollingRun(t, vm, frontendRemovalFocusHarness)
	frontendPollingRun(t, vm, `
var dialogButton={isConnected:true},dialog={contains:target=>target===dialogButton};
dialogButton.closest=()=>dialog;owner.activeElement=dialogButton;
var guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);
emitFocus("focusin",dialogButton);emitFocus("focusin",trigger);trigger.isConnected=false;owner.activeElement=owner.body;
guard.check();assert(focused===1,"normal confirmation focus restoration canceled fallback");
trigger.isConnected=true;owner.activeElement=trigger;guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);
assert(guard.check(true)&&focused===1&&listeners()===0,"retained row did not settle without moving focus");
guard=Insights.createRemovalFocusGuard(trigger,()=>fallback);guard.cancel();trigger.isConnected=false;owner.activeElement=owner.body;guard.check();assert(focused===1&&listeners()===0,"canceled operation moved focus");
`)
}

func TestFrontendRemovedRowFocusWaitsForPublishedRowsAndCleansUnmount(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/removalFocus.js")
	frontendPollingRun(t, vm, frontendRemovalFocusHarness)
	frontendPollingRun(t, vm, `
document=owner;var rows=[{id:"one"}],fallbackRef={current:fallback};
function renderFocus(){hookCursor=0;const value=Insights.useRemovedRowFocus(rows,fallbackRef);for(const run of effectQueue.splice(0))run();return value;}
var hook=renderFocus();var ticket=hook.begin(hook.capture());hook.complete(ticket,true);
assert(focused===0&&listeners()===3,"removal did not wait for DOM commit");
trigger.isConnected=false;owner.activeElement=owner.body;rows=[];hook=renderFocus();
assert(focused===1&&listeners()===0,"published removal did not recover focus");
trigger.isConnected=true;owner.activeElement=trigger;ticket=hook.begin(hook.capture());hook.complete(ticket,false);hook=renderFocus();assert(listeners()===0,"failed removal retained listeners");
hook.begin(trigger);unmount();assert(listeners()===0,"unmount retained focus guard");
`)
}

func TestFrontendTrashRestoreFocusAndCanceledConfirmation(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-trash.jsx")
	frontendPollingRun(t, vm, frontendRemovalFocusHarness)
	frontendPollingRun(t, vm, `
document=owner;var mutationResolve,mutations=0;
var trash={loaded:true,entries:[{id:"archive-one",email:"focus@example.invalid",recordCount:1,canRestore:true,state:"trashed",jobs:[]}],settings:{retentionDays:0},mutate(){mutations++;return new Promise(resolve=>{mutationResolve=resolve;});},reloadList(){}};
var props={token:"synthetic",available:true,trash,onBack(){},onConfigure(){}};
var tree=renderComponent("AccountTrashPage",props);var heading=findMatching(tree,node=>node.type==="h2"&&content(node)==="回收记录");heading.props.ref.current=fallback;
assert(heading.props.tabIndex===-1&&findButton(tree,"详情与日志")&&findButton(tree,"恢复")&&findButton(tree,"彻底删除"),"stable heading or directly discoverable row actions missing");
confirmAccepted=false;findButton(tree,"恢复").props.onClick();
`)
	frontendPollingRun(t, vm, `assert(mutations===0&&listeners()===0&&owner.activeElement===trigger,"cancel changed focus or submitted restore");confirmAccepted=true;findButton(tree,"恢复").props.onClick();`)
	frontendPollingRun(t, vm, `assert(mutations===1&&listeners()===3,"confirmed restore did not arm focus guard");mutationResolve({count:1,outcome:"completed"});`)
	frontendPollingRun(t, vm, `assert(focused===0,"receipt moved focus before row removal");trigger.isConnected=false;owner.activeElement=owner.body;trash={...trash,entries:[]};tree=renderComponent("AccountTrashPage",{...props,trash});assert(focused===1&&listeners()===0,"successful restore left focus on body");unmount();`)
}
