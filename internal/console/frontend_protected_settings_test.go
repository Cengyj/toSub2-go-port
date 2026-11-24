package console

import "testing"

const protectedSettingsHarness = `
var protectedOptions={token:"A",enabled:true,scope:"mail",legacyKeys:["legacy-mail"]};
function renderProtected(){hookCursor=0;const value=Insights.useProtectedIntegrationSettings(protectedOptions);for(const run of effectQueue.splice(0))run();return value;}
function protectedSnapshot(config,revision=0,verified=false){return {version:1,scope:protectedOptions.scope,revision,config,credentialPersistence:"secure",verified,applied:false};}
`

func TestFrontendProtectedSettingsRequireVerifiedReadbackBeforeRemovingBrowserSecrets(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/protectedIntegrationSettings.js")
	frontendPollingRun(t, vm, protectedSettingsHarness+`
storage.set("legacy-mail","private-browser-copy");var protection=renderProtected();
assert(requests.length===1&&!requests[0].options.method,"initial protection read dispatched a write");
ok(requests[0],protectedSnapshot({method:"GET",serverOnly:true},4));
`)
	frontendPollingRun(t, vm, `
protection=renderProtected();assert(protection.migrationRequired&&!protection.adopted&&!protection.ready,"read silently adopted conflicting browser settings");
var candidate={method:"POST",unknown:{enabled:false,count:0,models:[]}};var oldSave=protection.save;
oldSave(candidate);oldSave(candidate);
assert(requests.length===2&&requests[1].options.method==="PUT","duplicate secure save escaped before render");
assert(JSON.parse(requests[1].options.body).revision===4,"save omitted loaded revision");
assert(JSON.stringify(JSON.parse(requests[1].options.body).config)===JSON.stringify(candidate),"unknown/false/zero/empty config changed");
ok(requests[1],protectedSnapshot(candidate,5,false));
`)
	frontendPollingRun(t, vm, `
oldSave(candidate);assert(requests.length===2&&storage.has("legacy-mail"),"unverified write cleared browser copy or replayed");
protection=renderProtected();assert(protection.uncertain&&!protection.canSave,"unverified response was accepted");
protection.confirm();protection.confirm();assert(requests.length===3&&!requests[2].options.method,"confirmation did not read first or duplicated");
ok(requests[2],protectedSnapshot({unknown:{models:[],count:0,enabled:false},method:"POST"},5));
`)
	frontendPollingRun(t, vm, `
assert(requests.length===4&&requests[3].url.endsWith("/confirm")&&requests[3].options.method==="POST","confirmation repeated config write");
assert(storage.has("legacy-mail"),"read alone cleared old secret before confirmation");
ok(requests[3],protectedSnapshot(candidate,5,true));
`)
	frontendPollingRun(t, vm, `
protection=renderProtected();assert(!storage.has("legacy-mail")&&!protection.uncertain&&protection.ready,"confirmed migration did not settle");
assert(requests.length===4,"migration contacted provider or retried");unmount();assert(timers.size===0,"protection leaked request deadline");
`)
}

func TestFrontendProtectedSettingsConflictRequiresExplicitChoice(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/protectedIntegrationSettings.js")
	frontendPollingRun(t, vm, protectedSettingsHarness+`
storage.set("legacy-mail","keep-until-confirmed");renderProtected();ok(requests[0],protectedSnapshot({method:"GET"},1));
`)
	frontendPollingRun(t, vm, `var protection=renderProtected();protection.save({method:"POST"});requests[1].resolve({ok:false,status:409,json:async()=>({code:"SETTINGS_CONFLICT",error:"conflict"})});`)
	frontendPollingRun(t, vm, `protection=renderProtected();protection.confirm();ok(requests[2],protectedSnapshot({method:"GET",otherBrowser:true},2));`)
	frontendPollingRun(t, vm, `
protection=renderProtected();assert(requests.length===3&&storage.has("legacy-mail")&&!protection.adopted&&protection.canSave,"conflict auto-adopted or cleared source");
protection.adoptServer();ok(requests[3],protectedSnapshot({method:"GET",otherBrowser:true},2));
`)
	frontendPollingRun(t, vm, `ok(requests[4],protectedSnapshot({method:"GET",otherBrowser:true},2,true));`)
	frontendPollingRun(t, vm, `protection=renderProtected();assert(protection.adopted.explicit&&protection.adopted.config.otherBrowser&&!storage.has("legacy-mail"),"explicit adopt failed");assert(requests.filter(r=>r.options.method==="PUT").length===1,"conflict overwrote another browser");unmount();`)
}

