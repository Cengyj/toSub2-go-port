import assert from "node:assert/strict";
import crypto from "node:crypto";
import { EventEmitter } from "node:events";
import fs from "node:fs/promises";
import vm from "node:vm";

// Execute the server's actual lifecycle functions with controlled I/O. This
// covers races without starting the UI server or making authentication calls.
const source = await fs.readFile(new URL("../src/console-server.mjs", import.meta.url), "utf8");
const functionNames = [
  "prepareAndLaunchJob", "launchJob", "enqueueJob", "handleChildClose", "consumeOutput",
  "restartAfterProxyRisk", "isCurrentProxyRiskRestart", "resetProxyRiskState",
  "finishProxyRiskRetries", "cancelJob", "retryJob", "restartJobAfterConfigurationUpdate",
  "isActive", "isTerminalStatus", "sanitizeLog",
];
const functions = functionNames.map((name) => {
  const match = new RegExp(`^(?:async )?function ${name}\\(`, "m").exec(source);
  assert.ok(match, `missing server function ${name}`);
  const end = source.indexOf("\n}", match.index);
  assert.ok(end > match.index, `missing end of server function ${name}`);
  return source.slice(match.index, end + 2);
}).join("\n\n");

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function harness({ probe = async () => "chrome142", env = {} } = {}) {
  const launches = [];
  const failures = [];
  const context = vm.createContext({
    crypto,
    process: { execPath: "/mock/node", env },
    PROTOCOL_SCRIPT: "/mock/protocol.mjs",
    WORKSPACE_ROOT: "/mock",
    DEFAULT_TLS_PROFILE: "chrome146",
    MAX_PROXY_RISK_RETRIES: 10,
    MAX_PROXY_CONNECTION_FAILURES: 20,
    MAX_LOG_CHARS: 20_000,
    PROXY_CONNECTION_RETRY_BASE_MS: 1,
    PROXY_CONNECTION_RETRY_MAX_MS: 5,
    shuttingDown: false,
    directTlsProfileProbe: { resolve: probe },
    proxySupportsSessionRotation: () => true,
    removePrivateFile: async () => {},
    loadMailboxBaseline: async () => {},
    saveJobMetadata: async () => {},
    fileExists: async () => false,
    delay: async () => {},
    stopMailPolling() {},
    stopSmsPolling() {},
    releaseSmsNumber() {},
    scheduleQueuedJobs() {},
    touch() {},
    clearAutoRepairBlock() {},
    recordJobOperation() {},
    beginAuthorizationAutomationAttempt() {},
    appendJobLog(job, text) { job.logs += text; },
    failJob(job, message) {
      failures.push(message);
      job.status = "failed";
      job.lastError = message;
    },
    withEmailJobLock: async (_email, work) => work(),
    spawn(_command, _args, options) {
      const child = new EventEmitter();
      child.stdout = new EventEmitter();
      child.stderr = new EventEmitter();
      child.kill = () => {};
      launches.push({ child, options });
      return child;
    },
  });
  vm.runInContext(functions, context, { filename: "console-risk-lifecycle.mjs" });
  const job = {
    email: "test@example.com",
    status: "starting",
    runId: "original-run",
    runMode: "full",
    queuedMode: "full",
    checkpointPath: "/mock/checkpoint",
    outputPath: "/mock/output",
    logs: "",
    parserTail: "",
    proxyAttemptParserTail: "",
    proxyUrl: null,
    proxyRiskRetryCount: 0,
    proxyConnectionFailureCount: 0,
    proxyRiskRestarting: false,
    proxySessionAttemptIds: new Set(),
    directTlsFallbackAttempted: false,
    directTlsProfile: null,
    lastRiskControlError: null,
    authAutomationAttempt: {},
    mailCandidateCounts: new Map(),
    attempt: 1,
  };
  return { context, job, launches, failures };
}

// The selected profile must survive the real queue/preparation/launch path.
{
  const gate = deferred();
  let probes = 0;
  const h = harness({ probe: () => { probes += 1; return gate.promise; } });
  const retry = h.context.restartAfterProxyRisk(h.job);
  assert.notEqual(h.job.runId, "original-run");
  await h.context.handleChildClose(h.job, { code: 1, mode: "full", runId: "original-run" });
  assert.equal(h.job.status, "starting", "the old process exit must not fail a pending retry");
  assert.equal(h.failures.length, 0);
  gate.resolve("chrome142");
  await retry;
  assert.equal(h.job.status, "queued");
  assert.equal(h.job.directTlsProfile, "chrome142");
  h.job.status = "starting";
  h.job.queueRunId = "queue-2";
  await h.context.prepareAndLaunchJob(h.job, "full", "queue-2");
  assert.equal(h.launches[0].options.env.TOSUB2_TLS_PROFILE, "chrome142");
  assert.equal(h.launches[0].options.windowsHide, true);
  assert.doesNotMatch(h.job.logs, /正在继续更换|0\/10 个代理会话/);
  h.job.lastRiskControlError = "PROXY_RISK_CONTROL: GET https://example.com/ returned HTTP 403";
  await h.context.restartAfterProxyRisk(h.job);
  assert.equal(probes, 1, "one manual attempt must use at most one direct fallback");
  assert.equal(h.job.status, "failed");
  assert.match(h.job.lastError, /本次直连 TLS 兜底已尝试.*HTTP 403/);
}

// Explicit operator configuration always takes precedence over a cached choice.
{
  const h = harness({ env: { TOSUB2_TLS_PROFILE: "chrome136" } });
  h.job.directTlsProfile = "chrome142";
  h.context.launchJob(h.job, { mode: "full" });
  assert.equal(h.launches[0].options.env.TOSUB2_TLS_PROFILE, "chrome136");
}

