package console

import "testing"

func TestFrontendProbeModelsHidePreviousSessionBeforeEffects(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-model-select.jsx")
	frontendPollingRun(t, vm, `
var calls=[],sessionA={},catalogQuality={manualSession:sessionA,modelsAPI(id,options){return new Promise(resolve=>calls.push({resolve,options}));}};
function renderModels(){hookCursor=0;const value=Insights.useProbeModels(catalogQuality,"one");for(const effect of effectQueue.splice(0))effect();return value;}
var catalog=renderModels();
`)
	frontendPollingRun(t, vm, `calls[0].resolve({models:[{id:"old-session-model",reasoningEfforts:["high"]}]});`)
	frontendPollingRun(t, vm, `
catalog=renderModels();assert(catalog.loaded&&catalog.models.length===1,"fixture did not load the original catalog");
catalogQuality={...catalogQuality,manualSession:{}};catalog=renderModels();assert(!catalog.loaded&&catalog.models.length===0,"new session first render exposed old model choices");
catalogQuality={...catalogQuality,manualSession:sessionA};catalog=renderModels();assert(!catalog.loaded&&catalog.models.length===0,"session A-B-A revived the first catalog");unmount();
`)
}

func TestFrontendProbeDialogWaitsForModelReadBeforeExplicitDetection(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var job={id:"directory-before-detection",authorizationMethod:"chatgpt"};
var quality={manualSession:{},settings:{enabled:true,model:"fixture-model",reasoningEffort:"high"},settingsLoaded:true,snapshots:{[job.id]:{state:"eligible",persisted:true,lastAttempt:{status:"never"}}},canSubmit(){return true;},modelsAPI(id,options){return fetch("/api/account-quality/models?id="+encodeURIComponent(id),options).then(response=>response.json());}};
var props={job,quality,onClose(){},onCheck(options){return fetch("/api/account-quality/refresh",{method:"POST",body:JSON.stringify({ids:[job.id],options})});}},tree=renderComponent("AccountQualityDialog",props);
assert(findButton(tree,"开始检测").props.disabled&&content(tree).includes("正在读取模型，完成后可开始"),"eligible account could submit while its directory read held the account lease");
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/account-quality/models?id="+job.id&&!requests[0].options.method,"opening the dialog submitted detection instead of the scoped directory read");
tree=renderComponent("AccountQualityDialog",props);assert(findButton(tree,"开始检测").props.disabled&&requests.length===1,"rerendering admitted detection while directory read was pending");
ok(requests[0],{models:[{id:"fixture-model",reasoningEfforts:["low"]}]});
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
var start=findButton(tree,"开始检测");assert(!start.props.disabled&&requests.length===1&&!content(tree).includes("正在读取模型，完成后可开始"),"completed directory read submitted automatically or failed to unlock explicit detection");
start.props.onClick();assert(requests.length===2&&requests[1].url==="/api/account-quality/refresh"&&requests[1].options.method==="POST","explicit detection did not submit exactly once after the read");
var body=JSON.parse(requests[1].options.body);assert(body.ids.join()===job.id&&body.options.model==="fixture-model"&&body.options.reasoningEffort==="low","detection submitted obsolete parameters from before the directory read");
unmount();assert(timers.size===0,"dialog left the model read deadline active");
`)
}

func TestFrontendProbeDialogCorrectsUnavailableEffortAfterActiveTaskEnds(t *testing.T) {
	for _, test := range []struct {
		name, efforts, want string
	}{
		{"only-low", "low", "low"},
		{"prefer-medium", "low,medium", "medium"},
		{"preserve-supported-choice", "low,high", "high"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/account-quality.jsx")
			_ = vm.Set("declaredEfforts", test.efforts)
			_ = vm.Set("expectedEffort", test.want)
			frontendPollingRun(t, vm, `
var checks=[],catalogReads=0,resolveModels,job={id:"active-effort-account",authorizationMethod:"chatgpt"};
var quality={manualSession:{},settings:{enabled:true,model:"fixture-model",reasoningEffort:"high"},settingsLoaded:true,snapshots:{[job.id]:{state:"active",persisted:false,lastAttempt:{status:"running"}}},canSubmit(){return quality.snapshots[job.id].state==="eligible";},modelsAPI(){catalogReads++;return new Promise(resolve=>{resolveModels=resolve;});}};
var savedPolicy=JSON.stringify(quality.settings),props={job,quality,onClose(){},onCheck(options){checks.push(options);}},tree=renderComponent("AccountQualityDialog",props);
function effortSelect(tree){return findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="推理强度");}
assert(effortSelect(tree).props.disabled&&effortSelect(tree).props.value==="high","active task did not preserve its locked selection");
`)
			frontendPollingRun(t, vm, `
