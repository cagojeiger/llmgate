# Logs

게이트웨이는 구조화 JSON 로그를 stdout 으로 내보낸다. DB 없이 운영하므로 Loki / ELK 같은
외부 sink 가 운영 사실 원장이 된다.

정확한 필드 목록은 코드가 source of truth 다.

- `access`: HTTP 전송 사실. probes 와 `/metrics` 는 business traffic 이 아니므로 제외한다.
  법정 접속기록을 대체하지 않는 보조 필드로 `remote_addr`, `user_agent`, `request_id`를 남긴다.
- `audit`: 운영 / 보안 사실. chat 요청은 인증 실패를 포함해 한 줄을 남긴다.
- `call`: upstream LLM 호출 사실. vendor 호출이 시도된 요청에만 남긴다.

## Policy

`access`, `audit`, `call` 은 같은 요청을 다른 관점에서 본 기록이다. 세 줄기는 `request_id` 로
조인할 수 있지만, 한 로그 줄이 모든 사실을 담으려고 하지 않는다.

`audit` 은 실패해도 남긴다. 특히 인증 실패처럼 호출자를 특정할 수 없는 요청도 운영자가
거부 사실을 볼 수 있어야 한다.

`call` 은 vendor 호출 이후의 사실만 맡는다. 인증 실패, 요청 decode 실패, consumer allowlist
거부처럼 gateway 안에서 끝난 요청은 `audit` 에 남고 `call` 에는 남지 않는다.

로그 schema 를 후처리 시스템이 의존할 수 있으므로, 의미가 바뀌는 필드 변경은 schema version
경계로 다룬다. 새 필드는 기존 의미를 깨지 않는 방향으로 추가한다.

## Sensitive Data

로그에는 다음을 남기지 않는다.

```text
Authorization header
raw consumer key
vendor API key
prompt / message body
response body
tool payload
raw client prompt / completion text
raw error message that may contain upstream internals
```

호출자 식별은 이름과 짧은 key id 로 충분하다. 원문 키와 요청 / 응답 본문은 로그가 아니라
보안 통제된 별도 저장소나 추적 계층이 필요할 때만 다룬다.

`remote_addr`와 `user_agent`는 접속기록 보조 증적에 쓸 수 있는 운영 필드다. trusted proxy 경계와 보관기간은
[security operations baseline](security/02-operations.md)에 맞춘다.

## Source Of Truth

- access log field: `internal/platform/http/middleware`
- audit / call event shape: `internal/domain/telemetry`
- stdout JSON sink: `internal/platform/telemetry/slog`
- auth / policy denial behavior: `internal/platform/http/auth`, `internal/platform/http/chat`
