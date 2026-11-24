// Console receipts are evidence of acceptance or persistence, never evidence
// that an authorization/security workflow has completed. Keep the wire-format
// aliases here, away from command orchestration and selection updates.
/** @typedef {'queued'|'partial'|'skipped'|'failed'|'unknown'} SubmissionOutcome */
/** @typedef {{outcome: SubmissionOutcome, jobs: object[], confirmedIds: string[], queued: number, skipped: number, failed: number, unknown: number, uncertain: boolean}} SubmissionReceipt */

export function unverifiedCommandReceipt() {
  const error = new Error("操作回执不完整或与本次请求不一致，结果待核实。请刷新并核对账号状态，避免重复提交。");
  error.code = "ACCOUNT_RECEIPT_UNVERIFIED";
  error.outcome = "unknown";
  return error;
}

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const count = value => Number.isSafeInteger(value) && value >= 0;
const own = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
function requireReceipt(condition) { if (!condition) throw unverifiedCommandReceipt(); }
function optionalCount(value, key, fallback = 0) {
  if (!own(value, key)) return fallback;
  requireReceipt(count(value[key]));
  return value[key];
}
function envelope(value) {
  requireReceipt(object(value));
  for (const key of ["ok", "uncertain"]) if (own(value, key)) requireReceipt(typeof value[key] === "boolean");
}
function idsOf(items, requested = null) {
  requireReceipt(Array.isArray(items));
  const seen = new Set();
  for (const item of items) {
    requireReceipt(object(item) && typeof item.id === "string" && item.id.length > 0 && !seen.has(item.id) && (!requested || requested.has(item.id)));
    seen.add(item.id);
  }
  return [...seen];
}

/** @returns {SubmissionReceipt} */
export function decodeBatchSubmission(value, requestedIds) {
  envelope(value);
  const requested = new Set(requestedIds);
  requireReceipt(requested.size > 0 && requested.size === requestedIds.length);
  const queued = optionalCount(value, "queued", value.started);
  requireReceipt(count(queued) && count(value.skipped));
  if (own(value, "started")) requireReceipt(count(value.started) && value.started === queued);
  const failed = optionalCount(value, "failed"), unknown = optionalCount(value, "unknown");
  const selected = optionalCount(value, "selected", requested.size);
  requireReceipt(selected === requested.size && queued + value.skipped + failed + unknown === selected);
  const confirmedIds = idsOf(value.jobs, requested);
  requireReceipt(confirmedIds.length === queued);
  const failures = value.failures == null ? [] : value.failures;
  const failureIds = idsOf(failures, requested);
  requireReceipt(failureIds.length <= failed + unknown && !failureIds.some(id => confirmedIds.includes(id)));
  for (const failure of failures) {
    requireReceipt(typeof failure.error === "string" && (!own(failure, "uncertain") || typeof failure.uncertain === "boolean"));
  }
  if (own(value, "uncertain")) requireReceipt(value.uncertain === (unknown > 0));
  if (own(value, "ok")) requireReceipt(value.ok === (queued > 0 && failed === 0 && unknown === 0));
  const outcome = queued ? (failed || unknown ? "partial" : "queued") : unknown ? "unknown" : failed ? "failed" : "skipped";
  if (own(value, "outcome")) requireReceipt(value.outcome === outcome);
  return { ...value, outcome, selected, queued, started: queued, skipped: value.skipped, failed, unknown, uncertain: unknown > 0, failures, confirmedIds };
}

export function decodeSingleSubmission(value, { id, email } = {}) {
  envelope(value);
  requireReceipt(value.ok !== false && value.uncertain !== true && object(value.job));
  if (own(value, "outcome")) requireReceipt(["queued", "accepted", "completed", "succeeded"].includes(value.outcome));
  idsOf([value.job]);
  if (id) requireReceipt(value.job.id === id);
  if (email) requireReceipt(typeof value.job.email === "string" && value.job.email.trim().toLowerCase() === email.trim().toLowerCase());
  return value.job;
}

export function decodeImportReceipt(value, submittedLines) {
  envelope(value);
  // Older successful imports carried only created/updated/jobs. New receipts
  // explicitly distinguish confirmed saves, uncertain rows, and unprocessed rows.
  const created = optionalCount(value, "created", NaN), updated = optionalCount(value, "updated", NaN);
  const completed = optionalCount(value, "completed", created + updated);
  const selected = optionalCount(value, "selected", submittedLines);
  const failed = optionalCount(value, "failed"), unknown = optionalCount(value, "unknown"), unprocessed = optionalCount(value, "unprocessed");
  requireReceipt(count(created) && count(updated) && count(completed) && selected === submittedLines);
  requireReceipt(completed === created + updated && completed + failed + unknown + unprocessed === selected);
  // Repeated email rows may deliberately update the same account more than once.
  requireReceipt(Array.isArray(value.jobs) && value.jobs.length === completed);
  for (const job of value.jobs) {
    idsOf([job]);
    requireReceipt(typeof job.email === "string" && job.email.length > 0);
  }
  if (own(value, "uncertain")) requireReceipt(value.uncertain === (unknown > 0));
  const outcome = completed === selected ? "completed" : completed ? "partial" : unknown ? "unknown" : "failed";
  if (own(value, "outcome")) requireReceipt(value.outcome === outcome);
  if (own(value, "ok")) requireReceipt(value.ok === (outcome === "completed"));
  return { ...value, outcome, selected, completed, created, updated, failed, unknown, unprocessed, uncertain: unknown > 0 };
}

