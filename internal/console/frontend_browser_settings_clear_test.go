package console

import "testing"

func TestFrontendBrowserSettingsClearCompletesWithoutChangingServerState(t *testing.T) {
	for _, outcome := range []string{"accepted", "canceled", "storage-failed"} {
		t.Run(outcome, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("clearOutcome", outcome); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
const proxyKey="chatgpt-onboarding.account-proxy-v1", mailKey="chatgpt-onboarding.mail-request-settings-v1", uploadKey="chatgpt-onboarding.sub2api-upload-settings-v1", legacyMonitorKey="tosub2.monitor-target-v1";
storage.set(proxyKey,"http://synthetic.invalid:8123");
storage.set(mailKey,JSON.stringify({method:"POST",body:"synthetic-body",headersText:"X-Synthetic: fixture"}));
storage.set(legacyMonitorKey,"cpa");
storage.set("unrelated-setting","keep");
var page=find(render(),"ConfigurationPage"), originalMail=JSON.stringify(page.props.mail), originalUpload=JSON.stringify(page.props.sub2api);
page.props.onProxyDraftChange("http://draft.invalid:9000");
page.props.onSectionChange("sub2api");
var tree=render();assert(find(tree,"Sub2ApiSettingsDialog"),"fixture did not open connection settings");
confirmAccepted=clearOutcome!=="canceled";
if(clearOutcome==="storage-failed")window.localStorage.removeItem=()=>{throw new Error("synthetic storage denial");};
var clearErrors=[],cleared=false;
find(tree,"ConfigurationPage").props.onClear().then(()=>{cleared=true;},error=>clearErrors.push(error.message));
`)
			frontendPollingRun(t, vm, `
tree=render();page=find(tree,"ConfigurationPage");
assert(cleared&&clearErrors.length===0,"browser settings clear did not complete: "+clearErrors.join());
assert(requests.length===0,"browser-only cleanup wrote to the service or an upstream endpoint");
assert(storage.get("unrelated-setting")==="keep","cleanup removed unrelated site data");
if(clearOutcome==="accepted") {
 assert(!storage.has(proxyKey)&&!storage.has(mailKey)&&!storage.has(uploadKey)&&!storage.has(legacyMonitorKey),"confirmed cleanup retained browser credentials or legacy preferences");
 assert(page.props.proxy===""&&page.props.proxyDraft===""&&!page.props.sub2api.baseUrl&&!page.props.sub2api.adminApiKey,"cleanup stopped before resetting connection state and drafts");
 assert(!find(tree,"Sub2ApiSettingsDialog")&&!page.props.clearError&&content(tree).includes("已清除当前浏览器保存"),"cleanup failed to close the editor or publish its success receipt");
} else {
 assert(storage.get(proxyKey)==="http://synthetic.invalid:8123"&&storage.has(mailKey)&&storage.has(uploadKey)&&storage.get(legacyMonitorKey)==="cpa","canceled or failed cleanup removed existing browser data");
 assert(page.props.proxy==="http://synthetic.invalid:8123"&&JSON.stringify(page.props.mail)===originalMail&&JSON.stringify(page.props.sub2api)===originalUpload,"canceled or failed cleanup replaced active settings");
 assert(find(tree,"Sub2ApiSettingsDialog")&&!content(tree).includes("已清除当前浏览器保存"),"canceled or failed cleanup closed the editor or claimed success");
 assert(clearOutcome==="storage-failed"?page.props.clearError.includes("未能全部清除"):!page.props.clearError,"cleanup did not distinguish cancellation from storage failure");
}
`)
		})
	}
}

func TestFrontendBrowserSettingsClearRejectsPendingConnectionOptions(t *testing.T) {
	for _, result := range []string{"success", "failure"} {
		t.Run(result, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("optionsResult", result); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
find(render(),"Sub2ApiSettingsDialog").props.loadSub2ApiOptions();
confirmAccepted=true;var clearError="";
find(render(),"ConfigurationPage").props.onClear().catch(error=>{clearError=error.message;});
`)
			frontendPollingRun(t, vm, `
assert(!clearError&&!find(render(),"Sub2ApiSettingsDialog"),"clear did not close the connection editor");
assert(requests.length===1&&requests[0].url==="/api/sub2api/options","clear submitted a configuration write or repeated the pending option read");
if(optionsResult==="success")requests[0].resolve({ok:true,status:200,json:async()=>({groups:[{id:"old-backend-group"}],proxies:[{id:"old-backend-proxy"}]})});
else requests[0].reject(new Error("synthetic disconnected old backend"));
`)
			frontendPollingRun(t, vm, `
find(render(),"ConfigurationPage").props.onSectionChange("sub2api");
var dialog=find(render(),"Sub2ApiSettingsDialog");
assert(dialog&&!dialog.props.sub2apiSettingsDraft.baseUrl&&!dialog.props.sub2apiSettingsDraft.adminApiKey,"late option response restored a cleared backend");
assert(dialog.props.sub2apiGroups.length===0&&dialog.props.sub2apiProxies.length===0&&!dialog.props.sub2apiSettingsError&&!dialog.props.sub2apiGroupsLoading,"late option result polluted cleared settings or retained a loading state");
assert(requests.length===1&&timers.size===0,"clear or late result repeated requests or leaked its deadline");
`)
		})
	}
}
