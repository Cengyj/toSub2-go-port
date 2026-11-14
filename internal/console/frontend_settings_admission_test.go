package console

import (
	"encoding/json"
	"testing"
)

func TestFrontendLatestReadRejectsRetainedScopeHandler(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	frontendPollingRun(t, vm, `
var key="token-A",loading=[],onLoading=value=>loading.push(value),outcomes=[];
function renderRead(){hookCursor=0;const read=Insights.useLatestRead(key,onLoading);for(const run of effectQueue.splice(0))run();return read;}
var first=renderRead();key="token-B";renderRead();key="token-A";renderRead();
first(signal=>fetch("/stale-options",{signal})).catch(error=>outcomes.push(error.name));
`)
	frontendPollingRun(t, vm, `assert(requests.length===0&&outcomes.join()==="AbortError","old A-B-A settings handler dispatched a read in a new scope");unmount();`)
}

func TestFrontendQualityUnknownWriteBlocksRetainedHandlerBeforeRender(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountQuality.js")
	frontendPollingRun(t, vm, qualityHookSetup)
	frontendPollingRun(t, vm, `qualityOptions={...qualityOptions,active:false};var hook=renderQuality(qualityOptions);`)
	frontendPollingRun(t, vm, `ok(requests[0],qualityConfig);`)
	frontendPollingRun(t, vm, `hook=renderQuality(qualityOptions);hook.saveSettings();requests[1].reject(new Error("synthetic lost response"));`)
	frontendPollingRun(t, vm, `hook.saveSettings();assert(requests.length===2,"retained handler replayed unknown quality policy write before render");unmount();`)
}

func TestFrontendTrashUnknownWriteBlocksRetainedHandlerBeforeRender(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountTrash.js")
	frontendPollingRun(t, vm, frontendTrashHarness)
	frontendPollingRun(t, vm, `var hook=renderTrash();ok(requests[0],{settings:trashSettings});`)
	frontendPollingRun(t, vm, `hook=renderTrash();hook.saveSettings();requests[1].reject(new Error("synthetic lost response"));`)
	frontendPollingRun(t, vm, `hook.saveSettings();assert(requests.filter(request=>request.options.method==="PUT").length===1,"retained handler replayed unknown retention policy write before render");unmount();`)
}

