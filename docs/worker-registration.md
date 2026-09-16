# Mac worker 연동

consumer의 기존 key_hashes와 별도로 allowed_worker_profiles에 허용 profile을 적는다.
미설정/빈 목록은 거부한다. API 호출 allowed_aliases와 독립적이다.

LLMGATE_WORKERS_CONFIG는 다음 JSON 파일 경로다. 개인키는 PKCS#8 또는 EC PEM의 P-256이다.

```json
{
  "issuer": "https://llmgate.example/worker-issuer",
  "audience": "relaygate",
  "key_id": "worker-1",
  "private_key_file": "/run/secrets/worker-issuer.pem",
  "gateway_endpoint": "tls://relaygate.example:443",
  "ttl_seconds": 300,
  "profiles": {
    "embedding": {"destination": "llmgate/qwen3-embedding-0.6b-v1", "version": "1"},
    "stt": {"destination": "llmgate/qwen3-asr-0.6b-v1", "version": "1"}
  }
}
```

POST /v1/workers/token은 기존 Bearer 키와 JSON
`{"protocol_version":1,"profile":"embedding","profile_version":"1"}`을 받는다.
응답은 protocol/profile 버전, exact destination, gateway_endpoint, access_token, expires_at이다.
허용한 publish만 발급하고 no-store를 적용한다. 키·token은 audit에 남기지 않는다.

오류: 잘못된 키 401, worker 권한 없음 403, 요청/버전 오류 400. Gateway JWT 검증은 별도다.
HTTPS로 노출한다. token 만료·키 삭제는 기존 Binding·Pipe를 종료하지 않는다.

현재 Mac worker profile은 Qwen3 Embedding과 Qwen3 ASR 파일 STT다.
RelayGate pool을 사용하는 OpenAI transcription model 설정에는
`new_connection_per_request: true`를 지정한다. 기본 false라 기존 provider 연결 재사용은 유지한다.
embedding 경로는 이미 요청별 연결을 사용한다.

consumer YAML에 `allowed_worker_profiles: [embedding, stt]`를 명시한다. Gateway에는 같은 namespace·issuer·audience·key_id와 대응 공개키를 별도로 설정한다. 개인키는 LLMGate 서버에만 둔다. 서버 Caller의 dial 권한은 worker publish 권한과 별도로 공급한다. [서버 Caller](../caller/README.md)는 같은 issuer 설정과 PKCS#8 개인키를 읽고 매 dial마다 새 JWT를 발급한다. Mac CLI는 이 키를 받지 않는다.

## 기본 모델 자동 구성

worker 기능이 켜지면 설정된 profile에 대해 다음 catalog가 서버 시작 시 메모리에 구성된다. Mac 등록은 catalog 파일을 쓰거나 서버 재시작을 유발하지 않는다.

| profile / 호출 alias | 모델 ID | Caller 기본 주소 |
| --- | --- | --- |
| embedding | qwen3-embedding-0.6b | 127.0.0.1:18081 |
| stt | qwen3-asr-0.6b | 127.0.0.1:18082 |

별도 model/alias YAML·caller routes는 필요 없다. 포트를 바꾸려면 workers.json의 해당 profile에 `"caller_address":"127.0.0.1:19081"`처럼 지정한다. Go와 Caller가 같은 값을 읽는다. 기존 수동 catalog는 동등한 경로만 허용하며 이름·경로 충돌은 시작 오류다. Caller의 기존 routes를 유지한다면 공유 profile 주소와 일치해야 한다.

관리자는 최초에 issuer·Gateway 신뢰·Caller 배포와 consumer 권한을 준비한다. `allowed_worker_profiles`는 publish 권한이며 호출 권한을 자동 확대하지 않는다. `allowed_aliases`가 비어 있으면 기존처럼 호출 제한이 없고, 제한 목록이 있으면 `embedding`, `stt`를 포함한다. 워커가 없거나 꺼져 있으면 해당 모델 요청은 unavailable이며, 다음 Mac이 start하면 새 요청부터 사용한다.
