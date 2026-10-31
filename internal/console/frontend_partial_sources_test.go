package console

import "testing"

func TestFrontendPartialSummaryNamesOnlyVerifiedCurrentSources(t *testing.T) {
	vm := frontendBrowserVM(t, "accountInsights")
	frontendPollingRun(t, vm, `
const start="2026-10-31T12:00:00Z", old="2026-10-30T12:00:00Z", current="2026-10-31T12:00:01Z", finish="2026-10-31T12:00:02Z";
const attempt={status:"partial",startedAt:start,finishedAt:finish};
for(const [snapshot,expected] of [
  [{lastAttempt:attempt,usage:{observedAt:current},subscription:{observedAt:old},errors:[{component:"subscription"}]},"Codex 额度已更新；账号与订阅未更新"],
  [{lastAttempt:attempt,usage:{observedAt:old,windows:[{usedPercent:83}]},bps:{observedAt:current},errors:[{component:"usage"}]},"BPS 模型权限已更新；Codex 额度未更新"],
  [{lastAttempt:attempt,usage:{observedAt:old},bps:{observedAt:current}},"BPS 模型权限已更新；Codex 额度保留历史结果"],
  [{lastAttempt:attempt,subscription:{observedAt:current},errors:[{component:"subscription",code:"subscription_scope_mismatch"}]},"账号与订阅结果不完整"],
  [{lastAttempt:attempt,usage:{observedAt:current,resetCredits:{observedAt:old}}},"Codex 额度已更新；额度重置券保留历史结果"],
  [{lastAttempt:attempt,attemptedComponents:["bps"],usage:{observedAt:current}},"查询结果不完整"],
  [{lastAttempt:{status:"partial"},usage:{observedAt:current},errors:[{component:"subscription"}]},"查询结果不完整"],
  [{lastAttempt:attempt,usage:{observedAt:"2026-10-31T12:00:03Z"}},"查询结果不完整"],
  [{lastAttempt:attempt,stale:true,usage:{observedAt:current}},"查询结果不完整"],
]) {
  const before=JSON.stringify(snapshot), summary=Browser.describeInsightSummary(snapshot);
  assert(summary.label===expected,"partial summary invented or hid source freshness: "+summary.label+"; wanted "+expected);
  assert(summary.tone==="warning"&&JSON.stringify(snapshot)===before,"source summary changed stored data or claimed full success");
  assert(!summary.label.includes("部分更新"),"generic partial-update label returned");
}
`)
}

func TestFrontendPartialDetailsNameSourcesAndKeepHistoricValues(t *testing.T) {
	vm := frontendInsightErrorVM(t)
	frontendPollingRun(t, vm, `
const old="2026-10-30T12:00:00Z",start="2026-10-31T12:00:00Z",current="2026-10-31T12:00:01Z";
const tree=errorDialog({persisted:true,lastAttempt:{status:"partial",startedAt:start,message:"部分信息已更新，请查看各来源结果"},
  usage:{observedAt:old,windows:[{usedPercent:83}]},bps:{observedAt:current,allowed:true},errors:[{component:"usage",message:"合成额度读取受限"}]});
const result=errorClass(tree,"insight-result")[0];
assert(content(result).includes("BPS 模型权限已更新；Codex 额度未更新"),"detail header failed to identify fresh and retained sources");
assert(find(tree,"progress").props.value===83&&content(tree).includes("历史观察"),"specific partial label discarded or refreshed retained usage");
assert(!content(tree).includes("部分信息已更新")&&!content(tree).includes("部分更新")&&content(tree).includes("合成额度读取受限"),"specific status was repeated as a generic update or lost its real diagnostic");
assert(errorMutations===0,"source status presentation initiated an upstream operation");unmount();
`)
}
