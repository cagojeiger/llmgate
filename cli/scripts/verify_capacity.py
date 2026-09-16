"""Real socket cancellation and sustained mixed work on an existing E2E fixture."""
import concurrent.futures
import io
import json
import math
import socket
import threading
from contextlib import contextmanager
import subprocess
import time
import wave

import httpx


@contextmanager
def memory_samples(home, readings):
    stopped = threading.Event()
    def sample():
        while not stopped.is_set():
            try:
                total = max(json.loads((home/f'control/{p}-memory.json').read_text())['total_bytes'] for p in ('embedding','stt'))
                readings.append((time.monotonic(), total))
            except (OSError, ValueError):
                pass
            stopped.wait(.25)
    thread = threading.Thread(target=sample)
    thread.start()
    try:
        yield
    finally:
        stopped.set()
        thread.join()


def run(base, key, home, work, seconds, concurrency=2):
    audio = (work / 'speech.wav').read_bytes()
    with wave.open(io.BytesIO(audio), 'rb') as source:
        params = source.getparams()
        pcm = source.readframes(source.getnframes())
    output = io.BytesIO()
    with wave.open(output, 'wb') as target:
        target.setparams(params)
        length = params.framerate * params.sampwidth * params.nchannels * 60
        target.writeframes((pcm * (length // len(pcm) + 1))[:length])
    long_audio = output.getvalue()
    states = {p: json.loads((home / f'control/{p}.json').read_text()) for p in ('embedding','stt')}
    results = []
    for profile, stream in (('embedding', False), ('stt', False), ('stt', True)):
        # Send a complete body then close before reading a response; do not retry it.
        with httpx.Client(base_url=base, headers={'Authorization':'Bearer ' + key}, trust_env=False) as client:
            if profile == 'embedding':
                request = client.build_request('POST', '/v1/embeddings', json={'model':'embedding','input':[' hello' * 2047] * 4})
            else:
                request = client.build_request('POST', '/v1/audio/transcriptions', data={'model':'stt','language':'English','stream':str(stream).lower()}, files={'file':('long.wav',long_audio,'audio/wav')})
            body = request.read()
            wire = f'POST {request.url.raw_path.decode()} HTTP/1.1\r\n'.encode() + b''.join(k + b': ' + v + b'\r\n' for k,v in request.headers.raw) + b'\r\n' + body
            began = time.monotonic()
            with socket.create_connection((request.url.host, request.url.port), timeout=10) as connection:
                connection.sendall(wire)
                time.sleep(.05)
            # Successful subsequent requests prove capacity is usable, not immediate compute cancellation.
            if profile == 'embedding':
                response = client.post('/v1/embeddings', json={'model':profile,'input':'capacity recovery'}, timeout=30)
            else:
                response = client.post('/v1/audio/transcriptions', data={'model':profile,'language':'English'},files={'file':('speech.wav',audio,'audio/wav')}, timeout=30)
            response.raise_for_status()
            results.append(dict(test='disconnect_recovery', profile=profile, mode='sse' if stream else 'response', seconds=round(time.monotonic()-began,3), status=response.status_code))
            print(json.dumps(results[-1]), flush=True)
    with httpx.Client(base_url=base, headers={'Authorization':'Bearer '+key}, trust_env=False, timeout=120) as client:
        began = time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            embedding = pool.submit(client.post, '/v1/embeddings', json={'model':'embedding','input':[' hello' * 2047] * 4})
            stt = pool.submit(client.post, '/v1/audio/transcriptions', data={'model':'stt','language':'English'}, files={'file':('long.wav',long_audio,'audio/wav')})
            for job in (embedding, stt):
                job.result().raise_for_status()
            assert embedding.result().json()['usage']['prompt_tokens'] == 8192
            assert stt.result().json()['text'].strip()
        results.append(dict(test='maximum_input_mixed', embedding_tokens=8192, audio_seconds=60, seconds=round(time.monotonic()-began,3)))
        print(json.dumps(results[-1]), flush=True)
    started = time.monotonic()
    samples, latencies, statuses = [], [], {}
    swap_before = subprocess.check_output(['sysctl','vm.swapusage'], text=True).strip()
    with memory_samples(home, samples), httpx.Client(base_url=base, headers={'Authorization':'Bearer '+key}, trust_env=False, timeout=30) as client:
        def request(profile):
            began = time.monotonic()
            if profile == 'embedding':
                response = client.post('/v1/embeddings',json={'model':profile,'input':['Local inference concurrency benchmark. ' * 32] * 4})
            else:
                response = client.post('/v1/audio/transcriptions',data={'model':profile,'language':'English'},files={'file':('speech.wav',audio,'audio/wav')})
            response.raise_for_status()
            body = response.json()
            assert (len(body.get('data',[])) == 4 if profile == 'embedding' else 'hello' in body.get('text','').lower())
            return profile, time.monotonic()-began
        next_report = started + 30
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
            while time.monotonic() - started < seconds:
                jobs = [pool.submit(request, ('embedding','stt')[i % 2]) for i in range(concurrency)]
                for job in jobs:
                    profile, latency = job.result()
                    statuses[profile] = statuses.get(profile,0)+1
                    latencies.append(latency)
                if time.monotonic() >= next_report:
                    print(json.dumps(dict(soak_elapsed=round(time.monotonic()-started), successful_requests=sum(statuses.values()))),flush=True)
                    next_report += 30
    elapsed = time.monotonic()-started
    # Same two-profile load after sustained work isolates model/host time from routing.
    for route in ('direct', 'relay'):
        def probe(profile):
            endpoint = base if route == 'relay' else f"http://127.0.0.1:{states[profile]['port']}"
            model = profile if route == 'relay' else {'embedding':'qwen3-embedding-0.6b','stt':'qwen3-asr-0.6b'}[profile]
            with httpx.Client(base_url=endpoint, headers={'Authorization':'Bearer '+key} if route == 'relay' else {}, trust_env=False,timeout=30) as client:
                began = time.monotonic()
                if profile == 'embedding':
                    result = client.post('/v1/embeddings',json={'model':model,'input':['Local inference concurrency benchmark. ' * 32] * 4})
                else:
                    result = client.post('/v1/audio/transcriptions',data={'model':model,'language':'English'},files={'file':('speech.wav',audio,'audio/wav')})
                result.raise_for_status()
                return time.monotonic()-began
        timings = []
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            for _ in range(5):
                jobs = [pool.submit(probe,p) for p in states]
                timings.extend(job.result() for job in jobs)
        results.append(dict(test='after_sustained_probe',route=route,mean_seconds=round(sum(timings)/len(timings),3),max_seconds=round(max(timings),3)))
        print(json.dumps(results[-1]),flush=True)
    after = {p:json.loads((home/f'control/{p}.json').read_text()) for p in states}
    assert all(after[p]['restarts'] == states[p]['restarts'] for p in states), 'model restarted during load'
    latencies.sort()
    area = sum(value * (samples[i+1][0] - sampled) for i, (sampled, value) in enumerate(samples[:-1]))
    average = area / (samples[-1][0] - samples[0][0]) if len(samples) > 1 else samples[0][1]
    results.append(dict(test='sustained_mixed', concurrency=concurrency, seconds=round(elapsed,2), successes=statuses,
                        successful_rps=round(len(latencies)/elapsed,3), p95_seconds=round(latencies[math.ceil(len(latencies)*.95)-1],3),
                        average_gib=round(average/1024**3,3),peak_gib=round(max(value for _, value in samples)/1024**3,3),
                        swap_before=swap_before,swap_after=subprocess.check_output(['sysctl','vm.swapusage'],text=True).strip(),model_restarts=0))
    (work/'capacity-results.json').write_text(json.dumps(results,indent=2))
    print(json.dumps(results[-1]),flush=True)
    assert average < 4 * 1024**3, f'workload average exceeded 4 GiB: {average / 1024**3:.3f}'
