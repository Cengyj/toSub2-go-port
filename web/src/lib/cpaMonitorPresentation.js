export function describeCPAMonitor(state = {}) {
  const running = Boolean(state.running);
  const error = typeof state.lastError === "string" ? state.lastError.trim() : "";
  // CPA always returns zero counters, including before its first inspection.
  const result = state.lastCheckAt && state.lastResult ? state.lastResult : null;
  const count = key => Math.max(0, Math.floor(Number(result?.[key]) || 0));
  const parts = result ? [`检查 ${count("checked")} 个授权文件`] : [];
  for (const [key, label] of [
    ["started", "已排队自动重登"], ["excludedPlan", "按套餐跳过"],
    ["busy", "正在处理"], ["cooldown", "冷却中"],
    ["ineligible", "不符合自动重登条件"], ["missing", "未匹配本地账号"], ["failed", "入队失败"],
  ]) {
    if (count(key)) parts.push(`${label} ${count(key)} 个`);
  }
  if (count("started")) parts.push("重登与上传结果请在账号列表查看");
  if (error) parts.push(error);
  const outcome = (code, label, tone, icon) => ({ code, label, tone, icon, spinning: false, text: parts.join("，") || label });
  const resultPresentation = error ? outcome("error", "CPA 巡检需关注", "warning", "CircleAlert")
    : !result ? outcome("empty", "尚未执行 CPA 巡检", "neutral", "History")
    : count("failed") ? outcome("failed", "部分账号未能排队", "warning", "CircleAlert")
    : count("missing") || count("ineligible") ? outcome("attention", "有账号需关注", "warning", "CircleAlert")
    : count("started") ? outcome("queued", "已排队自动重登", "info", "LogIn")
    : count("excludedPlan") ? outcome("excluded", "已按套餐跳过", "neutral", "Filter")
    : count("busy") || count("cooldown") ? outcome("deferred", "暂缓处理", "neutral", "Clock3")
    : outcome("no_action", "本次未发现需自动重登的账号", "neutral", "FileText");
  const status = running ? { label: "正在巡检", tone: "active" }
    : !state.configured ? { label: "未配置", tone: "neutral" }
    : !state.enabled ? { label: "未启用", tone: "neutral" }
    : error || count("failed") ? { label: "上次巡检需关注", tone: "warning" }
    : { label: "已启用", tone: "success" };
  return { status, result, error, resultPresentation, resultTitle: running ? "上次巡检结果" : "最近巡检结果" };
}
