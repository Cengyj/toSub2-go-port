// Only classify explicit runtime script diagnostics. HTTP errors, cancellation,
// and validation failures keep their own explanations.
export function verificationScriptFailureMessage(error, { authorization = false, uploadSkipped = false } = {}) {
  if (typeof error !== "string" || !/\bLEGACY_RUNTIME_CHALLENGE:/.test(error)
    || !/\bGO_JS_RUNTIME:\s+(?:SCRIPT_(?:EXCEPTION|TYPEERROR|SYNTAXERROR|RANGEERROR|REFERENCEERROR|REJECTION|UNHANDLED_REJECTION)|WORKER_SCRIPT_EXCEPTION)\b/.test(error)) return "";
  return ["网站验证脚本执行失败", authorization && "本次授权未完成", uploadSkipped && "上传未开始"].filter(Boolean).join("，") + "。可展开查看技术详情。";
}
