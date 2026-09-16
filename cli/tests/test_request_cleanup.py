"""Exercise adapter ownership with fake model libraries; no downloads or GPU required."""
import asyncio
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from types import SimpleNamespace
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1] / 'python'

class HTTPException(Exception):
    def __init__(self, status_code, detail, **_):
        self.status_code, self.detail = status_code, detail

class App:
    def __init__(self, **_): pass
    def get(self, *_): return lambda f: f
    post = get
    def add_middleware(self, *_args, **_kwargs): pass

class Response:
    def __init__(self, content, status_code=200, **_):
        self.content, self.status_code = content, status_code

class Upload:
    async def read(self, _): return b'audio'
    async def close(self): pass

@unittest.skipUnless(sys.platform == 'darwin', 'Darwin resources')
class CleanupTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory()
        for name in ['control', 'locks']: (Path(self.temp.name)/name).mkdir()
        self.env = patch.dict(os.environ, LLMGATE_MANAGED_ROOT=self.temp.name,
                              LLMGATE_PROFILE='stt', LLMGATE_SERVED_MODEL='fixture')
        self.env.start()
        def load(name, filename):
            spec = importlib.util.spec_from_file_location(name, SOURCE/filename)
            module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
            return module
        self.resources = load('cleanup_resources', 'resources.py')
        core = SimpleNamespace(clear_cache=lambda: None)
        dependencies = {'resources': self.resources, 'numpy': SimpleNamespace(),
            'miniaudio': SimpleNamespace(), 'uvicorn': SimpleNamespace(),
            'mlx': SimpleNamespace(core=core), 'mlx.core': core,
            'mlx_audio': SimpleNamespace(), 'mlx_audio.stt': SimpleNamespace(),
            'mlx_audio.stt.utils': SimpleNamespace(load=lambda _: None),
            'fastapi': SimpleNamespace(FastAPI=App, HTTPException=HTTPException,
                File=lambda *a: None, Form=lambda *a: None, UploadFile=Upload),
            'fastapi.responses': SimpleNamespace(JSONResponse=Response, StreamingResponse=Response),
            'http_limits': SimpleNamespace(BodyLimit=object)}
        with patch.dict(sys.modules, dependencies):
            self.module = load('cleanup_stt', 'stt_server.py')
        self.module.decode_audio = lambda _: 'decoded'
        self.module.MODEL = SimpleNamespace(generate=lambda *a, **kw: SimpleNamespace(text='ok', generation_tokens=2))

    async def asyncTearDown(self):
        self.module.EXECUTOR.shutdown(wait=True)
        self.env.stop(); self.temp.cleanup()

    async def request(self, upload=None, stream=False):
        return await self.module.transcribe(file=upload or Upload(), model='fixture', stream=stream,
            response_format='json', language=None, prompt=None, temperature=0.0)

    async def idle(self):
        for _ in range(100):
            if self.module.CAPACITY.pending == 0: return
            await asyncio.sleep(.01)
        self.fail('request capacity leaked')

    async def test_cache_cleanup_failure_releases_lock_and_slot(self):
        with patch.object(self.module.mx, 'clear_cache', side_effect=RuntimeError('secret')):
            result = await asyncio.wait_for(self.request(), 2)
            self.assertEqual(result.status_code, 502)
            self.assertNotIn('secret', str(result.content))
            await self.idle()
        self.assertEqual((await self.request()).status_code, 200)
        await self.idle()

    async def test_admission_io_failure_releases_slot(self):
        @contextmanager
        def failed():
            raise OSError('disk fault')
            yield
        with patch.object(self.module, 'admission', failed):
            self.assertEqual((await self.request()).status_code, 502)
            await self.idle()
        self.assertEqual((await self.request()).status_code, 200)
        await self.idle()

    async def test_file_close_failure_releases_slot(self):
        class Broken(Upload):
            async def close(self): raise OSError('close failure')
        with self.assertRaises(OSError): await self.request(Broken())
        await self.idle()

    async def test_invalid_stream_audio_is_http_error_before_stream(self):
        with patch.object(self.module, 'decode_audio', side_effect=HTTPException(413, 'too long')):
            self.assertEqual((await self.request(stream=True)).status_code, 413)
            await self.idle()

    async def test_executor_rejection_releases_slot(self):
        with patch.object(self.module.EXECUTOR, 'submit', side_effect=RuntimeError('closed')):
            with self.assertRaises(RuntimeError): await self.request()
        await self.idle()

    async def test_cancellation_keeps_slot_until_work_finishes(self):
        started, finish = threading.Event(), threading.Event()
        def work():
            started.set(); finish.wait(3)
        capacity = self.resources.RequestCapacity()
        with ThreadPoolExecutor(max_workers=1) as executor:
            await capacity.acquire()
            future = capacity.submit(executor, work)
            async def caller(): await asyncio.shield(future)
            task = asyncio.create_task(caller())
            while not started.is_set(): await asyncio.sleep(.01)
            task.cancel(); task.cancel()
            with self.assertRaises(asyncio.CancelledError): await task
            self.assertEqual(capacity.pending, 1)
            with patch.object(self.resources.time, 'monotonic', return_value=capacity.started + 121):
                self.assertFalse(capacity.healthy())
            finish.set()
            await future
            await asyncio.sleep(0)
            self.assertEqual(capacity.pending, 0)
            self.assertTrue(capacity.healthy())

if __name__ == '__main__': unittest.main()
