# ADR 007: 결과 이벤트 본문 정책은 consumer 단위 capture 모드로 정의한다

- Status: Accepted
- Date: 2026-05-26
- 관련: 003, 005

## 문제

결과 이벤트는 현재 request의 `messages`와 response의 `choices`를 그대로 브로커에 발행한다.
프롬프트와 응답이 운영자 손에 닿는 보관소까지 흘러가므로 컨슈머가 PII나 키를 본문에
담는 순간 그대로 누출된다. 반대로 컨슈머 입장에서는 재현·재처리·품질 분석을 위해
본문이 필수일 때가 있어서 "그냥 끈다"는 답도 안전망이 아니다.

스트리밍 경로(`assembly/stream_response.go`)는 모든 chunk를 메모리에 누적해 결과
이벤트를 만든다. 본문 정책이 없으면 누적 크기와 truncate 규칙도 정할 근거가 없고,
브로커의 페이로드 상한을 넘기는 순간 손실이 일어난다.

세 가지 시나리오가 운영 안에 공존한다.
1. 본문 없이 메타데이터·usage만 필요한 컨슈머 (감사·과금).
2. 본문이 평문으로 필요한 컨슈머 (현재 동작, 분석·재처리).
3. 본문이 필요하지만 브로커 보관자에게는 가려야 하는 컨슈머 (정책·규제).

## 결정

**본문 발행은 consumer 단위 `body_capture` 모드로 정의한다. v1은 `plain` 모드만 구현하고
`omit`과 `encrypted`는 같은 schema 자리를 예약만 둔다.**

- `consumers/<name>.yaml`에 신규 필드:
  ```yaml
  body_capture: plain   # 기본값 (current behavior)
  # 향후: omit | encrypted
  ```
- 모드별 event payload 형태:

  | 모드 | request / response | 추가 필드 |
  | --- | --- | --- |
  | `plain` (구현) | 원본 그대로 | — |
  | `omit` (예약) | `null` | `body_capture: "omit"` |
  | `encrypted` (예약) | `null` | `request_ciphertext` / `response_ciphertext` (base64), `body_encryption: "<scheme>"`, `body_recipients_hint: [...]` |

- 기본값은 `plain` — 이번 PR로 깨질 컨슈머가 없게 한다. `omit`로 옮길지는 컨슈머가
  명시적으로 yaml을 바꿔야 일어난다.
- 스트리밍은 본문 capture가 켜진 모드(`plain`)에서만 누적한다. 누적 상한은 별도 PR에서
  per-field byte cap + `request_truncated` / `response_truncated` flag로 도입한다.
- `omit`·`encrypted` 모드는 schema 자리를 미리 잡아 둔다 — consumer는 v1 시점부터
  `body_capture` 필드를 보고 분기하도록 코드를 짜 두면, 다음 단계에서 게이트웨이만
  업그레이드해도 새 모드를 받을 수 있다.

### 경계선

- **암호화 방식 / 키 관리는 미정.** 알고리즘과 키 분배 모델은 `encrypted` 모드 구현
  시점에 별도 ADR로 결정한다.
- **메시지 dedup 키 정책은 이 ADR의 범위 밖.** 클라이언트 제어 `X-Request-Id`가 그대로
  브로커 dedup 키로 쓰이는 문제는 별도 변경에서 server-generated ID로 분리한다.
- **per-model·per-request override는 도입하지 않는다.** 정책 표면이 늘어나 감사하기
  어려워진다. 모델별로 본문 정책이 다른 경우라면 그 모델을 부르는 컨슈머를 별도로 등록한다.
- **vendor 4xx error body·attempts.Error는 정책 대상이 아니다.** 메타데이터로 분류한다.

## 근거

- 본문은 컨슈머가 만들어 넣는 데이터이므로, 본문 발행 여부도 컨슈머 단위로 결정하는
  것이 책임 경계와 일치한다 — [ADR 003](003-consumers.md)이 정한 "컨슈머 = 데이터 정책의
  단위" 원칙의 연장.
- v1을 `plain`만 구현해도 schema에 모드 필드를 박아두면, 후속 PR에서 컨슈머 yaml 한 줄만
  바꿔 발행 동작을 끊거나 암호화로 옮길 수 있다. 단계적 변경이 가능해야 운영이 멈추지
  않는다.

## 결과

- `consumers/<name>.yaml`에 옵셔널 필드 1개 추가. 부재 시 `plain`으로 해석해 기존 동작을
  유지한다 — 컨슈머 yaml 갱신 없이 부팅 가능.
- `Event` schema에 `body_capture` 필드를 새로 둔다. 다운스트림은 이 값을 보고 본문 존재
  여부와 형태를 알 수 있다.
- 누적 상한·truncate 마커·dedup 키 분리는 후속 PR로 이어진다. 이 ADR은 그 PR들이 어떤
  policy 표면 위에서 동작할지를 고정한다.
