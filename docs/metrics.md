# Metrics

Prometheus endpoint 는 `LLMGATE_METRICS_ENABLED=true`일 때만 `GET /metrics`로 열린다.
기본값은 disabled 다. `/healthz/*` 와 같이 business auth / access-log / request-id
middleware 밖에 둔다. scrape traffic 이 app request metric 과 로그를 오염시키지 않게 하기
위해서다.

`/metrics`는 앱 내부 bearer 인증을 제공하지 않는다. 운영 환경에서는 ServiceMonitor,
네트워크 정책, ingress 정책으로 Prometheus만 접근하게 제한한다.

정확한 metric 이름, label, bucket, error kind 값은 코드와 dashboard 가 source of truth 다.

## Policy

Prometheus 는 낮은 cardinality 의 운영 신호만 담는다. metric label 은 집계 축이어야 하고,
개별 요청이나 호출자를 식별하는 값이면 안 된다.

허용하는 label 축은 코드가 통제하는 작은 집합이다.

```text
operation
status
error_kind
vendor
model
direction
mode
reason
payload_mode
```

다음 값은 metric label 로 쓰지 않는다.

```text
request_id
consumer_key_id
consumer_name
raw error message
prompt / response body
```

`vendor` 와 `model` 은 catalog 로 통제되는 값이므로 허용한다. 비용, 사용자별 사용량,
프롬프트 / 응답 분석, 품질 평가는 metric 이 아니라 audit/call log 또는 trace 분석 계층의
책임이다.

## Runtime Boundary

gateway metric 은 크게 네 입력에서 나온다.

- `AuditEvent`: gateway RED
- `CallEvent`: upstream LLM RED / routing / token usage
- `LifecycleObserver`: in-flight saturation
- `llm.result.finalized` sink observer: result event drop / publish failure
- Go / process collector: runtime resource

Grafana dashboard 는 Prometheus 에 저장된 metric 을 읽는 운영 화면일 뿐, metric 계약을 새로
정의하지 않는다. dashboard query 가 code-emitted metric 과 어긋나면 코드를 기준으로 고친다.

## Source Of Truth

- metric registration: `internal/platform/telemetry/prometheus`
- emitted `error_kind`: `internal/domain/llmtypes`
- `/metrics` route wiring: `internal/app/gateway`, `internal/platform/http/server`
- Prometheus scrape target: `monitoring/prometheus/prometheus.yml`
- Grafana query usage: `monitoring/grafana/dashboards`
