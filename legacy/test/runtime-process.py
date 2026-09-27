"""Local-only regressions for Windows process pipes and runtime cleanup."""

from __future__ import annotations

import json
import queue
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


NODE = sys.argv.pop(1) if len(sys.argv) > 1 and not sys.argv[1].startswith("-") else shutil.which("node")
if not NODE:
    raise RuntimeError("Node.js is required for local runtime process tests")
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src" / "cloudflare-ctf"))

import cloudflare_solver
import runtime_process
import sentinel_dynamic


class RuntimeProcessTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory(prefix="tosub2-runtime-test-")
        self.runtimes = []

    def tearDown(self) -> None:
        for process, input_file in self.runtimes:
            runtime_process.stop_runtime(process)
            input_file.close()
            self.assertFalse(Path(input_file.name).exists())
            self.assertIsNotNone(process.poll())
            self.assertTrue(all(not thread.is_alive() for thread in runtime_process.runtime_output(process).threads))
            self.assertTrue(all(stream.closed for stream in (process.stdin, process.stdout, process.stderr)))
        self.directory.cleanup()

    def script(self, source: str, filename: str = "fixture.cjs") -> Path:
        path = Path(self.directory.name) / filename
        path.write_text(source, encoding="utf-8")
        return path

    def start(self, source_text: str, payload=None, **kwargs):
        process, input_file = runtime_process.start_json_runtime(
            self.script(source_text, f"fixture-{len(self.runtimes)}.cjs"), payload or {}, NODE, **kwargs,
        )
        self.runtimes.append((process, input_file))
        return process

    def line(self, process, timeout=5):
        _name, _process, line = runtime_process.runtime_output(process).events.get(timeout=timeout)
        return json.loads(line) if line is not None else None

    def test_child_reads_closed_utf8_file_and_pipe(self):
        process = self.start("""
            const fs = require('node:fs');
            const input = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
            process.stdout.write(JSON.stringify(input) + '\\n');
            const lines = require('node:readline').createInterface({input: process.stdin});
            lines.on('line', line => process.stdout.write(JSON.stringify(JSON.parse(line)) + '\\n'));
        """, {"text": "Windows 中文 👋"})
        self.assertEqual(self.line(process), {"text": "Windows 中文 👋"})
        runtime_process.write_runtime_message(process, {"message": "管道往返 ✓"})
        self.assertEqual(self.line(process), {"message": "管道往返 ✓"})

    def test_stderr_flood_and_buffered_stdout_do_not_deadlock(self):
        process = self.start("""
            const fs = require('node:fs');
            fs.writeSync(2, Buffer.alloc(512 * 1024, 120));
            fs.writeSync(2, 'TAIL-MARKER');
            fs.writeSync(1, '{"n":1}\\n{"n":2}\\n{"n":3}\\n');
        """)
        self.assertEqual([self.line(process) for _ in range(3)], [{"n": 1}, {"n": 2}, {"n": 3}])
        self.assertIsNone(self.line(process))
        runtime_process.stop_runtime(process)
        diagnostics = runtime_process.runtime_output(process).stderr_tail
        self.assertTrue(diagnostics.endswith("TAIL-MARKER"))
        self.assertLessEqual(len(diagnostics), 4096)

    def test_two_processes_share_queue_without_selectors(self):
        events = queue.Queue()
        first = self.start("process.stdout.write('{\"side\":\"a\"}\\n');", events=events, source="a")
        second = self.start("process.stdout.write('{\"side\":\"b\"}\\n');", events=events, source="b")
        found = {}
        for _ in range(4):
            source, process, line = events.get(timeout=5)
            if line is not None:
                found[source] = (process, json.loads(line))
        self.assertEqual(found, {"a": (first, {"side": "a"}), "b": (second, {"side": "b"})})

    def test_spawn_failure_removes_input_file(self):
        created = []
        real_mkstemp = tempfile.mkstemp

        def capture_file(*args, **kwargs):
            descriptor, filename = real_mkstemp(*args, **kwargs)
            created.append(Path(filename))
            return descriptor, filename

        with mock.patch.object(runtime_process.tempfile, "mkstemp", side_effect=capture_file):
            with self.assertRaises(OSError):
                runtime_process.start_json_runtime(self.script(""), {}, str(Path(self.directory.name) / "missing-node.exe"))
        self.assertEqual(len(created), 1)
        self.assertFalse(created[0].exists())

    def test_blocked_stdin_write_times_out_and_kills_process(self):
        process = self.start("process.stdout.write('{\"ready\":true}\\n'); setInterval(() => {}, 1000);")
        self.assertEqual(self.line(process), {"ready": True})
        started = time.monotonic()
        with self.assertRaisesRegex(TimeoutError, "stdin write timed out"):
            runtime_process.write_runtime_message(process, {"body": "x" * (2 * 1024 * 1024)}, timeout=0.2)
        self.assertLess(time.monotonic() - started, 3)
        self.assertIsNotNone(process.poll())

    def test_sentinel_result_after_noisy_stderr(self):
        fixture = self.script("""
            const fs = require('node:fs');
            const input = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
            const lines = require('node:readline').createInterface({input: process.stdin});
            lines.on('line', line => {
                fs.writeSync(2, Buffer.alloc(128 * 1024, 120));
                process.stdout.write(JSON.stringify({type:'result', value:input.cookies.fixture}) + '\\n');
            });
        """)
        with mock.patch.object(sentinel_dynamic, "RUNTIME", fixture):
            process, input_file = sentinel_dynamic.start_runtime({}, "https://fixture.invalid/", {"fixture": "中文"}, node_command=NODE)
        self.runtimes.append((process, input_file))
        result = sentinel_dynamic.run_call(process, None, None, "https://fixture.invalid/", "token", "fixture", timeout_seconds=3)
        self.assertEqual(result["value"], "中文")
        self.assertEqual(result["networkResponses"], [])

    def test_sentinel_partial_stdout_times_out_and_cleans_process(self):
        process = self.start("process.stdout.write('{'); setInterval(() => {}, 1000);")
        started = time.monotonic()
        with self.assertRaisesRegex(TimeoutError, "Sentinel runtime call timed out"):
            sentinel_dynamic.run_call(process, None, None, "https://fixture.invalid/", "token", "fixture", timeout_seconds=0.3)
        self.assertLess(time.monotonic() - started, 3)
        self.assertIsNotNone(process.poll())

    def test_sentinel_eof_reports_exit_and_stderr(self):
        process = self.start("""
            const lines = require('node:readline').createInterface({input: process.stdin});
            lines.once('line', () => {
                require('node:fs').writeSync(2, 'fixture-crash');
                process.exit(7);
            });
        """)
        with self.assertRaisesRegex(RuntimeError, r"exit code 7.*fixture-crash"):
            sentinel_dynamic.run_call(process, None, None, "https://fixture.invalid/", "token", "fixture", timeout_seconds=3)

    def solver_session(self):
        session = SimpleNamespace(cookies=SimpleNamespace(jar=[]), requests=[], gets=[])

        def request(method, url, **kwargs):
            session.requests.append((method, url, kwargs))
            session.cookies.jar.append(SimpleNamespace(
                name="cf_clearance", value="local-fixture", domain="fixture.invalid", path="/", expires=None,
            ))
            return SimpleNamespace(status_code=200, url=url, content=b"ok", text="ok", headers={})

        def get(url, **kwargs):
            session.gets.append((url, kwargs))
            return SimpleNamespace(status_code=200, url=url, content=b"fixture frame", text="fixture frame", headers={})

        session.request = request
        session.get = get
        return session

    def track_solver_start(self):
        real_start = cloudflare_solver.start_runtime

        def track(*args, **kwargs):
            result = real_start(*args, **kwargs)
            self.runtimes.append(result)
            return result

        return mock.patch.object(cloudflare_solver, "start_runtime", side_effect=track)

    def test_cloudflare_parent_child_local_message_bridge(self):
        parent = self.script("""
            const emit = data => process.stdout.write(JSON.stringify(data) + '\\n');
            emit({type:'frame', url:'https://frame.fixture.invalid/'});
            for (const event of ['init','extraParams','execute']) emit({type:'frame-message', message:{data:{event}}});
            process.stdin.resume();
        """, "parent.cjs")
        child = self.script("""
            const emit = data => process.stdout.write(JSON.stringify(data) + '\\n');
            emit({type:'parent-message', message:{data:{event:'fixture'}}});
            emit({type:'request', request:{id:1, kind:'fetch', method:'POST', url:'https://fixture.invalid/local', body:'fixture'}});
            process.stdin.resume();
        """, "child.cjs")
        session = self.solver_session()
        with mock.patch.object(cloudflare_solver, "PARENT_RUNTIME", parent), mock.patch.object(cloudflare_solver, "CHILD_RUNTIME", child), self.track_solver_start():
            result = cloudflare_solver.solve_challenge(
                session, challenge_url="https://fixture.invalid/", challenge_html="<script>_cf_chl_opt={}</script>",
                challenge_size=42, user_agent="fixture", node_command=NODE, timeout_seconds=3,
            )
        self.assertTrue(result["ok"])
        self.assertEqual(len(self.runtimes), 2)
        self.assertEqual(len(session.requests), 1)
        self.assertEqual(len(session.gets), 2)
        self.assertTrue(all(0 < options["timeout"] <= 3 for _method, _url, options in session.requests))
        self.assertTrue(all(0 < options["timeout"] <= 3 for _url, options in session.gets))
        for process, input_file in self.runtimes:
            self.assertIsNotNone(process.poll())
            self.assertFalse(Path(input_file.name).exists())

    def test_cloudflare_partial_stdout_timeout_cleans_all_files(self):
        fixture = self.script("process.stdout.write('{'); setInterval(() => {}, 1000);")
        with mock.patch.object(cloudflare_solver, "PARENT_RUNTIME", fixture), self.track_solver_start():
            with self.assertRaisesRegex(RuntimeError, "before timeout"):
                cloudflare_solver.solve_challenge(
                    self.solver_session(), challenge_url="https://fixture.invalid/", challenge_html="_cf_chl_opt",
                    challenge_size=10, user_agent="fixture", node_command=NODE, timeout_seconds=0.3,
                )
        self.assertIsNotNone(self.runtimes[0][0].poll())
        self.assertFalse(Path(self.runtimes[0][1].name).exists())

    def test_cloudflare_crash_reports_immediate_failure(self):
        fixture = self.script("require('node:fs').writeSync(2, 'fixture-startup-error'); process.exit(4);")
        with mock.patch.object(cloudflare_solver, "PARENT_RUNTIME", fixture), self.track_solver_start():
            started = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, r"Cloudflare parent runtime exited unexpectedly.*exit code 4.*fixture-startup-error"):
                cloudflare_solver.solve_challenge(
                    self.solver_session(), challenge_url="https://fixture.invalid/", challenge_html="_cf_chl_opt",
                    challenge_size=10, user_agent="fixture", node_command=NODE, timeout_seconds=10,
                )
            self.assertLess(time.monotonic() - started, 3)


if __name__ == "__main__":
    unittest.main(verbosity=2)
