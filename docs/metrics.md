# Metrics

← [architecture.md](architecture.md) 로 돌아가기

Prometheus scrape endpoint:

```text
GET /metrics
```

`/metrics` 는 `/healthz/*` 처럼 auth / access-log / request-id middleware 밖에 있다.
Prometheus scrape 트래픽이 앱 요청 지표를 오염시키지 않게 하기 위해서다. 외부 노출 제어는
k8s ServiceMonitor / 네트워크 정책 / ingress 정책의 책임이다.

## 로컬 대시보드

로컬 compose 는 llmgate / Prometheus / Grafana 를 한 번에 띄운다.

```bash
docker compose up --build llmgate prometheus grafana
```

접속 지점:

```text
llmgate     http://localhost:8080
Prometheus  http://localhost:9090
Grafana     http://localhost:3000
```

포트가 이미 사용 중이면 host port 만 바꿀 수 있다.

```bash
LLMGATE_PROMETHEUS_PORT=19090 LLMGATE_GRAFANA_PORT=13000 \
  docker compose up --build llmgate prometheus grafana
```

Grafana 는 로컬 개발 편의를 위해 anonymous admin 으로 열린다. Prometheus datasource 와
`llmgate RED / USE` dashboard 는 `monitoring/grafana/provisioning/` 으로 자동 provision 된다.
개인 `docker-compose.override.yaml` 이 llmgate host port 를 바꾸더라도 Prometheus 는 compose
내부 DNS 인 `llmgate:8080` 을 scrape 하므로 영향을 받지 않는다.

로컬 compose 의 Prometheus 설정은 단일 `llmgate:8080` target 을 scrape 한다. 운영의 멀티 pod
환경에서는 ServiceMonitor / PodMonitor / scrape discovery 가 각 pod 의 `/metrics` 를 scrape 해야
한다. 대시보드는 gateway RED 와 LLM 지표를 pod 전체로 합산하고, runtime resource 지표는
`instance` 별로 보여 한 pod 만 과부하인 상황을 구분한다.

## 운영 범위

llmgate 는 request-driven gateway 이므로 운영 대시보드는 세 층을 함께 본다.

```text
Client
  |
  v
Handler
  |
  +-- AuditEvent ------------> gateway RED metrics
  |
  +-- CallEvent -------------> upstream LLM metrics
  |
  +-- LifecycleObserver -----> USE saturation gauges
  |
  +-- Go/process collectors --> runtime/resource metrics
```

## Gateway RED

```text
R - Rate       요청 수
E - Errors     status / error_kind 별 실패
D - Duration   요청 wall-clock 지연
```

| metric | type | labels | 의미 |
|---|---|---|---|
| `llmgate_requests_total` | counter | `operation`, `status`, `error_kind` | gateway 요청 수 |
| `llmgate_request_duration_seconds` | histogram | `operation`, `status`, `error_kind` | gateway 요청 wall-clock |

`operation` 값:

```text
chat.completions
chat.completions.stream
```

성공 요청의 `error_kind` 는 `none` 으로 기록한다.

운영 해석:

```text
caller 오류:  auth, bad_request, forbidden
gateway/vendor 오류: upstream, timeout, panic, empty_response, client_closed
```

전체 error ratio 만 보면 caller 입력 문제와 gateway/vendor 장애가 섞인다. 대시보드는 둘을
분리해서 보여준다.

## LLM upstream

Call metrics 는 실제 vendor attempt 가 있었던 요청에만 기록한다. auth 실패, JSON decode 실패,
unknown model 처럼 upstream 에 도달하지 않은 요청은 gateway RED 에만 남는다.

| metric | type | labels | 의미 |
|---|---|---|---|
| `llmgate_llm_requests_total` | counter | `operation`, `vendor`, `model`, `status`, `error_kind` | 최종 vendor/model 로 집계한 LLM 요청 수 |
| `llmgate_llm_attempts_total` | counter | `operation`, `vendor`, `model`, `status`, `error_kind` | vendor/model attempt 수 |
| `llmgate_llm_attempt_duration_seconds` | histogram | `operation`, `vendor`, `model`, `status`, `error_kind` | vendor/model attempt 지연 |
| `llmgate_llm_attempts_per_request` | histogram | `operation`, `status`, `error_kind` | 요청당 upstream attempt 수 |
| `llmgate_llm_fallback_requests_total` | counter | `operation`, `status`, `error_kind` | fallback 이 발생한 LLM 요청 수 |
| `llmgate_llm_tokens_total` | counter | `operation`, `vendor`, `model`, `direction` | provider 가 보고한 prompt / completion token |
| `llmgate_llm_token_usage` | histogram | `operation`, `vendor`, `model`, `direction` | 요청당 prompt / completion token 분포 |
| `llmgate_llm_io_bytes_total` | counter | `operation`, `direction` | gateway LLM 요청 / 응답 bytes |
| `llmgate_llm_generation_duration_seconds` | histogram | `operation`, `vendor`, `model`, `mode` | output 생성 구간 지연 |
| `llmgate_llm_output_tokens_per_second` | histogram | `operation`, `vendor`, `model`, `mode` | completion token 생산 속도 |
| `llmgate_llm_stream_first_byte_seconds` | histogram | `operation`, `vendor`, `model`, `status`, `error_kind` | stream attempt 시작부터 첫 chunk 까지 |
| `llmgate_llm_stream_chunks_total` | counter | `operation`, `vendor`, `model` | emit 된 stream chunk 수 |

