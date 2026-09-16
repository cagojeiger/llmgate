"""Real MLX -> RelayGate -> locally built LLMGate/Compose validation.

Run with the test-only Python requirements in scripts/test-requirements.txt.
Models run natively on this Mac. No Linux agent or vendor API is used.
"""
import argparse
import base64
import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import time

import httpx
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

ROOT = Path(__file__).resolve().parents[1]
RELAY = ROOT.parent.parent / "relaygate"
CLI = ROOT / "target/aarch64-apple-darwin/debug/llmgate-cli"
HOME = Path.home() / ".llmgate"
WORK = ROOT / ".local/e2e-embedding-stt"
PROFILES = {"embedding": ("qwen3-embedding-0.6b", "embeddings"), "stt": ("qwen3-asr-0.6b", "transcription")}
PROCS = []
FILES = []
RESULTS = []
STARTED = []
REGISTERED = False


def record(name, **data):
    RESULTS.append({"test": name, **data})
    print(json.dumps(RESULTS[-1], ensure_ascii=False), flush=True)
    (WORK / "results.json").write_text(json.dumps(RESULTS, ensure_ascii=False, indent=2))


def b64(value):
    return base64.urlsafe_b64encode(value).decode().rstrip("=")


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def start(name, argv, environment):
    log = open(WORK / (name + ".log"), "w")
    FILES.append(log)
    process = subprocess.Popen(argv, env={**os.environ, **environment}, stdout=log, stderr=log, start_new_session=True)
    PROCS.append(process)
    return process


def stop(process):
    if process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()


def ready_port(process, number):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        assert process.poll() is None, "test process exited; see logs"
        try:
            with socket.create_connection(("127.0.0.1", number), 0.2):
                return
        except OSError:
            time.sleep(0.2)
    raise TimeoutError("test listener startup")


def write_json(path, value):
    path.write_text(json.dumps(value))
    path.chmod(0o600)


