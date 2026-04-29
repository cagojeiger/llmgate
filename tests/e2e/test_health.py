"""Liveness probe."""

from __future__ import annotations

import httpx


def test_healthz_returns_ok(gate_base_url: str) -> None:
    r = httpx.get(f"{gate_base_url}/healthz", timeout=2.0)
    assert r.status_code == 200
    assert r.json() == {"status": "ok"}
    assert r.headers["content-type"].startswith("application/json")