`vendor` 와 `model` 은 catalog 로 통제되는 낮은 cardinality 값이다. 요청자 식별자, raw error
message, request id 는 metric label 로 올리지 않는다.

Langfuse 같은 LLM 관측 도구의 기본 축 중 llmgate 운영에 필요한 latency / usage / volume 을
Prometheus 에서는 위 metric 으로 본다. 단, usage 는 provider 응답이나 stream summary 에 이미
들어온 값만 기록한다. provider 가 usage 를 주지 않는 요청을 위해 prompt / response body 를
tokenizer 로 다시 세는 일은 기본 경로에서 하지 않는다.

비용, 사용자, 프롬프트, 응답, 품질 평가는 Prometheus metric 이 아니라 audit/call log 또는 trace
분석 계층에서 다룬다. provider pricing 은 모델/라우터/계약마다 의미가 달라 llmgate 자체 운영
상태를 나타내는 metric 으로 취급하지 않는다.

`mode` 값:

```text
end_to_end                 non-stream completion_tokens / 전체 attempt duration
stream_after_first_chunk   stream completion_tokens / (전체 attempt duration - TTFT)
```

따라서 `stream_after_first_chunk` 는 사용자가 첫 chunk 를 본 뒤 실제 output 이 흘러나온 속도에
가깝고, `end_to_end` 는 non-stream 에서 gateway 가 관측 가능한 전체 응답 기준 생산량이다.

## USE

Go / process 기본 collector 도 함께 노출한다.

```text
process_cpu_seconds_total
process_resident_memory_bytes
go_goroutines
go_threads
go_memstats_*
```

llmgate 자체 saturation gauge:

| metric | type | labels | 의미 |
|---|---|---|---|
| `llmgate_inflight_requests` | gauge | 없음 | 현재 처리 중인 gateway 요청 |
| `llmgate_inflight_streams` | gauge | 없음 | 현재 열린 SSE stream |

## Telemetry delivery

원격 messaging sink 는 요청 경로를 막지 않도록 bounded async queue 뒤에 붙인다. stdout
`audit` / `call` 로그와 Prometheus request / LLM metric 은 sync 경로에 남고, 아래 metric 은
async delivery 자체가 건강한지 확인하는 용도다.

| metric | type | labels | 의미 |
|---|---|---|---|
| `llmgate_telemetry_events_enqueued_total` | counter | `sink`, `event_type` | async delivery queue 에 들어간 event 수 |
| `llmgate_telemetry_events_dropped_total` | counter | `sink`, `event_type`, `reason` | async delivery 전에 drop 된 event 수 |
| `llmgate_telemetry_queue_depth` | gauge | `sink` | 현재 async delivery queue depth |
| `llmgate_telemetry_send_errors_total` | counter | `sink`, `event_type` | exporter 전송 실패 수 |
| `llmgate_telemetry_flush_duration_seconds` | histogram | `sink` | shutdown flush duration |

이 지표의 drop 은 stdout 증적 유실을 뜻하지 않는다. llmgate 는 stateless 이므로 durable retry
queue 를 내장하지 않고, 원격 stream 전송 실패 / 포화는 metric 과 로그로 노출한다.

## 라벨 경계

Prometheus 라벨은 낮은 cardinality 값만 허용한다.

```text
사용:
  operation
  status
  error_kind
  vendor
  model
  direction
  mode
  sink
  reason

사용하지 않음:
  request_id
  consumer_key_id
  consumer_name
  raw error message
  prompt / response body
```

`consumer_name` 과 `consumer_key_id` 는 audit/call log 에만 둔다. Prometheus label 로 올리면
고객 수와 key rotation 에 비례해서 time series 가 늘어나기 때문이다.

## 대시보드 해석

Grafana 는 같은 folder 아래 세 장으로 나눈다.

| dashboard | 용도 |
|---|---|
| `llmgate Operations Overview` | 장애 대응 첫 화면. Gateway RED, error split, final LLM request, fallback, TTFT, output TPS, resource pressure 만 본다. |
| `llmgate LLM Routing and Performance` | LLM drill-down. final request 와 attempt 를 구분하고 fallback, attempts/request, TTFT, generation duration, token throughput 을 본다. |
| `llmgate Runtime Resources` | Go/process drill-down. in-flight, goroutine/thread, CPU, memory, GC, FD 로 local saturation 을 본다. |

분석 순서:

1. Overview 에서 caller-visible 이상 여부를 본다.
2. caller 오류인지 gateway/vendor 오류인지 error kind 로 나눈다.
3. LLM dashboard 에서 final request 와 attempt 차이를 봐 fallback 또는 특정 vendor/model 문제인지 확인한다.
4. TTFT / generation duration / output tokens per second 로 stream 시작 지연과 token 생산 속도를 분리한다.
5. LLM 쪽 설명이 부족하면 Runtime dashboard 로 넘어가 local saturation 을 확인한다.
