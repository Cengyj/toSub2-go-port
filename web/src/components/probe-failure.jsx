import React from "react";
import { CircleAlert } from "lucide-react";
import { isRateLimited } from "../lib/accountHTTP.js";

const reasons = {
  forbidden: "上游拒绝了本次请求，请核对账号权限和所选模型。",
  unauthorized: "上游拒绝了已保存的授权，请核对账号授权状态。",
  rate_limited: "上游请求受限，请稍后再试。",
  timeout: "检测超时，未取得完整结果。",
  invalid_response: "未取得符合要求的完整回答，本次没有评分。",
  unexpected_content_type: "上游返回的内容类型不符合要求，未取得有效结果。",
  invalid_stream: "响应数据流格式异常，未取得完整有效结果。",
  invalid_answer_format: "模型答案格式不符合要求，本次没有评分。",
  incomplete: "模型回答未完整结束，本次没有评分。",
  model_mismatch: "响应模型与本次选择不符，本次结果未采用。",
  network_error: "连接中断或网络请求失败，未自动重试。",
  http_error: "上游请求失败，请核对响应状态和账号可用性。",
  upstream_error: "上游返回错误，未取得有效结果。",
  response_too_large: "模型响应超出大小限制，本次结果未采用。",
  refused: "模型拒绝了本次请求，未取得有效结果。",
  authorization_expired: "已保存的授权已过期，请先核对账号授权。",
  authorization_unavailable: "没有可用的已保存授权，未发起检测。",
  invalid_credentials: "已保存授权身份无效，未发起检测。",
  invalid_options: "本次模型或推理强度配置无效。",
  superseded: "账号授权或检测配置已变化，本次结果未采用。",
};

export function ProbeFailure({ message, code, httpStatus, title = "检测失败", tone = "danger" }) {
  const limited = isRateLimited(httpStatus, code);
  const failureTone = limited ? "warning" : tone;
  const heading = limited && title === "检测失败" ? "请求限流" : title;
  const reason = typeof message === "string" && message.trim() ? message : reasons[code] || "服务未返回具体原因，请核对账号授权、模型与网络后再试。";
  const status = Number.isInteger(httpStatus) && httpStatus >= 100 && httpStatus <= 599 ? httpStatus : null;
  return <div className={`quality-diagnostic probe-failure is-${failureTone}`} role="status" aria-label="检测错误详情">
    <div className="probe-failure-heading"><strong><CircleAlert size={15} aria-hidden="true" />{heading}</strong><span>{status && <span className="probe-http-status">响应 HTTP {status}</span>}{code && <code>{code}</code>}</span></div>
    <p>{reason}</p>
  </div>;
}
