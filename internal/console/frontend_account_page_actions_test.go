package console

import "testing"

func TestFrontendAccountPageRemovesHeaderAndKeepsSelectedRefresh(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `
for(const accountInsights of [false,true])for(const accountTrash of [false,true]) {
 appState.setFeatures({accountInsights,accountTrash,bulkActions:true});tree=render();
 const header=findMatching(tree,node=>node.type==="PageHeader"&&node.props.title==="账号管理");
 assert(!header&&!find(tree,"AccountsPageActions"),"account page retained the removed heading or actions component");
 assert(!findMatching(tree,node=>node.props?.className==="account-page-actions")&&!findButton(tree,"刷新全部用量"),"removed header actions were rendered elsewhere in the account page");
 assert(findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="刷新所选账号用量"),"removing header actions also removed selected refresh");
 const overview=find(tree,"AccountsOverview");
 assert(overview.props.refreshSummary===noticeInsights.refreshSummary&&!("onRefreshAllInsights" in overview.props)&&!("refreshControl" in overview.props),"overview retained a deleted action or lost read-only query status");
}
assert(noticeCalls.length===0&&requests.length===0,"changing header visibility submitted account work");
`)
}

func TestFrontendAccountPageSidebarKeepsConditionalTrashNavigation(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, `
var navigations=[];const props={page:"accounts",theme:"light",connected:true,total:2,trashAvailable:true,onNavigate(page){navigations.push(page);},onThemeChange(){}};
var tree=RepairUI.AppShell(props),trash=findButton(tree,"回收站");
assert(typeof RepairUI.AccountsPageActions==="undefined","obsolete header-actions export remains available");
assert(trash&&trash.props.className.includes("nav-link"),"removing the header entry removed sidebar recycle navigation");
trash.props.onClick();assert(navigations.join()==="trash"&&requests.length===0,"sidebar recycle navigation submitted account work or selected a wrong route");
hookCursor=0;tree=RepairUI.AppShell({...props,page:"trash"});assert(findButton(tree,"回收站").props["aria-current"]==="page","sidebar does not identify the active trash page");
hookCursor=0;tree=RepairUI.AppShell({...props,trashAvailable:false});assert(!findButton(tree,"回收站")&&findButton(tree,"服务配置"),"trash feature gate removed unrelated navigation or exposed unsupported trash");
`)
}

func TestFrontendAccountPageTrashNavigationAndDirectHashRemainAvailable(t *testing.T) {
	for _, entry := range []string{"sidebar", "hash"} {
		t.Run(entry, func(t *testing.T) {
			vm := frontendAppVM(t)
			if err := vm.Set("entry", entry); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
location.hash=entry==="hash"?"#trash":"#accounts";var tree=render();
appState.setToken("synthetic");appState.setFeatures({accountTrash:true,bulkActions:true});tree=render();
if(entry==="sidebar"){const shell=find(tree,"AppShell");assert(shell.props.trashAvailable,"App no longer exposes trash to the sidebar");shell.props.onNavigate("trash");tree=render();}
assert(find(tree,"AppShell").props.page==="trash"&&find(tree,"AccountTrashPage"),"trash route depends on the deleted header button");
assert(requests.length===0,"trash navigation submitted a refresh, upload, or account mutation");
`)
		})
	}
}

func TestFrontendAccountPageSelectedRefreshRetainsConfigurationGuards(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `
function selectedRefresh(tree){return findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="刷新所选账号用量");}
assert(!selectedRefresh(tree).props.disabled,"synthetic selected refresh was not initially available");
for(const field of ["settingsLoading","verificationRequired","saving","actionBusy"]){
 noticeInsights[field]=true;tree=render();assert(selectedRefresh(tree).props.disabled,"removing global controls bypassed selected "+field+" guard");noticeInsights[field]=false;
}
noticeInsights.settingsLoaded=false;tree=render();assert(selectedRefresh(tree).props.disabled,"unread settings permit selected refresh");
noticeInsights.settingsLoaded=true;noticeInsights.settings={...noticeInsights.settings,enabled:false};tree=render();assert(selectedRefresh(tree).props.disabled,"disabled settings permit selected refresh");
noticeInsights.settings={...noticeInsights.settings,enabled:true};noticeInsights.selectedRefreshControl={...noticeInsights.selectedRefreshControl,disabled:true,busy:true,label:"刷新中（1）"};tree=render();
assert(selectedRefresh(tree).props.disabled&&selectedRefresh(tree).props["aria-busy"]===true,"selected busy state permits duplicate clicks or lost accessibility");
noticeInsights.selectedRefreshControl={...noticeInsights.selectedRefreshControl,disabled:false,busy:false};appState.setSelectedJobIds(new Set());tree=render();assert(selectedRefresh(tree).props.disabled,"empty selection permits query submission");
assert(noticeCalls.length===0&&requests.length===0,"configuration or selection guard inspection submitted work");
`)
}

func TestFrontendAccountPageSelectedRefreshErrorsStayVisibleWithoutGlobalAction(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `
noticeInsights.submit=async ids=>{noticeCalls.push({scope:"selected",ids});throw new Error("合成选中查询失败");};
submitNoticeFrom(tree);
`)
	frontendPollingRun(t, vm, `
tree=render();assert(content(tree).includes("合成选中查询失败"),"selected query failure disappeared with the global action");
assert(noticeCalls.length===1&&noticeCalls[0].ids.join()==="one"&&!find(tree,"AccountsPageActions")&&requests.length===0,"selected failure changed scope, restored header actions, or replayed work");
`)
}
