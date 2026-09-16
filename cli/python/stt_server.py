"""Pinned Qwen ASR adapter: one model, bounded admission, JSON or real token SSE."""
import asyncio
import json
import os
import threading
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager

import numpy as np
import miniaudio
import mlx.core as mx
import uvicorn
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse, StreamingResponse
from mlx_audio.stt.utils import load

from http_limits import BodyLimit
from resources import RequestCapacity, Busy, MemoryPressure, admission, configure_mlx, start_monitor

NAME = os.environ["LLMGATE_SERVED_MODEL"]
EXECUTOR = ThreadPoolExecutor(max_workers=1, thread_name_prefix="mlx-stt")
CAPACITY = RequestCapacity()
MODEL = None


def initialize():
    global MODEL
    with admission(wait=True):
        configure_mlx()
        MODEL = load(os.environ["LLMGATE_MODEL_PATH"])
        MODEL.generate(np.zeros(16000, dtype=np.float32), max_tokens=2, verbose=False)
        mx.clear_cache()


@asynccontextmanager
async def lifespan(_app):
    monitor = start_monitor()
    try:
        await asyncio.get_running_loop().run_in_executor(EXECUTOR, initialize)
        yield
    finally:
        monitor.set()
        EXECUTOR.shutdown(wait=True, cancel_futures=True)


app = FastAPI(lifespan=lifespan)
app.add_middleware(BodyLimit, limit=6 * 1024 * 1024)


@app.get("/health")
async def health():
    return {"ready": MODEL is not None}


@app.get("/v1/models")
async def models():
    return {"object": "list", "data": [{"id": NAME, "object": "model"}]}


def decode_audio(raw):
    parts, length = [], 0
    try:
        chunks = miniaudio.stream_memory(raw, output_format=miniaudio.SampleFormat.FLOAT32,
                                        nchannels=1, sample_rate=16000, frames_to_read=16000)
        try:
            for chunk in chunks:
                length += len(chunk)
                if length > 16000 * 60:
                    raise HTTPException(413, "audio exceeds 60 seconds")
                parts.append(np.asarray(chunk, dtype=np.float32))
        finally:
            chunks.close()
    except HTTPException:
        raise
    except Exception:
        raise HTTPException(400, "unsupported or invalid audio") from None
    if not length:
        raise HTTPException(400, "empty audio")
    return np.concatenate(parts)


def frame(event):
    return "data: " + json.dumps(event, ensure_ascii=False) + "\n\n"


@app.post("/v1/audio/transcriptions")
async def transcribe(
    file: UploadFile = File(...), model: str = Form(...), stream: bool = Form(False),
    response_format: str = Form("json"), language: str | None = Form(None),
    prompt: str | None = Form(None), temperature: float = Form(0.0),
):
    if model != NAME:
        raise HTTPException(400, "unsupported model")
    if response_format != "json":
        raise HTTPException(400, "this profile supports response_format=json only")
    if temperature != 0.0:
        raise HTTPException(400, "this profile supports temperature=0 only")
    try:
        await CAPACITY.acquire()
    except Busy as exc:
        raise HTTPException(429, str(exc), headers={"Retry-After": "1"}) from None
    lease = admission()
    try:
        lease.__enter__()
    except (Busy, MemoryPressure) as exc:
        CAPACITY.release()
        raise HTTPException(429 if isinstance(exc, Busy) else 503, str(exc)) from None
    try:
        raw = await file.read(5 * 1024 * 1024 + 1)
        if len(raw) > 5 * 1024 * 1024:
            raise HTTPException(413, "audio upload too large")
        if prompt and (len(prompt) > 8192 or len(MODEL._tokenizer.encode(prompt)) > 256):
            raise HTTPException(400, "prompt exceeds 256 tokens")
        if language and len(language) > 64:
            raise HTTPException(400, "invalid language")
        decoding = asyncio.get_running_loop().run_in_executor(EXECUTOR, decode_audio, raw)
        try:
            audio = await asyncio.shield(decoding)
        except asyncio.CancelledError:
            await decoding
            raise
    except BaseException:
        lease.__exit__(None, None, None)
        CAPACITY.release()
        raise
    finally:
        await file.close()
    loop = asyncio.get_running_loop()
    queue = asyncio.Queue(maxsize=32)
    cancelled = threading.Event()

    def emit(value):
        if cancelled.is_set():
            return False
        future = asyncio.run_coroutine_threadsafe(queue.put(value), loop)
        while not cancelled.is_set():
            try:
                future.result(timeout=0.2)
                return True
            except TimeoutError:
                pass
        future.cancel()
        return False

    def infer():
        try:
            kwargs = dict(max_tokens=1024, language=language, verbose=False, chunk_duration=30.0)
            if prompt:
                kwargs["system_prompt"] = prompt
            if stream:
                text = []
                for chunk in MODEL.stream_transcribe(audio, **kwargs):
                    if cancelled.is_set():
                        break
                    if chunk.text:
                        text.append(chunk.text)
                        if not emit({"type": "transcript.text.delta", "delta": chunk.text}):
                            break
                if chunk.generation_tokens >= 1024:
                    raise ValueError("transcription output limit reached")
                emit({"type": "transcript.text.done", "text": "".join(text)})
            else:
                result = MODEL.generate(audio, **kwargs)
                if result.generation_tokens >= 1024:
                    raise ValueError("transcription output limit reached")
                emit({"text": result.text})
        except Exception:
            # Audio or prompt contents must not escape through traceback logs.
            emit({"error": {"message": "local transcription failed", "type": "upstream_error"}})
        finally:
            mx.clear_cache()
            lease.__exit__(None, None, None)
            emit(None)
            loop.call_soon_threadsafe(CAPACITY.release)

    EXECUTOR.submit(infer)

    async def events():
        try:
            while True:
                event = await queue.get()
                if event is None:
                    yield "data: [DONE]\n\n"
                    break
                yield frame(event)
        finally:
            cancelled.set()

    if stream:
        return StreamingResponse(events(), media_type="text/event-stream", headers={"Cache-Control": "no-store"})
    try:
        result = await queue.get()
        return JSONResponse(result, status_code=502 if "error" in result else 200)
    finally:
        cancelled.set()


if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ["LLMGATE_MODEL_PORT"]), access_log=False)