// A user cancellation must survive both late success and late failure.
for (const rejectProbe of [false, true]) {
  const gate = deferred();
  const h = harness({ probe: () => gate.promise });
  const retry = h.context.restartAfterProxyRisk(h.job);
  await h.context.cancelJob(h.job);
  if (rejectProbe) gate.reject(new Error("late probe failure"));
  else gate.resolve("chrome142");
  await retry;
  assert.equal(h.job.status, "canceled");
  assert.equal(h.job.directTlsProfile, null);
  assert.equal(h.failures.length, 0);
}

// Reconfiguration creates a new attempt; an earlier pending operation owns none
// of its fields, including the restart flag and retry budget.
for (const rejectProbe of [false, true]) {
  const gate = deferred();
  const h = harness({ probe: () => gate.promise });
  const retry = h.context.restartAfterProxyRisk(h.job);
  h.context.restartJobAfterConfigurationUpdate(h.job);
  const newRunId = h.job.runId;
  h.job.proxyRiskRestarting = true;
  if (rejectProbe) gate.reject(new Error("old operation failed"));
  else gate.resolve("chrome142");
  await retry;
  assert.equal(h.job.runId, newRunId);
  assert.equal(h.job.status, "queued");
  assert.equal(h.job.directTlsFallbackAttempted, false);
  assert.equal(h.job.directTlsProfile, null);
  assert.equal(h.job.proxyRiskRestarting, true, "old finally must not clear a newer operation's flag");
  assert.equal(h.failures.length, 0);
}

for (const invalidation of ["shutdown", "delete"]) {
  const gate = deferred();
  const h = harness({ probe: () => gate.promise });
  const retry = h.context.restartAfterProxyRisk(h.job);
  if (invalidation === "shutdown") h.context.shuttingDown = true;
  else h.job.deleted = true;
  gate.resolve("chrome142");
  await retry;
  assert.notEqual(h.job.status, "queued", `${invalidation} must prevent requeueing`);
  assert.equal(h.job.directTlsProfile, null);
}

// Every awaited preparation step needs the same stale-operation guard.
for (const invalidation of ["cancel", "new_run"]) {
  const gate = deferred();
  const h = harness();
  h.job.mailApiUrl = "http://mail.example/messages";
  h.job.queueRunId = "old-queue";
  h.context.loadMailboxBaseline = () => gate.promise;
  const preparing = h.context.prepareAndLaunchJob(h.job, "full", "old-queue");
  if (invalidation === "cancel") {
    await h.context.cancelJob(h.job);
  } else {
    h.context.restartJobAfterConfigurationUpdate(h.job);
    h.job.status = "starting";
    h.job.queueRunId = "new-queue";
  }
  gate.reject(new Error("late mailbox baseline failure"));
  await preparing;
  assert.equal(h.job.status, invalidation === "cancel" ? "canceled" : "starting");
  assert.equal(h.failures.length, 0, "a stale baseline failure must not overwrite a canceled or new run");
  assert.equal(h.launches.length, 0);
}

{
  const gate = deferred();
  const h = harness();
  h.context.removePrivateFile = () => gate.promise;
  const retry = h.context.restartAfterProxyRisk(h.job);
  await new Promise((resolve) => setImmediate(resolve));
  await h.context.cancelJob(h.job);
  gate.resolve();
  await retry;
  assert.equal(h.job.status, "canceled");
}

{
  const gate = deferred();
  const h = harness();
  h.job.proxyUrl = "socks5://proxy.example:5000";
  h.context.delay = () => gate.promise;
  const retry = h.context.restartAfterProxyRisk(h.job, { connectionFailure: true });
  await new Promise((resolve) => setImmediate(resolve));
  await h.context.cancelJob(h.job);
  gate.resolve();
  await retry;
  assert.equal(h.job.status, "canceled");
}

// New manual attempts receive fresh state, while automatic retries stay bounded.
{
  const h = harness();
  Object.assign(h.job, {
    status: "failed", directTlsFallbackAttempted: true, directTlsProfile: "chrome142",
    proxyRiskRetryCount: 10, proxyConnectionFailureCount: 20,
    lastRiskControlError: "previous failure",
  });
  await h.context.retryJob(h.job);
  assert.equal(h.job.status, "queued");
  assert.equal(h.job.directTlsFallbackAttempted, false);
  assert.equal(h.job.directTlsProfile, null);
  assert.equal(h.job.lastRiskControlError, null);
  assert.equal(h.job.proxyRiskRetryCount, 0);
  assert.equal(h.job.proxyConnectionFailureCount, 0);
}

// Preserve the complete, sanitized reason even when stdout splits the marker.
{
  const gate = deferred();
  let probes = 0;
  const h = harness({ probe: () => { probes += 1; return gate.promise; } });
  h.context.consumeOutput(h.job, "[proxy-risk-retry] PROXY_RISK_CON");
  assert.equal(probes, 0, "an incomplete diagnostic line must not start a retry");
  h.context.consumeOutput(h.job, "TROL: GET https://example.com/?token=secret returned HTTP 403\n");
  assert.equal(probes, 1);
  assert.match(h.job.lastRiskControlError, /PROXY_RISK_CONTROL.*HTTP 403/);
  assert.doesNotMatch(h.job.lastRiskControlError, /secret/);
  await h.context.cancelJob(h.job);
  gate.resolve("chrome142");
  await new Promise((resolve) => setImmediate(resolve));
}

console.log("Console risk retry lifecycle regression tests passed.");