assert(catalogReads===1,"opening the dialog did not request one model directory");resolveModels({models:[{id:"fixture-model",reasoningEfforts:declaredEfforts.split(",")}]});
`)
			frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
assert(effortSelect(tree).props.disabled&&effortSelect(tree).props.value==="high"&&checks.length===0,"loading model capabilities mutated or resubmitted an active task");
quality.snapshots[job.id]={state:"eligible",persisted:true,lastAttempt:{status:"success"}};
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
var select=effortSelect(tree),start=findButton(tree,"开始检测");
assert(!select.props.disabled&&select.props.value===expectedEffort&&!start.props.disabled,"finished task remained stuck on an unavailable reasoning effort");
assert(checks.length===0&&catalogReads===1&&JSON.stringify(quality.settings)===savedPolicy,"unlocking generated content, refetched models or changed saved policy");
start.props.onClick();assert(checks.length===1&&checks[0].model==="fixture-model"&&checks[0].reasoningEffort===expectedEffort,"explicit next detection did not use the valid selected parameters");unmount();assert(timers.size===0,"model lookup deadline survived dialog close");
`)
		})
	}
}

func TestFrontendProbeReasoningSettingsKeepMaxAndUltra(t *testing.T) {
	vm := frontendBrowserVM(t, "accountQuality")
	frontendPollingRun(t, vm, `
for(const reasoningEffort of ["max","ultra"]){
 const saved={...Browser.DEFAULT_QUALITY_SETTINGS,enabled:true,model:"catalog-model",reasoningEffort};
 const read=Browser.validateQualitySettings(saved),payload=Browser.qualitySettingsPayload(read);
 assert(read.reasoningEffort===reasoningEffort&&payload.reasoningEffort===reasoningEffort,"reading or saving valid upstream reasoning effort discarded max/ultra");
}
let rejected=false;try{Browser.validateQualitySettings({...Browser.DEFAULT_QUALITY_SETTINGS,reasoningEffort:"invented-effort"});}catch(error){rejected=true;}
assert(rejected,"new efforts weakened settings validation");
`)
}

