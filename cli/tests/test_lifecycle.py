"""Real CLI processes, fake HTTP engine: no MLX download or Keychain access."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT/'target/aarch64-apple-darwin/debug/llmgate-cli'


@unittest.skipUnless(sys.platform == 'darwin' and BINARY.is_file(), 'built macOS CLI required')
class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='lgt-', dir='/tmp')
        self.home = Path(self.temp.name).resolve()
        self.rt = self.home/'runtimes/embedding'
        self.rt.mkdir(parents=True)
        (self.rt/'runtime_guard.py').write_bytes((ROOT/'python/runtime_guard.py').read_bytes())
        (self.rt/'installed.json').write_text(json.dumps({'python':sys.executable,'model':str(self.home),'fingerprint':'fixture'}))
        (self.rt/'embedding_server.py').write_text('''import http.server,json,os,pathlib
root=pathlib.Path(os.environ['LLMGATE_MANAGED_ROOT'])
(root/'engine.json').write_text(json.dumps({'pid':os.getpid(),'port':int(os.environ['LLMGATE_MODEL_PORT'])}))
class H(http.server.BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(b'{"ready":true}')
 def do_POST(self):
  (root/'unexpected-warmup').touch();self.send_response(429);self.end_headers()
http.server.ThreadingHTTPServer(('127.0.0.1',int(os.environ['LLMGATE_MODEL_PORT'])),H).serve_forever()
''')
        self.processes = []
        self.engines = []

    def tearDown(self):
        for p in self.processes:
            if p.poll() is None:
                p.terminate()
                try:p.wait(timeout=15)
                except subprocess.TimeoutExpired:p.kill();p.wait()
            p.stderr.close()
        for pid in self.engines:
            command=subprocess.run(['/bin/ps','-o','command=','-p',str(pid)],capture_output=True,text=True).stdout
            if str(self.home) not in command:continue
            try:os.kill(pid,signal.SIGCONT)
            except ProcessLookupError:continue
            # Captured child of this test; never discover or signal user processes.
            try:os.kill(pid,signal.SIGKILL)
            except ProcessLookupError:pass
        self.temp.cleanup()

    def command(self,*args):
        return subprocess.run([str(BINARY),'--home',str(self.home),*args],capture_output=True,text=True,timeout=35)

    def launch(self):
        p=subprocess.Popen([str(BINARY),'--home',str(self.home),'run','embedding','--local-only'],stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
        self.processes.append(p)
        deadline=time.monotonic()+10
        while time.monotonic()<deadline:
            if p.poll() is not None:self.fail(p.stderr.read().decode())
            output=self.command('status')
            if output.returncode==0:
                state=json.loads(output.stdout.splitlines()[0])
                if state.get('runtime')=='ready':
                    engine=json.loads((self.home/'engine.json').read_text())
                    self.engines.append(engine['pid'])
                    return p,engine
            time.sleep(.1)
        log=self.home/'logs/embedding.log'
        self.fail('engine did not become ready: '+(log.read_text()[-2000:] if log.exists() else 'no log'))

    @staticmethod
    def alive(port):
        try:
            with socket.create_connection(('127.0.0.1',port),.1):return True
        except OSError:return False

    def test_crash_retains_leases_until_engine_exits_then_recovers(self):
        # Simulate a stuck engine that cannot handle the guardian's EOF notification.
        guard=self.rt/'runtime_guard.py'
        guard.write_text(guard.read_text().replace('threading.Thread(target=watch_supervisor, daemon=True).start()', '# stuck guardian fixture'))
        supervisor,engine=self.launch()
        supervisor.kill();supervisor.wait(timeout=5)
        for args in [('start','embedding','--local-only'),('install','embedding'),('down','embedding'),('cache','clean')]:
            result=self.command(*args)
            self.assertNotEqual(result.returncode,0,(args,result.stdout))
        self.assertTrue(self.alive(engine['port']))
        os.kill(engine['pid'],signal.SIGKILL)
        guard.write_bytes((ROOT/'python/runtime_guard.py').read_bytes())
        deadline=time.monotonic()+5
        while self.alive(engine['port']) and time.monotonic()<deadline:time.sleep(.05)
        self.assertFalse(self.alive(engine['port']))
        next_supervisor,next_engine=self.launch()
        result=self.command('down','embedding')
        self.assertEqual(result.returncode,0,result.stderr)
        next_supervisor.wait(timeout=5)
        self.assertFalse(self.rt.exists())
        self.assertFalse(self.alive(next_engine['port']))
        self.assertFalse(self.alive(engine['port']))

    def test_supervisor_crash_stops_guarded_engine(self):
        supervisor,engine=self.launch()
        supervisor.kill();supervisor.wait(timeout=5)
        deadline=time.monotonic()+5
        while self.alive(engine['port']) and time.monotonic()<deadline:time.sleep(.05)
        self.assertFalse(self.alive(engine['port']))
        result=self.command('down','embedding')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertFalse(self.rt.exists())

    def test_health_ready_does_not_call_busy_inference(self):
        supervisor,_=self.launch()
        time.sleep(.3)
        self.assertIsNone(supervisor.poll())
        self.assertFalse((self.home/'unexpected-warmup').exists())
        result=self.command('down','embedding')
        self.assertEqual(result.returncode,0,result.stderr)

    def test_legacy_unclean_state_cannot_be_overwritten(self):
        (self.home/'control').mkdir()
        state={'profile':'embedding','runtime':'ready','publish':'inactive','port':0,'model':'old','revision':'old','restarts':0,'dropped_log_chunks':0,'error':None}
        path=self.home/'control/embedding.json';path.write_text(json.dumps(state))
        result=self.command('start','embedding','--local-only')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('legacy unclean',result.stderr)
        self.assertEqual(json.loads(path.read_text()),state)
