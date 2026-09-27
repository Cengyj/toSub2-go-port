import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const configured = String(process.env.TOSUB2_PYTHON || "").trim();
const candidates = configured
  ? [{ command: configured, args: [] }]
  : process.platform === "win32"
    ? [{ command: "python", args: [] }, { command: "py", args: ["-3"] }]
    : [{ command: "python3", args: [] }, { command: "python", args: [] }];
const python = candidates.find(({ command, args }) => spawnSync(command, [...args, "-c", "import curl_cffi"], {
  stdio: "ignore",
  windowsHide: true,
  timeout: 10_000,
}).status === 0);
assert.ok(python, "Set TOSUB2_PYTHON to a Python interpreter with curl_cffi installed");

const result = spawnSync(python.command, [
  ...python.args,
  fileURLToPath(new URL("./runtime-process.py", import.meta.url)),
  process.execPath,
], {
  encoding: "utf8",
  env: { ...process.env, PYTHONDONTWRITEBYTECODE: "1", PYTHONIOENCODING: "utf-8" },
  windowsHide: true,
  timeout: 90_000,
});
assert.equal(result.status, 0, `Runtime process tests failed:\n${result.error?.message || ""}\n${result.stderr || result.stdout}`);
process.stdout.write(result.stdout || "Runtime process tests passed\n");
