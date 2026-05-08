"""Probe surface."""

from __future__ import annotations

import httpx


def test_healthz_returns_ready(gate_base_url: str) -> None:
    # /healthz is the readiness alias (legacy path); body says "ready"
    # while the process is serving and "shutting_down" once SIGTERM hits.
    r = httpx.get(f"{gate_base_url}/healthz", timeout=2.0)
    assert r.status_code == 200
    assert r.json() == {"status": "ready"}
    assert r.headers["content-type"].startswith("application/json")


def test_healthz_live_returns_ok(gate_base_url: str) -> None:
    # /healthz/live is the liveness probe; always 200 unless the process
    # itself is wedged.
    r = httpx.get(f"{gate_base_url}/healthz/live", timeout=2.0)
    assert r.status_code == 200
    assert r.json() == {"status": "ok"}
