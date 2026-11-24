package console

import "testing"

const frontendPrivateInfoFixture = `
function privateInfo(job, patch={}) { return {id:job.id,email:job.email,loginMode:"email",password:"",totpSecret:"",mailApiUrl:"",mailRequestMethod:"",mailRequestBody:"",mailRequestHeaders:{},proxyUrl:"",credentialSource:"none",totpStatus:"none",manualExpiresAt:"",subscriptionExpiresAt:"",expiresAt:"",expiresAtSource:"",...patch}; }
`

func TestFrontendPrivateInfoRejectsIncompleteAndForeignResponses(t *testing.T) {
	for _, response := range []string{
		`null`, `{}`, `[]`,
		`privateInfo(editorJob,{id:"other"})`,
		`privateInfo(editorJob,{email:"another@example.invalid"})`,
		`privateInfo(editorJob,{password:undefined})`,
		`privateInfo(editorJob,{password:123})`,
		`privateInfo(editorJob,{totpSecret:null})`,
		`privateInfo(editorJob,{mailRequestHeaders:[]})`,
		`privateInfo(editorJob,{mailRequestHeaders:{"Authorization":false}})`,
		`privateInfo(editorJob,{credentialSource:"unavailable"})`,
		`privateInfo(editorJob,{totpStatus:"saved"})`,
		`privateInfo(editorJob,{credentialSource:"none",password:"synthetic-secret"})`,
	} {
		t.Run(response, func(t *testing.T) {
			vm := frontendExpirationComponentVM(t, "account-info-dialog")
			frontendPollingRun(t, vm, frontendPrivateInfoFixture+`
renderInfo(); requests[0].resolve({ok:true,status:200,json:async()=>editorJob});
`)
			frontendPollingRun(t, vm, `requests[1].resolve({ok:true,status:200,json:async()=> (`+response+`)});`)
			frontendPollingRun(t, vm, `
var tree=renderInfo();
assert(content(tree).includes("账号资料响应不完整或与当前账号不一致")||content(tree).includes("服务响应格式异常"),"invalid private response became empty or absent credentials");
assert(!findMatching(tree,node=>typeof node.type==="function"&&["AccountInfoField","AccountCredentialCopyButton"].includes(node.type.name)),"unverified credentials retained viewing or copy actions");
var editor=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="AccountExpirationEditor");
assert(editor&&!editor.props.refreshing&&editor.props.job.id===editorJob.id,"private failure disabled independent expiration metadata");
assert(requests.length===2&&requests.every(request=>!request.options.method),"validation replayed a request or mutation");
unmount();assert(timers.size===0,"private response validation leaked a deadline");
`)
		})
	}
}

func TestFrontendPrivateInfoPreservesExplicitEmptyAndPasswordWhitespace(t *testing.T) {
	for _, password := range []string{"", "  synthetic password  "} {
		vm := frontendExpirationComponentVM(t, "account-info-dialog")
		if err := vm.Set("password", password); err != nil {
			t.Fatal(err)
		}
		frontendPollingRun(t, vm, frontendPrivateInfoFixture+`
renderInfo();requests[0].resolve({ok:true,status:200,json:async()=>editorJob});
`)
		frontendPollingRun(t, vm, `requests[1].resolve({ok:true,status:200,json:async()=>privateInfo(editorJob,{password,credentialSource:password?"secure-store":"none"})});`)
		frontendPollingRun(t, vm, `
var tree=renderInfo(),field=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="AccountInfoField"&&node.props.label==="密码");
assert(field&&field.props.value===password,"saved password whitespace or explicit empty value was changed");
var copy=findMatching(tree,node=>typeof node.type==="function"&&node.type.name==="AccountCredentialCopyButton");
assert(copy&&copy.props.value===editorJob.email+"----"+password+"----","validated credential export lost explicit empty fields");
unmount();
`)
	}
}

func TestFrontendPrivateInfoLateResponseAfterCloseStaysUnavailable(t *testing.T) {
	vm := frontendExpirationComponentVM(t, "account-info-dialog")
	frontendPollingRun(t, vm, frontendPrivateInfoFixture+`
renderInfo();requests[0].resolve({ok:true,status:200,json:async()=>editorJob});
`)
	frontendPollingRun(t, vm, `findButton(renderInfo(),"关闭").props.onClick();`)
	frontendPollingRun(t, vm, `
assert(closeCalls===1&&requests[1].options.signal.aborted,"close did not cancel the private read");
requests[1].resolve({ok:true,status:200,json:async()=>privateInfo(editorJob,{password:"late-secret",credentialSource:"secure-store"})});
`)
	frontendPollingRun(t, vm, `assert(!findMatching(renderInfo(),node=>typeof node.type==="function"&&node.type.name==="AccountCredentialCopyButton"),"late response revived closed private contents");unmount();assert(timers.size===0,"closed private read leaked a deadline");`)
}
