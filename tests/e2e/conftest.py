"""Pytest configuration for llmgate e2e.

Two modes via ``LLMGATE_E2E_MODE``:

  live      — real upstream from .env (default; ``make e2e``)
  cassette  — replays tests/e2e/fixtures via cassette.py (``make e2e-mock``)

Set ``LLMGATE_E2E_EXTERNAL=1`` to reuse a gate already running on :8080.
``@pytest.mark.live_only`` auto-skips in cassette mode. ADR 006.
"""

from __future__ import annotations

import os
import re
import shutil
import socket
import subprocess
import time
from pathlib import Path
from typing import Iterator, Optional

import httpx
import pytest
from dotenv import load_dotenv

import cassette


REPO_ROOT = Path(__file__).resolve().parents[2]
GATE_PORT = 8080
GATE_BASE_URL = f"http://127.0.0.1:{GATE_PORT}"
FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers",
        "live_only: test requires a real upstream; auto-skipped in cassette mode",
    )


def pytest_collection_modifyitems(
    config: pytest.Config, items: list[pytest.Item]
) -> None:
    if os.environ.get("LLMGATE_E2E_MODE", "live").lower() != "cassette":
        return
    skip_marker = pytest.mark.skip(reason="cassette mode: live_only test")
    for item in items:
        if "live_only" in item.keywords:
            item.add_marker(skip_marker)


def _tail(path: Path, n_chars: int = 2000) -> str:
    try:
        return path.read_text(errors="replace")[-n_chars:]
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
        except Exception as exc:
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


def _build_gate(work_dir: Path) -> Path:
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
    return bin_path


def _ensure_port_free(port: int) -> None:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        sock.bind(("127.0.0.1", port))
    except OSError:
        sock.close()
        pytest.fail(f"port {port} already in use; free it or set LLMGATE_E2E_EXTERNAL=1")
    finally:
        sock.close()


def _materialize_cassette_catalog(dst_root: Path, upstream_url: str) -> Path:
    """Copy catalog/ into dst_root, rewriting every base_url: to upstream_url."""
    src = REPO_ROOT / "catalog"
    dst = dst_root / "catalog"
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    base_url_pat = re.compile(r"^(\s*base_url:).*$", flags=re.MULTILINE)
    for yaml_path in (dst / "models").glob("*.yaml"):
        text = yaml_path.read_text()
        yaml_path.write_text(base_url_pat.sub(rf"\1 {upstream_url}", text))
    return dst


@pytest.fixture(scope="session")
def gate_base_url(tmp_path_factory) -> Iterator[str]:
    if os.environ.get("LLMGATE_E2E_EXTERNAL") == "1":
        _wait_healthz(GATE_BASE_URL, timeout=5)
        yield GATE_BASE_URL
        return

    mode = os.environ.get("LLMGATE_E2E_MODE", "live").lower()
    work_dir = tmp_path_factory.mktemp(f"llmgate-e2e-{mode}")
    bin_path = _build_gate(work_dir)
    _ensure_port_free(GATE_PORT)

    env = os.environ.copy()
    env.setdefault("LLMGATE_LOG_LEVEL", "debug")

    upstream_server: Optional[object] = None
    if mode == "cassette":
        upstream_server, upstream_port = cassette.start(FIXTURES_DIR)
        env["LLMGATE_CATALOG"] = str(
            _materialize_cassette_catalog(work_dir, f"http://127.0.0.1:{upstream_port}")
        )
        # gate factory requires the env to exist; cassette doesn't validate it.
        env.setdefault("LLMGATE_OPENCODE_API_KEY", "dummy-cassette-key")
    elif mode == "live":
        load_dotenv(REPO_ROOT / ".env")
        if not os.environ.get("LLMGATE_OPENCODE_API_KEY"):
            pytest.skip("LLMGATE_OPENCODE_API_KEY not set; populate .env")
        env["LLMGATE_OPENCODE_API_KEY"] = os.environ["LLMGATE_OPENCODE_API_KEY"]
    else:
        pytest.fail(f"unknown LLMGATE_E2E_MODE: {mode}")

    stderr_path = work_dir / "gate.stderr.log"
    out = open(work_dir / "gate.stdout.log", "wb")
    err = open(stderr_path, "wb")
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
        if upstream_server is not None:
            upstream_server.shutdown()
            upstream_server.server_close()


def assert_streaming_progressive(
    timestamps: list[float],
    *,
    label: str,
    min_chunks_for_gap: int = 5,
) -> None:
    """SSE stream is progressive (>50ms gap somewhere). Skipped for short streams."""
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
    """Read a vendor-extension field from an openai SDK Pydantic model."""
    val = getattr(obj, name, None)
    if val is not None:
        return val
    return (getattr(obj, "model_extra", None) or {}).get(name)


def cassette_has_fixture(model: str) -> bool:
    return (FIXTURES_DIR / "models" / model / "chat-completion.json").exists()


@pytest.fixture(autouse=True)
def _skip_if_no_cassette_fixture(request: pytest.FixtureRequest) -> None:
    """In cassette mode, skip parametrized tests whose ``model`` has no fixture."""
    if os.environ.get("LLMGATE_E2E_MODE", "live").lower() != "cassette":
        return
    callspec = getattr(request.node, "callspec", None)
    if callspec is None:
        return
    model = callspec.params.get("model")
    if model and not cassette_has_fixture(model):
        pytest.skip(f"cassette: no fixture for {model}")


def discover_catalog_models(protocol: str | None = None) -> list[str]:
    """Catalog model ids (sorted), optionally filtered by ``protocol:``.

    Avoids a PyYAML dep by regex-extracting top-level fields. Adding a yaml
    under catalog/models/ flows into the matrix on the next run.
    """
    id_pat = re.compile(r"^id:\s*(\S+)", re.MULTILINE)
    proto_pat = re.compile(r"^protocol:\s*(\S+)", re.MULTILINE)
    ids: list[str] = []
    for path in sorted((REPO_ROOT / "catalog" / "models").glob("*.yaml")):
        text = path.read_text()
        if protocol is not None:
            m_proto = proto_pat.search(text)
            if not m_proto or m_proto.group(1) != protocol:
                continue
        m_id = id_pat.search(text)
        if m_id:
            ids.append(m_id.group(1))
    return ids


def raw_consumer_key(consumer: str = "example", index: int = 1) -> str:
    """Read raw client key from the ``# raw key #N: ...`` comment in consumers/<name>.yaml."""
    path = REPO_ROOT / "consumers" / f"{consumer}.yaml"
    match = re.search(rf"raw key #{index}:\s*(\S+)", path.read_text())
    if not match:
        raise RuntimeError(
            f"{path}: missing '# raw key #{index}: ...' comment alongside the sha256 hash"
        )
    return match.group(1)