func TestFrontendProbeDialogOffersDeclaredMaxAndUltraWithoutChangingPolicy(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var checks=[],job={id:"max-ultra-account",email:"synthetic@example.invalid",authorizationMethod:"chatgpt"};
var quality={manualSession:{},settings:{enabled:true,model:"next-model",reasoningEffort:"high"},settingsLoaded:true,snapshots:{[job.id]:{id:job.id,state:"eligible",persisted:true,lastAttempt:{status:"never"}}},canSubmit(){return true;},modelsAPI(){return Promise.resolve({models:[{id:"next-model",displayName:"Next Model",reasoningEfforts:["max","ultra"]}]});}};
var original=JSON.stringify(quality.settings),props={job,quality,onCheck(options){checks.push(options);},onClose(){}},tree=renderComponent("AccountQualityDialog",props);
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
var select=findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="推理强度");
assert(select&&JSON.stringify(select.props.children.flat().filter(Boolean).map(option=>option.props.value))===JSON.stringify(["max","ultra"]),"dialog filtered out declared max/ultra or invented unsupported efforts");
select.props.onChange({target:{value:"ultra"}});tree=renderComponent("AccountQualityDialog",props);findButton(tree,"开始检测").props.onClick();
assert(checks.length===1&&checks[0].model==="next-model"&&checks[0].reasoningEffort==="ultra","single-run invocation lost selected Ultra effort");
findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="检测题目").props.onChange({target:{value:"pelican"}});
tree=renderComponent("AccountQualityDialog",props);var manual=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ManualProbePanel");
assert(manual&&manual.props.options.reasoningEffort==="ultra"&&JSON.stringify(quality.settings)===original&&requests.length===0,"manual selection lost Ultra or changed saved policy");
unmount();assert(timers.size===0,"model read deadline survived close");
`)
}

func TestFrontendProbeSettingsUsesDeclaredEffortsAndPreservesExistingDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var updates=[],quality={draft:{model:"next-model",reasoningEffort:"high"},settings:{},settingsLoaded:true,settingsLoading:false,saving:false,dirty:false,settingsError:"",uncertain:false,settingsVerificationRequired:false,manualSession:{},modelsAPI(){return Promise.resolve({models:[{id:"next-model",reasoningEfforts:["max","ultra"]}]});},setDraft(update){quality.draft=update(quality.draft);updates.push(quality.draft);},saveSettings(){throw new Error("implicit settings save");}};
var props={quality,jobs:[{id:"catalog-source",authorizationMethod:"chatgpt"}]},tree=renderComponent("AccountQualitySettings",props);
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualitySettings",props);
var select=findMatching(tree,node=>node.type==="select"&&node.props.value===quality.draft.reasoningEffort),options=select.props.children.filter(Boolean).flat();
assert(options.length===3&&options[0].props.value==="high"&&options[0].props.disabled&&content(options[0]).includes("当前配置"),"catalog read silently replaced a saved effort missing from the source account");
assert(options.slice(1).map(option=>option.props.value).join()==="max,ultra"&&content(select).includes("Max")&&content(select).includes("Ultra"),"settings hid declared efforts or added unsupported choices");
assert(updates.length===0&&quality.draft.reasoningEffort==="high","reading the directory modified the unsaved draft");
select.props.onChange({target:{value:"ultra"}});tree=renderComponent("AccountQualitySettings",props);
select=findMatching(tree,node=>node.type==="select"&&node.props.value===quality.draft.reasoningEffort);
assert(updates.length===1&&quality.draft.reasoningEffort==="ultra"&&select.props.children.filter(Boolean).flat().map(option=>option.props.value).join()==="max,ultra","explicit Ultra selection did not stay in settings draft");
assert(requests.length===0,"effort selection performed model inference");unmount();assert(timers.size===0,"settings model deadline survived close");
`)
}

