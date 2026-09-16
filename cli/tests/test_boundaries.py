import asyncio
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

PYTHON = Path(__file__).resolve().parents[1]/'python'
sys.path.insert(0, str(PYTHON))
from http_limits import BodyLimit


class BodyTests(unittest.IsolatedAsyncioTestCase):
    async def test_chunked_oversize_never_reaches_parser(self):
        called, output = [], []
        async def app(*_): called.append(True)
        messages = iter([{'type':'http.request','body':b'abcd','more_body':True},
                         {'type':'http.request','body':b'efgh','more_body':False}])
        async def receive(): return next(messages)
        async def send(value): output.append(value)
        await BodyLimit(app, 7)({'type':'http'}, receive, send)
        self.assertFalse(called)
        self.assertEqual(output[0]['status'], 413)

    async def test_boundary_preserves_body(self):
        output = []
        async def app(scope, receive, send): output.append(await receive())
        async def receive(): return {'type':'http.request','body':b'abcd','more_body':False}
        await BodyLimit(app, 4)({'type':'http'}, receive, None)
        self.assertEqual(output[0]['body'], b'abcd')


@unittest.skipUnless(sys.platform == 'darwin', 'Darwin physical footprint')
class ResourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        for directory in ('control','locks'): (self.root/directory).mkdir()
        self.environment = patch.dict(os.environ, {'LLMGATE_MANAGED_ROOT':str(self.root),'LLMGATE_PROFILE':'embedding'})
        self.environment.start()
        spec = importlib.util.spec_from_file_location('test_resources', PYTHON/'resources.py')
        self.resources = importlib.util.module_from_spec(spec); spec.loader.exec_module(self.resources)

    def tearDown(self):
        self.environment.stop(); self.temp.cleanup()

    def test_other_process_cannot_infer_until_release(self):
        code = 'from resources import admission\nwith admission(): pass'
        env = {**os.environ, 'PYTHONPATH':str(PYTHON),'LLMGATE_PROFILE':'embedding'}
        with self.resources.admission():
            p = subprocess.run([sys.executable,'-c',code],env=env,capture_output=True)
            self.assertNotEqual(p.returncode,0)
            self.assertIn(b'this model profile',p.stderr)
        self.assertEqual(subprocess.run([sys.executable,'-c',code],env=env,capture_output=True).returncode,0)

    def test_other_profile_can_infer_concurrently(self):
        code = 'from resources import admission\nwith admission(): pass'
        env = {**os.environ, 'PYTHONPATH':str(PYTHON), 'LLMGATE_PROFILE':'stt'}
        with self.resources.admission():
            result = subprocess.run([sys.executable, '-c', code], env=env, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_average_target_does_not_reject_a_temporary_peak(self):
        with patch.object(self.resources, 'total_memory', return_value=self.resources.MEMORY_TARGET + 1):
            with self.resources.admission():
                pass

    def test_bounded_queue_cancellation_timeout_and_recovery(self):
        async def check():
            capacity = self.resources.RequestCapacity(limit=2, timeout=.03)
            await capacity.acquire()
            waiting = asyncio.create_task(capacity.acquire())
            await asyncio.sleep(0)
            with self.assertRaises(self.resources.Busy):
                await capacity.acquire()
            waiting.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await waiting
            self.assertEqual(capacity.pending, 1)
            with self.assertRaises(self.resources.Busy):
                await capacity.acquire()
            self.assertEqual(capacity.pending, 1)
            capacity.release()
            await capacity.acquire()
            capacity.release()
            self.assertEqual(capacity.pending, 0)
        asyncio.run(check())

    def test_monitor_separates_average_target_from_emergency_stop(self):
        class OneSample:
            stopped = False
            def is_set(self): return self.stopped
            def wait(self, _): self.stopped = True
        for amount, should_stop in ((self.resources.MEMORY_TARGET + 1, False),
                                    (self.resources.MEMORY_STOP + 1, True)):
            with self.subTest(should_stop=should_stop), \
                 patch.object(self.resources, 'total_memory', return_value=amount), \
                 patch.object(self.resources.threading, 'Event', side_effect=OneSample), \
                 patch.object(self.resources.threading, 'Thread', side_effect=lambda **kw: SimpleNamespace(start=kw['target'])), \
                 patch.object(self.resources.os, '_exit', side_effect=SystemExit) as exited, \
                 patch.object(self.resources.os, 'write'):
                if should_stop:
                    with self.assertRaises(SystemExit): self.resources.start_monitor()
                    exited.assert_called_once_with(75)
                else:
                    self.resources.start_monitor()
                    exited.assert_not_called()
                receipt = json.loads((self.root/'control/embedding-memory.json').read_text())
                self.assertEqual(receipt['average_bytes'], amount)
                self.assertEqual(receipt['observed_seconds'], 0)

    def test_reused_pid_receipt_is_ignored(self):
        parent = self.resources.usage(os.getppid())
        receipt = {'processes':[dict(parent, started=parent['started']+1)]}
        (self.root/'control/stt-memory.json').write_text(json.dumps(receipt))
        with patch.object(self.resources, 'usage', side_effect=lambda pid: {'pid':pid,'started':parent['started'],'bytes':100}):
            self.assertEqual(self.resources.total_memory(), 200)

    def test_pressure_rejects_before_inference(self):
        with patch.object(self.resources, 'total_memory', return_value=self.resources.ADMISSION_LIMIT):
            with self.assertRaises(self.resources.MemoryPressure):
                with self.resources.admission(): self.fail('admitted under memory pressure')
        self.assertGreater(self.resources.usage(os.getpid())['bytes'], 0)


if __name__ == '__main__': unittest.main()
