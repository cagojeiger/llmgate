# ADR 013: TypeSafe Jev의 판단 API

- Status: Accepted
- Date: 2026-11-07
- 관련: [호출자 인증](003-consumers.md), [결과 기록](007-audit-result-sink.md)

TypeSafe Jev는 질문 이름으로 답을 반환한다. 호출자가 이름과 답의 타입을 그대로
사용할 수 있도록 게이트웨이는 `POST /v1/systemone`에서 JSON 계약을 유지한다.
처음 지원하는 모델은 `jev-1.14.0`이다. 호출자가 명시적으로 이 모델을 지정하면
게이트웨이는 요청을 TypeSafe에 한 번 전달한다.

클라이언트는 기존 llmgate Bearer 키로 인증한다. 호출자에게 `allowed_aliases`가
설정된 경우 `jev-1.14.0`이 목록에 있어야 한다. 업스트림 인증에는 게이트웨이의
`LLMGATE_TYPESAFE_API_KEY`를 사용한다.

게이트웨이는 요청마다 운영 감사 로그를 남긴다. 결과 싱크가 켜진 환경에서는
공급자 호출을 시도한 요청을 기존 `llm.result.finalized` 이벤트에 기록한다.
`decision_request`에는 모델, 상태, 질문을 담고, 성공한 호출은 `decision_response`에
공급자가 반환한 답과 사용량을 담는다. 호출 전 인증 또는 정책 거부는 운영 감사에
남는다. 요청 본문과 키는 운영 로그에 쓰지 않는다.

[TypeSafe System One API](https://docs.typesafe.ai/api/system-one)
