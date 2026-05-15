# 요청 생애주기 + 스트리밍 폴백 경계

← [architecture.md](architecture.md) 로 돌아가기

요청 1 회가 어떤 컴포넌트를 어떤 순서로 거쳐 `AuditEvent` 와 `CallEvent` 로 끝나는가.

## 요청 생애주기

```mermaid
sequenceDiagram
    participant A as Agent
    participant M as auth middleware
    participant H as Handler
    participant S as llmrouter.Service
    participant P as Adapter
    participant T as Telemetry EventSink
    participant L as LifecycleObserver

    A->>M: POST /v1/chat/completions<br/>Authorization: Bearer ...
    Note over M: sha256(raw) lookup → ConsumerInfo on ctx<br/>(audit-always: pass through on failure)
    M->>H: next(r)
    H->>L: RequestStarted
    Note over H: ConsumerInfo → AuditEvent<br/>(auth 실패 시 401 emit + return)
    H->>S: Complete(req)
    Note over S: 별명 해석 → chain 시도<br/>실패마다 Attempt 누적
    S-->>H: Result
    H-->>A: 200 OK
    H->>T: Emit(AuditEvent)
    H->>T: Emit(CallEvent)
    H->>L: RequestFinished
```

`AuditEvent` 는 요청당 1 행이다. auth 실패, bad request, panic 도 남긴다. `CallEvent` 는
LLM 호출이 시도된 요청만 남긴다 — vendor 호출 전 끝난 요청은 운영 / 보안 증적으로 충분하므로
call event stream 을 오염시키지 않는다.

## 스트리밍 폴백 경계

```
Time ───────────────────────────────────────────────────────────────►

   ┌── status open ──┐    ┌── first event ──┐    ┌── mid-stream ────────┐
   │ HTTP status     │    │ 첫 chunk 검증    │    │ Recv 루프 / idle /   │
   │ 분류 (adapter)  │    │ (adapter)       │    │ [DONE] (responder)   │
   └────────┬────────┘    └─────────┬───────┘    └──────────┬───────────┘
            │                       │                       │
        ✅ fallback              ✅ fallback              ❌ no fallback
        (llmrouter.Service)      (llmrouter.Service)      SSE error frame
                                                          + [DONE], 종결

   ◄────────── 폴백 가능 영역 ──────────►◄────── 폴백 불가 ──────►
```

`llmrouter.Service` 는 status open / first event 단계의 실패만 받는다 — 와이어 분류는
adapter 가 끝낸 상태이므로 폴백 적격 판정 ([ADR 004](adr/004-fallback-policy.md)) 을
non-stream 과 같은 규칙으로 적용. 스트림 시작에 별도 timeout 을 만들지 않고 Handler 의
request context 를 그대로 넘긴다 ([ADR 005](adr/005-timeout-authority.md)) — 시작 / 첫 이벤트 /
전송 전체가 `LLMGATE_REQUEST_TIMEOUT` 하나를 공유.

Handler 가 200 OK 를 커밋한 뒤에는 streamRelay 가 SSE 전송. 이벤트 사이 idle 은
`LLMGATE_STREAM_IDLE_TIMEOUT`, end-of-stream 에서 `Stream.Summary()` 로 usage / finish
reason 을 `CallEvent` 에 finalize. streaming 구간의 live gauge 는 `LifecycleObserver` 가
`StreamStarted` / `StreamFinished` 로 받는다. mid-stream 폴백 거부 근거 (HTTP 시맨틱 / SDK
호환 / event 무결성) 는 [ADR 004](adr/004-fallback-policy.md).

## telemetry delivery boundary

Handler 는 audit / call 을 별도 recorder 에 직접 쓰지 않고 `telemetry.EventSink` 로 finalized
event 를 emit 한다. 기본 wiring 은 `SlogSink` 가 audit / call 로그 라인을 stdout 으로 라우팅한다.
이 경계는 추후 messaging stream sink 를 추가해도 요청 처리 코드가 바뀌지 않도록 둔 것이다.

sink 와 lifecycle observer 는 panic isolation 으로 감싸진다. 로그 / 관측 sink 의 결함이 API 응답
경로를 깨지 않아야 하기 때문이다. 단, 원격 messaging sink 는 별도 bounded async queue / drop
policy 를 가진 sink 로 붙인다 — 네트워크 backpressure 를 Handler defer 안으로 끌어오지 않는다.
