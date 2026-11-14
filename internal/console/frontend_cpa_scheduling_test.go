package console

import "testing"

func TestFrontendCPASchedulingRoundTripAndZeroWeight(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,{...cpaReady,priority:20,weight:5});`)
	frontendPollingRun(t, vm, `
assert(cpaState().draft.priority==="20"&&cpaState().draft.weight==="5"&&!cpaState().dirty,"saved scheduling was not restored");
cpaState().setDraft(current=>({...current,priority:"-10",weight:"0"}));
assert(cpaState().dirty,"scheduling-only edits missed unsaved guard");cpaState().save();
const body=JSON.parse(requests[1].options.body);
assert(body.priority===-10&&body.weight===0&&body.adminApiKey===""&&body.revision===1,"scheduling save lost zero or sent a stored key");
respondCPA(1,{...cpaReady,priority:-10,weight:0,revision:2});
`)
	frontendPollingRun(t, vm, `
assert(cpaState().draft.weight==="0"&&!cpaState().dirty,"zero weight became a default");
cpaState().setDraft(current=>({...current,priority:"",weight:""}));cpaState().save();
const defaults=JSON.parse(requests[2].options.body);assert(defaults.priority===0&&defaults.weight===1,"blank fields did not use documented defaults");
`)
}

func TestFrontendCPASchedulingValidationStopsRequests(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,cpaReady);`)
	frontendPollingRun(t, vm, `
for(const [priority,weight] of [["1.5","1"],["2147483648","1"],["-2147483649","1"],["0","-1"],["0","1000001"],["0","1.5"],["0","1e2"]]) {
 cpaState().setDraft(current=>({...current,priority,weight}));cpaState().save();
 assert(requests.length===1&&cpaState().error.includes("整数"),"invalid scheduling was submitted: "+priority+","+weight);
}
`)
}

func TestFrontendCPASchedulingReadRejectsInvalidReceipt(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, frontendCPASetup)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(0,{...cpaReady,priority:10,weight:2});`)
	frontendPollingRun(t, vm, `cpaState().reload();respondCPA(1,{...cpaReady,priority:10,weight:null,revision:2});`)
	frontendPollingRun(t, vm, `
assert(cpaState().settings.revision===1&&cpaState().draft.weight==="2"&&cpaState().error.includes("响应不完整"),"malformed weight replaced verified settings");
`)
}

func TestFrontendCPASchedulingFormExplainsUpstreamStrategy(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "cpa-settings")
	frontendPollingRun(t, vm, `
const draft={baseUrl:"https://cpa.synthetic.invalid",adminApiKey:"",priority:"10",weight:"5"};
const tree=RepairUI.CPASettings({cpa:{loaded:true,draft,settings:{},save(){},reload(){},setDraft(update){Object.assign(draft,update(draft))}}});
const priority=findMatching(tree,node=>node.type==="input"&&node.props.name==="cpa-priority");
const weight=findMatching(tree,node=>node.type==="input"&&node.props.name==="cpa-weight");
assert(priority&&weight&&weight.props.min==="0"&&weight.props.max==="1000000","scheduling controls missing range");
weight.props.onChange({target:{value:"0"}});assert(draft.weight==="0","weight zero lost by control");
assert(content(tree).includes("weighted-round-robin")&&content(tree).includes("数值越大越优先")&&content(tree).includes("下次手动或自动推送"),"scheduling effect or strategy hidden");
`)
}
