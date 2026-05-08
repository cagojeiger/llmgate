# ADR 001: 컴포넌트 책임 경계 — 한 머리에 들어오는 단위

- Status: Accepted
- Date: 2026-05-03
- 관련: 004, 005

## 문제

한 컴포넌트가 chain 진행 + 폴백 적격 + 회로 + 첫 이벤트 검증 + 시간 한도 + record 채우기까지 들면, 한 책임의 변경이 다른 책임의 invariant 를 모르고 통과한다. *한 사람 머리에 들어오는 작은 컴포넌트* 가 코드 단위에서 깨진다.

## 결정

각 컴포넌트는 *유일한 책임*. 같은 결정엔 권위자 1 개.

- **Handler** — HTTP 시맨틱 경계, stream / non-stream 분기. 요청 총 wall-clock 한도 권위자 (ADR 005).
- **llmrouter.Service** — 별명 chain → 후보, 폴백 적격성 + 회로 차단 (ADR 004). non-stream 시도당 한도 권위자 (ADR 005).
- **Adapter** (`internal/providers/{openai,anthropic}`) — 한 vendor 의 와이어 도메인. status 분류 + 첫 이벤트 검증.
- **streamRelay** — 스트림 열린 *이후* SSE transcript. idle / cancel / 종결. 스트림 idle 한도 권위자 (ADR 005).
- **Audit Recorder** — 요청당 사실 1 줄.

## 이유

- 변경 영역이 컴포넌트 단위로 끝남 — idle 변경은 streamRelay 만, 폴백 적격성은 Service 만, 새 vendor 는 Adapter 1 개.
- 책임이 두 컴포넌트에 새는 신호가 PR diff 로 즉시 보임 — 두 컴포넌트 동시 변경 = 경계 의심.
- pipeline / middleware 추상은 *그 추상의 invariant* 를 또 책임지게 함. 우리 규모엔 직접 분리가 우월.

## 대가

- 단일 호출이 컴포넌트 여럿 거침 → trace 수직 길이 ↑. 책임 명확한 trace 가 짧지만 모호한 trace 보다 디버깅 친화.
- 작은 변경도 책임 경계를 한 번 묻게 됨 — review 비용 < invariant 깨짐 비용.
