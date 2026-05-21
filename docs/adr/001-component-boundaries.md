# ADR 001: 컴포넌트마다 책임 권위자는 하나만 둔다

- Status: Accepted
- Date: 2026-05-03
- 관련: 000, 004, 005

## 문제

한 컴포넌트가 폴백 체인 진행, 적격성 판정, 회로 차단, 첫 이벤트 검증, 시간 한도, 감사 기록까지 모두 맡으면, 한 책임을 바꿀 때 다른 책임이 암묵적으로 기대하던 조건이 어느새 깨질 수 있다. 코드를 처음 읽는 사람도 이 컴포넌트가 어떤 단위인지 한눈에 잡지 못한다.

## 결정

**같은 결정에 대한 권위자는 한 컴포넌트만 둔다.** 모든 결정이 컴포넌트 하나로 좁혀지면, 변경 영역이 그 컴포넌트 안에서 끝난다.

- **Handler** — HTTP 의미 경계, 스트림 / 비스트림 분기. 요청 전체의 시간 한도(wall-clock)를 정한다 ([ADR 005](005-timeout-authority.md)).
- **routing.Service** — 별명을 후보 모델 체인으로 풀고, 폴백 적격성과 회로 차단을 판정한다 ([ADR 004](004-fallback-policy.md)). 비스트림에서 한 시도당 시간 한도도 여기서 정한다 ([ADR 005](005-timeout-authority.md)).
- **Adapter** (`internal/platform/providers/{openai,anthropic}`) — 한 벤더의 와이어 프로토콜을 다룬다. 응답 상태 분류와 첫 이벤트 검증이 여기 산다.
- **http/stream relay** — 스트림이 열린 뒤의 SSE 전송. 이벤트 사이 유휴 한도(idle), 취소, 종결을 담당한다 ([ADR 005](005-timeout-authority.md)).
- **Telemetry EventSink** — finalized `AuditEvent` / `CallEvent` delivery boundary. 기본 구현은 stdout JSON 이고, 원격 sink 는 이 경계 뒤에 붙는다.

### 경계선

- **두 컴포넌트가 한 PR에서 같이 바뀌면** 책임이 섞이고 있다는 신호다 — diff에서 바로 드러난다.
- **도메인 처리 파이프라인 추상은 두지 않는다.** 자기 자신의 가정을 또 책임지게 만들어, 우리 규모에서는 직접 분리가 더 단순하다. 단, telemetry delivery 는 요청 처리 결함 경계와 운영 sink 확장을 위해 `EventSink` 로 분리한다.

## 근거

- 변경 범위가 컴포넌트 안에서 끝난다. 유휴 한도는 http/stream relay만, 폴백 적격성은 Service만, 새 벤더는 Adapter 하나만 손대면 된다.
- 권위자 하나 원칙은 [ADR 000](000-identity.md)이 정한 "작은 실행기" 정체성을 코드 단위에서 표현한다 — 시스템이 작게 유지되려면 컴포넌트도 작아야 한다.

## 결과

- 한 호출이 여러 컴포넌트를 거치니 trace의 수직 길이가 늘어난다. 책임이 명확한 긴 trace가 모호한 짧은 trace보다 디버깅하기 쉽다.
- 작은 변경에도 책임 경계를 한 번 묻게 된다 — 리뷰 비용이 약간 늘지만, 가정이 모르는 새 깨지는 비용보다 작다.
