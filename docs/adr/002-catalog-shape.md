# ADR 002: 카탈로그는 데이터, 모델은 등록 단위, 별명은 제어 단위

- Status: Accepted
- Date: 2026-05-02

## 배경

ADR 001 은 카탈로그를 "외부 파일" 로 묶어 놓고 끝냈다. 그러면 외부 파일이 정확히 어떤 모양인지 — 한 yaml 인지, 디렉토리인지, 모델 메타가 어디까지 들어가는지, 폴백 정책은 카탈로그 안인지 밖인지 — 가 다음 결정으로 남았다. 1차 구현은 vendor 단위 yaml 한 장에 endpoints / models / aliases / fallback 을 다 넣었는데, 그 모양은 (a) 모델 추가 PR 과 alias 변경 PR 이 같은 파일에서 충돌하고, (b) "fallback policy" 라는 이름으로 사실 세 카테고리(폴백 적격 에러, 회로 차단, 디폴트 모델)를 묶어버려서, (c) catalog 패키지가 데이터·로더·정책을 동시에 들고 있었다. 운영 의미와 코드 위치가 어긋난다는 신호였다.

## 결정

**카탈로그는 *데이터 디렉토리*. 모델은 *기본 등록*. 별명이 *실제 제어 단위*. 알고리즘 정책은 *카탈로그 밖*(env).**

- **데이터 위치 분리** — `catalog/` 는 운영자가 보는 yaml 디렉토리. `internal/catalog/` 는 그 yaml 을 읽는 로더. embed 는 안 한다 — 디폴트는 cwd 의 `./catalog`, 운영은 `LLMGATE_CATALOG=<dir>` 로 mount.
- **모델 = yaml 1 개** — `catalog/models/<id>.yaml` 한 파일에 6 개 필드만: `id` / `vendor` / `protocol` / `base_url` / `auth_env` / `auth_scheme`. `protocol` 은 wire 프로토콜 (`openai` | `anthropic`) 이다. 같은 vendor model name 을 다른 `auth_env` 로 두 번 등록해도 catalog 안에서 별개 모델이 된다.
- **별명이 폴백의 단위** — `catalog/aliases/<name>.yaml` 가 chain 을 정의한다. 클라이언트가 raw model id 로 호출하면 chain 길이 1 → 폴백 발동 자체가 없다. 폴백은 *alias 호출에만* 의미가 있고, 그래서 alias 가 실제 제어 단위다.
- **정책은 env** — 폴백 적격 에러 (`LLMGATE_FALLBACK_ON`), 회로 차단 (`LLMGATE_CIRCUIT_FAILURES` / `_OPEN_DURATION` / `_MAX_OPEN_DURATION` / `_JITTER`), 타임아웃 (`LLMGATE_REQUEST_TIMEOUT` / `_COMPLETE_TIMEOUT` / `_STREAM_IDLE_TIMEOUT`) 모두 env. 거의 바꾸지 않으므로 코드에 합리적 디폴트를 박고 env override 만 둔다. yaml 에는 정책이 살지 않는다. 전체 목록은 `docs/architecture.md` 의 정책 env 표.
- **모델 메타정보는 안 둠** — context window / capabilities / pricing 같은 필드는 yaml 에 적지 않는다. 게이트웨이가 그 정보를 쓸 *destination 이 없기 때문* (capability matching 안 함, `/v1/models` 노출 안 함). 진짜 신호 뜨면 외부 registry enrich 패턴으로 가지 yaml 에 박지 않는다.
- **schema 는 평탄(flat), 헤더 없음** — `apiVersion / kind / metadata / spec` 같은 k8s/CRD 모양을 흉내내지 않는다. catalog 는 *config 파일* 이지 *versioned API* 가 아니다 (PostgreSQL 의 `postgresql.conf` 와 동일 입장). 호환 깨는 변경이 실제 발생하는 시점에 `apiVersion` 헤더를 도입하고, 헤더 부재 = v1 으로 해석한다 — 그때까지는 YAGNI.
- **`description` 은 yaml 코멘트, 데이터 필드 아님** — 운영자 라벨 / 사용 의도 / 가격 메모 같은 자유 텍스트는 `# ...` 코멘트로 둔다. 게이트웨이가 description 을 읽지 않으므로 데이터 schema 에 둘 이유 없음. 미래 operator 가 CR 의 description 을 yaml 코멘트로 렌더하는 식의 통합도 같은 자리에서 자연스럽다.
- **strict 파싱** — yaml 에 모르는 필드가 있으면 부팅 실패. `type:` (옛 이름), `specs:`, `notes:` 같은 잔재나 오타가 무음으로 통과하지 않게 한다. schema 추가는 *코드 먼저, yaml 나중* 의 순서로 자연 강제된다.