func TestFrontendProbeModelsDropsLateResponsesAcrossAccountsAndKeepsEmptyOrFailedCatalogsEmpty(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-model-select.jsx")
	frontendPollingRun(t, vm, `
var catalogCalls=[],catalogAccount="first-account",catalogQuality={manualSession:{},settings:{model:"global-must-not-be-a-fallback"},modelsAPI(id,options){return new Promise((resolve,reject)=>catalogCalls.push({id,options,resolve,reject}));}};
function renderModels(){hookCursor=0;const value=Insights.useProbeModels(catalogQuality,catalogAccount);for(const effect of effectQueue.splice(0))effect();return value;}
var catalog=renderModels();
`)
	frontendPollingRun(t, vm, `
assert(catalogCalls.length===1&&catalogCalls[0].id==="first-account"&&catalogCalls[0].options.cache==="no-store"&&!catalogCalls[0].options.method,"model metadata fetch generated content or targeted another account");
catalogAccount="second-account";catalog=renderModels();
assert(catalog.models.length===0&&catalog.suggestedModels.length===0&&catalog.loading&&catalogCalls[0].options.signal.aborted,"changing accounts kept selectable stale models or left old request active");
`)
	frontendPollingRun(t, vm, `
assert(catalogCalls.length===2&&catalogCalls[1].id==="second-account","new account directory was not fetched");
catalogCalls[0].resolve({models:[{id:"wrong-account-model",displayName:"Wrong",reasoningEfforts:["high"]}],suggestedModels:[{id:"wrong-suggested",reasoningEfforts:["high"]}]});
`)
	frontendPollingRun(t, vm, `
catalog=renderModels();assert(catalog.accountId==="second-account"&&catalog.models.length===0&&catalog.suggestedModels.length===0,"late old-account response leaked into current model choices");
catalogCalls[1].resolve({models:[{id:"second-model",displayName:"Second",reasoningEfforts:["low","high"]}],suggestedModels:[{id:"gpt-6.1-sol",reasoningEfforts:["high","ultra"]}],source:"codex-models",fetchedAt:"2026-11-05T00:00:00Z"});
`)
	frontendPollingRun(t, vm, `
catalog=renderModels();assert(catalog.loaded&&!catalog.loading&&catalog.models.length===1&&catalog.models[0].id==="second-model"&&catalog.suggestedModels.length===1&&catalog.suggestedModels[0].id==="gpt-6.1-sol","current account directory and suggestions did not stay separate");
catalog.reload();renderModels();
`)
	frontendPollingRun(t, vm, `assert(catalogCalls.length===3,"explicit model reload did not issue exactly one read");catalogCalls[2].resolve({models:[],source:"bps-access",fetchedAt:"2026-11-05T00:01:00Z"});`)
	frontendPollingRun(t, vm, `
catalog=renderModels();assert(catalog.loaded&&catalog.models.length===0&&catalog.suggestedModels.length===0&&!catalog.error,"empty directory fabricated a model from global settings or kept old suggestions");catalog.reload();renderModels();
`)
	frontendPollingRun(t, vm, `catalogCalls[3].reject(new Error("Synthetic model directory unavailable"));`)
	frontendPollingRun(t, vm, `
catalog=renderModels();assert(!catalog.loaded&&!catalog.loading&&catalog.models.length===0&&catalog.suggestedModels.length===0&&catalog.error.includes("Synthetic model directory unavailable"),"failed model lookup reused obsolete options or looked successful");
assert(catalogCalls.length===4&&catalogCalls.every(call=>!call.options.method),"catalog failure silently retried or generated model content");unmount();assert(timers.size===0,"directory timeout survived unmount");
`)
}

