# 환경 변수

게이트웨이 인프라/시크릿/타이밍 설정. yaml 이 운영 데이터를 담는다면, env 는 *프로세스가
어디서 어떻게 사는가* 를 담는다. 폴백 / 회로 결정 근거는 [ADR 004](adr/004-fallback-policy.md),
타임아웃 권위자 분리는 [ADR 005](adr/005-timeout-authority.md).

| 변수 | 디폴트 | 의미 |
|---|---|---|
| `LLMGATE_ADDR` | `:8080` | HTTP listen address |
| `LLMGATE_ENVIRONMENT` | `local` | 로그 / 텔레메트리의 배포 환경 라벨 (`local`, `staging`, `prod` 등) |
| `LLMGATE_LOG_LEVEL` | `info` | process log level (`debug`, `info`, `warn`, `error`) |
| `LLMGATE_FALLBACK_ON` | `rate_limit,upstream,timeout,network` | chain 진행 사유 |
| `LLMGATE_CIRCUIT_FAILURES` | `3` | 연속 실패 임계 (0 = 비활성) |
| `LLMGATE_CIRCUIT_OPEN_DURATION` | `30s` | 차단 기본 시간 |
| `LLMGATE_CIRCUIT_MAX_OPEN_DURATION` | `5m` | 차단 최대 시간 (백오프 cap) |
| `LLMGATE_CIRCUIT_JITTER` | `0.2` | 차단 시간 ±지터 |
| `LLMGATE_REQUEST_TIMEOUT` | `5m` | 요청 1 회 총 wall-clock |
| `LLMGATE_COMPLETE_TIMEOUT` | `1m` | non-stream 시도당 |
| `LLMGATE_STREAM_IDLE_TIMEOUT` | `1m` | 스트림 이벤트 사이 idle |
| `LLMGATE_METRICS_ENABLED` | `false` | `true`이면 `/metrics` endpoint 를 mount. 외부 노출 제어는 네트워크/ingress 책임 |
| `LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE` | `1000` | 요청 경로와 result sink 사이 bounded queue 크기 |
| `LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE` | `100` | worker 가 즉시 flush 하는 이벤트 개수 |
| `LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL` | `1s` | batch 가 가득 차지 않아도 flush 하는 최대 대기 시간 |
| `LLMGATE_LLMRESULT_ASYNC_EMIT_TIMEOUT` | `10s` | worker 의 downstream Emit 1회 상한 |
| `LLMGATE_LLMRESULT_ASYNC_CLOSE_TIMEOUT` | `60s` | shutdown 때 async worker 종료 대기 상한 |
| `LLMGATE_CATALOG` | `./catalog` | catalog 디렉토리 (부재 → fail) |
| `LLMGATE_CONSUMERS` | `./consumers` | consumers 디렉토리 (부재 → fail) |
| `LLMGATE_SHUTDOWN_DRAIN_TIMEOUT` | `5m` | drain 최대 wall-clock, 이후 force close |

vendor 별 API 키는 `LLMGATE_<VENDOR>_API_KEY` 패턴이다. 현재 기본값은 catalog `vendor` 를
대문자화한 문자열 그대로 쓰므로, 하이픈 등 shell 변수에 부적합한 vendor 는 catalog yaml 의
`auth_env` 를 명시한다.
catalog yaml 의 `auth_env` 가 명시적으로 다른 이름을 가리키면 그쪽이 우선.

## Audit 싱크 (결과 레코드 → 로컬 로테이션 → S3)

요청당 결과 레코드(프롬프트·응답 본문 포함)를 로컬에 시간버킷으로 쌓고 압축해 S3 로
best-effort 업로드한다. `LLMGATE_AUDIT_DIR` 이 비면 비활성. 설계는 [ADR 007](adr/007-audit-result-sink.md)·[008](adr/008-drop-nats-result-sink.md)·[009](adr/009-audit-scale-hardening.md).

| 변수 | 디폴트 | 의미 |
|---|---|---|
| `LLMGATE_AUDIT_DIR` | `` (비활성) | 작업 루트. 하위에 `active/ pending/ compressed/ uploaded/` 를 관리 |
| `LLMGATE_AUDIT_ROTATE_INTERVAL` | `1h` | 시간 버킷 = 파일 1개 범위. **UTC 클록 경계**에 봉인, **24h 약수만** 허용 |
| `LLMGATE_AUDIT_ROTATE_MAX_BYTES` | `134217728` | 크기 **안전판**(초과 시 조기 봉인). ≤ `DISK_CAP` |
| `LLMGATE_AUDIT_UPLOAD_INTERVAL` | `30s` | 유지보수 주기 (compress → upload → reap) |
| `LLMGATE_AUDIT_RETENTION` | `168h` | 업로드 후 로컬 보관 기간 (이후 삭제; S3 는 유지) |
| `LLMGATE_AUDIT_DISK_CAP` | `5368709120` | 로컬 디스크 상한 (초과 시 oldest-uploaded → compressed → pending 순 drop) |
| `LLMGATE_AUDIT_COMPRESSION` | `gzip` | `gzip` 또는 `none` (단일 코어 압축) |
| `LLMGATE_AUDIT_UPLOAD_CONCURRENCY` | `4` | 병렬 업로드 수 |
| `LLMGATE_AUDIT_S3_ENDPOINT` | `` | 비면 **로컬 전용**(업로드 없음). host:port |
| `LLMGATE_AUDIT_S3_BUCKET` | `` | **선존재 필수** — 앱은 버킷을 만들지 않음(부재 시 부팅 실패) |
| `LLMGATE_AUDIT_S3_REGION` | `us-east-1` | S3 리전 |
| `LLMGATE_AUDIT_S3_ACCESS_KEY` | `` | 액세스 키 (로그엔 안 찍힘) |
| `LLMGATE_AUDIT_S3_SECRET_KEY` | `` | 시크릿 키 (로그엔 안 찍힘) |
| `LLMGATE_AUDIT_S3_PREFIX` | `` | 객체키 prefix. 키 = `<prefix>/dt=YYYY-MM-DD/hour=HH/<pod>-<bucket>-<rand>.jsonl.gz` |
| `LLMGATE_AUDIT_S3_USE_SSL` | `false` | https 여부 (in-cluster MinIO 는 보통 plain http) |
| `LLMGATE_AUDIT_S3_PATH_STYLE` | `true` | path-style 주소(MinIO 필수) |

메트릭은 보류 상태라 관측은 **로그가 SLI** 다: 시작 시 `audit result sink enabled`(설정 요약,
크레덴셜 제외), 부팅 복구 시 `audit recovered orphaned files on boot`, 활동이 있는 유지보수마다
`audit maintenance`(compressed/uploaded/reaped/dropped/실패 카운트), 실패·유실은 `WARN`.
