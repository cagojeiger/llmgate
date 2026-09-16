"""Run against a local E2E fixture; credentials stay in memory."""
import fcntl
import io
import json
from pathlib import Path
import time
import wave

import httpx


def verify(base, key, home, fixture):
    results = []
    def record(name, **data):
        results.append({'test': name, **data})
        print(json.dumps(results[-1]), flush=True)
    with httpx.Client(base_url=base, headers={'Authorization': 'Bearer ' + key}, timeout=300) as client:
        for count, expected in [(2048, 200), (2049, 400)]:
            r = client.post('/v1/embeddings', json={'model': 'embedding', 'input': ' hello' * (count - 1)})
            assert r.status_code == expected, (count, r.status_code, r.text[:120])
            if expected == 200:
                assert r.json()['usage']['prompt_tokens'] == count
            record('embedding_token_boundary', tokens=count, status=r.status_code)
        for count, expected in [(4, 200), (5, 400)]:
            r = client.post('/v1/embeddings', json={'model': 'embedding', 'input': [' hello' * 2047] * count})
            assert r.status_code == expected, (r.status_code, r.text[:120])
            record('embedding_batch_boundary', tokens=count * 2048, status=r.status_code)
        # Each model admits one inference; the other profile remains independent.
        for profile in ('embedding', 'stt'):
            with open(home / f'locks/{profile}-inference.lock', 'a+b') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                state = json.loads((home / f'control/{profile}.json').read_text())
                endpoint = f"http://127.0.0.1:{state['port']}/v1/"
                if profile == 'embedding':
                    r = httpx.post(endpoint + 'embeddings', json={'model': 'qwen3-embedding-0.6b', 'input': 'hello'})
                else:
                    r = httpx.post(endpoint + 'audio/transcriptions', data={'model': 'qwen3-asr-0.6b'}, files={'file': ('a.wav', (fixture/'speech.wav').read_bytes(), 'audio/wav')})
                assert r.status_code == 429, r.text
                record('profile_admission', profile=profile, status=r.status_code)
        with wave.open(str(fixture/'speech.wav'), 'rb') as wav:
            pcm, rate, channels, width = wav.readframes(wav.getnframes()), wav.getframerate(), wav.getnchannels(), wav.getsampwidth()
        for seconds, expected in [(60, 200), (61, 400), (61, 400), (61, 400)]:
            size = rate * channels * width * seconds
            raw = (pcm * (size // len(pcm) + 1))[:size]
            buffer = io.BytesIO()
            with wave.open(buffer, 'wb') as wav:
                wav.setnchannels(channels); wav.setsampwidth(width); wav.setframerate(rate); wav.writeframes(raw)
            began = time.monotonic()
            r = client.post('/v1/audio/transcriptions', data={'model': 'stt', 'language': 'English'}, files={'file': ('bounded.wav', buffer.getvalue(), 'audio/wav')})
            # Duration rejection is a provider 413; the gateway translates to its upstream error contract.
            if expected == 200:
                assert r.status_code == 200 and r.json()['text'].strip(), (r.status_code, r.text[:200])
            else:
                assert r.status_code == 400, (r.status_code, r.text[:200])
            record('stt_duration_boundary', seconds=seconds, status=r.status_code, elapsed=round(time.monotonic()-began, 2))
        r = client.post('/v1/audio/transcriptions', data={'model': 'stt', 'prompt': ' hello' * 257}, files={'file': ('a.wav', (fixture/'speech.wav').read_bytes(), 'audio/wav')})
        assert r.status_code == 400, (r.status_code,r.text[:120])
        record('stt_prompt_boundary', tokens=257, status=r.status_code)
    readings = [json.loads((home / f'control/{p}-memory.json').read_text()) for p in ('embedding', 'stt')]
    peak = max(r['peak_bytes'] for r in readings)
    assert peak < 6 * 1024**3, peak
    record('managed_memory', peak_bytes=peak, target_bytes=4 * 1024**3, stop_bytes=6 * 1024**3, measurement='Darwin physical footprint, 250ms sampling')
    (fixture/'limits-results.json').write_text(json.dumps(results, indent=2))