func TestFrontendProbeModelPickerSearchAndKeyboardSelectionUseOnlyCatalogEntries(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-model-select.jsx")
	frontendPollingRun(t, vm, `
var chosen=[],pickerProps={id:"test-model-picker",value:"",onChange(value){chosen.push(value);pickerProps.value=value;},catalog:{accountId:"synthetic-account",models:[{id:"visual-alpha",displayName:"Visual Alpha",reasoningEfforts:["high"]},{id:"reasoning-beta",displayName:"Reasoning Beta",reasoningEfforts:["medium"]}],loaded:true,loading:false,error:"",reload(){}}};
var picker=renderComponent("ProbeModelSelect",pickerProps);
assert(!findMatching(picker,node=>node.type==="input"&&node.props["aria-label"]==="自定义模型 ID"),"picker defaulted to manual model entry");
var trigger=findMatching(picker,node=>node.type==="button"&&node.props.id===pickerProps.id);
trigger.props.onKeyDown({key:"ArrowDown",preventDefault(){}});picker=renderComponent("ProbeModelSelect",pickerProps);
var search=findMatching(picker,node=>node.type==="input"&&node.props.role==="combobox");
assert(search&&search.props["aria-expanded"]==="true"&&search.props["aria-controls"]==="test-model-picker-list","keyboard did not open an accessible model search");
search.props.onChange({target:{value:"ALPHA"}});picker=renderComponent("ProbeModelSelect",pickerProps);
var option=findMatching(picker,node=>node.type==="button"&&node.props.role==="option");
assert(option&&content(option).includes("Visual Alpha")&&!content(picker).includes("Reasoning Beta"),"search did not filter by model name and ID");
search=findMatching(picker,node=>node.type==="input"&&node.props.role==="combobox");search.props.onKeyDown({key:"Enter",preventDefault(){}});picker=renderComponent("ProbeModelSelect",pickerProps);
assert(chosen.join()==="visual-alpha"&&!findMatching(picker,node=>node.props?.role==="listbox"),"Enter selected a hidden/unavailable model or left the menu open");
trigger=findMatching(picker,node=>node.type==="button"&&node.props.id===pickerProps.id);trigger.props.onClick();picker=renderComponent("ProbeModelSelect",pickerProps);
search=findMatching(picker,node=>node.type==="input"&&node.props.role==="combobox");search.props.onKeyDown({key:"ArrowDown",preventDefault(){}});picker=renderComponent("ProbeModelSelect",pickerProps);
search=findMatching(picker,node=>node.type==="input"&&node.props.role==="combobox");search.props.onKeyDown({key:"Enter",preventDefault(){}});picker=renderComponent("ProbeModelSelect",pickerProps);
assert(chosen.join()==="visual-alpha,reasoning-beta","arrow navigation did not select the focused catalog entry");
pickerProps.catalog={accountId:"empty-account",models:[],loaded:true,loading:false,error:"",reload(){}};pickerProps.value="";picker=renderComponent("ProbeModelSelect",pickerProps);picker=renderComponent("ProbeModelSelect",pickerProps);
trigger=findMatching(picker,node=>node.type==="button"&&node.props.id===pickerProps.id);trigger.props.onClick();picker=renderComponent("ProbeModelSelect",pickerProps);
assert(!findMatching(picker,node=>node.props?.role==="option")&&content(picker).includes("此账号未提供模型列表"),"empty catalog invented a default model option");
search=findMatching(picker,node=>node.type==="input"&&node.props.role==="combobox");search.props.onKeyDown({key:"Escape",preventDefault(){},stopPropagation(){}});picker=renderComponent("ProbeModelSelect",pickerProps);
assert(!findMatching(picker,node=>node.props?.role==="listbox")&&chosen.length===2,"Escape changed the model or retained the popup");unmount();
`)
}

func TestFrontendProbeSuggestedModelsStaySeparateAndSelectBySearchOrKeyboard(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-model-select.jsx")
	frontendPollingRun(t, vm, `
function optionNodes(tree){if(!tree||typeof tree!=="object")return [];if(Array.isArray(tree))return tree.flatMap(optionNodes);return [...(tree.props?.role==="option"?[tree]:[]),...optionNodes(tree.props?.children)];}
var chosen=[],catalog={accountId:"synthetic-account",loaded:true,loading:false,error:"",models:[{id:"gpt-6-astra",displayName:"GPT-6 Astra",reasoningEfforts:["low"]}],suggestedModels:[{id:"gpt-6-astra",reasoningEfforts:["ultra"]},{id:"gpt-6.1-sol",displayName:"GPT-6.1 Sol",reasoningEfforts:["high","ultra"]},{id:"gpt-6.1-terra",displayName:"GPT-6.1 Terra",reasoningEfforts:["none","high","max"]}],reload(){}};
var before=JSON.stringify(catalog),choices=Insights.probeModelChoices(catalog);
assert(choices.map(model=>model.id).join()==="gpt-6-astra,gpt-6.1-sol,gpt-6.1-terra"&&!choices[0].suggested&&choices[1].suggested&&choices[0].reasoningEfforts.join()==="low","suggested models duplicated or overruled the live directory");
var props={id:"suggested-picker",value:"",catalog,onChange(value){chosen.push(value);props.value=value;}},tree=renderComponent("ProbeModelSelect",props);
findMatching(tree,node=>node.type==="button"&&node.props.id===props.id).props.onClick();tree=renderComponent("ProbeModelSelect",props);
var options=optionNodes(tree);assert(options.length===3&&!content(options[0]).includes("常用")&&content(options[1]).includes("常用")&&content(options[2]).includes("常用"),"common choices lost their source or preceded live options");
var search=findMatching(tree,node=>node.props?.role==="combobox");search.props.onKeyDown({key:"ArrowDown",preventDefault(){}});tree=renderComponent("ProbeModelSelect",props);
findMatching(tree,node=>node.props?.role==="combobox").props.onKeyDown({key:"ArrowDown",preventDefault(){}});tree=renderComponent("ProbeModelSelect",props);
findMatching(tree,node=>node.props?.role==="combobox").props.onKeyDown({key:"Enter",preventDefault(){}});tree=renderComponent("ProbeModelSelect",props);
assert(chosen.join()==="gpt-6.1-terra"&&content(tree).includes("账号目录未列出")&&!findMatching(tree,node=>node.props?.role==="listbox"),"keyboard could not select the extra model or falsely claimed directory membership");
findMatching(tree,node=>node.type==="button"&&node.props.id===props.id).props.onClick();tree=renderComponent("ProbeModelSelect",props);
findMatching(tree,node=>node.props?.role==="combobox").props.onChange({target:{value:"6.1-SOL"}});tree=renderComponent("ProbeModelSelect",props);
options=optionNodes(tree);assert(options.length===1&&content(options[0]).includes("GPT-6.1 Sol"),"model search omitted common choices");
findMatching(tree,node=>node.props?.role==="combobox").props.onKeyDown({key:"Enter",preventDefault(){}});tree=renderComponent("ProbeModelSelect",props);
assert(chosen.join()==="gpt-6.1-terra,gpt-6.1-sol"&&!findMatching(tree,node=>node.props?.["aria-label"]==="自定义模型 ID")&&JSON.stringify(catalog)===before,"common model selection required typing an ID or changed the directory");
assert(requests.length===0,"choosing a common model invoked inference");unmount();
`)
}

