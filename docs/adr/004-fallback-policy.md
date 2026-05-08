# ADR 004: 폴백 정책 — 적격성, 회로 차단, 스트림 경계

- Status: Accepted
- Date: 2026-05-03
- 관련: 001, 005

## 문제

별명 chain 으로 폴백 가능성을 열었지만 *언제 다음 후보로 가는가*, *반복 실패 모델을 어떻게 잠재울 것인가*, *스트리밍에서 어디까지 폴백 가능한가* 가 결정으로 남았다. 모든 에러를 적격으로 두면 키 실수가 chain 끝까지 끌리고, 스트림 폴백 경계가 흐리면 mid-stream 폴백이 record 무결성과 SDK 호환을 동시에 깬다.

## 결정

**적격성**:
- 일시 실패 (`rate_limit`, `upstream` 5xx, `timeout`, `network`) 만 chain 진행. `bad_request` / `auth` 는 즉시 종결.
- `auth` 는 디폴트 *제외* — 키 mis-config 의 swap-mask 위험.
- 운영자 디폴트 변경 가능 (`LLMGATE_FALLBACK_ON` env, ADR 002).

**회로 차단**:
- 연속 N 실패 → 차단. 차단 동안 후보에서 skip.
- cooldown + 지수 백오프 + 지터 + 상한 (cap).
- half-open probe 단계 *없음* — 차단 끝의 첫 호출이 자연 probe.
- 회로 상태는 프로세스 메모리. 인스턴스 간 공유 없음.

**스트림 폴백 경계**:
- 폴백 가능: status open + first event 단계. 검증은 어댑터 안.
- 폴백 *불가*: mid-stream — 첫 chunk 송출 후 vendor 교체 거부.
- 거부 근거 셋: HTTP 시맨틱 (200 OK + partial body 송출됨), SDK 호환 (호출자는 한 모델 답으로 가정), record 무결성 (모델 X·Y 토큰 봉합 = 평가·단가·디버깅 부서짐).
- mid-stream 실패는 SSE 에러 이벤트 + 종결 신호 — 받은 chunk 그대로.

## 이유

- 영구 실패는 첫 후보에서 즉시 종결 → 호출자 fast fail. 일시 실패만 chain 끝까지.
- 회로 동작이 "cooldown 한 개" 로 설명됨. 모델 상태가 둘 (열림 / 닫힘).
- 첫 이벤트 검증이 어댑터 도메인 → 새 vendor 추가 시 검증 로직도 어댑터 내부. Service 는 변경 영역 밖.

## 대가

- `auth` 제외 디폴트 → 운영자 의도적 활성화 필요. 디폴트가 안전 쪽.
- half-open 미도입 → 차단 끝 첫 호출이 *운 나쁜 1 회* 로 다시 차단 가능.
- 회로 상태 인스턴스별 → 다중 인스턴스에서 vendor 부담이 인스턴스 수에 비례.
- 첫 이벤트 직후 끊김 → 호출자에 거의 빈 응답. 1 토큰 송출 = substitute 불가, 의도된 결과.
