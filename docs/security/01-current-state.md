# 01. Current security state

- 조사 기준일: 2026-05-26
- 기준 문서: [00-standards.md](00-standards.md)
- 범위: 현재 저장소에 있는 코드, 문서, CI, Docker/compose 설정

이 문서는 현재 `llmgate`가 보안 기준에 대해 이미 갖춘 것과 아직 비어 있는 것을 정리한다. 프로덕션 Kubernetes/Ingress/NATS 설정, secret manager, vendor 계약/DPA, 로그 보관 정책은 이 저장소에 없으므로 여기서는 확정하지 않는다.

## Summary

| 영역 | 현재 상태 | 판단 |
|---|---|---|
| API 인증 | Bearer key + sha256 hash lookup + closed-by-default consumers | 기본 방향 좋음 |
| 모델 접근제어 | `allowed_aliases`로 consumer별 모델 제한 가능 | 빈 allowlist가 unrestricted라 운영 승인 기준 필요 |
| 감사 로그 | auth 실패 포함 audit-always, 원문 key/body 배제, access source field 기록 | 법정 접속기록 충족 여부, 점검 주기, trusted proxy 정책은 운영 판단 필요 |
| 메트릭 | 낮은 cardinality label 정책 + 명시적 `/metrics` enable 옵션 | 기본 disabled. enable 시 네트워크 통제는 운영 증적 필요 |
| result event | NATS publish 활성화 시 원문 request/response 포함 | 원문 export 승인과 retention/파기 증적 필요 |
| upstream vendor | catalog 기반 vendor endpoint/API key 사용 | vendor별 개인정보 처리/국외이전/학습 사용 정책 문서 필요 |
| 공급망 | `govulncheck`, Dependabot, distroless nonroot image | image scan, SBOM, signing/provenance 보강 필요 |

## What is already implemented

### Authentication and consumer registry

- `consumers/`가 없거나 비어 있으면 부팅 실패한다. 익명 허용 모드가 없다.
- consumer raw key는 디스크에 저장하지 않고 `sha256:` hash만 저장한다.
- `scripts/gen-consumer.sh`는 256-bit random raw key를 만들고 hash yaml을 생성한다.
- 인증 실패는 handler까지 도달해서 audit record가 남는다.

근거:

- `internal/domain/consumers/consumers.go`: closed-by-default, strict yaml parsing, duplicate name/hash rejection, raw key hash lookup.
- `internal/platform/http/auth/auth.go`: `Authorization: Bearer <key>` 분류, raw key 미로그.
- `scripts/gen-consumer.sh`: `openssl rand -hex 32` 또는 `/dev/urandom` fallback.
- `internal/platform/http/chat/handler.go`: auth failure도 audit defer 경로를 탄다.

### Logging and metrics minimization

- `docs/logs.md`는 Authorization header, raw consumer key, vendor API key, prompt/message body, response body, tool payload, raw error message를 로그 금지 대상으로 둔다.
- `internal/platform/telemetry/slog`의 audit/call log는 request id, consumer name/key id, auth/policy result, model/vendor, status/error kind, usage 같은 운영 필드만 남긴다.
- `docs/metrics.md`는 request id, consumer key id/name, raw error message, prompt/response body를 metric label로 금지한다.
- Prometheus metrics 구현도 `operation`, `status`, `error_kind`, `vendor`, `model`, `direction`, `mode` 중심의 낮은 cardinality label만 쓴다.

근거:

- `docs/logs.md`
- `docs/metrics.md`
- `internal/platform/telemetry/slog/slog.go`
- `internal/platform/telemetry/prometheus/prometheus.go`

### Request and transport guardrails

- chat request body는 `http.MaxBytesReader`로 1 MiB 제한을 둔다.
- HTTP server는 `ReadHeaderTimeout`, `ReadTimeout`, `IdleTimeout`을 둔다. SSE 때문에 `WriteTimeout`은 0이다.
- upstream client는 stdlib TLS 기본값을 쓰고, `InsecureSkipVerify` 같은 우회 설정은 보이지 않는다.
- upstream URL은 operator catalog에서만 오며, catalog validation은 `http`/`https` scheme과 host 존재를 검증한다.
- client Authorization/Cookie를 upstream으로 그대로 전달하지 않고 provider API key만 새로 붙인다.

근거:

- `internal/platform/http/chat/request.go`
- `internal/platform/http/server/server.go`
- `internal/platform/upstream/http.go`
- `internal/domain/catalog/validation.go`
- `internal/platform/providers/openai/client.go`
- `internal/platform/providers/anthropic/client.go`

### CI and release baseline

- CI는 `gofmt`, `go vet`, `golangci-lint`, `govulncheck`, `go test -race`, cassette e2e를 실행한다.
- Dependabot이 Go module과 GitHub Actions 업데이트를 weekly로 연다.
- Docker runtime image는 distroless static nonroot를 사용한다.

근거:

- `.github/workflows/ci.yml`
- `.github/dependabot.yml`
- `Dockerfile`

## What is not yet covered

