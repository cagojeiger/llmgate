"""Qwen embeddings with explicit token rejection and sequential batch inference."""
import asyncio
import base64
import os
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager

import mlx.core as mx
import numpy as np
import uvicorn
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from mlx_embeddings.utils import load

from http_limits import BodyLimit
from resources import RequestCapacity, Busy, MemoryPressure, admission, configure_mlx, start_monitor

NAME = os.environ['LLMGATE_SERVED_MODEL']
EXECUTOR = ThreadPoolExecutor(max_workers=1)
MODEL = TOKENIZER = None
CAPACITY = RequestCapacity()
MAX_TEXT_TOKENS, MAX_BATCH_TOKENS, MAX_DOCUMENTS = 2048, 8192, 128


def initialize():
    global MODEL, TOKENIZER
    with admission(wait=True):
        configure_mlx()
        MODEL, tokenizer = load(os.environ['LLMGATE_MODEL_PATH'])
        TOKENIZER = getattr(tokenizer, '_tokenizer', tokenizer)
        embed({'input': ['ready'], 'model': NAME}, lock=False)


@asynccontextmanager
async def lifespan(_):
    monitor = start_monitor()
    try:
        await asyncio.get_running_loop().run_in_executor(EXECUTOR, initialize)
        yield
    finally:
        monitor.set()
        EXECUTOR.shutdown(wait=True, cancel_futures=True)


app = FastAPI(lifespan=lifespan)
app.add_middleware(BodyLimit, limit=256 * 1024)


@app.get('/health')
async def health():
    return {'ready': MODEL is not None and CAPACITY.healthy()}


@app.get('/v1/models')
async def models():
    return {'object': 'list', 'data': [{'id': NAME, 'object': 'model'}]}


def validate(body):
    if body.get('model') != NAME:
        raise HTTPException(400, 'unsupported model')
    texts = body.get('input')
    if isinstance(texts, str):
        texts = [texts]
    if not isinstance(texts, list) or not 1 <= len(texts) <= MAX_DOCUMENTS:
        raise HTTPException(400, 'input must contain 1..128 texts')
    if any(not isinstance(text, str) or not text.strip() for text in texts):
        raise HTTPException(400, 'input must contain non-empty text strings')
    if body.get('encoding_format', 'float') not in ('float', 'base64'):
        raise HTTPException(400, 'unsupported encoding_format')
    if body.get('dimensions') not in (None, 1024):
        raise HTTPException(400, 'this profile requires 1024 dimensions')
    encoded, count = [], 0
    for text in texts:
        tokens = TOKENIZER(text, return_tensors='np', truncation=False)
        length = tokens['input_ids'].shape[1]
        if length > MAX_TEXT_TOKENS:
            raise HTTPException(400, 'input exceeds 2048 tokens per text; split it before embedding')
        count += length
        if count > MAX_BATCH_TOKENS:
            raise HTTPException(400, 'input exceeds 8192 tokens per request')
        encoded.append(tokens)
    return encoded, count


def infer(body):
    encoded, count = validate(body)
    data = []
    try:
        for index, tokens in enumerate(encoded):
            vector = MODEL(mx.array(tokens['input_ids']), attention_mask=mx.array(tokens['attention_mask'])).text_embeds
            values = np.asarray(vector[0], dtype=np.float32)
            if len(values) != 1024 or not np.isfinite(values).all():
                raise RuntimeError('invalid model vector')
            output = base64.b64encode(values.astype('<f4').tobytes()).decode() if body.get('encoding_format') == 'base64' else values.tolist()
            data.append({'object': 'embedding', 'index': index, 'embedding': output})
            del vector, values
            mx.clear_cache()
        return {'object': 'list', 'model': NAME, 'data': data,
                'usage': {'prompt_tokens': count, 'total_tokens': count}}
    finally:
        mx.clear_cache()


def embed(body, lock=True):
    if not lock:
        return infer(body)
    with admission():
        return infer(body)


@app.post('/v1/embeddings')
async def embeddings(request: Request):
    try:
        await CAPACITY.acquire()
    except Busy as exc:
        return JSONResponse({"error": {"type": "worker_capacity", "message": str(exc)}}, status_code=429, headers={"Retry-After": "1"})
    try:
        try:
            body = await request.json()
        except ValueError:
            raise HTTPException(400, 'invalid JSON') from None
        if not isinstance(body, dict):
            raise HTTPException(400, 'expected JSON object')
    except BaseException:
        CAPACITY.release()
        raise
    # Only the executor completion callback returns the slot, including repeated cancellation.
    task = CAPACITY.submit(EXECUTOR, embed, body)
    try:
        return await asyncio.shield(task)
    except Busy:
        return JSONResponse({"error": {"type": "worker_capacity", "message": "model busy"}}, status_code=429, headers={"Retry-After": "1"})
    except MemoryPressure:
        raise HTTPException(503, 'local memory emergency guard reached') from None



if __name__ == '__main__':
    uvicorn.run(app, host='127.0.0.1', port=int(os.environ['LLMGATE_MODEL_PORT']), access_log=False)
