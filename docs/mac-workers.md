# Mac MLX 워커

| 경계 | 책임 |
| --- | --- |
| Go LLMGate | OpenAI 형식 API, consumer 인증·로그, worker publish JWT 발급 |
| Rust CLI | Keychain, profile 설치·시작·준비·Relay 공개·종료 |
| Python adapter | 모델별 입력 한도·추론·JSON/SSE, 공유 admission·메모리 관측 |
| RelayGate | local-first Binding 선택·Pipe 전송. SDK 0.5.1 의존 |

```text
등록: Mac CLI ── 기존 API 키 ──▶ LLMGate /v1/workers/token ── publish JWT
호출: OpenClaw → LLMGate → 서버 Caller Bridge → RelayGate → Mac MLX
```

같은 저장소의 `cli/`는 MacBook 전용 별도 Rust package다. Linux/vLLM CLI·실시간 STT·chat 모델은 포함하지 않는다.
같은 destination에 게시하는 모든 워커는 모델 revision·양자화·차원을 동일하게 유지한다.
embedding 요청별 새 연결, 파일 STT의 `new_connection_per_request` 설정으로 새 Pipe를 연다. 전달 후 자동 replay는 하지 않는다.

[CLI·제한·수명주기](../cli/README.md) · [서버 인증 설정](worker-registration.md) · [OpenClaw](../cli/docs/openclaw.md) · [배포](../cli/docs/distribution.md) · [검증](../cli/docs/validation-2026-09-16.md)
