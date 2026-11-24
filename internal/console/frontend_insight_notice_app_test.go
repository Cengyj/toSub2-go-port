package console

import (
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// Keep the production App handlers, notice hook and account read lifecycle.
// Only the insights service hook is synthetic; account GETs return local fixtures.
func frontendInsightNoticeAppVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	vm := frontendAppVM(t, api.Plugin{Name: "insight-notice-app-boundaries", Setup: func(build api.PluginBuild) {
		build.OnResolve(api.OnResolveOptions{Filter: `/hooks/accountInsights\.js$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			return api.OnResolveResult{Path: "insights-service", Namespace: "insight-notice-app"}, nil
		})
		build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "insight-notice-app"}, func(api.OnLoadArgs) (api.OnLoadResult, error) {
			source := `export function useAccountInsights(){return globalThis.noticeInsights;}`
			return api.OnLoadResult{Contents: &source, Loader: api.LoaderJS}, nil
		})
		build.OnResolve(api.OnResolveOptions{Filter: `^react$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			if strings.HasSuffix(args.Importer, "/hooks/notice.js") || strings.HasSuffix(args.Importer, "/hooks/serverState.js") {
				return api.OnResolveResult{Path: "synthetic-trash-react", External: true}, nil
			}
			return api.OnResolveResult{}, nil
		})
	}})
	frontendPollingRun(t, vm, `
var noticeEffects=[],noticeClock=0;
TrashReact.useEffect=(run,deps)=>{const id=hookCursor++,previous=hookSlots[id];if(!previous||deps.some((v,i)=>!Object.is(v,previous.deps[i]))){hookSlots[id]={deps,cleanup:previous?.cleanup};noticeEffects.push(()=>{hookSlots[id].cleanup?.();hookSlots[id].cleanup=run();});}};
var renderBeforeNoticeEffects=render;
render=()=>{const tree=renderBeforeNoticeEffects();for(const run of noticeEffects.splice(0))run();return tree;};
window.setTimeout=(fn,delay)=>{const id=++nextTimer;timers.set(id,{fn,delay,at:noticeClock+delay});return id;};
function advanceNoticeTime(ms){noticeClock+=ms;for(const [id,timer] of [...timers])if(timer.at<=noticeClock){timers.delete(id);timer.fn();}}
var document={hidden:false,addEventListener(){},removeEventListener(){}},navigator={onLine:true};
window.addEventListener=()=>{};window.removeEventListener=()=>{};
var noticeRows=[{id:"one",email:"notice@example.invalid",status:"completed",authorizationMethod:"chatgpt",canDownload:true}];
var fetchBeforeNoticeFixture=fetch;
fetch=(url,options)=>url.startsWith("/api/jobs?selection=compact&page=")?Promise.resolve({ok:true,status:200,json:async()=>({jobs:noticeRows,selection:noticeRows,pagination:{page:1,pageSize:20,total:1,totalAll:1,totalPages:1},stats:{active:0,queued:0,completed:1}})}):fetchBeforeNoticeFixture(url,options);
var noticeCalls=[],noticeReceipt={queryId:"synthetic-notice",accepted:["one"],merged:[],skipped:[]};
var noticeSettings={schemaVersion:1,revision:1,enabled:true,automatic:false,intervalMinutes:15,excludedAccountIds:[]};
var noticeInsights={settings:noticeSettings,draft:noticeSettings,settingsLoaded:true,settingsLoading:false,verificationRequired:false,saving:false,dirty:false,actionBusy:false,refreshPending:false,snapshots:{},status:{queued:0,active:0},statusError:"",summaryError:"",
 refreshSummary:{disabled:false,busy:false,label:"用量查询状态",phase:"ready",description:"Synthetic ready"},selectedRefreshControl:{disabled:false,busy:false,label:"刷新所选用量",phase:"ready",description:"Synthetic ready"},
 submit:async ids=>{noticeCalls.push({scope:"selected",ids});return noticeReceipt;},resetDraft(){},reloadStatus(){}};
function noticeBanner(tree){return findMatching(tree,node=>typeof node.props?.className==="string"&&node.props.className.split(" ").includes("global-notice"));}
function noticeHasSpinner(tree){return !!findMatching(tree,node=>typeof node.props?.className==="string"&&node.props.className.split(" ").includes("spin"));}
function submitNoticeFrom(tree){const button=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="刷新所选账号用量");assert(button&&!button.props.disabled,"selected refresh is not actionable");button.props.onClick();}
location.hash="#accounts";var tree=render();appState.setToken("synthetic-notice-console");appState.setFeatures({accountInsights:true,bulkActions:true,sub2apiUpload:true});tree=render();
`)
	frontendPollingRun(t, vm, `tree=render();appState.setSelectedJobIds(new Set(["one"]));tree=render();assert(requests.length===0,"test initialization sent a non-fixture request");`)
	t.Cleanup(func() {
		frontendPollingRun(t, vm, `for(const slot of hookSlots)slot?.cleanup?.();assert(timers.size===0,"App notice fixture leaked timers after unmount");`)
	})
	return vm
}