func TestFrontendProtectedSettingsStaleTokenCannotCleanOrCommit(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/protectedIntegrationSettings.js")
	frontendPollingRun(t, vm, protectedSettingsHarness+`
storage.set("legacy-mail","keep-on-stale-write");renderProtected();ok(requests[0],protectedSnapshot(null));
`)
	frontendPollingRun(t, vm, `
var first=renderProtected();first.save({method:"POST"});
protectedOptions={...protectedOptions,token:"B"};renderProtected();protectedOptions={...protectedOptions,token:"A"};renderProtected();
var count=requests.length;first.save({method:"GET"});assert(requests.length===count,"old A-B-A save handler dispatched");
ok(requests[1],protectedSnapshot({method:"POST"},1,true));
`)
	frontendPollingRun(t, vm, `assert(storage.has("legacy-mail")&&!renderProtected().loaded,"stale token response cleared source or published state");unmount();`)
}

func TestFrontendProtectedSettingsCleanupFailureKeepsConfirmedServerResult(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/protectedIntegrationSettings.js")
	frontendPollingRun(t, vm, protectedSettingsHarness+`
storage.set("legacy-mail","copy");renderProtected();ok(requests[0],protectedSnapshot(null));
`)
	frontendPollingRun(t, vm, `var protection=renderProtected();window.localStorage.removeItem=()=>{throw new Error("denied")};protection.save({method:"GET"});ok(requests[1],protectedSnapshot({method:"GET"},1,true));`)
	frontendPollingRun(t, vm, `protection=renderProtected();assert(protection.loaded&&protection.snapshot.verified&&protection.migrationRequired&&protection.error.includes("服务端已确认"),"browser failure concealed protected commit or claimed cleanup");assert(storage.has("legacy-mail")&&requests.length===2,"cleanup failure replayed server write");unmount();`)
}

func TestFrontendProtectedMailSeparatesStorageApplicationAndDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/mailSettings.js")
	frontendPollingRun(t, vm, `
var mailOptions={token:"A",protectedStorage:true,onError:()=>{}};
function renderMail(){hookCursor=0;const value=Insights.useMailSettings(mailOptions);for(const run of effectQueue.splice(0))run();return value;}
function mailSnapshot(config,revision=0,verified=false){return {version:1,scope:"mail",revision,config,credentialPersistence:"secure",verified,applied:false};}
var mail=renderMail();assert(requests.length===1&&requests[0].url==="/api/integration-settings/mail"&&!requests[0].options.method,"mail bootstrap applied config");
mail.editor.setMailRequestSettingsDraft({method:"GET",url:"",headersText:'{"Draft":"keep"}'});
ok(requests[0],mailSnapshot({method:"GET",url:"",headersText:'{"Server":"saved"}'},1));
`)
	frontendPollingRun(t, vm, `renderMail();mail=renderMail();assert(mail.editor.mailRequestSettingsDraft.headersText.includes("Draft")&&!mail.applied&&requests.length===1,"passive secure read overwrote draft or applied config");mail.editor.saveMailRequestSettings({preventDefault(){}});mail.editor.saveMailRequestSettings({preventDefault(){}});assert(requests.length===2&&requests[1].options.method==="PUT","mail did not secure before apply or duplicated");var saved=JSON.parse(requests[1].options.body).config;mail.editor.setMailRequestSettingsDraft({...saved,headersText:'{"Newer":"edit"}'});ok(requests[1],mailSnapshot(saved,2,true));`)
	frontendPollingRun(t, vm, `assert(requests.length===3&&requests[2].url==="/api/mail-request-config"&&JSON.parse(requests[2].options.body).protectedRevision===2,"confirmed storage did not proceed to versioned apply");requests[2].reject(new Error("lost apply response"));`)
	frontendPollingRun(t, vm, `
mail=renderMail();assert(!mail.applied&&mail.protection.snapshot.verified&&mail.editor.mailRequestSettingsDraft.headersText.includes("Newer"),"apply failure lost stored result/newer draft");
mail.editor.applySaved();mail.editor.applySaved();assert(requests.length===4&&requests[3].url==="/api/mail-request-config"&&JSON.parse(requests[3].options.body).protectedRevision===2,"explicit local apply repeated secure write, omitted revision, or duplicated");ok(requests[3],{});
`)
	frontendPollingRun(t, vm, `mail=renderMail();assert(mail.applied&&mail.editor.mailRequestSettingsDraft.headersText.includes("Newer")&&requests.filter(r=>r.options.method==="PUT").length===1,"apply recovery overwrote draft/replayed secure save");unmount();`)
}

