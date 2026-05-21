# ADR 003: 호출자 인증은 consumers 디렉토리와 해시 키로만 처리한다

- Status: Accepted
- Date: 2026-05-03
- 관련: 000, 001, 002

## 문제

감사 기록(audit record)에 *누가* 호출했는지가 비어 있다. 외부 IdP나 프록시에 기대지 않고, 게이트웨이 자체의 무상태(stateless) 원칙 안에서 호출자를 식별해야 한다. 그리고 호출자 명단과 벤더 카탈로그는 권한, 마운트 경로, 변경 주기가 다르다 — 쿠버네티스에서 Secret과 ConfigMap을 나누는 이유, PostgreSQL이 `postgresql.conf`와 `pg_hba.conf`를 나누는 이유와 같다.

## 결정

**호출자는 Bearer 키 하나만 보낸다. 게이트웨이는 그 키의 해시만 저장한다.**

- `consumers/`는 `catalog/`와 형제(sibling) 디렉토리. 마운트 경로도 따로 받는다 (`LLMGATE_CONSUMERS=<dir>`). 비어 있으면 부팅 실패.
- `consumers/<name>.yaml` = `name` + `key_hashes` + optional `allowed_aliases`. 키 해시(sha256)만 저장하고, 평문 키는 두지 않는다.
- 키 회전은 다중 키 배열로 처리: 새 해시 추가 → 배포 → 옛 해시 제거.
- 인증 헤더는 `Authorization: Bearer <key>` — OpenAI SDK 관례를 그대로 따른다.
- 인증에 실패해도 감사 기록을 남긴다 (`consumer_name`은 비우고, `error_kind=auth`, `status=401`, `auth_error=missing|format|unknown`).
- 감사 레코드에 `consumer_name`과 `consumer_key_id`(해시 앞 8자)를 추가한다.
- `allowed_aliases` 가 비어 있으면 unrestricted, 값이 있으면 요청 `model` 이 그 목록에 있어야 한다. 거부 시 `deny_reason=model_not_allowed`.
- 이름 규칙은 `^[a-z0-9][a-z0-9_-]{0,63}$`. 한 번 정해진 이름은 영구 식별자로 둔다.

### 경계선

- **외부 IdP / OAuth / JWT 거부.** [ADR 000](000-identity.md)의 "작은 실행기" 정체성에 따라, 외부 신원 시스템에 의존하지 않는다.
- **한도(rate limit)나 quota 는 두지 않는다** — 후처리 단계의 책임. 게이트웨이는 호출자 식별과 coarse model allowlist 까지만 맡는다.
- **이름 재사용 금지는 운영자 책임이다.** 게이트웨이가 강제하지 않는다.
- **엄격한 파싱** — 스키마에 없는 필드가 있으면 부팅 실패.

## 근거

- 감사 기록의 *누가*가 채워진다. 한 줄만 봐도 호출자, 오류 종류, 상태 코드 같은 필요한 사실이 함께 갖춰진다.
- 운영자의 머릿속 모델이 [ADR 002](002-catalog-shape.md) 카탈로그와 같은 모양이라, 새로 익힐 게 거의 없다.
- 자격증명에 가까운 데이터가 벤더 카탈로그와 떨어져 있어, 권한과 마운트가 자연스럽게 분리된다.
- 외부 IdP나 Vault에 의존하지 않는다. 키 회전은 YAML 수정 + 배포로 끝난다.

## 결과

- 평문 키를 잃어버리면 영구 분실 — 해시는 되돌릴 수 없다. 운영자 책임.
- 호출자가 수만 단위가 되면 YAML 디렉토리가 비대해진다. 내부 운영의 가정 규모(수십~수백)에서는 문제 없다.
- 비어 있으면 부팅 실패라는 잠금형 디폴트 — 잠긴 채로 실패하는 편이, 잠금이 풀린 채로 사고 나는 것보다 안전하다.