func TestFrontendInsightNoticeAppAcceptedAndMergedExpireWithoutSpinner(t *testing.T) {
	for _, scope := range []string{"selected"} {
		for _, kind := range []string{"accepted", "merged"} {
			t.Run(scope+"/"+kind, func(t *testing.T) {
				vm := frontendInsightNoticeAppVM(t)
				if err := vm.Set("noticeScope", scope); err != nil {
					t.Fatal(err)
				}
				if err := vm.Set("noticeKind", kind); err != nil {
					t.Fatal(err)
				}
				frontendPollingRun(t, vm, `noticeReceipt={queryId:"synthetic-notice",accepted:noticeKind==="accepted"?["one"]:[],merged:noticeKind==="merged"?["one"]:[],skipped:[]};submitNoticeFrom(tree);`)
				frontendPollingRun(t, vm, `
tree=render();var banner=noticeBanner(tree);assert(banner&&banner.props.className==="global-notice info"&&!noticeHasSpinner(banner),"admission receipt still shows an active or successful result");
assert(content(banner).includes(noticeKind==="accepted"?"已受理 1":"已合并在途查询 1")&&content(banner).includes("受理不代表查询完成"),"admission scope or result qualification disappeared");
assert(noticeCalls.length===1&&noticeCalls[0].scope===noticeScope&&noticeCalls[0].ids.join()==="one","App routed refresh to the wrong scope");
advanceNoticeTime(5999);tree=render();assert(noticeBanner(tree),"admission receipt disappeared before six seconds");
`)
				frontendPollingRun(t, vm, `advanceNoticeTime(1);tree=render();assert(!noticeBanner(tree),"admission receipt did not disappear after six seconds");assert(requests.length===0,"notice expiry triggered a mutation or unexpected request");`)
			})
		}
	}
}

func TestFrontendInsightNoticeAppSkippedReasonsStayVisible(t *testing.T) {
	for _, scope := range []string{"selected"} {
		t.Run(scope, func(t *testing.T) {
			vm := frontendInsightNoticeAppVM(t)
			if err := vm.Set("noticeScope", scope); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `noticeReceipt={queryId:"synthetic-skipped",accepted:["one"],merged:[],skipped:[{id:"unavailable",reason:"合成账号尚无授权"}]};submitNoticeFrom(tree);`)
			frontendPollingRun(t, vm, `
tree=render();var banner=noticeBanner(tree);assert(banner&&banner.props.className==="global-notice warning"&&!noticeHasSpinner(banner)&&content(banner).includes("已跳过 1")&&content(banner).includes("合成账号尚无授权"),"skipped query lost its visible warning");
assert(![...timers.values()].some(timer=>timer.delay===6000),"warning receipt incorrectly scheduled dismissal");advanceNoticeTime(6000);
`)
			frontendPollingRun(t, vm, `tree=render();assert(noticeBanner(tree)&&content(noticeBanner(tree)).includes("合成账号尚无授权"),"empty queue or expiry erased skipped-query warning");`)
		})
	}
}

func TestFrontendInsightNoticeAppUploadProgressSurvivesOldReceiptTimer(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `submitNoticeFrom(tree);`)
	frontendPollingRun(t, vm, `
tree=render();var oldReceiptTimer=[...timers.values()].find(timer=>timer.delay===6000);assert(oldReceiptTimer,"normal receipt did not schedule dismissal");
var upload=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="上传 Sub2API");assert(upload&&!upload.props.disabled,"synthetic upload is not actionable");upload.props.onClick();
`)
	frontendPollingRun(t, vm, `
tree=render();var banner=noticeBanner(tree);assert(banner&&banner.props.className==="global-notice active"&&noticeHasSpinner(banner)&&content(banner).includes("正在同步 Sub2API"),"real upload progress lost its active spinner");
assert(requests.length===1&&requests[0].url==="/api/sub2api/upload"&&JSON.parse(requests[0].options.body).ids.join()==="one","upload handler used a different scope or duplicated submission");
assert(![...timers.values()].some(timer=>timer.delay===6000),"replacement notice did not cancel old receipt timeout");oldReceiptTimer.fn();advanceNoticeTime(6000);
`)
	frontendPollingRun(t, vm, `tree=render();var banner=noticeBanner(tree);assert(banner&&noticeHasSpinner(banner)&&content(banner).includes("正在同步 Sub2API"),"stale receipt timeout cleared the newer upload progress");requests[0].resolve({ok:false,status:503,json:async()=>({error:"合成上传结果待核实",uncertain:true})});`)
}