func TestFrontendProtectedProxyPreservesEditsMadeDuringSave(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/proxySettings.js")
	frontendPollingRun(t, vm, `
function renderProxy(){hookCursor=0;const value=Insights.useProxySettings({token:"A",protectedStorage:true});for(const run of effectQueue.splice(0))run();return value;}
var proxy=renderProxy();ok(requests[0],{version:1,scope:"proxy",revision:0,config:null,credentialPersistence:"secure",verified:false,applied:false});
`)
	frontendPollingRun(t, vm, `proxy=renderProxy();proxy.setDraft("http://first.invalid:8080");proxy=renderProxy();proxy.save(proxy.draft);proxy.setDraft("http://later.invalid:8080");ok(requests[1],{version:1,scope:"proxy",revision:1,config:{url:"http://first.invalid:8080"},credentialPersistence:"secure",verified:true,applied:false});`)
	frontendPollingRun(t, vm, `proxy=renderProxy();assert(proxy.value==="http://first.invalid:8080"&&proxy.draft==="http://later.invalid:8080","proxy save overwrote newer draft");unmount();`)
}

func TestFrontendProtectedSub2UnknownApplyNeverRepeatsStorageOrRemoteWork(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/sub2apiSettings.js")
	frontendPollingRun(t, vm, `
function renderSub2(){hookCursor=0;const value=Insights.useSub2ApiSettings({token:"A",enabled:false,activePage:"settings",settingsTab:"sub2api",protectedStorage:true});for(const run of effectQueue.splice(0))run();return value;}
var sub2=renderSub2();ok(requests[0],{version:1,scope:"sub2api",revision:0,config:null,credentialPersistence:"secure",verified:false,applied:false});
`)
	frontendPollingRun(t, vm, `sub2=renderSub2();sub2.editor.saveSub2ApiSettings({preventDefault(){}});var saved=JSON.parse(requests[1].options.body).config;ok(requests[1],{version:1,scope:"sub2api",revision:1,config:saved,credentialPersistence:"secure",verified:true,applied:false});`)
	frontendPollingRun(t, vm, `assert(requests.length===3&&requests[2].url==="/api/sub2api/monitor"&&JSON.parse(requests[2].options.body).protectedRevision===1,"protected save did not apply separately with its committed revision");requests[2].reject(new Error("lost monitor apply"));`)
	frontendPollingRun(t, vm, `sub2=renderSub2();assert(sub2.verificationRequired&&!sub2.applied&&sub2.protection.snapshot.verified,"unknown apply discarded storage or claimed applied");sub2.editor.applySaved();sub2.editor.saveSub2ApiSettings({preventDefault(){}});assert(requests.length===3,"unknown monitor apply replayed");sub2.editor.verifySub2ApiSettings();`)
	frontendPollingRun(t, vm, `ok(requests[3],{configured:true,enabled:false,baseUrl:"https://synthetic.invalid",groupIds:[],excludedPlanTypes:[],openaiRequestTimezone:"UTC"});`)
	frontendPollingRun(t, vm, `sub2=renderSub2();assert(!sub2.verificationRequired&&!sub2.applied&&sub2.editor.sub2apiSettingsError.includes("持久化"),"readback falsely proved apply durability");assert(requests.filter(r=>r.options.method==="PUT").length===1&&requests.filter(r=>r.url==="/api/sub2api/monitor"&&r.options.method==="POST").length===1,"verification repeated a mutation");unmount();`)
}

