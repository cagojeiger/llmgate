"""Public command behavior; local fixtures, no model downloads or Keychain writes."""
from contextlib import contextmanager
import fcntl
import http.server
import json
from pathlib import Path
import socket
import select
import signal
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import unittest

BINARY=Path(__file__).resolve().parents[1]/'target/aarch64-apple-darwin/debug/llmgate-cli'

@unittest.skipUnless(sys.platform=='darwin' and BINARY.is_file(),'built macOS CLI required')
class UXTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(prefix='lgux-',dir='/tmp')
        self.home=Path(self.temp.name).resolve();(self.home/'control').mkdir()
        (self.home/'registration.json').write_text(json.dumps({'url':'https://unused.invalid','profiles':['embedding'],'ca_file':None,'allow_loopback_http':False}))
    def tearDown(self):self.temp.cleanup()
    def command(self,*args,input=None):
        return subprocess.run([str(BINARY),'--home',str(self.home),*args],input=input,text=True,capture_output=True,timeout=12)
    @contextmanager
    def supervisor(self,phase):
        path=self.home/'control/embedding.sock'
        server=socket.socket(socket.AF_UNIX);server.bind(str(path));server.listen();server.settimeout(.1)
        # Match the real supervisor's ownership even if a status client times out.
        (self.home/'locks').mkdir(exist_ok=True)
        lock=(self.home/'locks/embedding.lock').open('a+')
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        stop=threading.Event();began=time.monotonic()
        def serve():
            while not stop.is_set():
                try:conn,_=server.accept()
                except socket.timeout:continue
                with conn:
                    while conn.recv(64):pass
                    runtime,publish=phase(time.monotonic()-began)
                    state={'profile':'embedding','runtime':runtime,'publish':publish,'port':12345,'model':'fixture','revision':'fixture','restarts':0,'dropped_log_chunks':0,'error':None,'lease_protected':True}
                    try:conn.sendall(json.dumps(state).encode())
                    except (BrokenPipeError,ConnectionResetError):pass
        thread=threading.Thread(target=serve);thread.start()
        try:yield
        finally:stop.set();thread.join(timeout=2);server.close();path.unlink(missing_ok=True);lock.close()
    def test_start_all_waits_for_both_model_and_publication_and_uses_registered_profiles(self):
        def phase(elapsed):
            if elapsed<1:return 'starting','active'
            if elapsed<2:return 'ready','connecting'
            return 'ready','active'
        with self.supervisor(phase):
            began=time.monotonic();result=self.command('start','--all','--wait-timeout','5')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertGreaterEqual(time.monotonic()-began,1.9)
        self.assertIn('서빙 준비 완료',result.stdout)
        self.assertIn('연결 대기',result.stderr)
        self.assertFalse((self.home/'runtimes/stt').exists())
    def test_disconnected_status_client_does_not_kill_supervisor(self):
        with self.supervisor(lambda _:('ready','active')):
            with socket.socket(socket.AF_UNIX) as client:
                client.connect(str(self.home/'control/embedding.sock'))
                client.sendall(b'status');client.shutdown(socket.SHUT_RDWR)
            result=self.command('status','--json')
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(json.loads(result.stdout.splitlines()[0])['runtime'],'ready')

    def test_timeout_is_failure_without_stopping_worker(self):
        with self.supervisor(lambda _:('ready','connecting')):
            result=self.command('start','--all','--wait-timeout','1')
            self.assertNotEqual(result.returncode,0)
            self.assertNotIn('서빙 준비 완료',result.stdout)
            self.assertIn('백그라운드 워커는 계속',result.stderr)
            self.assertIn('logs embedding',result.stderr)
            state=self.command('status','--json')
            self.assertEqual(json.loads(state.stdout.splitlines()[0])['publish'],'connecting')
    def test_interrupt_wait_leaves_background_worker_available(self):
        with self.supervisor(lambda _:('ready','connecting')):
            process=subprocess.Popen([str(BINARY),'--home',str(self.home),'start','--all'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            try:
                self.assertTrue(select.select([process.stderr],[],[],5)[0])
                first=process.stderr.readline()
                process.send_signal(signal.SIGINT)
                out,err=process.communicate(timeout=5)
                self.assertNotEqual(process.returncode,0)
                self.assertNotIn('서빙 준비 완료',out)
                self.assertIn('백그라운드 워커는 계속',first+err)
                self.assertEqual(self.command('status','--json').returncode,0)
            finally:
                if process.poll() is None:process.kill();process.wait()
                process.stdout.close();process.stderr.close()

    def test_local_only_does_not_claim_relay_readiness(self):
        with self.supervisor(lambda _:('ready','disabled')):
            normal=self.command('start','embedding','--wait-timeout','1')
            local=self.command('start','embedding','--local-only','--wait-timeout','1')
        self.assertNotEqual(normal.returncode,0)
        self.assertIn('로컬 전용으로 실행 중',normal.stderr)
        self.assertEqual(local.returncode,0,local.stderr)
        self.assertIn('Relay 공개 안 함',local.stdout)
    def test_status_human_and_json_keep_starting_distinct(self):
        with self.supervisor(lambda _:('starting','active')):
            human=self.command('status');machine=self.command('status','--json')
        self.assertIn('모델 준비 중',human.stdout)
        self.assertNotIn('서빙 가능',human.stdout)
        self.assertEqual(len(machine.stdout.splitlines()),2)
        self.assertEqual(json.loads(machine.stdout.splitlines()[0])['runtime'],'starting')
    def test_stale_receipt_is_not_shown_as_serving(self):
        receipt={'profile':'embedding','runtime':'ready','publish':'active','port':1,'model':'fixture','revision':'fixture','restarts':0,'dropped_log_chunks':0,'error':None,'lease_protected':True}
        (self.home/'control/embedding.json').write_text(json.dumps(receipt))
        result=self.command('status')
        self.assertNotIn('서빙 가능',result.stdout)
        self.assertIn('정지',result.stdout)
    def test_logs_include_supervisor_failures_and_explain_empty_logs(self):
        empty=self.command('logs','embedding');self.assertIn('로그가 아직 없습니다',empty.stdout)
        logs=self.home/'logs'
        (logs/'embedding-supervisor.log').write_text('fixture: publish permission denied\n')
        (logs/'embedding.log').write_text('fixture: model loaded\n')
        result=self.command('logs','embedding')
        self.assertIn('publish permission denied',result.stdout);self.assertIn('model loaded',result.stdout)
    def test_missing_registration_keeps_custom_home_in_next_command(self):
        (self.home/'registration.json').unlink()
        result=self.command('start','--all')
        self.assertNotEqual(result.returncode,0)
        self.assertIn(str(self.home),result.stderr)
        self.assertNotIn('os error',result.stderr)
        self.assertFalse((self.home/'tools/uv-0.12.15').exists())

    def test_registration_denials_give_actions_without_saving_key(self):
        (self.home/'registration.json').unlink()
        class Server(http.server.ThreadingHTTPServer):
            def server_bind(self):
                socketserver.TCPServer.server_bind(self)
                self.server_name='localhost';self.server_port=self.server_address[1]
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self,*args):pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get('Content-Length','0')))
                self.send_response(self.server.code);self.send_header('Content-Length','0');self.end_headers()
        server=Server(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
        try:
            for code,hint in [(401,'API 키'),(403,'allowed_worker_profiles'),(404,'worker 등록 API')]:
                server.code=code
                result=self.command('register','--url',f'http://127.0.0.1:{server.server_port}','--allow-loopback-http','--profiles','embedding','--key-stdin',input='fixture-not-a-real-key\n')
                self.assertNotEqual(result.returncode,0);self.assertIn(hint,result.stderr)
                self.assertNotIn('fixture-not-a-real-key',result.stderr)
                self.assertFalse((self.home/'registration.json').exists())
        finally:server.shutdown();thread.join();server.server_close()
