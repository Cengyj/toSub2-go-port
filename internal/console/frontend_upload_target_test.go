package console

import "testing"

func TestFrontendUploadDestinationHasNoGlobalPanels(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
location.hash="#accounts";render();
appState.setFeatures({sub2apiUpload:true,cpaUpload:true});
const accounts=findMatching(render(),node=>node.type==="section"&&node.props.className==="accounts-section");
assert(!find(accounts,"UploadTargetPicker"),"account page still has a global destination panel");
assert(!findMatching(accounts,node=>(node.props?.className||"").includes("upload-settings-shortcut")),"account page gained an unrequested replacement control");
find(render(),"AppShell").props.onNavigate("settings");
const tree=RepairUI.ConfigurationPage(find(render(),"ConfigurationPage").props);
assert(!findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="UploadTargetPicker"),"configuration page still has a global destination panel");
assert(findButton(tree,"Sub2API")&&findButton(tree,"CPA"),"separate configuration categories were merged");
assert(!findButton(tree,"推送配置"),"unrequested shared configuration category remained");
assert(requests.length===0,"removing the panels introduced requests");
`)
}

func TestFrontendProviderConfigurationDoesNotChangeUploadPreference(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
location.hash="#settings";render();appState.setFeatures({sub2apiUpload:true,cpaUpload:true});
appState.setUploadTarget("cpa");
const before=JSON.stringify([...storage]);
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
assert(find(render(),"Sub2ApiSettingsDialog")&&!find(render(),"Sub2ApiSettingsDialog").props.targetPicker,"Sub2 settings was merged with a global choice");
find(render(),"AppShell").props.onNavigate("accounts");
const job={id:"unchanged-target",email:"synthetic@example.invalid",status:"completed",canDownload:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);appState.setFeatures({sub2apiUpload:true,cpaUpload:true,bulkActions:true});
assert(findMatching(render(),node=>node.type==="button"&&node.props["aria-label"]==="上传 CPA"),"viewing Sub2 settings changed the actual push target");
assert(JSON.stringify([...storage])===before,"viewing provider configuration changed target preference");
`)
}

func TestFrontendWorkflowDestinationChoiceStillUpdatesPreference(t *testing.T) {
	for _, workflow := range []string{"relogin", "totp"} {
		vm := frontendAppVM(t)
		if err := vm.Set("workflow", workflow); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, `
location.hash="#accounts";render();
appState.setFeatures({sub2apiUpload:true,cpaUpload:true,reloginAutoUpload:true,forceRelogin:true,totpReset:true,totpSetup:true});
const job={id:"workflow-target",email:"synthetic@example.invalid",status:"completed",canDownload:true,canForceRelogin:true,canResetTotp:true};
appState.setJobs([job]);appState.setJobSelectionIndex([job]);
const row=find(render(),"JobRow");
if(workflow==="relogin")row.props.onRelogin();else row.props.onSetupTotp(true,false);
const type=workflow==="relogin"?"ReloginActionDialog":"TotpActionDialog";
find(render(),type).props.onUploadTargetChange("cpa");
assert(find(render(),type).props.uploadTarget==="cpa"&&storage.get("tosub2.upload-target-v1")==="cpa","workflow destination preference no longer works");
assert(requests.length===0,"workflow target choice submitted an operation");
`)
	}
}