## 결과

좋아지는 점:
- 모델 추가 = 파일 1 개. PR diff 가 명확하고 자동 sync 와 친화적이다 — 동시 PR 충돌이 사라진다.
- 운영자 시점이 단순해진다: yaml 은 *모델·별명*만, env 는 *정책*만, 코드는 *알고리즘*만.
- alias chain 이 vendor 경계를 자유롭게 넘는다. 별명 1 개가 여러 vendor 의 모델을 묶어 라우팅 단위가 된다.
- catalog 패키지가 "yaml → 데이터" 한 가지 책임만 가진다. 정책 / 디폴트 / 메타가 빠졌다.
- 같은 모델 다른 키를 두 yaml 로 두는 게 *부수 효과로* 가능하다 — 추가 코드 없이.

받아들이는 단점:
- 같은 vendor 의 14 개 모델이 base_url / auth_env 를 14 번 반복한다. 정규화 가능하지만 V1 엔 sync 도구가 자동 생성할 거라 사람이 손으로 쓸 일이 거의 없다.
- 별명 별로 다른 정책(예: alias 마다 다른 OnKinds)을 표현할 수 없다. 진짜 필요해질 때 alias yaml 안에 정책 override 를 추가하는 식으로 확장한다.
- `LLMGATE_CATALOG` 미설정 + cwd 에 ./catalog 없음 = 부팅 실패. binary 어디서 실행하든 catalog 위치를 알려야 한다.

## 다른 선택지 검토

- **vendor 단위 yaml 1 장** — 1 차 시도. 모델 / 별명 / 정책 동시 변경에 약하고 fallback 정책의 "글로벌" 의미가 흐려졌다. 폐기.
- **embed default + LLMGATE_CATALOG override** — binary 에 vendor 정보가 박혀 public repo 에 노출된다. 운영자가 LLMGATE_CATALOG 를 잊어도 묵묵히 디폴트가 떠서 실수 감지가 늦다. 폐기.
- **policy 도 yaml** — 거의 안 바뀌는 값을 굳이 yaml 에 두면 운영자가 매번 같은 값을 옮겨 적게 된다. 12-factor 상 env 가 자연스럽다. 폐기.
- **별도 repo (ai-model-list 식)** — vendor 1 개, client 1 개인 우리 규모엔 과대. registry 패턴은 다중 client 를 가정한다. V1 미적용.
- **모델 메타정보 포함** — destination 없음 (capability matching / models endpoint 없음). 늘릴 가치가 신호와 맞지 않는다. V1 미적용.

## 운영 흐름

```
vendor 가 새 모델 발표
  → catalog/models/<id>.yaml 추가 (PR 1 줄짜리 diff)
  → 필요하면 catalog/aliases/<name>.yaml 의 chain 에 추가
  → 다음 deploy 시 재시작 (ADR 001 의 "재시작 적용")

정책 튜닝 (드물게)
  → LLMGATE_CIRCUIT_FAILURES 등 env 변경
  → 재시작
```

게이트웨이는 yaml 출처 / env 결정자 / sync 자동화 도구를 모른다 — ADR 001 정신 그대로 *사실 발행자* 역할만 한다.