/** @returns {{receipt: object, confirmedIds: string[]}} */
export function decodeUploadReceipt(value, requestedIds, target) {
  envelope(value);
  const requested = new Set(requestedIds);
  requireReceipt(requested.size > 0 && requested.size === requestedIds.length);
  const selected = optionalCount(value, "selected", requested.size);
  requireReceipt(selected === requested.size);
  if (own(value, "target")) requireReceipt(value.target === target);
  const counts = value.result;
  envelope(counts);
  const created = optionalCount(counts, "created"), updated = optionalCount(counts, "updated");
  const success = optionalCount(counts, "success", created + updated);
  const failed = optionalCount(counts, "failed"), unknown = optionalCount(counts, "unknown"), notAttempted = optionalCount(counts, "not_attempted");
  const skipped = optionalCount(value, "skipped"), innerSkipped = optionalCount(counts, "skipped");
  requireReceipt(success <= selected && failed <= selected && unknown <= selected && notAttempted <= selected && skipped <= selected && innerSkipped <= selected);
  if (target === "cpa") {
    const uploaded = optionalCount(value, "uploaded", success);
    requireReceipt(uploaded === success);
    if (!Array.isArray(value.results)) {
      // Aggregate-only old CPA receipts can be displayed, but never establish
      // which local selection was confirmed. A blocked preflight has no results.
      requireReceipt(!own(value, "results"));
      return { receipt: value, confirmedIds: [] };
    }
    const ids = idsOf(value.results, requested);
    requireReceipt(ids.length === selected);
    const byOutcome = { succeeded: [], failed: [], unknown: [], skipped: [] };
    for (const item of value.results) {
      requireReceipt(own(byOutcome, item.outcome));
      byOutcome[item.outcome].push(item.id);
    }
    requireReceipt(byOutcome.succeeded.length === success);
    if (own(counts, "failed")) requireReceipt(failed === byOutcome.failed.length);
    if (own(counts, "unknown")) requireReceipt(unknown === byOutcome.unknown.length);
    if (own(counts, "not_attempted")) requireReceipt(notAttempted >= byOutcome.skipped.length && notAttempted <= byOutcome.skipped.length + byOutcome.failed.length);
    if (own(value, "skipped")) requireReceipt(skipped >= byOutcome.skipped.length && skipped <= byOutcome.skipped.length + byOutcome.failed.length);
    if (own(value, "uncertain")) requireReceipt(value.uncertain === (byOutcome.unknown.length > 0));
    if (value.ok === true) requireReceipt(success === selected);
    const outcome = success === selected ? "succeeded" : success ? "partial" : byOutcome.unknown.length ? "unknown" : "failed";
    if (own(value, "outcome")) requireReceipt(value.outcome === outcome || outcome === "succeeded" && value.outcome === "completed");
    if (own(value, "attempted")) {
      const attempted = optionalCount(value, "attempted");
      // A version check can fail before sending. Server skipped counts include
      // this failed item, so do not equate skipped with per-item skipped receipts.
      requireReceipt(attempted >= success && attempted <= selected && attempted + skipped === selected);
    }
    return { receipt: { ...value, outcome, uncertain: byOutcome.unknown.length > 0 }, confirmedIds: byOutcome.succeeded };
  }
  requireReceipt(success <= created + updated && created + updated <= selected);
  const uploaded = optionalCount(value, "uploaded", selected - skipped);
  requireReceipt(uploaded + skipped === selected);
  if (own(counts, "total")) requireReceipt(count(counts.total) && counts.total === uploaded);
  requireReceipt(success + failed + unknown + notAttempted + innerSkipped <= uploaded);
  if (own(value, "outcome")) requireReceipt(["completed", "succeeded", "partial", "failed", "unknown", "blocked", "skipped"].includes(value.outcome));
  if (value.ok === true) requireReceipt(!["partial", "failed", "unknown", "blocked"].includes(value.outcome)
    && value.uncertain !== true && counts.ok !== false && !failed && !unknown && !notAttempted
    && success + innerSkipped === uploaded);
  const complete = value.ok === true && value.uncertain !== true && !value.error
    && !["partial", "failed", "unknown", "blocked"].includes(value.outcome)
    && counts.ok !== false && !failed && !unknown && !notAttempted && !skipped && !innerSkipped
    && success === selected && success === created + updated;
  // Sub2API provides aggregate results, so partial success cannot identify local
  // IDs. Only a complete batch can safely remove the original selection.
  return { receipt: value, confirmedIds: complete ? [...requested] : [] };
}

export function decodeCancellationReceipt(value) {
  envelope(value);
  requireReceipt(count(value.canceled) && value.ok !== false && value.uncertain !== true);
  // Current cancel-all success carries only canceled. If an outcome is supplied,
  // acceptance or partial progress does not confirm that cancellation completed.
  if (own(value, "outcome")) requireReceipt(["completed", "succeeded"].includes(value.outcome));
  return value;
}
