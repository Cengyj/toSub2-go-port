package console

import "testing"

func TestFrontendAccountQualityUploadBlockMeansNoRemoteWrites(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const payload={ok:false,outcome:"blocked",qualityBlocked:true,uncertain:false,selected:3,uploaded:0,skipped:3,result:{success:0,created:0,updated:0},error:"检测正确题数未达到上传要求"};
const blocked=Browser.describeUpload(payload,{error:payload.error});
assert(blocked.label==="上传已阻止"&&blocked.tone==="warning"&&!blocked.spinning,"quality refusal lost its distinct terminal outcome");
for(const text of [payload.error,"本批次均未上传","已保存授权保留","核对推理探针列后重新选择账号"])assert(blocked.text.includes(text),"batch refusal lost its safe follow-up: "+text);
for(const misleading of ["结果未知","远端账号状态","部分同步","失败 3"])assert(!blocked.text.includes(misleading)&&!blocked.label.includes(misleading),"preflight refusal implied remote side effects: "+misleading);
for(const conflicting of [{...payload,uncertain:true},{...payload,uploaded:1,result:{success:1,created:1,updated:0}},{...payload,result:{success:0,created:0,updated:1,unknown:1}}]) {
  const view=Browser.describeUpload(conflicting);assert(view.label!=="上传已阻止"&&!view.text.includes("本批次均未上传"),"contradictory response falsely promised zero remote writes");
}
`)
}

func TestFrontendAccountQualityUploadHTTP409KeepsSelectionAndShowsWarning(t *testing.T) {
	vm := frontendInsightNoticeAppVM(t)
	frontendPollingRun(t, vm, `
const upload=findMatching(tree,node=>node.type==="button"&&node.props["aria-label"]==="上传 Sub2API");assert(upload&&!upload.props.disabled,"synthetic upload not actionable");upload.props.onClick();
`)
	frontendPollingRun(t, vm, `
assert(requests.length===1&&requests[0].url==="/api/sub2api/upload","upload route changed");
requests[0].resolve({ok:false,status:409,json:async()=>({ok:false,outcome:"blocked",qualityBlocked:true,uncertain:false,selected:1,uploaded:0,skipped:1,result:{success:0,created:0,updated:0},error:"合成检测未通过，未开始上传"})});
`)
	frontendPollingRun(t, vm, `
tree=render();const banner=noticeBanner(tree);assert(banner&&banner.props.className==="global-notice warning"&&!noticeHasSpinner(banner),"HTTP refusal looked active or successful");
for(const text of ["上传已阻止","本批次均未上传","已保存授权保留","合成检测未通过"])assert(content(banner).includes(text),"App discarded structured blocked result: "+text);
assert(find(tree,"JobRow").props.selected===true,"blocked batch cleared account selection");
assert(requests.length===1&&!content(banner).includes("结果未知"),"blocked batch automatically retried or became unknown");
`)
}
