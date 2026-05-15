# 로그 세 줄기 (access / audit / call)

← [architecture.md](architecture.md) 로 돌아가기

게이트웨이는 세 종류의 구조화 로그를 같은 stdout 으로 흘린다 — 같은 요청에서 나온
사실이지만, 목적이 다른 증적은 `log` attr 로 갈래를 분리한다. DB 없이 운영하므로 stdout
JSON 로그가 Loki / ELK 같은 외부 sink 에서 검색 가능한 사실 원장이 된다.

```
log=access  → HTTP 전송 사실 (프로브 제외 모든 경로, 한 줄/요청)
log=audit   → 운영 / 보안 fact (chat 요청 한정, auth 실패 포함 한 줄/요청)
log=call    → LLM 호출 결과 fact (LLM 호출이 시도된 요청만 한 줄)
```

## 키 스키마

| 키 | access | audit | call | 의미 |
|---|---|---|---|---|
| `request_id` | ✓ | ✓ | ✓ | 세 줄기를 묶는 조인 키 |
| `schema_version` | — | ✓ | ✓ | 로그 스키마 버전. 후처리 호환성 경계 |
| `event_type` | — | ✓ | ✓ | `audit` / `call` |
| `operation` | — | ✓ | ✓ | 도메인 호출 (`chat.completions` / `chat.completions.stream`) |
| `method` | ✓ | — | — | HTTP method (`POST`, `GET`) |
| `path` | ✓ | — | — | URL path |
| `status` | ✓ | ✓ | ✓ | HTTP status |
| `duration_ms` | ✓ | ✓ | ✓ | 요청 wall-clock |
| `consumer_name` | ✓ (성공시만) | ✓ (성공시만) | ✓ (성공시만) | 호출자 이름. auth 실패시 키 자체가 omit |
| `consumer_key_id` | — | ✓ (성공시만) | ✓ (성공시만) | 매칭된 키의 sha256 앞 8자 |
| `auth_error` | ✓ (실패시만) | ✓ (실패시만) | — | `missing` / `format` / `unknown` |
| `error_kind` | — | ✓ (실패시만) | ✓ (실패시만) | `llmtypes.ErrorKind` 값 |
| `model_requested` | — | — | ✓ | 호출자가 보낸 alias / model |
| `model_used` | — | — | ✓ | 실제 성공 또는 최종 시도 model. 요청값과 같으면 omit 가능 |
| `vendor` | — | — | ✓ | 실제 성공 또는 최종 시도 vendor |
| `request_bytes` / `response_bytes` | — | — | ✓ | 게이트웨이 관측 바이트 수 |
| `prompt_tokens` / `completion_tokens` / `total_tokens` | — | — | ✓ | vendor usage 값. 없으면 omit |
| `vendor_cost` | — | — | ✓ | vendor 가 준 비용 값. 게이트웨이는 계산하지 않음 |
| `attempts` | — | — | ✓ | fallback chain 이 실제로 2회 이상 시도된 경우만 기록 |

핵심: `method` 와 `operation` 은 *별도 키*. 같은 stdout 에 섞여 있어도 의미 충돌 없음.
`Loki` / `ELK` 에서 `log=access|audit|call` 로 사전 필터한 다음 `request_id` 로 조인하면
"누가 / 어떤 키로 / 어떤 vendor 폴백을 거쳐 / 몇 ms 에 / 어떤 상태로 끝났나" 가 한 쿼리에
들어온다.

## audit-always 보장

인증 실패 (401) 도 audit emit 한다 — `consumer_name` 은 omit 되지만 `auth_error` /
`error_kind=auth` / `status=401` 은 박힌다. 결정 근거 [ADR 003](adr/003-consumers.md).

`call` 은 LLM 호출이 시도된 요청에만 emit 한다. auth 실패, JSON decode 실패, consumer
allowlist 거부처럼 vendor 호출 전 끝난 요청은 `audit` 에만 남긴다.

## 민감정보 경계

로그에는 다음을 남기지 않는다.

```
Authorization header
raw consumer key
vendor API key
prompt / message body
response body
tool payload
```

호출자 식별은 `consumer_name` 과 `consumer_key_id` 로 충분하다. `consumer_key_id` 는 매칭된
sha256 해시의 앞 8자라 키 회전 중 어떤 키가 쓰였는지 구분할 수 있지만, 원문 키를 복구할 수
없다.
