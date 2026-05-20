# 환경 변수

← [architecture.md](architecture.md) 로 돌아가기

게이트웨이 인프라/시크릿/타이밍 설정. yaml 이 운영 데이터를 담는다면, env 는 *프로세스가
어디서 어떻게 사는가* 를 담는다. 폴백 / 회로 결정 근거는 [ADR 004](adr/004-fallback-policy.md),
타임아웃 권위자 분리는 [ADR 005](adr/005-timeout-authority.md).

| 변수 | 디폴트 | 의미 |
|---|---|---|
| `LLMGATE_ENVIRONMENT` | `local` | 로그 / 텔레메트리의 배포 환경 라벨 (`local`, `staging`, `prod` 등) |
| `LLMGATE_FALLBACK_ON` | `rate_limit,upstream,timeout,network` | chain 진행 사유 |
| `LLMGATE_CIRCUIT_FAILURES` | `3` | 연속 실패 임계 (0 = 비활성) |
| `LLMGATE_CIRCUIT_OPEN_DURATION` | `30s` | 차단 기본 시간 |
| `LLMGATE_CIRCUIT_MAX_OPEN_DURATION` | `5m` | 차단 최대 시간 (백오프 cap) |
| `LLMGATE_CIRCUIT_JITTER` | `0.2` | 차단 시간 ±지터 |
| `LLMGATE_REQUEST_TIMEOUT` | `5m` | 요청 1 회 총 wall-clock |
| `LLMGATE_COMPLETE_TIMEOUT` | `1m` | non-stream 시도당 |
| `LLMGATE_STREAM_IDLE_TIMEOUT` | `1m` | 스트림 이벤트 사이 idle |
| `LLMGATE_LLMRESULT_NATS_URL` | — | 비어 있으면 llmresult 원격 publish 비활성. 설정하면 NATS JetStream URL (`nats://host:4222`) |
| `LLMGATE_LLMRESULT_NATS_STREAM` | `LLMRESULT` | llmresult 이벤트를 저장할 JetStream stream 이름 |
| `LLMGATE_LLMRESULT_NATS_SUBJECT` | `llmgate.llmresult.finalized` | llmresult 이벤트 publish subject |
| `LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE` | `1000` | 요청 경로와 NATS publish 사이 bounded queue 크기 |
| `LLMGATE_CATALOG` | `./catalog` | catalog 디렉토리 (부재 → fail) |
| `LLMGATE_CONSUMERS` | `./consumers` | consumers 디렉토리 (부재 → fail) |
| `LLMGATE_SHUTDOWN_DRAIN_TIMEOUT` | `5m` | drain 최대 wall-clock, 이후 force close |

vendor 별 API 키는 `LLMGATE_<VENDOR>_API_KEY` 패턴 (예: `LLMGATE_OPENCODE_API_KEY`,
`LLMGATE_OPENROUTER_API_KEY`).
catalog yaml 의 `auth_env` 가 명시적으로 다른 이름을 가리키면 그쪽이 우선.