func TestFrontendProbeModelChoicesRankAcrossSourcesAndKeepUnknownOrder(t *testing.T) {
	vm := frontendInsightsVM(t, "components/probe-model-select.jsx")
	frontendPollingRun(t, vm, `
var catalog={models:[{id:"unknown-first",reasoningEfforts:[]},{id:"gpt-5.6-luna",reasoningEfforts:[]},{id:"gpt-6-luna",reasoningEfforts:[]},{id:"gpt-6.1-sol",reasoningEfforts:["ultra"]},{id:"unknown-second",reasoningEfforts:[]}],suggestedModels:[{id:"gpt-6.1-sol",reasoningEfforts:["low"]},{id:"gpt-5.6-terra",reasoningEfforts:[]},{id:"gpt-6-sol",reasoningEfforts:[]},{id:"gpt-6-astra",reasoningEfforts:[]},{id:"gpt-5.6-sol",reasoningEfforts:[]},{id:"gpt-6.1-terra",reasoningEfforts:[]},{id:"unknown-third",reasoningEfforts:[]}]};
var before=JSON.stringify(catalog),choices=Insights.probeModelChoices(catalog);
assert(choices.map(model=>model.id).join()==="gpt-6-astra,gpt-6.1-sol,gpt-6.1-terra,gpt-6-sol,gpt-6-luna,gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna,unknown-first,unknown-second,unknown-third","model ordering failed across live and suggested sources or reordered unknown models");
assert(!choices[1].suggested&&choices[1].reasoningEfforts.join()==="ultra"&&choices[0].suggested&&JSON.stringify(catalog)===before,"sorting changed live model precedence, metadata, or source catalog");
`)
}

