# ADR 008: 호출자 등록 — `consumers/` sibling, 해시만 yaml, 닫힘 default

- Status: Accepted
- Date: 2026-05-03

## 배경

ADR 002 는 *catalog* 를 vendor-side 등록의 yaml 디렉토리로 박았다 (모델 / 별명). 이 ADR 은 게이트웨이의 반대편 — *누가 호출할 수 있는가* — 를 yaml 기반으로 등록하는 결정을 묶는다. ADR 001 은 audit record 를 *사실* 의 단위로 정했지만, 가장 핵심 차원인 *누가 호출했는가* 가 비어 있었다. 게이트웨이 앞단에 외부 IdP / proxy 가 항상 있다고 가정할 수 없고, ADR 001 의 *상태 없음* 정신 안에서 해결할 수 있는 가장 작은 단위가 yaml 기반 키 등록이다.

호출자를 모델 / 별명과 같은 `catalog/` 안에 둘지 — 다른 디렉토리로 분리할지가 첫 결정점. 모델 / 별명은 *제공 메뉴* 이고, 호출자는 *허락된 명단* 이다 — 두 카테고리가 사실상 다르다. 운영 면에서도 둘은 분리되는 경향이 있다. 모델 등록은 운영팀이 자주 손대는 *공개 가능* 정보, 호출자 등록은 보안팀이 가끔 손대는 *credential-adjacent* 정보. PostgreSQL 의 `postgresql.conf` 와 `pg_hba.conf` 분리, k8s ConfigMap 과 Secret 분리, nginx server config 와 password file 분리가 같은 자세다.

## 결정

**호출자는 `consumers/` 별도 디렉토리. 키는 sha256 해시만 yaml. 닫힘 default. audit 는 인증 실패도 발행.**

- **`consumers/` sibling** — `catalog/` 와 동등한 root. 단일 우산 안에 묶지 않는다. mount 도 따로 (`LLMGATE_CONSUMERS=<dir>`). vendor catalog 와 caller roster 가 다른 권한 / mount / 변경 주기로 운영될 여지를 처음부터 열어 둔다.
- **consumer = yaml 1 개** — `consumers/<name>.yaml` 파일 1 개가 호출자 1 개. 필드 minimal:
  - `name` — 운영자 라벨, 영구 식별자. yaml field 가 진실, 파일명은 일치 강제 (불일치 시 부팅 fail).
  - `key_hashes` — sha256 해시 배열. raw 키는 디스크에 없다. 운영자가 raw 키를 따로 보관할지는 게이트웨이 영역 밖.
  - 자유 메모는 yaml 코멘트 (ADR 002 와 동일).
- **multi-key array** — 한 호출자가 여러 활성 키를 동시에 가질 수 있다. 회전 시 새 해시 추가 → deploy → 옛 해시 제거. 두 키 동시 유효 구간이 회전 윈도우. *별도 회전 메커니즘은 만들지 않는다* — yaml + deploy 가 회전 도구.
- **인증은 OpenAI SDK 컨벤션** — `Authorization: Bearer <key>`. 호출자가 OpenAI SDK 를 그대로 쓰는 것이 ADR 001 *호환* 정신. 별도 X-API-Key / 커스텀 스킴 안 쓴다.
- **닫힘 default** — `LLMGATE_CONSUMERS` 미설정 + cwd 에 `./consumers` 없음 = 부팅 fail (ADR 002 의 catalog 패턴과 동일). 디렉토리 존재하지만 등록된 호출자 0 개 = 부팅 fail. 의도된 *완전 공개* 는 운영자가 의도적 행동 (예: 단일 `public.yaml` 등록 + 키 공유) 으로 표현한다.
- **audit 는 인증 실패도 발행** — 매치 실패 호출도 record 에 들어간다. `consumer_name=""`, `error_kind=auth`, `status_code=401`. brute-force / 잘못된 키 사용 시도가 *invisible* 하지 않게. ADR 001 *사실만 발행* 정신과 정확히 짝.
- **audit Record 에 consumer 필드 추가** — `consumer_name` (매치된 yaml name, 실패 시 ""), `consumer_key_id` (매치된 해시의 앞 8자, 회전 추적용; 풀 해시 / raw 키는 절대 로그 안 됨). 두 필드면 *누가 / 어떤 키로* 가 후처리에서 가능.
- **이름 규칙** `^[a-z0-9][a-z0-9_-]{0,63}$` — 모델 id / 별명 name 과 정합. lowercase 정규화, 슬래시 / 콜론 / 공백 / 점 거부 (audit 파싱 / 로그 grep 안전). 길이 64자 상한.
- **이름은 영구 식별자, 재사용 금지** — 폐기된 consumer name 을 새 호출자에 재배정하지 않는다. 게이트웨이는 검증하지 않는다 (상태 없음 — ADR 001) — 운영자 책임. 후처리 시스템이 consumer_name 으로 비용 / 호출량 집계하므로 재사용은 *옛 호출 + 새 호출* 을 한 consumer 로 합쳐 보게 한다 (별명 name 과 같은 자세).
- **권한 / 한도는 yaml 에 없다** — `allowed_aliases`, `rate_limit`, `quota` 같은 필드 안 둔다. ADR 003 의 *사전 한도 차단 미지원* + *후처리 책임* 정신. 권한 / 한도가 진짜 필요해지면 ADR 갱신.
- **strict 파싱 + 부팅 시 unique 검증** — 모르는 필드 → 부팅 fail. 같은 name 두 yaml → 부팅 fail. 빈 name / 규칙 위반 → 부팅 fail. 빈 `key_hashes` → 부팅 fail (의미 없는 entry). ADR 002 strict 정신 그대로.

