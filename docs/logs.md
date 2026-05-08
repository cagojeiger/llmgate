# 로그 두 줄기 (access / audit)

← [architecture.md](architecture.md) 로 돌아가기

게이트웨이는 두 종류의 구조화 로그를 같은 stdout 으로 흘린다 — 같은 핸들러를 공유하지만
`log` attr 로 갈래를 분리한다.

```
log=access  → HTTP 전송 사실 (모든 경로, 한 줄/요청)
log=audit   → 게이트웨이 도메인 fact (chat 요청 한정, ADR 003 audit-always)
```

## 키 스키마

| 키 | access | audit | 의미 |
|---|---|---|---|
| `request_id` | ✓ | ✓ | 두 줄을 묶는 조인 키 |
| `method` | ✓ | — | HTTP method (`POST`, `GET`) |
| `path` | ✓ | — | URL path |
| `operation` | — | ✓ | 도메인 호출 (`chat.completions` / `chat.completions.stream`) |
| `status` | ✓ | ✓ | HTTP status |
| `duration_ms` | ✓ | ✓ | 요청 wall-clock |
| `consumer_name` | ✓ (성공시만) | ✓ (성공시만) | 호출자 이름. auth 실패시 키 자체가 omit |
| `consumer_key_id` | — | ✓ (성공시만) | 매칭된 키의 sha256 앞 8자 |
| `auth_error` | ✓ (실패시만) | ✓ (실패시만) | `missing` / `format` / `unknown` |
| `error_kind` | — | ✓ (실패시만) | `llmtypes.ErrorKind` 값 |
| `vendor` / `model_used` / `usage` / `attempts` | — | ✓ | 라우팅 결과 |

핵심: `method` 와 `operation` 은 *별도 키*. 같은 stdout 에 섞여 있어도 의미 충돌 없음.
`Loki` / `ELK` 에서 `log=access` 와 `log=audit` 로 사전 필터한 다음 `request_id` 로 조인하면
"누가 / 어떤 키로 / 어떤 vendor 폴백을 거쳐 / 몇 ms 에 / 어떤 상태로 끝났나" 가 한
컴포지션 쿼리에 들어온다.

## audit-always 보장

인증 실패 (401) 도 audit emit 한다 — `consumer_name` 은 omit 되지만 `auth_error` /
`error_kind=auth` / `status=401` 은 박힌다. 결정 근거 [ADR 003](adr/003-consumers.md).
