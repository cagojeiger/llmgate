"""Bounded mixed-load probe; only sanitized aggregates are persisted."""
import concurrent.futures
import json
import math
import statistics
import threading
import time

import httpx


def run(base, key, home, work, rounds=30, allow_rejections=False):
    states = {p: json.loads((home / f'control/{p}.json').read_text()) for p in ('embedding', 'stt')}
    audio = (work / 'speech.wav').read_bytes()
    results = []
    for route in ('direct', 'relay'):
        for concurrency in (1, 2, 4, 8):
            samples, outcomes = [], []
            stop = threading.Event()
            def sample():
                while not stop.is_set():
                    try:
                        receipts = [json.loads((home / f'control/{p}-memory.json').read_text()) for p in states]
                        samples.append(max(r['total_bytes'] for r in receipts))
                    except (OSError, ValueError):
                        pass
                    stop.wait(.25)
            monitor = threading.Thread(target=sample)
            monitor.start()
            start = time.monotonic()
            def request(index, barrier):
                profile = 'embedding' if index % 2 == 0 else 'stt'
                endpoint = base if route == 'relay' else f"http://127.0.0.1:{states[profile]['port']}"
                model = profile if route == 'relay' else {'embedding':'qwen3-embedding-0.6b','stt':'qwen3-asr-0.6b'}[profile]
                with httpx.Client(base_url=endpoint, headers={'Authorization': 'Bearer ' + key} if route == 'relay' else {}, timeout=180, trust_env=False) as client:
                    barrier.wait()
                    began = time.monotonic()
                    try:
                        if profile == 'embedding':
                            response = client.post('/v1/embeddings', json={'model': model, 'input': ['Local inference concurrency benchmark. ' * 32] * 4})
                        else:
                            response = client.post('/v1/audio/transcriptions', data={'model':model, 'language':'English'}, files={'file':('speech.wav', audio,'audio/wav')})
                        code = str(response.status_code)
                        if response.status_code not in (200, 429, 502, 503):
                            raise RuntimeError(f'unexpected benchmark status: {response.status_code}')
                        if response.status_code == 200:
                            body = response.json()
                            valid = len(body.get('data', [])) == 4 if profile == 'embedding' else 'hello' in body.get('text', '').lower()
                            if not valid:
                                code = 'invalid_output'
                    except httpx.HTTPError as exc:
                        code = type(exc).__name__
                    return profile, code, time.monotonic() - began
            try:
                with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                    for batch in range(rounds):
                        barrier = threading.Barrier(concurrency)
                        futures = [pool.submit(request, batch * concurrency + slot, barrier) for slot in range(concurrency)]
                        outcomes.extend(f.result() for f in futures)
            finally:
                stop.set()
                monitor.join()
            elapsed = time.monotonic() - start
            success = sorted(latency for _, code, latency in outcomes if code == '200')
            counts = {}
            for profile, code, _ in outcomes:
                label = profile + ':' + code
                counts[label] = counts.get(label, 0) + 1
            result = dict(route=route, concurrency=concurrency, requests=len(outcomes), statuses=counts,
                          seconds=round(elapsed, 3), successful_rps=round(len(success)/elapsed, 3),
                          p95_success_seconds=round(success[math.ceil(len(success)*.95)-1],3) if success else None,
                          average_gib=round(statistics.mean(samples)/1024**3,3) if samples else None,
                          peak_gib=round(max(samples)/1024**3,3) if samples else None)
            results.append(result)
            print(json.dumps({'benchmark':result}), flush=True)
            (work / 'benchmark.json').write_text(json.dumps(results, indent=2))
            # Do not let overload from the previous case contaminate circuit-breaker recovery.
            recovery_started = time.monotonic()
            while True:
                recovered = [request(i, threading.Barrier(1))[1] for i in (0, 1)]
                if recovered == ['200', '200']:
                    break
                if time.monotonic() - recovery_started > 120:
                    raise RuntimeError('capacity did not recover within 120 seconds')
                time.sleep(2)
            result['recovery_seconds'] = round(time.monotonic() - recovery_started, 3)
            (work / 'benchmark.json').write_text(json.dumps(results, indent=2))
    if not allow_rejections:
        assert all(all(code.endswith(':200') for code in row['statuses']) for row in results), 'mixed benchmark had rejected or failed requests; see benchmark.json'
    return results