func TestFrontendAppSettingsWritesSingleFlightBeforeRender(t *testing.T) {
	for _, section := range []string{"mail", "sub2api"} {
		t.Run(section, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("section", section); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange(section);
var dialog=find(render(),section==="mail"?"MailSettingsDialog":"Sub2ApiSettingsDialog");
var save=section==="mail"?dialog.props.saveMailRequestSettings:dialog.props.saveSub2ApiSettings;
save({preventDefault(){}});save({preventDefault(){}});
assert(requests.length===1,"configuration double submit before render sent two writes");
(section==="mail"?dialog.props.setMailRequestSettingsOpen:dialog.props.setSub2apiSettingsOpen)(false);
assert(find(render(),section==="mail"?"MailSettingsDialog":"Sub2ApiSettingsDialog"),"stale close handler dismissed an active save");
`)
		})
	}
}

func TestFrontendMonitorUnknownSaveRequiresExplicitReadbackWithoutReplay(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
var dialog=find(render(),"Sub2ApiSettingsDialog"),oldSave=dialog.props.saveSub2ApiSettings;
oldSave({preventDefault(){}});
requests[0].resolve({ok:false,status:503,json:async()=>({outcome:"unknown",uncertain:true,error:"synthetic durability uncertainty"})});
`)
	frontendPollingRun(t, vm, `
oldSave({preventDefault(){}});assert(requests.length===1,"unknown monitor write replayed before render");
dialog=find(render(),"Sub2ApiSettingsDialog");assert(dialog.props.sub2apiSettingsVerificationRequired,"unknown monitor write has no verification barrier");
dialog.props.setSub2apiSettingsOpen(false);find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
dialog=find(render(),"Sub2ApiSettingsDialog");dialog.props.saveSub2ApiSettings({preventDefault(){}});assert(requests.length===1,"leaving settings dropped unknown-write protection");
dialog.props.verifySub2ApiSettings();dialog.props.verifySub2ApiSettings();
`)
	frontendPollingRun(t, vm, `
assert(requests.length===2&&requests[1].url==="/api/sub2api/monitor"&&!requests[1].options.method,"verification was duplicated or performed a mutation");
requests[1].resolve({ok:true,status:200,json:async()=>({configured:true,enabled:false,baseUrl:"https://synthetic.invalid",groupIds:[7],excludedPlanTypes:["team"],openaiRequestTimezone:"UTC"})});
`)
	frontendPollingRun(t, vm, `
dialog=find(render(),"Sub2ApiSettingsDialog");assert(!dialog.props.sub2apiSettingsVerificationRequired&&dialog.props.sub2apiSettingsDraft.groupIds.join()==="7"&&dialog.props.sub2apiSettingsDraft.excludedPlanTypes.join()==="team","readback lost actual policy or retained the pre-read barrier");
assert(dialog.props.sub2apiSettingsError.includes("持久化")&&requests.length===2,"readback falsely confirmed durability or retried automatically");
dialog.props.saveSub2ApiSettings({preventDefault(){}});assert(requests.length===3&&requests[2].options.method==="POST","explicit save remained blocked after verification");
`)
}

func TestFrontendMonitorVerificationAcceptsActualEmptyPolicyAndRejectsIncompleteRead(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	state, err := json.Marshal(s.publicMonitorState())
	if err != nil {
		t.Fatal(err)
	}
	vm := frontendAppVM(t)
	if err := vm.Set("monitorReadback", string(state)); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
find(render(),"Sub2ApiSettingsDialog").props.saveSub2ApiSettings({preventDefault(){}});requests[0].reject(new Error("synthetic lost write"));
`)
	frontendPollingRun(t, vm, `find(render(),"Sub2ApiSettingsDialog").props.verifySub2ApiSettings();`)
	frontendPollingRun(t, vm, `requests[1].resolve({ok:true,status:200,json:async()=>({})});`)
	frontendPollingRun(t, vm, `var dialog=find(render(),"Sub2ApiSettingsDialog");assert(dialog.props.sub2apiSettingsVerificationRequired,"incomplete read released unknown write protection");dialog.props.verifySub2ApiSettings();`)
	frontendPollingRun(t, vm, `requests[2].resolve({ok:true,status:200,json:async()=>JSON.parse(monitorReadback)});`)
	frontendPollingRun(t, vm, `
var dialog=find(render(),"Sub2ApiSettingsDialog");assert(!dialog.props.sub2apiSettingsVerificationRequired&&dialog.props.sub2apiSettingsDraft.baseUrl===""&&!dialog.props.sub2apiSettingsDraft.adminApiKey&&dialog.props.sub2apiSettingsDraft.groupIds.length===0,"actual empty policy was rejected or retained another backend key");
assert(requests.length===3,"read verification automatically resaved configuration");
`)
}

func TestFrontendAppUploadSingleFlightBeforeRender(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `
var upload=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="上传 Sub2API");
upload.props.onClick();upload.props.onClick();
assert(requests.length===1&&requests[0].url==="/api/sub2api/upload","double upload dispatched duplicate remote work");
`)
}

func TestFrontendAppPasswordConfirmationSingleFlightBeforeRender(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `var tree=prepareBatch(), add=findButton(tree,"批量添加密码");add.props.onClick();add.props.onClick();`)
	frontendPollingRun(t, vm, `assert(requests.length===1&&requests[0].url==="/api/jobs/add-password-batch","duplicate confirmation dispatched two password operations");`)
}

func TestFrontendAccountRowMutationSingleFlightBeforeRender(t *testing.T) {
	for _, action := range []string{"resend", "sms", "password"} {
		t.Run(action, func(t *testing.T) {
			vm := frontendInsightsVM(t, "components/accounts.jsx")
			if err := vm.Set("action", action); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
confirmAccepted=true;
var props={job:{id:"one",email:"synthetic@example.invalid",status:action==="resend"?"email_otp":action==="sms"?"phone":"completed",canAddPassword:true},token:"synthetic",onError(){},passwordAddAvailable:true,accountProxyUrl:"",smsProviderAvailable:true,smsProvider:{ready:true,id:"synthetic",name:"测试",config:{}}};
var row=renderComponent("JobRow",props),button=action==="resend"?findButton(row,"重发"):action==="sms"?findButton(row,"测试取号"):findMatching(row,node=>node.type==="button"&&node.props.title==="添加密码");
assert(button,"fixture action missing");button.props.onClick();button.props.onClick();
`)
			frontendPollingRun(t, vm, `assert(requests.length===1,"retained account-row handler dispatched a duplicate mutation");unmount();`)
		})
	}
}

func TestFrontendAccountRowRejectsPreviousIdentityHandler(t *testing.T) {
	vm := frontendInsightsVM(t, "components/accounts.jsx")
	frontendPollingRun(t, vm, `
var props={job:{id:"one",email:"synthetic@example.invalid",status:"email_otp"},token:"synthetic",onError(){}};
var resend=findButton(renderComponent("JobRow",props),"重发");
props={...props,token:"other"};renderComponent("JobRow",props);props={...props,token:"synthetic"};renderComponent("JobRow",props);
resend.props.onClick();assert(requests.length===0,"token A-B-A revived an old account input handler");unmount();
`)
}
