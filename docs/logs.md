# 로그 세 줄기 (access / audit / call)

게이트웨이는 세 종류의 구조화 로그를 같은 stdout 으로 흘린다 — 같은 요청에서 나온
사실이지만, 목적이 다른 증적은 `log` attr 로 갈래를 분리한다. DB 없이 운영하므로 stdout
JSON 로그가 Loki / ELK 같은 외부 sink 에서 검색 가능한 사실 원장이 된다.

```
log=access  → HTTP 전송 사실 (프로브 / metrics 제외 business 경로, 한 줄/요청)
log=audit   → 운영 / 보안 fact (chat 요청 한정, auth 실패 포함 한 줄/요청)
log=call    → LLM 호출 결과 fact (LLM 호출이 시도된 요청만 한 줄)
```

`audit` / `call` 은 Handler 가 `telemetry.EventSink` 로 emit 한 finalized event 를
`platform/telemetry/slog` sink 가 stdout JSON 으로 라우팅한 결과다.

## 키 스키마

| 키 | access | audit | call | 의미 |
|---|---|---|---|---|
| `request_id` | ✓ | ✓ | ✓ | 세 줄기를 묶는 조인 키 |
| `schema_version` | — | ✓ | ✓ | 로그 스키마 버전. 후처리 호환성 경계 |
| `event_type` | — | ✓ | ✓ | `audit` / `call` |
| `service_name` / `service_version` / `environment` | — | ✓ | ✓ | 어떤 배포 단위가 낸 증적인지 구분 |
| `operation` | — | ✓ | ✓ | 도메인 호출 (`chat.completions` / `chat.completions.stream`) |
| `method` | ✓ | — | — | HTTP method (`POST`, `GET`) |
| `path` | ✓ | — | — | URL path |
| `status` | ✓ | ✓ | ✓ | HTTP status |
| `duration_ms` | ✓ | ✓ | ✓ | 요청 wall-clock |
| `bytes_out` | ✓ | — | — | HTTP response body bytes |
| `consumer_name` | ✓ | ✓ (성공시만) | ✓ (성공시만) | 호출자 이름. access 는 실패/비인증 경로에서 빈 문자열 가능, audit/call 은 auth 실패시 omit |
| `consumer_key_id` | — | ✓ (성공시만) | ✓ (성공시만) | 매칭된 키의 sha256 앞 8자 |
| `auth_result` | — | ✓ | — | `success` / `failure` |
| `auth_error` | ✓ (실패시만) | ✓ (실패시만) | — | `missing` / `format` / `unknown` |
| `policy_result` / `deny_reason` | — | ✓ | — | gateway 정책 판정. 예: `denied` + `model_not_allowed` |
| `resource_type` / `resource_id` | — | ✓ | — | 정책 대상. 현재 `llm_model` + 요청 model / alias |
| `error_kind` | — | ✓ (실패시만) | ✓ (실패시만) | `llmtypes.ErrorKind` 값 |
| `model_requested` | — | — | ✓ | 호출자가 보낸 alias / model |
| `model_used` | — | — | ✓ | 실제 성공 또는 최종 시도 model. 요청값과 같으면 omit 가능 |
| `vendor` | — | — | ✓ | 실제 성공 또는 최종 시도 vendor |
| `attempts_count` | — | — | ✓ | vendor 시도 횟수. fallback 탐지의 우선 키 |
| `final_attempt_vendor` / `final_attempt_model` / `final_attempt_status` | — | — | ✓ | 마지막 vendor 시도 결과. `attempts` 생략 시에도 검색 가능 |
| `final_attempt_error_kind` | — | — | ✓ (실패시만) | 마지막 시도의 실패 분류 |
| `request_bytes` / `response_bytes` | — | — | ✓ | 게이트웨이 관측 바이트 수 |
| `prompt_tokens` / `completion_tokens` / `total_tokens` | — | — | ✓ | vendor usage 값. 없으면 omit |
| `vendor_cost` | — | — | ✓ | vendor 가 준 비용 값. 게이트웨이는 계산하지 않음 |
| `attempts` | — | — | ✓ | fallback chain 이 실제로 2회 이상 시도된 경우만 기록 |

`method` 와 `operation` 은 별도 키다. `log=access|audit|call` 로 필터한 뒤 `request_id` 로
조인하면 한 요청의 HTTP 사실, 보안/정책 사실, vendor 호출 사실을 묶어 볼 수 있다.

## audit-always 보장

인증 실패 (401) 도 audit emit 한다. `consumer_name` 은 omit 되지만 `auth_result=failure`,
`auth_error`, `policy_result=denied`, `deny_reason=auth`, `error_kind=auth`, `status=401` 은
남긴다. 결정 근거 [ADR 003](adr/003-consumers.md).

`call` 은 LLM 호출이 시도된 요청에만 emit 한다. auth 실패, JSON decode 실패, consumer
allowlist 거부처럼 vendor 호출 전 끝난 요청은 `audit` 에만 남긴다.

## 운영 점검 시작점

```
log=audit auth_result=failure
log=audit policy_result=denied deny_reason=model_not_allowed
log=audit status>=500
log=call error_kind=timeout
log=call error_kind=upstream
log=call attempts_count>1
log=call final_attempt_status>=500
```

새 필드는 의미가 바뀌지 않는 한 제거하지 않고, 의미가 바뀌면 `schema_version` 을 올린다.

## 민감정보 경계

로그에는 다음을 남기지 않는다.

```
Authorization header
raw consumer key
vendor API key
prompt / message body
response body
tool payload
raw client prompt / completion text
```

호출자 식별은 `consumer_name` 과 `consumer_key_id` 로 충분하다. `consumer_key_id` 는 매칭된
sha256 해시의 앞 8자라 키 회전 중 어떤 키가 쓰였는지 구분할 수 있지만, 원문 키를 복구할 수
없다.
