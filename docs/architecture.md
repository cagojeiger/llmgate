# Architecture

## 정체성

OpenAI SDK 와이어 호환 게이트웨이. 모델은 *기본 등록 단위*, **별명**이 *실제 제어 단위*다. 별명 하나가 chain 으로 풀리고 chain 을 따라 자동 폴백한다. DB 없음, fact 만 발행, 비용 / 한도 계산은 후처리 시스템 책임. 자세한 동기는 `docs/adr/001-identity.md`.

## 코드 구조

```
catalog/                     데이터 (운영자 영역, 코드 0줄)
  models/<id>.yaml           endpoint = vendor + type + base_url + auth_env
  aliases/<name>.yaml        호출 단위 = chain
internal/catalog/            yaml → Catalog struct 로더
internal/config/             env → Server 설정 (서버 + 라우터 정책)
internal/provider/           Provider 어댑터 계약 + 공통 타입 (정책 0줄)
  └─ openai/                 OpenAI 와이어 어댑터
  └─ anthropic/              Anthropic 와이어 어댑터 (응답을 OpenAI 와이어로 정규화)
internal/router/             별명 → chain 해석, 폴백 시도, 회로 차단
internal/server/             chi + middleware + handler + sseWriter + errorPayload
internal/audit/              Recorder 인터페이스 + LogRecorder (stdout)
cmd/llmgate/                 wiring + shutdown
docs/adr/                    Accepted 결정 기록
```

데이터 / 정책 / 코드가 세 자리에 산다. yaml 은 운영자가 손대는 운영 데이터, env 는 인프라 / 시크릿, 코드는 알고리즘. ADR 002 가 이 분리의 근거를 적었다.

## 컴포넌트 구성

```mermaid
graph LR
    Agent[Agent / OpenAI SDK]

    subgraph Gateway[llmgate process]
        Server["HTTP Server<br/>(chi + middleware)"]
        Handler[Handler]
        Router[Router]
        OAI[OpenAI Adapter]
        Anth[Anthropic Adapter]
        Audit[Audit Recorder]
    end

    Catalog[(catalog/ yaml dir)]
    Env[(env / Server config)]
    UpOAI[OpenAI-protocol upstream]
    UpAnth[Anthropic-protocol upstream]
    Sink[stdout]

    Agent -->|/v1/chat/completions| Server
    Server --> Handler
    Handler -->|Request| Router
    Router -->|RouteResult| Handler
    Router --> OAI
    Router --> Anth
    OAI --> UpOAI
    Anth --> UpAnth
    Handler --> Audit
    Audit --> Sink

    Catalog -.boot.-> Router
    Env -.boot.-> Server
    Env -.boot.-> Router
```

| 컴포넌트 | 역할 |
|---|---|
| HTTP Server | chi 라우터 + request_id / access log / recoverer 미들웨어. `/v1/chat/completions`, `/healthz` 노출 |
| Handler | 요청 디코드, stream/non-stream 분기, RouteResult 와 Stream.Summary 로 audit Record 조립 |
| Router | 별명 → chain 해석, 폴백 시도, 회로 차단. 정책은 부팅 시 env 에서 받는다 |
| OpenAI Adapter | OpenAI 와이어로 upstream 호출 |
| Anthropic Adapter | Anthropic 와이어로 변환 후 호출, OpenAI 와이어로 응답 정규화 |
| Audit Recorder | 요청당 1 개 fact record 발행 (stdout / 향후 이벤트 파이프라인 등) |

## 카탈로그 모양

```
catalog/
  models/<id>.yaml         id / vendor / type / base_url / auth_env
  aliases/<name>.yaml      alias / chain
```

- **모델 yaml** = endpoint 1 개 + model 1 개. 같은 vendor model name 을 다른 auth_env 로 두 yaml 에 두면 catalog 안에서 별개 endpoint 가 된다.
- **별명 yaml** = chain. 클라이언트가 별명으로 부르면 chain 을 순서대로 시도. raw model id 로 부르면 chain 길이 1 → 폴백 발동 자체가 없다. **폴백은 별명 호출에만 의미가 있다.**
- **정책은 yaml 에 없다** — `LLMGATE_FALLBACK_ON`, `LLMGATE_CIRCUIT_FAILURES`, `LLMGATE_CIRCUIT_OPEN_DURATION` 으로 env 에서 받는다. 코드에 합리적 디폴트가 박혀 있어서 운영자는 거의 손대지 않는다.

자세한 결정 근거는 `docs/adr/002-catalog-shape.md`.

### 부팅 순서

1. env → Server config 로드 (addr / shutdown / log level / 라우터 정책)
2. `catalog/` 또는 `LLMGATE_CATALOG=<dir>` 의 yaml 파싱 → endpoint / model / 별명 확정
3. protocol 별 adapter factory 호출 → 각 endpoint 마다 Adapter 인스턴스 생성
4. Router 조립 (model → adapter 매핑, 별명 chain, 회로 상태 초기화, 정책 주입)
5. Audit Recorder 구성 → Handler / HTTP Server 기동

## 요청 생애주기

```mermaid
sequenceDiagram
    participant A as Agent
    participant H as Handler
    participant R as Router
    participant P as Adapter
    participant Au as Audit

    A->>H: POST /v1/chat/completions
    H->>R: Complete(req)
    Note over R: 별명 해석 → chain 순서대로 시도<br/>실패마다 Attempt 누적
    R-->>H: RouteResult (Response, Attempts, Vendor, ModelUsed)
    H-->>A: 200 OK
    H->>Au: Record (fact)
```

스트리밍 요청은 stream 시작 전 실패에 한해 폴백한다. 아직 SSE header / chunk 가 client 에게 나가지 않았기 때문이다. stream 이 열린 뒤의 mid-stream 실패는 partial output 이 이미 전달됐을 수 있어 폴백하지 않는다. end-of-stream 에서 `Stream.Summary()` 로 usage / finish reason 을 audit 에 finalize 한다.

## 상태가 어디 사는가

| 데이터 | 위치 | 수명 |
|---|---|---|
| 모델 / endpoint / 별명 | `catalog/` (외부 yaml) | 외부 갱신 시 재시작 |
| 라우터 정책 + 서버 런타임 | env → Server config | 프로세스 수명 |
| 회로 차단 상태 | Router 메모리 (per-process) | 프로세스 수명 |
| 요청별 시도 이력 | RouteResult → Handler → Record | 요청 1 회 |
| 감사 record | Sink 가 결정 | Sink 정책 |
| 비용 / 한도 / 카탈로그 단가 | **gateway 가 보관하지 않음** | 후처리 시스템 책임 |

## 의도적 미지원

멀티모달 capability 매칭 / `/v1/models` discovery / hot-reload / pre-call 한도 / mid-stream 폴백 / 모델 메타정보(가격 · context window) 보유 / multi-key smart distribution — 모두 V1 범위 밖. 누적 결정은 `docs/adr/003-out-of-scope.md` (작성 예정) 에 정리한다.
