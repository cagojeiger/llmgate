"""Local mock upstream — replays canned vendor responses from
tests/e2e/fixtures/models/<id>/chat-completion.{json,sse}, looked up by
the request body's ``model`` field. Missing fixture → OpenAI-shaped 400.
ADR 006."""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Optional, Tuple


# >50ms inter-chunk gap is required by assert_streaming_progressive.
SSE_REPLAY_GAP = 0.06


def start(fixtures_dir: Path) -> Tuple[ThreadingHTTPServer, int]:
    """Start the cassette server on an ephemeral port. Caller owns shutdown."""
    handler = type(
        "BoundCassetteHandler",
        (_CassetteHandler,),
        {"fixtures_dir": fixtures_dir},
    )
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    port = server.server_address[1]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, port


class _CassetteHandler(BaseHTTPRequestHandler):
    fixtures_dir: Path  # injected via type()

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0") or 0)
        body = self.rfile.read(length) if length > 0 else b""
        try:
            req = json.loads(body) if body else {}
        except Exception as exc:
            return self._error(400, f"invalid json: {exc}", "invalid_request_error")

        model = req.get("model")
        if not isinstance(model, str) or not model:
            return self._error(400, "missing 'model' field", "invalid_request_error")

        is_stream = bool(req.get("stream"))
        fname = "chat-completion.sse" if is_stream else "chat-completion.json"
        fixture = self.fixtures_dir / "models" / model / fname
        if not fixture.exists():
            return self._error(400, f"unknown model: {model}", "invalid_request_error")

        try:
            data = fixture.read_bytes()
        except Exception as exc:
            return self._error(500, str(exc), "api_error")

        if is_stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
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
        body = json.dumps({"error": {"message": message, "type": kind}}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args) -> None:  # noqa: A002, N802
        return  # silence default per-request stderr line
