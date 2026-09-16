# Mac MLX 워커

| 경계 | 책임 |
| --- | --- |
| Go LLMGate | OpenAI 형식 API, consumer 인증·로그, worker publish JWT 발급 |
| Rust 서버 Caller | loopback HTTP → Pipe, exact dial JWT 자동 발급·SDK reconnect |
| Rust CLI | Keychain, profile 설치·시작·준비·Relay 공개·종료 |
| Python adapter | 모델별 입력 한도·추론·JSON/SSE, 모델별 admission·합산 메모리 관측 |
| RelayGate | local-first Binding 선택·Pipe 전송. SDK 0.5.1 의존 |

```text
등록: Mac CLI ── 기존 API 키 ──▶ LLMGate /v1/workers/token ── publish JWT
호출: OpenClaw → LLMGate → 서버 Caller Bridge → RelayGate → Mac MLX
```

같은 저장소의 `cli/`는 MacBook 전용 별도 Rust package다. Linux/vLLM CLI·실시간 STT·chat 모델은 포함하지 않는다.
같은 destination에 게시하는 모든 워커는 모델 revision·양자화·차원을 동일하게 유지한다.
embedding 요청별 새 연결, 파일 STT의 `new_connection_per_request` 설정으로 새 Pipe를 연다. 전달 후 자동 replay는 하지 않는다.

[CLI·제한·수명주기](../cli/README.md) · [서버 인증 설정](worker-registration.md) · [OpenClaw](../cli/docs/openclaw.md) · [배포](../cli/docs/distribution.md) · [검증](../cli/docs/validation-2026-09-16.md)

[운영 Caller 설정·이미지](../caller/README.md). 모델 warmup은 Python이 소유하고 Rust는 health·소유 포트만 검사한다. 모델이 상속받은 engine/cache lease와 supervisor 생존 pipe가 강제 종료 후 중복 실행·삭제를 방지한다.

운영 방어 계약: 모델별 실행 슬롯은 실제 executor 작업 완료 때 반환한다. 요청 점유 120초 초과는 health 실패로 전달하고 Rust 감독이 등록 철회·프로세스 그룹 종료·제한된 재시작을 수행한다. Go는 `429` + `error.type=worker_capacity`를 embedding/STT의 일시적 수용 거절로 처리하며 회로 차단 집계·자동 fallback에서 제외한다. 일반 제공자의 429 정책은 유지한다. 로그 저장·보관 정책은 [CLI 문서](../cli/README.md#장애-복구와-로그)를 따른다.

서버의 worker profile은 기본 모델·alias와 Caller 주소를 자동 구성한다. Mac 추가 시 catalog 파일 변경이나 서버 재시작은 없다. [ADR 012](adr/012-worker-default-catalog.md).
