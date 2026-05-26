# ADR 007: 결과 이벤트 본문 정책은 consumer 단위 capture 모드로 정의한다

- Status: Accepted
- Date: 2026-05-26
- 관련: 003, 005

## 문제

결과 이벤트의 request·response 본문에는 운영자가 통제할 수 없는 데이터가 들어간다.
컨슈머가 보낸 프롬프트와 모델이 만든 응답은 PII, 자격증명, 영업 비밀, 규제 대상
데이터를 자연스럽게 포함할 수 있고, 한 번 브로커 보관소에 들어가면 그 보관소를
다루는 모든 사람이 본다.

본문을 일률적으로 끄는 답도 안전망이 아니다. 어떤 컨슈머는 메타데이터·usage만으로
충분하지만, 다른 컨슈머는 재현·재처리·품질 분석을 위해 본문이 반드시 필요하고,
또 다른 컨슈머는 본문이 필요하지만 브로커 보관자에게는 가려야 한다. 본문 발행
여부는 게이트웨이 차원에서 결정할 일이 아니라 각 컨슈머의 정책이다.

스트리밍 경로도 같은 이유로 정책이 필요하다. 정책 없이는 누적 상한과 truncate
규칙이 임의로 결정되고, 결과적으로 브로커의 페이로드 상한을 넘기는 순간 손실이
일어난다.

## 결정

**본문 발행은 consumer 단위 `body_capture` 모드로 정의한다. 기본은 `omit`이며,
컨슈머가 명시적으로 옵션을 선언해야 본문이 발행된다.**

- `consumers/<name>.yaml`에 신규 필드:
  ```yaml
  body_capture: omit | plain | encrypted   # 기본: omit
  ```
- 모드별 event payload 형태:

  | 모드 | request / response | 추가 필드 |
  | --- | --- | --- |
  | `omit` | `null` | `body_capture: "omit"` |
  | `plain` | 원본 그대로 | `body_capture: "plain"` |
  | `encrypted` | `null` | `request_ciphertext` / `response_ciphertext` (base64), `body_encryption: "<scheme>"`, `body_recipients_hint: [...]`, `body_capture: "encrypted"` |

- 본문을 담는 모드(`plain`·`encrypted`)는 per-field byte cap을 적용하고, 초과 시
  `request_truncated` / `response_truncated` flag를 세운다. 스트리밍 누적도 같은 cap
  안에서 멈춘다.
- 다운스트림 컨슈머는 항상 `body_capture` 필드를 먼저 보고 본문 존재 여부와 형태를
  결정한다.

### 경계선

- **암호화 알고리즘 / 키 분배 모델은 `encrypted` 모드 구현 시점에 별도 ADR로
  결정한다.** 이 ADR은 그 모드가 들어갈 schema 자리만 정의한다.
- **메시지 dedup 키 정책은 이 ADR의 범위 밖.** 클라이언트 제어 `X-Request-Id`가
  그대로 브로커 dedup 키로 쓰이는 문제는 별도 변경에서 server-generated ID로 분리한다.
- **per-model·per-request override는 도입하지 않는다.** 정책 표면이 늘어나 감사하기
  어려워진다. 모델별로 본문 정책이 다른 경우라면 그 모델을 부르는 컨슈머를 별도로
  등록한다.
- **vendor 4xx error body·attempts.Error는 정책 대상이 아니다.** 메타데이터로 분류한다.

## 근거

- 본문은 컨슈머가 만들어 넣는 데이터이므로, 본문 발행 여부도 컨슈머 단위로 결정하는
  것이 책임 경계와 일치한다 — [ADR 003](003-consumers.md)이 정한 "컨슈머 = 데이터
  정책의 단위" 원칙의 연장.
- 기본을 `omit`으로 두는 것은 [ADR 003](003-consumers.md)이 채택한 "닫힘 default"
  원칙과 같은 형태다. 컨슈머가 명시하지 않으면 본문은 흘러나가지 않는다.
- `plain`·`encrypted`를 한 정책 표면 위에 함께 정의해 두면, 컨슈머가 한 줄을 바꿔서
  발행 동작을 끊거나 암호화로 옮길 수 있다. 정책이 미리 존재해야 보안 강화가 코드
  재설계 없이 가능하다.

## 결과

- `consumers/<name>.yaml`에 `body_capture` 필드를 둔다. 부재 시 `omit`. 본문이 필요한
  컨슈머는 yaml에 명시적으로 `plain` 또는 `encrypted`를 선언한다.
- `Event` schema에 `body_capture` 필드와 모드별 본문/암호문 자리를 잡는다. 다운스트림은
  이 값으로 분기한다.
- 누적 상한·truncate 마커·dedup 키 분리·암호화 방식은 후속 ADR/PR로 이어진다. 이
  ADR은 그 변경들이 어떤 policy 표면 위에서 동작할지를 고정한다.