def fixture():
    WORK.mkdir(parents=True, exist_ok=True)
    WORK.chmod(0o700)
    api_key = secrets.token_urlsafe(32)
    signing = ec.generate_private_key(ec.SECP256R1())
    public = signing.public_key().public_numbers()
    (WORK / "issuer.pem").write_bytes(signing.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    (WORK / "issuer.pem").chmod(0o600)
    gateway, http_port = port(), port()
    callers = {p: port() for p in PROFILES}
    write_json(WORK / "auth.json", {"version": 1, "audience": "relaygate", "clock_skew_seconds": 0, "issuers": [{"namespace": "llmgate", "issuer": "local-mlx-test", "keys": [{"kid": "test", "kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "x": b64(public.x.to_bytes(32, "big")), "y": b64(public.y.to_bytes(32, "big"))}]}], "verification": {"concurrency": 8, "timeout_ms": 1000}})
    now = datetime.datetime.now(datetime.timezone.utc)
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "LLMGate local test CA")])
    ca = x509.CertificateBuilder().subject_name(ca_name).issuer_name(ca_name).public_key(signing.public_key()).serial_number(x509.random_serial_number()).not_valid_before(now - datetime.timedelta(minutes=1)).not_valid_after(now + datetime.timedelta(days=1)).add_extension(x509.BasicConstraints(ca=True, path_length=None), True).sign(signing, hashes.SHA256())
    leaf_key = ec.generate_private_key(ec.SECP256R1())
    leaf = x509.CertificateBuilder().subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "localhost")])).issuer_name(ca_name).public_key(leaf_key.public_key()).serial_number(x509.random_serial_number()).not_valid_before(now - datetime.timedelta(minutes=1)).not_valid_after(now + datetime.timedelta(days=1)).add_extension(x509.BasicConstraints(ca=False, path_length=None), True).add_extension(x509.SubjectAlternativeName([x509.DNSName("localhost"), x509.DNSName("host.docker.internal")]), False).add_extension(x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH]), False).sign(signing, hashes.SHA256())
    (WORK / "ca.pem").write_bytes(ca.public_bytes(serialization.Encoding.PEM))
    (WORK / "server.pem").write_bytes(leaf.public_bytes(serialization.Encoding.PEM))
    (WORK / "server-key.pem").write_bytes(leaf_key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    (WORK / "server-key.pem").chmod(0o600)
    write_json(WORK / "workers.json", {"issuer": "local-mlx-test", "audience": "relaygate", "key_id": "test", "private_key_file": "/fixture/issuer.pem", "gateway_endpoint": f"tls://localhost:{gateway}", "ttl_seconds": 60, "profiles": {p: {"destination": f"llmgate/{p}-v1", "version": "1"} for p in PROFILES}})
    for directory in ("catalog/models", "catalog/aliases", "consumers"):
        (WORK / directory).mkdir(parents=True, exist_ok=True)
    for p, (model, api) in PROFILES.items():
        (WORK / f"catalog/models/{p}.yaml").write_text(f"id: {model}\nvendor: local-mlx\nprotocol: openai\napi: {api}\nbase_url: http://127.0.0.1:{callers[p]}/v1\nnew_connection_per_request: true\n")
        (WORK / f"catalog/aliases/{p}.yaml").write_text(f"alias: {p}\nchain: [{model}]\n")
    hashed = hashlib.sha256(api_key.encode()).hexdigest()
    (WORK / "consumers/test.yaml").write_text(f"name: test\nkey_hashes: [sha256:{hashed}]\nallowed_aliases: [embedding, stt]\nallowed_worker_profiles: [embedding, stt]\n")
    write_json(WORK / "caller.json", {"issuer_config":"/fixture/workers.json", "ca_file":"/fixture/ca.pem", "gateway_endpoint":f"tls://host.docker.internal:{gateway}", "routes":{p:f"127.0.0.1:{callers[p]}" for p in PROFILES}})
    return api_key, signing, gateway, http_port, callers


def operation_token(key, profile, action="dial"):
    header = b64(json.dumps({"alg": "ES256", "kid": "test", "typ": "relaygate-operation+jwt"}).encode())
    claims = b64(json.dumps({"iss": "local-mlx-test", "aud": "relaygate", "nbf": int(time.time()) - 1, "exp": int(time.time()) + 7200, "permissions": [{"action": action, "namespace": "llmgate", "scope": {"kind": "exact", "name": f"{profile}-v1"}}]}).encode())
    message = (header + "." + claims).encode()
    r, s = decode_dss_signature(key.sign(message, ec.ECDSA(hashes.SHA256())))
    return message.decode() + "." + b64(r.to_bytes(32, "big") + s.to_bytes(32, "big"))


def events(response):
    response.raise_for_status()
    for line in response.iter_lines():
        if line.startswith("data: ") and line != "data: [DONE]":
            value = json.loads(line[6:])
            assert "error" not in value, value
            yield value


def main():
    global REGISTERED, CLI
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli-binary", type=Path, default=CLI)
    parser.add_argument("--gateway-binary", type=Path, default=RELAY / "target/debug/relaygate-server")
    parser.add_argument("--keep-running", action="store_true")
    parser.add_argument("--verify-limits", action="store_true")
    parser.add_argument("--verify-recovery", action="store_true")
    parser.add_argument("--openclaw-node")
    parser.add_argument("--openclaw-cli", type=Path)
    args = parser.parse_args()
    CLI = args.cli_binary.resolve()
    assert not (HOME / "registration.json").exists(), "existing registration preserved; use a separate test home"
    for p in PROFILES:
        assert not (HOME / f"control/{p}.sock").exists(), "existing profile preserved; stop it explicitly first"
    key, signing, gateway_port, http_port, callers = fixture()
    env = {**os.environ, "LLMGATE_TEST_PORT": str(http_port), "LLMGATE_TEST_CONFIG": str(WORK)}
    compose = ["docker", "compose", "--project-name", "llmgate-mlx-local", "--env-file", "/dev/null", "-f", str(ROOT / "examples/compose/compose.yaml")]
    try:
        record("topology", models="native Apple Silicon MLX", api="locally built LLMGate via Docker Compose", workers=len(PROFILES))
        gateway = start("gateway", [str(args.gateway_binary.resolve())], {"RELAYGATE_SDK_TRANSPORT": "tls", "RELAYGATE_BIND_ADDR": f"127.0.0.1:{gateway_port}", "RELAYGATE_AUTH_CONFIG_PATH": str(WORK / "auth.json"), "RELAYGATE_SDK_TLS_CERT_PATH": str(WORK / "server.pem"), "RELAYGATE_SDK_TLS_KEY_PATH": str(WORK / "server-key.pem")})
        ready_port(gateway, gateway_port)
        subprocess.run([str(ROOT / "tests/relay-bridge/target/aarch64-apple-darwin/debug/llmgate-relay-probe"), "verify-limits"],
            env={**os.environ,"RELAYGATE_ADDR":f"tls://localhost:{gateway_port}","RELAYGATE_CA_FILE":str(WORK/"ca.pem"),
                 "PUBLISH_TOKEN":operation_token(signing,"capacity","publish"),"DIAL_TOKEN":operation_token(signing,"capacity")}, check=True, timeout=30)
        record("worker_pipe_cap", maximum=1, extra_pipe="RESOURCE_EXHAUSTED")
        with open(WORK / "compose-build.log", "w") as build_log:
            subprocess.run(compose + ["up", "--build", "--detach"], env=env, stdout=build_log, stderr=subprocess.STDOUT, check=True)
        base = f"http://127.0.0.1:{http_port}"
        with httpx.Client(base_url=base, headers={"Authorization": "Bearer " + key}, timeout=180) as client:
            for _ in range(100):
                try:
                    if client.get("/healthz").status_code == 200:
                        break
                except httpx.TransportError:
                    pass
                time.sleep(0.2)
            else:
                raise TimeoutError("LLMGate health")
            version = subprocess.check_output(compose + ["exec", "-T", "llmgate", "/app/llmgate", "--version"], env=env, text=True).strip()
            record("compose_built_version", version=version)
            no_worker = client.post("/v1/embeddings", json={"model":"embedding","input":"not yet registered"})
            assert no_worker.status_code in (502, 503), no_worker.text
            record("zero_workers", status=no_worker.status_code)
            body = {"protocol_version": 1, "profile": "embedding", "profile_version": "1"}
            assert client.post("/v1/workers/token", json=body, headers={"Authorization": "Bearer invalid"}).status_code == 401
            assert client.post("/v1/workers/token", json={**body, "action": "dial"}).status_code == 400
            record("token_api_negative_cases", invalid_key=401, dial_escalation=400)
            subprocess.run([str(CLI), "register", "--url", base, "--profiles", *PROFILES, "--key-stdin", "--allow-loopback-http", "--ca-file", str(WORK / "ca.pem")], input=key + "\n", text=True, check=True)
            REGISTERED = True
            for p in PROFILES:
                STARTED.append(p)
                subprocess.run([str(CLI), "start", p], check=True)
                states = [json.loads(line) for line in subprocess.check_output([str(CLI), "status", "--json"], text=True).splitlines()]
                current = next(state for state in states if state["profile"] == p)
                assert current.get("runtime") == "ready" and current.get("publish") == "active", current
                record("start_ready_on_return", profile=p, runtime=current["runtime"], publish=current["publish"])
            human_status = subprocess.check_output([str(CLI), "status"], text=True)
            assert human_status.count("서빙 가능") == len(PROFILES), human_status
            record("human_status", serving_profiles=len(PROFILES))
            deadline = time.monotonic() + 900
            seen = set()
            while time.monotonic() < deadline:
                states = [json.loads(line) for line in subprocess.check_output([str(CLI), "status", "--json"], text=True).splitlines()]
                for state in states:
                    if state.get("runtime") == "ready" and state.get("publish") == "active" and state["profile"] not in seen:
                        seen.add(state["profile"])
                        record("worker_ready", profile=state["profile"], port=state["port"])
                    assert (state.get("last_state") or {}).get("runtime") != "cleanup_failed", state
                if len(seen) == len(PROFILES):
                    break
                time.sleep(2)
            assert len(seen) == len(PROFILES), "model readiness timeout"
            published_at = time.monotonic()
            for encoding in ("float", "base64"):
                result = client.post("/v1/embeddings", json={"model": "embedding", "input": ["hello", "안녕하세요"], "encoding_format": encoding})
                result.raise_for_status()
                vectors = result.json()["data"]
                assert len(vectors) == 2
                for v in vectors:
                    if encoding == "float":
                        assert len(v["embedding"]) == 1024 and all(math.isfinite(x) for x in v["embedding"])
                    else:
                        assert len(base64.b64decode(v["embedding"])) == 4096
                record("embedding", encoding=encoding, vectors=2, dimensions=1024)
            subprocess.run(["say", "-v", "Samantha", "-o", str(WORK / "speech.aiff"), "Hello, this is a local speech recognition test."], check=True)
            subprocess.run(["afconvert", "-f", "WAVE", "-d", "LEI16@24000", "-c", "1", str(WORK / "speech.aiff"), str(WORK / "speech.wav")], check=True)
            audio = (WORK / "speech.wav").read_bytes()
            result = client.post("/v1/audio/transcriptions", data={"model": "stt", "response_format": "json", "language": "English"}, files={"file": ("speech.wav", audio, "audio/wav")})
            result.raise_for_status()
            assert "hello" in result.json()["text"].lower(), result.text
            record("stt_response", text=result.json()["text"])
            with client.stream("POST", "/v1/audio/transcriptions", data={"model": "stt", "response_format": "json", "stream": "true", "language": "English"}, files={"file": ("speech.wav", audio, "audio/wav")}) as response:
                output = list(events(response))
                deltas = [e["delta"] for e in output if e.get("type") == "transcript.text.delta"]
                assert len(deltas) > 1 and "hello" in "".join(deltas).lower(), output
                record("stt_sse", deltas=len(deltas), text="".join(deltas))
            record("all_inference_modes", passed=True)
            if args.verify_recovery:
                while time.monotonic() < published_at + 62:
                    remaining = round(published_at + 62 - time.monotonic())
                    print(json.dumps({"test":"waiting_for_token_expiry","remaining_seconds":remaining}),flush=True)
                    time.sleep(min(10, max(0.1, remaining)))
                renewed = client.post("/v1/embeddings", json={"model":"embedding","input":"caller token after expiry"})
                renewed.raise_for_status()
                record("caller_after_token_expiry", status=renewed.status_code)
                stop(gateway)
                unavailable = client.post("/v1/embeddings", json={"model":"embedding","input":"Gateway stopped"})
                assert unavailable.status_code >= 500, unavailable.text
                gateway = start("gateway-restarted", [str(args.gateway_binary.resolve())], {"RELAYGATE_SDK_TRANSPORT":"tls",
                    "RELAYGATE_BIND_ADDR":f"127.0.0.1:{gateway_port}","RELAYGATE_AUTH_CONFIG_PATH":str(WORK/"auth.json"),
                    "RELAYGATE_SDK_TLS_CERT_PATH":str(WORK/"server.pem"),"RELAYGATE_SDK_TLS_KEY_PATH":str(WORK/"server-key.pem")})
                ready_port(gateway,gateway_port)
                deadline=time.monotonic()+90
                while time.monotonic()<deadline:
                    result=client.post("/v1/embeddings",json={"model":"embedding","input":"after Gateway recovery"})
                    if result.status_code==200:break
                    time.sleep(2)
                result.raise_for_status()
                record("gateway_restart_after_token_expiry", status=result.status_code, caller_restart=False, worker_restart=False)

            if args.verify_limits:
                from verify_limits import verify
                verify(base, key, HOME, WORK)
            if args.openclaw_cli:
                from verify_openclaw import verify
                verify(base, key, ROOT, WORK, args.openclaw_node or "node", args.openclaw_cli)
    finally:
        if not args.keep_running:
            for p in STARTED:
                subprocess.run([str(CLI), "down", p], check=False)
            if REGISTERED:
                subprocess.run([str(CLI), "unregister"], check=False)
            with open(WORK / "compose-runtime.log", "w") as log:
                subprocess.run(compose + ["logs", "--no-color"], env=env, stdout=log, stderr=subprocess.STDOUT)
            subprocess.run(compose + ["down", "--volumes", "--remove-orphans"], env=env, check=False)
            for proc in reversed(PROCS):
                stop(proc)
            for f in FILES:
                f.close()
            for p in PROFILES:
                if (HOME / f"runtimes/{p}").exists():
                    raise RuntimeError(f"runtime cleanup incomplete: {p}")
            if (HOME / "registration.json").exists():
                raise RuntimeError("registration cleanup incomplete")
            record("cleanup", runtimes_removed=True, registration_removed=True)


if __name__ == "__main__":
    main()
