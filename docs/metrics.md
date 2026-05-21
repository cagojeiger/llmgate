# Metrics

Prometheus endpoint 는 `GET /metrics`. `/healthz/*` 처럼 auth / access-log / request-id
middleware 밖에 둔다. scrape 트래픽이 앱 요청 지표와 로그를 오염시키지 않게 하기 위해서다.
외부 노출 제어는 ServiceMonitor / 네트워크 정책 / ingress 책임이다.

## 로컬 대시보드

```bash
docker compose up --build llmgate prometheus grafana
```

```text
llmgate     http://localhost:8080
Prometheus  http://localhost:9090
Grafana     http://localhost:3000
```

```bash
LLMGATE_PROMETHEUS_PORT=19090 LLMGATE_GRAFANA_PORT=13000 \
  docker compose up --build llmgate prometheus grafana
```

Grafana 는 로컬 개발용 anonymous admin 으로 열린다. datasource 와 dashboard 는
`monitoring/grafana/provisioning/` 에서 provision 된다. compose 안의 Prometheus 는
`llmgate:8080` 을 scrape 하므로 host port override 와 무관하다.

## 지표 범위

| 입력 | metric 영역 |
|---|---|
| `AuditEvent` | gateway RED |
| `CallEvent` | upstream LLM |
| `LifecycleObserver` | in-flight saturation |
| Go / process collector | runtime resource |

## Gateway RED

| metric | type | labels | 의미 |
|---|---|---|---|
| `llmgate_requests_total` | counter | `operation`, `status`, `error_kind` | gateway 요청 수 |
| `llmgate_request_duration_seconds` | histogram | `operation`, `status`, `error_kind` | gateway 요청 wall-clock |

```text
operation = chat.completions | chat.completions.stream
success error_kind = none
```

```text
caller 오류:  auth, bad_request, forbidden
gateway/vendor 오류: upstream, timeout, panic, empty_response, client_closed
```

## LLM upstream

실제 vendor attempt 가 있었던 요청만 기록한다. auth 실패, JSON decode 실패, unknown model 은
gateway RED 에만 남는다.

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

```text
end_to_end                 non-stream completion_tokens / 전체 attempt duration
stream_after_first_chunk   stream completion_tokens / (전체 attempt duration - TTFT)
```

usage 는 provider 응답이나 stream summary 에 들어온 값만 기록한다. prompt / response body 를
tokenizer 로 다시 세지 않는다. 비용, 사용자, 프롬프트, 응답, 품질 평가는 metric 이 아니라
audit/call log 또는 trace 분석 계층의 책임이다.

## USE

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

## 라벨 경계

Prometheus 라벨은 낮은 cardinality 값만 허용한다. `vendor` / `model` 은 catalog 로 통제되는
값이므로 허용한다.

```text
사용:
  operation
  status
  error_kind
  vendor
  model
  direction
  mode

사용하지 않음:
  request_id
  consumer_key_id
  consumer_name
  raw error message
  prompt / response body
```

`consumer_name` 과 `consumer_key_id` 는 audit/call log 에만 둔다.

## 대시보드 해석

| dashboard | 용도 |
|---|---|
| `llmgate Operations Overview` | Gateway RED, error split, final LLM request, fallback, TTFT, output TPS, resource pressure |
| `llmgate LLM Routing and Performance` | final request vs attempt, fallback, attempts/request, generation duration, token throughput |
| `llmgate Runtime Resources` | in-flight, goroutine/thread, CPU, memory, GC, FD |

장애 대응 순서: Overview → error kind 분리 → LLM final/attempt 비교 → TTFT/generation/TPS 확인
→ 필요 시 Runtime saturation 확인.
