package console

import "testing"

func TestFrontendAppSettingsSaveRetainsEditsMadeInFlight(t *testing.T) {
	for _, section := range []string{"mail", "sub2api"} {
		t.Run(section, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("draftSection", section); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
const dialogType=draftSection==="mail"?"MailSettingsDialog":"Sub2ApiSettingsDialog";
find(render(),"ConfigurationPage").props.onSectionChange(draftSection);
var dialog=find(render(),dialogType);
const save=draftSection==="mail"?dialog.props.saveMailRequestSettings:dialog.props.saveSub2ApiSettings;
save({preventDefault(){}});
const update=draftSection==="mail"?dialog.props.setMailRequestSettingsDraft:dialog.props.setSub2apiSettingsDraft;
update(current=>({...current,...(draftSection==="mail"?{headersText:'{"X-Synthetic":"later-edit"}'}:{baseUrl:"https://later-edit.invalid"})}));
// The input update can be queued before React publishes another render.
requests[0].resolve({ok:true,status:200,json:async()=>({configured:true})});
`)
			frontendPollingRun(t, vm, `
dialog=find(render(),dialogType);
assert(dialog,"earlier settings save closed the editor and discarded newer input");
const draft=draftSection==="mail"?dialog.props.mailRequestSettingsDraft:dialog.props.sub2apiSettingsDraft;
assert(draftSection==="mail"?draft.headersText==='{"X-Synthetic":"later-edit"}':draft.baseUrl==="https://later-edit.invalid","earlier settings receipt replaced a newer draft");
const page=find(render(),"ConfigurationPage");
assert(page.props.dirty&&!page.props.saving,"newer unsaved input lost its navigation protection");
assert(draftSection==="mail"?!page.props.mail.headersText.includes("later-edit"):page.props.sub2api.baseUrl==="https://synthetic.invalid","unsaved edits were advertised as active configuration");
assert(requests.length===1,"preserving later input automatically resubmitted configuration");
(draftSection==="mail"?dialog.props.saveMailRequestSettings:dialog.props.saveSub2ApiSettings)({preventDefault(){}});
assert(requests.length===2,"preserved draft could not be explicitly saved");
const submitted=JSON.parse(requests[1].options.body).config;
assert(draftSection==="mail"?submitted.headers["X-Synthetic"]==="later-edit":submitted.baseUrl==="https://later-edit.invalid","explicit second save sent the older draft");
requests[1].resolve({ok:true,status:200,json:async()=>({configured:true})});
`)
			frontendPollingRun(t, vm, `
var tree=render(),finalPage=find(tree,"ConfigurationPage");
assert(!find(tree,dialogType)&&!finalPage.props.saving,"unchanged successful draft no longer closed normally");
assert(draftSection==="mail"?finalPage.props.mail.headersText.includes("later-edit"):finalPage.props.sub2api.baseUrl==="https://later-edit.invalid","confirmed later save did not publish its active configuration");
assert(requests.length===2,"explicit second save triggered an automatic extra write");
`)
		})
	}
}

func TestFrontendSub2SettingsSavePreservesReturnToPreviousDraft(t *testing.T) {
	vm := frontendAppVM(t)
	frontendPollingRun(t, vm, `
// Exercise the production passive settings synchronization effect while
// keeping unrelated App polling disabled in this focused harness.
var previousEffectSettings=null,settingsEffects=[];
React.useEffect=(run,deps)=>{if(deps.length===1&&typeof deps[0]?.baseUrl==="string"&&deps[0]!==previousEffectSettings){previousEffectSettings=deps[0];settingsEffects.push(run);}};
function renderSettings(){const tree=render();for(const run of settingsEffects.splice(0))run();return tree;}
find(renderSettings(),"ConfigurationPage").props.onSectionChange("sub2api");
var dialog=find(renderSettings(),"Sub2ApiSettingsDialog"), originalDraft={...dialog.props.sub2apiSettingsDraft};
dialog.props.setSub2apiSettingsDraft(current=>({...current,baseUrl:"https://submitted-change.invalid"}));
dialog=find(renderSettings(),"Sub2ApiSettingsDialog");dialog.props.saveSub2ApiSettings({preventDefault(){}});
dialog.props.setSub2apiSettingsDraft(originalDraft);
requests[0].resolve({ok:true,status:200,json:async()=>({configured:true})});
`)
	frontendPollingRun(t, vm, `
renderSettings();var tree=renderSettings(),dialog=find(tree,"Sub2ApiSettingsDialog"),page=find(tree,"ConfigurationPage");
assert(dialog&&JSON.stringify(dialog.props.sub2apiSettingsDraft)===JSON.stringify(originalDraft),"passive synchronization discarded an in-flight edit back to the previous configuration");
assert(page.props.sub2api.baseUrl==="https://submitted-change.invalid"&&page.props.dirty,"preserved previous-value draft was advertised as saved or lost its navigation guard");
assert(requests.length===1,"draft preservation automatically reverted the saved configuration");
`)
}
