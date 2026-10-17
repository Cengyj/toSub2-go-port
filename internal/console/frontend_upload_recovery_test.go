package console

import "testing"

func TestFrontendUploadSeparatesCredentialUpdateFromCompletedRecovery(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const failed = Browser.describeUpload({ok:false, outcome:"failed", error:"Sub2API 授权已更新，但清除错误状态未完成", result:{created:0,updated:1,success:0,failed:1,unknown:0,not_attempted:2}});
assert(failed.tone === "warning" && !failed.spinning, "incomplete recovery appeared successful or active");
assert(!failed.label.includes("同步已完成") && !failed.label.includes("部分同步"), "updated credentials counted as a completed sync");
for (const text of ["更新 1 条", "确认完成 0 条", "授权已更新，状态恢复未完成", "清除错误状态未完成", "失败 1 条", "未执行 2 条"]) assert(failed.text.includes(text), "recovery failure lost detail: " + text);
const mixed = Browser.describeUpload({ok:false, outcome:"partial", result:{created:1,updated:2,success:2,failed:1}});
assert(mixed.tone === "warning" && mixed.text.includes("确认完成 2 条"), "confirmed progress was lost");
assert(mixed.text.includes("授权已更新，状态恢复未完成"), "partial recovery was presented as all complete");
const incomplete = Browser.describeUpload({created:0,updated:1,success:0});
assert(incomplete.tone === "warning" && incomplete.text.includes("状态恢复未完成"), "zero success was ignored when no error text was supplied");
`)
}

func TestFrontendUploadRecoveryUnknownKeepsKnownUpdateAndRequiresVerification(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const result = Browser.describeUpload({ok:false, outcome:"unknown", uncertain:true, result:{created:0,updated:1,success:0,unknown:1}}, {error:"Sub2API 授权已更新，但恢复调度未完成；请先在 Sub2API 核对账号状态"});
assert(result.tone === "warning" && result.label.includes("待核实") && !result.label.includes("部分同步"), "unknown recovery became completed progress");
for (const text of ["更新 1 条", "确认完成 0 条", "授权已更新，状态恢复未完成", "结果未知 1 条", "核实远端账号状态", "避免重复创建"]) assert(result.text.includes(text), "unknown recovery lost distinction: " + text);
assert(!result.text.includes("自动重试"), "unknown recovery invited replay");
`)
}

func TestFrontendUploadCompletedAndLegacyResultsRemainCompatible(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
for (const counts of [{created:1,updated:2,success:3}, {created:1,updated:2}]) {
  for (const payload of [counts, {result:counts}]) {
    const result = Browser.describeUpload(payload);
    assert(result.tone === "success" && result.label === "同步已完成", "complete or legacy result regressed");
    assert(!result.text.includes("状态恢复未完成"), "completed upload acquired a false recovery warning");
  }
}
const unknownCreate = Browser.describeUpload({ok:false, uncertain:true, result:{created:0,updated:1,success:1,unknown:1}});
assert(unknownCreate.label === "部分同步 · 结果待核实" && !unknownCreate.text.includes("状态恢复未完成"), "later create uncertainty was mistaken for update recovery failure");
const skipped = Browser.describeUpload({created:0,updated:0,success:0,skipped:2});
assert(skipped.tone === "neutral" && skipped.label === "已跳过 · 未执行同步", "skip-only upload changed semantics");
`)
}
