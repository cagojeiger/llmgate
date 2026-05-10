# ADR 006: PR 검증은 cassette e2e로 벤더 의존을 끊는다

- Status: Accepted
- Date: 2026-05-08
- 관련: 000, 002, 003

## 문제

기존 `make e2e`는 빌드된 바이너리가 실제 opencode.ai를 호출하는 흐름이다. PR마다 돌리면 비용이 쌓이고, 벤더의 변동 때문에 결과가 비결정적이다. 키 만료나 벤더 장애처럼 코드와 무관한 이유로 CI가 빨갛게 뜨기도 한다. 게다가 `rate_limit`, content filter, 스트리밍 도중 오류, 폴백 체인, 회로 차단처럼 자연 발생하기 어려운 시나리오는 벤더가 만들어 주지 않으니 사실상 검증 경로가 없는 셈이다.

## 결정

**PR 검증은 cassette 모드로 한다. 라이브 호출은 nightly나 수동에만 둔다.**

- `make e2e-mock` (cassette) — PR마다 무료로 돈다. 결정론적.
- `make e2e` (라이브) — 벤더의 와이어 변화를 잡는다. nightly나 수동.
- **카탈로그가 source of truth.** fixture는 카탈로그에 등록된 모델 ID 집합을 따라간다.
- `tests/e2e/fixtures/models/<id>/chat-completion.{json,sse}`에 벤더 응답을 그대로 캡쳐한다. OpenAI 프로토콜 모델은 OpenAI 와이어 그대로, Anthropic 프로토콜 모델은 Anthropic 와이어 그대로 — 게이트웨이의 디코딩과 번역 경로가 실제 그대로 굴러간다.
- `LLMGATE_E2E_MODE=cassette`일 때 conftest가 로컬 cassette HTTP 서버를 띄우고, 임시 카탈로그 YAML의 `base_url`만 cassette URL로 덮어 바이너리를 부팅한다.

### 경계선

- **벤더 키가 없어도 동작한다.** 바이너리에 더미 값이 들어가도 cassette는 키를 보지 않는다.
- **자동 record는 채택하지 않는다** — 첫 호출의 비용과 캡쳐 시점은 운영자가 정한다. `scripts/refresh-fixtures.sh`는 카탈로그와 fixture의 차이만 알려 주고, 실제 record나 삭제는 사람이 한다.
- **fixture가 낡을 수 있다.** 벤더가 와이어를 바꾸면 다시 캡쳐할 때까지 잡지 못한다 — `make e2e`를 nightly로 유지해 보완한다.
- **스트리밍은 지원, 도구 호출(tool calls)·멀티턴 시나리오·`X-Test-Scenario` 분기는 미지원** — 시나리오 키 설계가 필요한 별도 작업이다.

## 근거

- 벤더 의존을 PR 경로 밖으로 빼면 PR 검증이 무료가 되고 벤더의 흔들림에 영향받지 않는다 — [ADR 000](000-identity.md)의 "작은 실행기" 정체성을 *외부 의존*까지 작게 유지하는 표현.
- fixture를 그대로 캡쳐하면 게이트웨이의 디코딩·번역 경로가 라이브와 같은 코드로 굴러간다.

## 결과

- 두 모드로 나뉘면서 conftest 분기가 늘어난다. 분기해야 할 코드가 조금 늘어나지만, PR마다 무료로 e2e가 돈다는 가치가 그 비용을 갚는다.
- **fixture 규약** (테스트 코드에 하드코딩 0):
  - fixture 경로 = `tests/e2e/fixtures/models/<카탈로그-모델-ID>/chat-completion.{json,sse}`. 카탈로그의 모델 ID가 그대로 키.
  - cassette는 경로를 보지 않는다 — 요청 본문의 `model` 필드와 `stream` 플래그만 보고 어떤 fixture를 쓸지 결정한다.
  - 테스트는 `discover_models_by_protocol()`로 카탈로그에서 모델을 자동으로 발견한다. 새 모델은 YAML 추가 + record면 끝, 테스트 코드는 한 줄도 안 바뀐다.
  - cassette 모드에서 fixture가 없는 모델은 `_skip_if_no_cassette_fixture` 자동 fixture가 자동 skip 처리한다.
