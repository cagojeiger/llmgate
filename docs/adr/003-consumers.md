# ADR 003: 호출자 — `consumers/` sibling, 해시만, 닫힘 default

- Status: Accepted
- Date: 2026-05-03
- 관련: 001, 002

## 문제

audit record 의 *누가* 가 비어있다. 외부 IdP / proxy 가정 없이 상태 없음 정신 안에서 호출자 인증이 필요하다. caller roster 와 vendor catalog 는 권한 / mount / 변경 주기가 다르다 (k8s Secret vs ConfigMap, postgresql.conf vs pg_hba.conf 패턴).

## 결정

- `consumers/` 는 `catalog/` sibling. mount 따로 (`LLMGATE_CONSUMERS=<dir>`). 0 개면 부팅 fail.
- `consumers/<name>.yaml` = `name` + `key_hashes` (sha256 만, raw 키 없음).
- multi-key array — 회전은 새 해시 추가 → deploy → 옛 해시 제거.
- 인증: `Authorization: Bearer <key>`. OpenAI SDK 컨벤션.
- 인증 실패도 audit emit (`consumer_name` omitted, `error_kind=auth`, `status_code=401`, `auth_error=missing|format|unknown`).
- audit Record 에 `consumer_name` + `consumer_key_id` (해시 앞 8자) 추가.
- 이름 규칙 `^[a-z0-9][a-z0-9_-]{0,63}$`. 영구 식별자 — 재사용 금지 (운영자 책임).
- 권한 / 한도 필드 없음 — 후처리 책임.
- strict 파싱.

## 이유

- audit 의 *누가* 가 채워짐 — *사실 발행* 이 *완전한 사실* 로 닫힘.
- 운영자 mental model = ADR 002 catalog 와 동형. 새 인지 부담 없음.
- credential-adjacent 데이터가 vendor catalog 와 분리 → 권한 / mount 분리 자연.
- 외부 IdP / Vault 의존성 없음. 키 회전 = yaml + deploy.

## 대가

- raw 키 분실 = 영구 분실 (해시 비가역). 운영자 책임.
- 호출자 수만 단위면 yaml 디렉토리 비대 — V1 가설 (수십~수백) 안에서 OK.
- 닫힘 default → 빈 consumers 부팅 fail. 잠긴 fail 이 잠금 풀린 사고보다 안전.
