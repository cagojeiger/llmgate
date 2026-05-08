# ADR 006: Cassette-based e2e — vendor 의존을 PR 검증 경로 밖으로

- Status: Accepted
- Date: 2026-05-08
- 관련: 002, 003

## 문제

기존 `make e2e` 는 binary → 실제 opencode.ai 호출 흐름. PR 마다 돌리면 credit 누적, vendor 변동에 비결정적, key 만료 / vendor 다운에 CI 가 무관 사고로 죽음. 트리거 어려운 시나리오 (rate_limit / content_filter / mid-stream error / fallback chain / circuit breaker) 는 vendor 가 자연 발생을 안 줘서 검증 경로가 사실상 없음.

## 결정

`make e2e` (live) 와 별도로 `make e2e-mock` (cassette) 모드. PR 마다 cassette, vendor wire drift 감지는 nightly/manual `make e2e`.

- **카탈로그 = source of truth.** fixture 는 카탈로그 model id 집합을 뒤따른다.
- `tests/e2e/fixtures/models/<id>/chat-completion.{json,sse}` 에 vendor 응답 캡쳐. OpenAI-protocol 모델은 OpenAI 와이어 그대로, Anthropic-protocol 모델은 Anthropic 와이어 그대로 — 게이트웨이의 정상 디코딩 / 번역 경로가 그대로 굴러간다.
- `LLMGATE_E2E_MODE=cassette` 일 때 conftest 가 로컬 cassette HTTP 서버 띄우고, 임시 catalog yaml 의 `base_url` 만 cassette URL 로 덮어 binary 부팅.
- vendor key 없어도 동작 (binary 가 dummy 값을 받아도 cassette 는 무시).
- 자동 record 는 의도적 미채택 — 첫 호출 비용 + 캡쳐 시점 결정을 운영자에게 둠. `scripts/refresh-fixtures.sh` 는 catalog↔fixture 차이만 보고 record / delete 는 수동.

**Convention** (test 코드 hardcode 0 보장):

- fixture path = `tests/e2e/fixtures/models/<catalog-model-id>/chat-completion.{json,sse}`. 카탈로그 model id 가 키.
- cassette 는 path 안 봄 — request body 의 `model` 필드 + `stream` 플래그만 보고 fixture 결정.
- 테스트는 `discover_models_by_protocol()` 로 catalog 에서 자동 발견. 새 모델 추가 = yaml + record, 코드 0줄.
- cassette 모드에서 fixture 가 없는 모델은 autouse fixture (`_skip_if_no_cassette_fixture`) 가 자동 skip — matrix 가 깨지지 않음.

## 대가

- fixture stale — vendor 가 와이어를 바꾸면 재캡쳐 전까지 못 잡음. 완화: `make e2e` 를 nightly 로 유지.
- 두 모드 conftest 분기 — 작은 표면 증가지만 PR 무료 검증 가치가 갚는다.

## V1 미지원

streaming 은 지원 (이 ADR), tool calls / multi-turn 시나리오 fixture / `X-Test-Scenario` 분기는 별도 단계 — 시나리오 키 설계가 필요한 다른 작업.