func TestFrontendProtectedMailDefaultRequiresExplicitSaveAndApply(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/mailSettings.js")
	frontendPollingRun(t, vm, `
function renderMail(){hookCursor=0;const value=Insights.useMailSettings({token:"A",protectedStorage:true,onError:()=>{}});for(const run of effectQueue.splice(0))run();return value;}
function mailSnapshot(config,revision=0,verified=false){return {version:1,scope:"mail",revision,config,credentialPersistence:"secure",verified,applied:false};}
renderMail();ok(requests[0],mailSnapshot(null));
`)
	frontendPollingRun(t, vm, `
var mail=renderMail();assert(!mail.applied&&!mail.ready&&mail.protection.ready,"missing saved mail config falsely claimed default process state");
mail.editor.applySaved();assert(requests.length===1,"unsaved default config was implicitly applied");
mail.editor.saveMailRequestSettings({preventDefault(){}});var saved=JSON.parse(requests[1].options.body).config;assert(saved.method==="GET","default contract changed");ok(requests[1],mailSnapshot(saved,1,true));
`)
	frontendPollingRun(t, vm, `assert(requests.length===3&&JSON.parse(requests[2].options.body).protectedRevision===1,"first process apply was not guarded by saved revision");ok(requests[2],{});`)
	frontendPollingRun(t, vm, `mail=renderMail();assert(mail.ready&&mail.applied,"explicitly saved and applied default did not become usable");mail.protection.reload();ok(requests[3],mailSnapshot({method:"POST",url:"https://mail.synthetic.invalid"},2));`)
	frontendPollingRun(t, vm, `mail=renderMail();assert(!mail.ready&&!mail.applied&&requests.length===4,"old apply receipt survived newer protected revision or triggered an automatic apply");unmount();`)
}

func TestFrontendProtectedMailApplyConflictRequiresExplicitChoice(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/mailSettings.js")
	frontendPollingRun(t, vm, `
function renderMail(){hookCursor=0;const value=Insights.useMailSettings({token:"A",protectedStorage:true,onError:()=>{}});for(const run of effectQueue.splice(0))run();return value;}
function mailSnapshot(config,revision,verified=false){return {version:1,scope:"mail",revision,config,credentialPersistence:"secure",verified,applied:false};}
var firstConfig={method:"GET",headersText:'{"First":"saved"}'},otherConfig={method:"GET",headersText:'{"Other":"saved"}'};
renderMail();ok(requests[0],mailSnapshot(firstConfig,3));
`)
	frontendPollingRun(t, vm, `renderMail();var mail=renderMail(),staleApply=mail.editor.applySaved;staleApply();assert(JSON.parse(requests[1].options.body).protectedRevision===3,"explicit apply did not bind loaded revision");requests[1].resolve({ok:false,status:409,json:async()=>({code:"SETTINGS_CONFLICT",error:"new revision"})});`)
	frontendPollingRun(t, vm, `
staleApply();assert(requests.length===2,"stale apply callback bypassed conflict barrier");
mail=renderMail();assert(!mail.applied&&!mail.ready&&mail.protection.uncertain&&mail.protection.conflict,"cross-browser conflict was hidden");
mail.protection.confirm();ok(requests[2],mailSnapshot(otherConfig,4));
`)
	frontendPollingRun(t, vm, `mail=renderMail();assert(requests.length===3&&mail.protection.conflict&&mail.settings.headersText.includes("First"),"conflict silently adopted other browser or repeated apply");mail.protection.reload();ok(requests[3],mailSnapshot(otherConfig,4));`)
	frontendPollingRun(t, vm, `mail=renderMail();mail.editor.applySaved();assert(requests.length===4&&!mail.ready,"ordinary reread bypassed explicit conflict choice");mail.protection.adoptServer();ok(requests[4],mailSnapshot(otherConfig,4));`)
	frontendPollingRun(t, vm, `ok(requests[5],mailSnapshot(otherConfig,4,true));`)
	frontendPollingRun(t, vm, `renderMail();mail=renderMail();assert(!mail.applied&&mail.settings.headersText.includes("Other")&&!mail.protection.conflict,"explicit server choice lost its boundary");mail.editor.applySaved();assert(JSON.parse(requests[6].options.body).protectedRevision===4,"chosen config used stale revision");ok(requests[6],{});`)
	frontendPollingRun(t, vm, `mail=renderMail();assert(mail.applied&&mail.ready&&!requests.some(request=>request.options.method==="PUT"),"conflict recovery rewrote protected configuration");unmount();`)
}

