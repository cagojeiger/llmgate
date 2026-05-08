"""Local upstream that replays canned vendor responses from fixtures/.

Boots a small HTTP server emulating the two upstream wire surfaces the
gateway calls:

    POST /chat/completions   ← OpenAI-protocol (deepseek, kimi, glm, mimo, qwen, ...)
    POST /messages           ← Anthropic-protocol (minimax)

Per-model fixture lookup:

    tests/e2e/fixtures/models/<model-id>/chat-completion.json

is returned as-is for the matching model. The fixture content is the
*upstream*'s response (OpenAI-shaped for /chat/completions, Anthropic-shaped
for /messages) so the gateway exercises its real translation/decoding path.

Missing fixtures fall through to a 400 with an OpenAI-style envelope —
this lets the unknown-model regression test pass without any per-test
config.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Optional, Tuple


# Minimum gap inserted between SSE events when replaying a stream
# fixture. The progressive-flush assertion in
# tests/e2e/conftest.py::assert_streaming_progressive needs at least one
# inter-chunk gap above 50ms to prove the gateway isn't silently
# batching; a single 60ms pause per event clears that bar without
# visibly slowing tests (~1s per stream).
SSE_REPLAY_GAP = 0.06


def start(fixtures_dir: Path) -> Tuple[ThreadingHTTPServer, int]:
    """Start the cassette server on an ephemeral port.

    Returns (server, port). Caller is responsible for `server.shutdown()`
    + `server.server_close()` at teardown.
    """
    handler = type(
        "BoundCassetteHandler",
        (_CassetteHandler,),
        {"fixtures_dir": fixtures_dir},
    )
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, port


class _CassetteHandler(BaseHTTPRequestHandler):
    fixtures_dir: Path  # set on subclass via type()

    def do_POST(self) -> None:  # noqa: N802 — http.server callback shape
        length = int(self.headers.get("Content-Length", "0") or 0)
        body = self.rfile.read(length) if length > 0 else b""
        try:
            req = json.loads(body) if body else {}
        except Exception as exc:
            return self._error(400, f"invalid json: {exc}", "invalid_request_error")

        model = req.get("model")
        if not isinstance(model, str) or not model:
            return self._error(400, "missing or non-string 'model' field", "invalid_request_error")

        # Anthropic's /v1/messages always streams when stream:true; OpenAI
        # uses the same flag on /v1/chat/completions. Either way the
        # request body's stream:true is the signal for the SSE branch.
        is_stream = bool(req.get("stream"))
        fname = "chat-completion.sse" if is_stream else "chat-completion.json"
        fixture = self.fixtures_dir / "models" / model / fname
        if not fixture.exists():
            # Mirror the OpenAI-style "model not found" envelope. The gateway
            # classifies this as KindBadRequest, which is the right answer
            # for an unknown-model lookup miss.
            return self._error(
                400,
                f"unknown model: {model} (no cassette fixture for {fname})",
                "invalid_request_error",
            )

        try:
            data = fixture.read_bytes()
        except Exception as exc:
            return self._error(500, f"fixture read failed: {exc}", "api_error")

        if is_stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            # Pace events out one at a time so the progressive-flush
            # assertion sees real gaps. Treat a blank line as the SSE
            # event boundary; flush + sleep there.
            buf = bytearray()
            for line in data.splitlines(keepends=True):
                buf.extend(line)
                if line in (b"\n", b"\r\n"):
                    self.wfile.write(bytes(buf))
                    self.wfile.flush()
                    buf.clear()
                    time.sleep(SSE_REPLAY_GAP)
            if buf:
                self.wfile.write(bytes(buf))
                self.wfile.flush()
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _error(self, status: int, message: str, kind: str) -> None:
        body = json.dumps(
            {"error": {"message": message, "type": kind}}
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args) -> None:  # noqa: A002, N802
        # Suppress default per-request stderr line — pytest output stays clean.
        return
