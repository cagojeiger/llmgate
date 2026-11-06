# ADR 014: Soniox 실시간 세션 원문 기록

- Status: Accepted
- Date: 2026-11-07
- 관련: [결과 기록](007-audit-result-sink.md), [감사 파이프라인](009-audit-scale-hardening.md)

Soniox의 실시간 WebSocket은 바이너리 오디오와 고유한 전사 이벤트를 사용한다.
서버 및 CLI 호출자는 Soniox 메시지 형식으로 `GET /v1/soniox/realtime`에
연결할 수 있다. 이 경로는 명시적으로 켜야 하며, 호출자 인증에는 llmgate
Bearer 키와 기존 모델 허용 목록을 사용한다. 모델은 `stt-rt-v6`이다.
게이트웨이는 공급자 키를 업스트림 연결 헤더에만 설정한다.

운영 진단에서 원래 오디오와 전사 이벤트를 확인할 수 있도록 세션별로
별도 JSONL 파일을 쓴다. 요청과 응답 방향, WebSocket 메시지 종류, 순서와
경과 시간, 원본 바이트를 기록한다. 바이너리 오디오와 JSON 모두 Base64로
기록해 바이트를 복원할 수 있다. 인증 헤더는 저장 대상에서 제외한다.
공급자에 보낸 시작 설정, 오디오, 제어 메시지, Soniox가 반환한 잠정·확정
토큰, 오류와 종료 메시지를 담는다.

게이트웨이는 호출자의 종료 프레임에 이어 Soniox가 `finished: true`를
보내고 캡처 파일 봉인이 성공해야 완료를 알린다. S3를 설정한 경우에는
업로드 성공도 확인한다. 임시 파드 볼륨을 잃어도 이미 완료를 알린
세션의 원문은 오브젝트 저장소에서 복구할 수 있다. 확인 시간 안에
업로드를 확인하지 못하면 호출자에게 저장 오류를 반환하고, 봉인된
파일의 업로드는 기존 정책에 따라 재시도한다.

연결 중단이나 저장 한도에 걸린 파일은 불완전으로 표시한다. 저장이
불가능하면 새 세션을 거부하거나 진행 중인 세션을 종료한다. 세션 단위
요약은 기존 감사 이벤트에 남기고, 원문 파일은 별도 경로와 객체 접두사에
둔다.

이 파일의 목적은 앱 운영과 Soniox 연동 확인이다. 공개 약관의 모델 학습
제한은 이 기록에도 적용된다.

[Soniox WebSocket 명세](https://docs.soniox.com/api-reference/stt/websocket-api)
· [Soniox 공개 약관 §7](https://soniox.com/policies/terms-of-service#7-no-model-training-or-competitive-use)