## 결과

좋아지는 점:
- audit record 의 *누가* 차원이 채워진다 — ADR 001 의 *사실만 발행* 정신이 *완전한 사실* 로 닫힌다.
- 운영자 mental model 이 ADR 002 catalog 와 동형 — yaml 1 개 = 1 entity, strict, 메모는 코멘트, 정책은 안 박힘. 새 인지 부담 거의 없음.
- credential-adjacent 데이터가 vendor catalog 와 분리돼 있어, 운영 환경에서 권한 / mount 분리가 자연 (k8s Secret vs ConfigMap 패턴, postgresql.conf vs pg_hba.conf 패턴).
- 외부 IdP / Vault 의존성 없음. ADR 001 *상태 없음* 정신 유지.
- 키 회전이 *yaml + deploy* 만으로 가능 — 별도 회전 도구 / 별도 시스템 없음.

받아들이는 단점:
- raw 키 분실 = 영구 분실 (해시는 비가역). 운영자가 raw 발급 시 어떻게 다룰지 결정한다 — 게이트웨이는 관여하지 않는다.
- 호출자 수가 *수만* 단위로 늘면 yaml 디렉토리가 비대. V1 가설 (호출자가 *팀 / 서비스* 단위, 수십 ~ 수백 개) 안에서는 OK. 가설이 깨지면 외부 등록소로 옮길 시점.
- 닫힘 default 는 v1 → v2 업그레이드 시 호출자 등록 없이 부팅 시도하면 부팅 fail. 의도된 단점 — 잠금이 풀린 *사고* 보다 *잠긴 부팅 fail* 이 안전.
- multi-key array 가 yaml 안에 살아 *어떤 키 / 누가 / 언제 추가* 가 git 로그로 드러난다. 회전 추적엔 자연스럽지만 보안 추적 노출이 운영자 의식을 요구.

## 다른 선택지

- **`registry/` 단일 우산** — vendor catalog 와 caller roster 를 같은 무게의 *등록 entity* 로 평탄화. 운영자 mental model 에서 둘이 분리되는 자연을 흐리고, 권한 / mount 분리도 awkward. 폐기.
- **`catalog/consumers/` (catalog 안에 sibling)** — 의미 충돌 (consumers 는 catalog 가 아님). mount 한 군데로 묶이면서 권한 분리 awkward. 폐기.
- **raw 키 yaml** — catalog 가 시크릿 저장소가 됨. ADR 002 *yaml = 메타, env = 시크릿* 정신 위반. configmap mount 시 키 노출. 폐기.
- **env per consumer (`LLMGATE_CONSUMER_<name>_KEY`)** — 호출자 수 늘면 env 폭발. *등록* 카테고리에 env 를 쓰는 비대칭. 폐기.
- **외부 IdP / OIDC / JWT** — 게이트웨이가 외부 의존을 가짐. ADR 001 *상태 없음 + 단순* 정신 약함. V1 미적용 — 신호가 모이면 ADR 갱신.
- **권한 / 한도 yaml 필드 추가** — ADR 003 *후처리 책임* 정신 위반. V1 미적용.
- **인증 실패는 audit 안 함** — *invisible* brute-force 위험. ADR 001 *사실만 발행* 위반. 폐기.
- **열림 default (consumers 비어있으면 인증 면제)** — 사고 위험. 잠금 default 가 안전. 폐기.
- **consumer name 자동 ID 보강 (uuid)** — 이름 + 별도 영구 ID 분리. minimal 정신 위반, 별명 name 과 비대칭. V1 미적용.

## 운영 흐름

```
호출자 등록
  → consumers/<name>.yaml 작성
  → raw 키 생성 (운영자 도구) → sha256 해시 → key_hashes 에 추가
  → raw 는 호출자에 1 회 전달, 게이트웨이는 raw 안 봄
  → 다음 deploy 시 재시작 (ADR 001 의 "재시작 적용")

키 회전
  → consumers/<name>.yaml 의 key_hashes 에 새 해시 추가 → deploy
  → 두 키 동시 유효 구간에 호출자가 새 키로 전환
  → audit 의 consumer_key_id 로 옛 키 사용 잔량 추적
  → 옛 해시 제거 → deploy → 회전 완료

호출자 폐기
  → consumers/<name>.yaml 삭제 → deploy → 그 호출자 모든 키 무효
  → name 은 영구 — 재사용 금지 (운영자 책임)
```

게이트웨이는 raw 키 / 발급 시점 / 회전 도구를 모른다 — ADR 001 정신대로 *사실 발행자* 역할만 한다.
