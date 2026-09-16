"""Exercise the real OpenClaw CLI in a disposable state directory."""
import json
import os
import subprocess


def verify(base, key, root, fixture, node, executable):
    state = root/'.local/openclaw-state'
    workspace = state/'workspace'
    (workspace/'memory').mkdir(parents=True, exist_ok=True)
    state.chmod(0o700)
    (workspace/'MEMORY.md').write_text('# Test memory\nThe project mascot is a blue otter named Miso.\n')
    config = {
        'agents': {'defaults': {'workspace': str(workspace)}},
        'memory': {'search': {'enabled': True, 'provider': 'openai-compatible', 'model': 'embedding',
                              'fallback': 'none', 'remote': {'baseUrl': base+'/v1', 'apiKey': '${LLMGATE_TEST_API_KEY}'}}},
        'models': {'providers': {'openai': {'baseUrl': base+'/v1', 'api': 'openai-completions', 'request': {'allowPrivateNetwork': True},
                                           'apiKey': '${LLMGATE_TEST_API_KEY}', 'models': [{'id': 'stt', 'name': 'File STT'}]}}},
        'tools': {'media': {'audio': {'enabled': True}, 'models': [{'provider': 'openai', 'model': 'stt',
                            'baseUrl': base+'/v1', 'capabilities': ['audio'], 'timeoutSeconds': 180}]}},
    }
    path = state/'openclaw.json'; path.write_text(json.dumps(config)); path.chmod(0o600)
    env = {**os.environ, 'OPENCLAW_STATE_DIR': str(state), 'OPENCLAW_CONFIG_PATH': str(path),
           'LLMGATE_TEST_API_KEY': key, 'OPENCLAW_SKIP_CHANNELS': '1'}
    commands = [
        ['--version'], ['config','validate'],
        ['memory','index','--force'], ['memory','search','--query','What is the project mascot?','--json'],
        ['infer','audio','transcribe','--file', str(fixture/'speech.wav'), '--model','openai/stt','--json'],
    ]
    results = []
    for args in commands:
        process = subprocess.run([node, str(executable), *args], env=env, capture_output=True, text=True, timeout=240)
        output = process.stdout + process.stderr
        if key in output:
            raise RuntimeError('credential unexpectedly present in command output')
        (fixture/('openclaw-'+args[0].replace('--','')+'-'+str(len(results))+'.log')).write_text(output)
        if process.returncode:
            raise RuntimeError(f'OpenClaw {args} failed: {output[:1500]}')
        if args[:2] == ['memory','search']:
            assert 'Miso' in output, output[:1500]
        if args[:3] == ['infer','audio','transcribe']:
            assert 'hello' in output.lower(), output[:1500]
        results.append({'command': args, 'passed': True})
        print(json.dumps(results[-1]), flush=True)
    (fixture/'openclaw-results.json').write_text(json.dumps(results, indent=2))