func TestFrontendProbeDialogFallbackPrefersAccountCatalog(t *testing.T) {
	vm := frontendInsightsVM(t, "components/account-quality.jsx")
	frontendPollingRun(t, vm, `
var job={id:"fallback-account",authorizationMethod:"chatgpt"},quality={manualSession:{},settings:{enabled:true,model:"unavailable-model",reasoningEffort:"high"},snapshots:{},canSubmit(){return true;},modelsAPI(){return Promise.resolve({models:[{id:"account-model",reasoningEfforts:["high"]}],suggestedModels:[{id:"gpt-6-astra",reasoningEfforts:["high"]}]});}};
var props={job,quality,onClose(){},onCheck(){}},tree=renderComponent("AccountQualityDialog",props);
`)
	frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
var picker=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect");
assert(picker.props.value==="account-model"&&quality.settings.model==="unavailable-model","sorted suggestions displaced the account catalog fallback or changed saved settings");unmount();
`)
}

func TestFrontendProbeSuggestedModelsKeepSavedDefaultsAndDeclaredEfforts(t *testing.T) {
	t.Run("single-run", func(t *testing.T) {
		vm := frontendInsightsVM(t, "components/account-quality.jsx")
		frontendPollingRun(t, vm, `
var checks=[],job={id:"suggested-account",authorizationMethod:"chatgpt"},quality={manualSession:{},settings:{enabled:true,model:"gpt-6.1-sol",reasoningEffort:"ultra"},snapshots:{[job.id]:{state:"eligible",persisted:true,lastAttempt:{status:"never"}}},canSubmit(){return true;},modelsAPI(){return Promise.resolve({models:[{id:"live-model",reasoningEfforts:["high"]}],suggestedModels:[{id:"gpt-6.1-sol",reasoningEfforts:["low","high","ultra"]}]});}},before=JSON.stringify(quality.settings);
var props={job,quality,onClose(){},onCheck(options){checks.push(options);}},tree=renderComponent("AccountQualityDialog",props);
`)
		frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualityDialog",props);tree=renderComponent("AccountQualityDialog",props);
var picker=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect"),select=findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="推理强度");
assert(picker.props.value==="gpt-6.1-sol"&&select.props.value==="ultra"&&select.props.children.flat().map(option=>option.props.value).join()==="low,high,ultra","directory loading replaced the saved common model or lost its declared efforts");
findButton(tree,"开始检测").props.onClick();assert(checks.length===1&&checks[0].model==="gpt-6.1-sol"&&checks[0].reasoningEffort==="ultra","common model did not reach the explicit single-run callback");
findMatching(tree,node=>node.type==="select"&&node.props["aria-label"]==="检测题目").props.onChange({target:{value:"pelican"}});tree=renderComponent("AccountQualityDialog",props);
var manual=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ManualProbePanel");assert(manual.props.options.model==="gpt-6.1-sol"&&manual.props.options.reasoningEffort==="ultra"&&JSON.stringify(quality.settings)===before&&requests.length===0,"manual question lost common choice or changed saved policy");unmount();
`)
	})
	t.Run("settings", func(t *testing.T) {
		vm := frontendInsightsVM(t, "components/account-quality.jsx")
		frontendPollingRun(t, vm, `
var updates=[],quality={draft:{model:"gpt-6.1-terra",reasoningEffort:"none"},settings:{},settingsLoaded:true,manualSession:{},modelsAPI(){return Promise.resolve({models:[{id:"live-model",reasoningEfforts:["ultra"]}],suggestedModels:[{id:"gpt-6.1-terra",reasoningEfforts:["none","low","high","max"]}]});},setDraft(change){quality.draft=change(quality.draft);updates.push(quality.draft);}};
var props={quality,jobs:[{id:"suggested-source",authorizationMethod:"chatgpt"}]},tree=renderComponent("AccountQualitySettings",props);
`)
		frontendPollingRun(t, vm, `
tree=renderComponent("AccountQualitySettings",props);var picker=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="ProbeModelSelect"),select=findMatching(tree,node=>node.type==="select"&&node.props.value===quality.draft.reasoningEffort);
assert(picker.props.value==="gpt-6.1-terra"&&select.props.children.filter(Boolean).flat().map(option=>option.props.value).join()==="none,low,high,max"&&updates.length===0,"settings hid common model metadata, offered Ultra, or modified saved draft while reading");
select.props.onChange({target:{value:"max"}});assert(quality.draft.model==="gpt-6.1-terra"&&quality.draft.reasoningEffort==="max"&&updates.length===1&&requests.length===0,"settings did not preserve explicit common-model parameters");unmount();
`)
	})
}
