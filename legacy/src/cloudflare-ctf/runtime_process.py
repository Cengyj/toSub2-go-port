"""Portable process and NDJSON pipe handling for local JavaScript runtimes."""

from __future__ import annotations

import json
import os
import queue
import subprocess
import tempfile
import threading
from pathlib import Path
from typing import Any


class RuntimeInputFile:
    """A closed, child-readable input file that remains until explicit cleanup."""

    def __init__(self, payload: dict[str, Any]) -> None:
        descriptor, self.name = tempfile.mkstemp(suffix=".json")
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(payload, stream, ensure_ascii=False)
        except BaseException:
            self.close()
            raise

    def close(self) -> None:
        Path(self.name).unlink(missing_ok=True)


class RuntimeOutput:
    """Consume both pipes concurrently; Windows selectors cannot watch pipes."""

    def __init__(self, process: subprocess.Popen[str], events: queue.Queue, source: str) -> None:
        self.process = process
        self.events = events
        self.source = source
        self.stderr_tail = ""
        self.read_error: Exception | None = None
        self.writes: queue.Queue = queue.Queue()
        self.closed = False
        self.threads = [
            threading.Thread(target=self._read_stdout, name=f"{source}-stdout", daemon=True),
            threading.Thread(target=self._read_stderr, name=f"{source}-stderr", daemon=True),
            threading.Thread(target=self._write_stdin, name=f"{source}-stdin", daemon=True),
        ]
        for thread in self.threads:
            thread.start()

    def _read_stdout(self) -> None:
        assert self.process.stdout is not None
        try:
            with self.process.stdout as stream:
                for line in stream:
                    self.events.put((self.source, self.process, line))
        except Exception as error:
            self.read_error = error
        finally:
            self.events.put((self.source, self.process, None))

    def _read_stderr(self) -> None:
        assert self.process.stderr is not None
        try:
            with self.process.stderr as stream:
                while chunk := stream.read(4096):
                    # Bound diagnostics even when a child continuously logs.
                    self.stderr_tail = (self.stderr_tail + chunk)[-4096:]
        except (OSError, ValueError):
            pass

    def _write_stdin(self) -> None:
        assert self.process.stdin is not None
        stream = self.process.stdin
        try:
            while True:
                item = self.writes.get()
                if item is None:
                    return
                payload, complete, result = item
                try:
                    stream.write(payload)
                    stream.flush()
                except (OSError, ValueError) as error:
                    result.append(error)
                finally:
                    complete.set()
        finally:
            try:
                stream.close()
            except (OSError, ValueError):
                pass


def runtime_output(process: subprocess.Popen[str]) -> RuntimeOutput:
    return process._runtime_output


def start_json_runtime(
    script: Path,
    runtime_input: dict[str, Any],
    node_command: str,
    *,
    events: queue.Queue | None = None,
    source: str = "runtime",
) -> tuple[subprocess.Popen[str], RuntimeInputFile]:
    input_file = RuntimeInputFile(runtime_input)
    process = None
    try:
        process = subprocess.Popen(
            [node_command, str(script), input_file.name],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
        )
        process._runtime_output = RuntimeOutput(
            process, events if events is not None else queue.Queue(), source,
        )
        return process, input_file
    except BaseException:
        if process is not None:
            stop_runtime(process)
        input_file.close()
        raise


def write_runtime_message(
    process: subprocess.Popen[str], message: dict[str, Any], timeout: float = 5,
) -> None:
    output = runtime_output(process)
    if output.closed or process.poll() is not None:
        raise RuntimeError(f"{output.source} runtime is no longer running")
    complete = threading.Event()
    result: list[Exception] = []
    output.writes.put((json.dumps(message, ensure_ascii=False) + "\n", complete, result))
    if not complete.wait(timeout=max(0, timeout)):
        stop_runtime(process)
        raise TimeoutError(f"{output.source} runtime stdin write timed out")
    if result:
        raise RuntimeError(f"{output.source} runtime stdin write failed") from result[0]


def runtime_failure(process: subprocess.Popen[str], label: str) -> RuntimeError:
    output = runtime_output(process)
    # EOF and process exit can arrive a few milliseconds apart. Let stderr drain.
    try:
        process.wait(timeout=0.2)
    except subprocess.TimeoutExpired:
        pass
    output.threads[1].join(timeout=0.2)
    detail = output.stderr_tail.strip()[-1000:]
    if output.read_error is not None:
        detail = f"stdout read failed: {type(output.read_error).__name__}"
    suffix = f": {detail}" if detail else ""
    return RuntimeError(f"{label} runtime exited unexpectedly (exit code {process.poll()}){suffix}")


def stop_runtime(process: subprocess.Popen[str]) -> None:
    output = getattr(process, "_runtime_output", None)
    if output is not None and output.closed:
        return
    if output is not None:
        output.closed = True
        output.writes.put(None)
    if process.poll() is None:
        process.kill()
    process.wait(timeout=5)
    if output is not None:
        for thread in output.threads:
            thread.join(timeout=1)
    else:
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                stream.close()
