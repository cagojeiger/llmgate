"""Pytest configuration for llmgate e2e tests.

Builds the gate binary once per session, boots it on :8080, polls
/healthz until ready, and terminates it on session end.

Set ``LLMGATE_E2E_EXTERNAL=1`` to skip subprocess management and reuse
a gate that's already running externally (CI / supervisor mode).
"""

from __future__ import annotations

import os
import socket
import subprocess
import time
from pathlib import Path
from typing import Iterator, Optional

import httpx
import pytest
from dotenv import load_dotenv


REPO_ROOT = Path(__file__).resolve().parents[2]
GATE_PORT = 8080
GATE_BASE_URL = f"http://127.0.0.1:{GATE_PORT}"


def _tail(path: Path, n_chars: int = 2000) -> str:
    try:
        text = path.read_text(errors="replace")
        return text[-n_chars:]
    except FileNotFoundError:
        return "(no log)"


def _wait_healthz(
    base_url: str,
    *,
    timeout: float,
    proc: Optional[subprocess.Popen] = None,
    stderr_path: Optional[Path] = None,
) -> None:
    deadline = time.monotonic() + timeout
    last_err = "no attempt"
    while time.monotonic() < deadline:
        try:
            r = httpx.get(f"{base_url}/healthz", timeout=1.0)
            if r.status_code == 200:
                return
            last_err = f"status={r.status_code}"
        except Exception as exc:  # connection refused etc. — expected during boot
            last_err = repr(exc)
        time.sleep(0.2)

    msg = f"gate did not become healthy within {timeout}s. last={last_err}"
    if stderr_path is not None:
        msg += f"\n--- stderr tail ---\n{_tail(stderr_path)}"
    if proc is not None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    pytest.fail(msg)


@pytest.fixture(scope="session")
def gate_base_url(tmp_path_factory) -> Iterator[str]:
    if os.environ.get("LLMGATE_E2E_EXTERNAL") == "1":
        _wait_healthz(GATE_BASE_URL, timeout=5)
        yield GATE_BASE_URL
        return

    load_dotenv(REPO_ROOT / ".env")
    if not os.environ.get("LLMGATE_OPENCODE_API_KEY"):
        pytest.skip("LLMGATE_OPENCODE_API_KEY not set; populate .env")

    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        sock.bind(("127.0.0.1", GATE_PORT))
    except OSError:
        sock.close()
        pytest.fail(
            f"port {GATE_PORT} already in use; free it or set LLMGATE_E2E_EXTERNAL=1"
        )
    finally:
        sock.close()

    work_dir = tmp_path_factory.mktemp("llmgate-e2e")
    bin_path = work_dir / "llmgate"

    build = subprocess.run(
        ["go", "build", "-o", str(bin_path), "./cmd/llmgate"],
        cwd=str(REPO_ROOT),
        capture_output=True,
    )
    if build.returncode != 0:
        pytest.fail(
            "go build failed:\n"
            f"stdout:\n{build.stdout.decode(errors='replace')}\n"
            f"stderr:\n{build.stderr.decode(errors='replace')}"
        )

    stdout_path = work_dir / "gate.stdout.log"
    stderr_path = work_dir / "gate.stderr.log"
    out = open(stdout_path, "wb")
    err = open(stderr_path, "wb")

    env = os.environ.copy()
    env.setdefault("LLMGATE_LOG_LEVEL", "debug")

    proc = subprocess.Popen(
        [str(bin_path)],
        cwd=str(REPO_ROOT),
        env=env,
        stdout=out,
        stderr=err,
        start_new_session=True,
    )

    try:
        _wait_healthz(GATE_BASE_URL, timeout=10, proc=proc, stderr_path=stderr_path)
        yield GATE_BASE_URL
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=2)
        out.close()
        err.close()


def assert_streaming_progressive(
    timestamps: list[float],
    *,
    label: str,
    min_chunks_for_gap: int = 5,
) -> None:
    """Assert SSE stream is genuinely progressive (not silently batched).

    A gap > 50ms between *some* pair of consecutive chunks proves the
    server isn't holding the response and flushing all at once. Short
    streams (< min_chunks_for_gap chunks) skip this check.
    """
    n = len(timestamps)
    assert n >= 2, f"{label}: chunk count {n} < 2"
    if n >= min_chunks_for_gap:
        gaps = [t2 - t1 for t1, t2 in zip(timestamps, timestamps[1:])]
        max_gap = max(gaps)
        assert max_gap > 0.05, (
            f"{label}: max inter-chunk gap {max_gap*1000:.1f}ms <= 50ms — "
            "likely silent batching (FlushInterval default?)"
        )


def field(obj, name):
    """Read a possibly-vendor-extension field from an openai SDK Pydantic model."""
    val = getattr(obj, name, None)
    if val is not None:
        return val
    extra = getattr(obj, "model_extra", None) or {}
    return extra.get(name)


import re


def discover_catalog_models() -> list[str]:
    """Read every catalog/models/*.yaml and return their declared ids, sorted.

    Avoids a PyYAML dependency by extracting the top-level ``id:`` line per
    file. Tests use this so adding a model under catalog/ automatically
    enrolls it in the matrix, with no test-file edit required.
    """
    models_dir = REPO_ROOT / "catalog" / "models"
    pattern = re.compile(r"^id:\s*(\S+)", re.MULTILINE)
    ids: list[str] = []
    for path in sorted(models_dir.glob("*.yaml")):
        match = pattern.search(path.read_text())
        if match:
            ids.append(match.group(1))
    return ids


def raw_consumer_key(consumer: str = "example", index: int = 1) -> str:
    """Extract a documented raw client key from consumers/<consumer>.yaml.

    consumers/*.yaml stores sha256 hashes only; the matching raw keys are
    documented in the file's comment block (``# raw key #1: ...``) for
    test fixtures. Tests use this helper so refreshed yaml comments and
    rotated keys flow through without test edits.
    """
    path = REPO_ROOT / "consumers" / f"{consumer}.yaml"
    text = path.read_text()
    match = re.search(rf"raw key #{index}:\s*(\S+)", text)
    if not match:
        raise RuntimeError(
            f"{path}: missing '# raw key #{index}: ...' comment — "
            "fixtures need a documented raw key alongside the sha256 hash"
        )
    return match.group(1)
