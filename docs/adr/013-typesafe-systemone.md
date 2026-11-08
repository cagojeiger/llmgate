# ADR 013: TypeSafe Jev의 판단 API

- Status: Accepted
- Date: 2026-11-07
- 관련: [호출자 인증](003-consumers.md), [결과 기록](007-audit-result-sink.md)

TypeSafe Jev는 질문 이름으로 답을 반환한다. 호출자가 이름과 답의 타입을 그대로
사용할 수 있도록 게이트웨이는 `POST /v1/systemone`에서 TypeSafe 계약을 유지한다.
처음 지원하는 모델은 `jev-1.14.0`이다. 호출자가 명시적으로 이 모델을 지정하면
게이트웨이는 요청을 TypeSafe에 한 번 전달한다.

JSON의 인라인 이미지 `assets`와 파일을 첨부하는 `multipart/form-data`를 모두
받는다. 호출자가 `true`로 명시한 TypeSafe Images 및 Long Context 헤더를 전달한다.
gzip, x-gzip, zstd로 압축한 요청은 게이트웨이에서 풀며 압축 전후 모두
`LLMGATE_MAX_REQUEST_BYTES`(기본 10 MiB)와 TypeSafe의 32 MiB 상한을 적용한다.
이미지 활성화 요청, multipart, 압축 요청, 본문 길이를 알 수 없는 요청, 또는
1 MiB를 넘는 요청은 파드마다 한 건씩 처리한다. 자리가 없으면 본문을 읽거나
TypeSafe를 호출하기 전에 `429`와 `Retry-After: 1`을 반환한다.

클라이언트는 기존 llmgate Bearer 키로 인증한다. 호출자에게 `allowed_aliases`가
설정된 경우 `jev-1.14.0`이 목록에 있어야 한다. 업스트림 인증에는 게이트웨이의
`LLMGATE_TYPESAFE_API_KEY`를 사용한다.

게이트웨이는 요청마다 운영 감사 로그를 남긴다. 결과 싱크가 켜진 환경에서는
공급자 호출을 시도한 요청을 기존 `llm.result.finalized` 이벤트에 기록한다.
`decision_request`는 JSON 요청의 모델, 상태, 질문과 이미지 자산을 담는다.
multipart 요청에서는 업스트림으로 보낸 본문 전체를 `body_base64`에 기록하고
`content_type`에 boundary를 포함한다. `decision_request_headers`에는 전달한
기능 헤더만 기록한다. 성공한 호출은 `decision_response`에 공급자가 반환한
답과 사용량을 담는다. 호출 전 인증 또는 정책 거부는 운영 감사에 남는다.
요청 본문과 키는 운영 로그에 쓰지 않는다.

동시 처리 수가 제한된 요청은 결과 싱크가 활성화된 환경에서 로컬 JSONL
기록의 fsync를 완료한 뒤 `200`을 반환한다. 기록에 실패하면 `503`을
반환한다. 객체 스토리지에는 기존 시간·크기 기준 로테이션으로 업로드한다.

[TypeSafe System One API](https://docs.typesafe.ai/api/system-one)