func TestFrontendProtectedSub2ApplySavedBindsRevisionAndRejectsConflict(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/sub2apiSettings.js")
	frontendPollingRun(t, vm, `
function renderSub2(){hookCursor=0;const value=Insights.useSub2ApiSettings({token:"A",enabled:false,activePage:"settings",settingsTab:"sub2api",protectedStorage:true});for(const run of effectQueue.splice(0))run();return value;}
var config={baseUrl:"https://synthetic.invalid",adminApiKey:"synthetic-key",groupIds:[]};
storage.delete("chatgpt-onboarding.sub2api-upload-settings-v1");
renderSub2();ok(requests[0],{version:1,scope:"sub2api",revision:7,config,credentialPersistence:"secure",verified:false,applied:false});
`)
	frontendPollingRun(t, vm, `renderSub2();var sub2=renderSub2(),staleApply=sub2.editor.applySaved;staleApply();assert(JSON.parse(requests[1].options.body).protectedRevision===7,"Sub2 saved application omitted revision");requests[1].resolve({ok:false,status:409,json:async()=>({code:"SETTINGS_CONFLICT",error:"new revision"})});`)
	frontendPollingRun(t, vm, `staleApply();sub2=renderSub2();assert(requests.length===2&&!sub2.ready&&!sub2.applied&&sub2.protection.conflict&&!sub2.verificationRequired,"Sub2 conflict replayed, claimed application, or mixed in monitor durability recovery");unmount();`)
}

func TestFrontendProtectedSub2PassiveReadDoesNotInventDirtyDraft(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/sub2apiSettings.js")
	frontendPollingRun(t, vm, `
function renderSub2(){hookCursor=0;const value=Insights.useSub2ApiSettings({token:"A",enabled:true,activePage:"settings",settingsTab:"sub2api",protectedStorage:true});for(const run of effectQueue.splice(0))run();return value;}
var sub2=renderSub2();sub2.open();sub2=renderSub2();assert(!sub2.dirty,"opening pristine Sub2 settings invented a draft");
`)
	frontendPollingRun(t, vm, `
ok(requests.find(request=>request.url==="/api/integration-settings/sub2api"),{version:1,scope:"sub2api",revision:0,config:null,credentialPersistence:"secure",verified:false,applied:false});
ok(requests.find(request=>request.url==="/api/sub2api/monitor"),{configured:true,enabled:false,groupIds:[],excludedPlanTypes:[],openaiRequestTimezone:"UTC"});
`)
	frontendPollingRun(t, vm, `
sub2=renderSub2();assert(!sub2.dirty,"passive monitor read briefly marked an unchanged editor dirty before its synchronization effect");
sub2=renderSub2();sub2.editor.setSub2apiSettingsDraft({...sub2.editor.sub2apiSettingsDraft,baseUrl:"https://edited.invalid"});
sub2=renderSub2();assert(sub2.dirty,"real unsaved edit lost its navigation guard");unmount();
`)
}

func TestFrontendInsightsSuccessfulHTTPDoesNotEraseDurabilityWarning(t *testing.T) {
	vm := frontendInsightsVM(t, "hooks/accountInsights.js")
	frontendPollingRun(t, vm, `options={...options,active:false};renderHook(options);`)
	frontendPollingRun(t, vm, `ok(requests[0],config);`)
	frontendPollingRun(t, vm, `var hook=renderHook(options),outcome;hook.saveSettings().then(value=>outcome=value);ok(requests[1],{...config,revision:2,persistenceWarning:"synthetic directory sync uncertain"});`)
	frontendPollingRun(t, vm, `hook=renderHook(options);assert(outcome===false&&hook.uncertain&&hook.verificationRequired&&hook.persistenceWarning,"HTTP success erased durability uncertainty");hook.saveSettings();assert(requests.length===2,"warning automatically retried save");unmount();`)
}
