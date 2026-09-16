# llmgate-caller

서버용 Rust sidecar다. Go LLMGate의 HTTP 요청을 published RelayGate SDK 0.5.1의 Pipe로 전달한다.
Mac CLI·모델과 독립적이며 `relaygate-token-issuer` 0.5.1로 매 dial마다 새 exact JWT를 발급한다.

```text
Client → Go LLMGate(API 키·로그) → loopback Caller → TLS RelayGate → Mac CLI → MLX
```

`LLMGATE_CALLER_CONFIG=/run/config/caller.json`:

```json
{
  "issuer_config": "/run/config/workers.json",
  "max_connections": 64,
  "connection_timeout_seconds": 180
}
```

`workers.json`은 Go의 `LLMGATE_WORKERS_CONFIG`와 동일한 [issuer/profile 설정](../docs/worker-registration.md)이다.
공유 개인키는 P-256 **PKCS#8 PEM**으로 mount한다. Gateway는 그 공개키를 신뢰해야 한다.
키·설정 파일은 runtime UID 65532가 읽을 수 있게 준비하며 이미지에 복사하지 않는다.
선택 `ca_file`로 private CA를, `gateway_endpoint`로 Caller 측 Gateway 주소를 지정한다. TLS만 허용한다.
키 교체는 Gateway 공개키 overlap을 먼저 설정한 뒤 Go와 Caller를 재시작한다.

Go와 Caller는 같은 pod/network namespace를 사용한다. Go의 모델·alias와 Caller의 route는 공유 workers.json에서 자동 구성된다. 기본 주소는 embedding `127.0.0.1:18081`, STT `127.0.0.1:18082`이며 profile의 `caller_address`로 함께 변경한다. STT의 요청별 새 연결도 자동 적용한다. 기존 routes를 명시하면 공유 profile 주소와 일치해야 한다.

- 워커가 없어도 Caller는 대기한다. 요청에 503을 반환하고 다음 요청에서 새 dial을 수행한다.
- Gateway가 처음부터 없으면 초기 연결을 재시도한다. 연결 후 reconnect는 SDK가 소유한다.
- 전송 버퍼는 SDK DATA chunk에 맞춰 방향별 64 KiB다. 작은 청크로 음성 업로드를 쪼개 Gateway 큐를 과도하게 채우지 않도록 한다.
- 연결 상한은 route 전체 합산이다. 포화 시 503, 전달한 요청의 자동 retry/replay는 없다.
- SIGTERM/SIGINT는 신규 연결을 닫고 최대 5초 drain 후 남은 Pipe를 종료한다.
- 원문 payload·API 키·JWT를 로그에 남기지 않는다. 요청 결과·오류의 관측은 Go audit가 소유한다.

```sh
cd caller
cargo test --locked
cargo clippy --locked --all-targets -- -D warnings
docker build -t llmgate-caller:local .
```

[실제 Compose 배선](../cli/examples/compose/compose.yaml)은 이 Dockerfile로 Caller를 빌드하며
`network_mode: service:llmgate`로 Go와 loopback을 공유한다. fixture의 root user는 로컬 시험 전용이다.
서버 release workflow는 `ghcr.io/cagojeiger/llmgate-caller:<서버 VERSION>`을 함께 빌드·게시한다.
이 PR에서 운영 배포나 release 게시를 실행하지 않는다.