### 1. `llm.result.finalized` 원문 데이터 통제

현재 result event는 기존 동작과 맞춰 원문 request/response를 포함한다. 이는 stdout 로그보다 훨씬 강한 개인정보/기밀정보 통제 대상이다.

비어 있는 것:

- consumer별 result export opt-in/deny 정책이 없다.
- NATS stream 보관기간/파기/소유자/승인 기준은 `02-operations.md`에 기본 원칙만 있고, 실제 운영 값과 증적은 환경별로 필요하다.

근거:

- `internal/domain/llmresult/schema/event.go`: `Request`, `Response` 필드가 durable event에 있음.
- `internal/domain/llmresult/schema/build.go`: request/response를 result event에 복제함.
- `internal/platform/config/config.go`: `LLMGATE_LLMRESULT_NATS_URL`이 비어 있으면 원격 publish 비활성.
- `internal/domain/llmresult/sink/async.go`: queue full/closed 시 warning log와 dropped count를 남김.
- `internal/platform/nats/llmresult/publisher.go`: publish 실패 시 warning log를 남김.

권장 보완:

- NATS stream retention/파기 기준을 운영 문서에 고정.

### 2. NATS / metrics / Grafana 관리 표면 접근통제

`/metrics`는 business auth/middleware 밖에 있지만, `LLMGATE_METRICS_ENABLED=true`일 때만 mount된다. local compose는 NATS, NATS monitoring, Prometheus, Grafana를 host port로 연다. local-only 의도는 문서에 있지만 프로덕션 운영 증적은 저장소 밖에 남아야 한다.

비어 있는 것:

- local compose의 Grafana anonymous admin은 local-only이다. prod 금지 기준은 문서화되어 있으나, 실제 prod network/ingress 통제 증적은 환경별로 필요하다.

근거:

- `internal/app/gateway/runtime.go`: `LLMGATE_METRICS_ENABLED=true`일 때만 `/metrics` handler를 wiring.
- `internal/platform/http/server/server.go`: `/metrics`가 mount되더라도 middleware 밖에 있음.
- `docs/metrics.md`: 외부 노출 제어는 ServiceMonitor/network policy/ingress 책임.
- `internal/platform/config/config.go`: local 외 환경에서 NATS URL은 `tls://` 필수, NATS user/password 필수.
- `docker-compose.yaml`: NATS/Prometheus/Grafana ports 공개, Grafana anonymous admin.

권장 보완:

- 별도 management listen address가 필요하면 `LLMGATE_METRICS_ADDR` 분리 검토.
- NATS mTLS 파일 옵션이 필요하면 CA/cert/key env 추가.
- prod 환경별 Service/Ingress/NetworkPolicy 또는 equivalent 설정 증적을 남긴다.

### 3. 감사 로그를 접속기록 보조 증적으로 정리

현재 audit는 “누가, 어떤 모델을, 어떤 결과로 호출했는가”를 잘 남긴다. access log는 `remote_addr`, `user_agent`, `request_id`를 남겨 접속기록 점검의 보조 필드로 쓸 수 있다. 다만 법정 개인정보처리시스템 접속기록을 단독 충족하는지는 운영 환경의 개인정보 처리 범위, 관리자 기능, downstream log/trace 체계와 함께 별도 판단해야 한다.

비어 있는 것:

- 운영 환경별 audit/access log 보관기간과 점검 주기가 코드 밖 운영 증적으로 필요하다.
- 법정 접속기록으로 사용할지, 보조 증적으로만 사용할지에 대한 운영 판단이 필요하다.
- trusted proxy 기반 forwarded-for trust boundary 정책이 없다.
- audit log schema에 보안 점검 목적의 필드 변경 정책은 있으나 retention 정책은 없다.

근거:

- `internal/platform/telemetry/slog/slog.go`: audit/call 필드.
- `internal/platform/http/middleware/middleware.go`: access log의 `remote_addr`, `user_agent`, `request_id` 필드.
- `docs/logs.md`: sensitive data 배제 정책은 있으나 보관/점검 주기 없음.
- `docs/security/02-operations.md`: 보조 증적 보관기간과 점검 책임 기준.

권장 보완:

- `docs/security/02-operations.md`에 audit/call/access log retention과 점검 주기 정의.
- trusted proxy 사용 여부를 명시하고 source IP field를 추가할지 결정.
- 개인정보를 직접 넣지 않는 선에서 `client_ip_hash` 또는 `source_zone` 같은 운영 필드 검토.

### 4. Upstream vendor error message 노출 축소

현재 opaque upstream failure는 generic message로 collapse하지만, auth/bad_request/context/rate_limit 등 caller-actionable kind는 vendor envelope message가 그대로 노출될 수 있다. vendor가 error message에 내부 detail이나 echo된 user input을 넣으면 caller에게 전달될 수 있다.

근거:

- `internal/platform/upstream/http.go`: `KindUpstream`만 `upstream unavailable`로 변경.
- `internal/platform/providers/openai/errors.go`: upstream envelope message를 `PublicProviderMessage`로 전달.
- `internal/platform/providers/anthropic/errors.go`: 동일한 방식.
- `internal/platform/http/response/errors.go`: error message를 wire response에 포함.

권장 보완:

- `LLMGATE_UPSTREAM_ERROR_DETAIL=public|generic` 같은 strict mode 추가.
- vendor message 길이 제한과 제어문자 제거.
- audit/operator log에는 kind/status/request_id 중심으로 남기고 raw vendor body는 별도 보안 sink로만 보낼지 결정.

### 5. Consumer lifecycle and authorization policy

현재 consumer schema는 단순하고 강하다. 하지만 보안 기준 관점에서는 키 발급/회전/폐기와 unrestricted allowlist 승인 증적이 부족하다.

비어 있는 것:

- `issued_at`, `expires_at`, `owner`, `purpose`, `ticket` 같은 운영 메타데이터가 없다.
- `allowed_aliases`가 비어 있으면 unrestricted인데, 의도적 unrestricted인지 실수인지 구분할 수 없다.
- key hash는 강한 raw key를 전제로 안전하다. `gen-consumer.sh`는 강한 키를 만들지만 외부에서 만든 key hash를 넣을 때의 엔트로피 검증/문서 기준은 약하다.

근거:

- `internal/domain/consumers/consumers.go`: `name`, `key_hashes`, `allowed_aliases`만 schema에 있음.
- `docs/data.md`: empty `allowed_aliases`는 unrestricted.
- `scripts/gen-consumer.sh`: 256-bit random raw key 생성.

권장 보완:

- consumer yaml schema v2 또는 optional metadata 추가.
- unrestricted는 `allowed_aliases: []` 대신 `unrestricted: true` 같은 명시적 승인 필드로 변경 검토.
- raw key entropy requirement를 `docs/security/02-operations.md`에 명시.

### 6. Vendor privacy registry

prompt/response는 upstream vendor로 전송된다. 현재 catalog는 endpoint/model/auth 정보만 담고 vendor별 개인정보 처리 목적, 국가, 보관, 학습 사용 여부, DPA 링크를 다루지 않는다.

비어 있는 것:

- vendor별 개인정보 처리/국외이전/재위탁/학습 사용 여부 매핑.
- alias chain 변경 시 개인정보/기밀정보 정책 영향 검토.
- vendor 사용 중지 또는 incident 대응 절차.

근거:

- `catalog/models/*.yaml`: vendor/base_url/protocol/auth_env/auth_scheme 중심.
- `docs/data.md`: catalog는 운영 데이터, env는 secret, code는 알고리즘으로 분리.

권장 보완:

- `docs/security/vendors.md` 또는 `catalog/vendors/*.yaml` 추가.
- alias chain PR에서 vendor privacy registry 변경 여부를 체크.

### 7. Supply-chain evidence

CI에 `govulncheck`가 있는 것은 좋다. 다만 SOC 2/ISMS-P 개발보안 증적으로는 container image scan, SBOM, signing/provenance가 추가되면 더 강하다.

비어 있는 것:

- release image vulnerability scan.
- SBOM 생성/게시.
- GHCR image signing.
- provenance attestation.

근거:

- `.github/workflows/ci.yml`: `govulncheck`.
- `.github/dependabot.yml`: weekly dependency updates.
- `.github/workflows/release.yml`: image build/push + tag/release만 수행.

권장 보완:

- Trivy/Grype image scan.
- Syft SBOM.
- cosign signing.
- GitHub build provenance attestation.

## Priority

| 우선순위 | 항목 | 이유 |
|---|---|---|
| P0 | result event retention | 원격 publish 활성화 시 원문 request/response를 durable event로 내보냄. 보관/파기 증적 필요 |
| P0 | prod 운영 surface 증적 | NATS TLS/auth는 fail-fast로 보강됨. `/metrics`는 opt-in이며 Grafana/Prometheus 운영 증적은 환경별로 필요 |
| P1 | audit retention/trusted proxy/점검 주기 | access source fields와 운영 보관 기준은 추가됨. proxy 신뢰 경계와 점검 증적 필요 |
| P1 | consumer lifecycle metadata/unrestricted 승인 | 키 회전/폐기/권한 증적 강화 |
| P1 | vendor privacy registry | upstream 전송, 국외이전, 학습 사용 여부 관리 |
| P2 | upstream error strict mode | vendor 내부정보/echo 노출 가능성 축소 |
| P2 | SBOM/signing/image scan | 공급망 보안 증적 강화 |

## Strong existing controls to preserve

- 인증 실패도 audit record를 남기는 audit-always 원칙.
- 로그/메트릭에서 prompt/response/key 원문을 배제하는 정책.
- consumer raw key를 저장하지 않고 hash만 저장하는 구조.
- consumers/catalog strict parsing과 closed-by-default boot.
- provider auth header를 client header pass-through가 아니라 gateway-owned secret으로 구성하는 구조.
- fallback 사유를 제한하고 auth error fallback을 기본 제외하는 정책.
