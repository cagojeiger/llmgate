# 02. Security operations baseline

- 조사 기준일: 2026-05-26
- 기준 문서: [00-standards.md](00-standards.md)
- 현재 상태 문서: [01-current-state.md](01-current-state.md)

이 문서는 코드로 강제할 수 없는 운영 기준을 리포 안의 계약으로 고정한다. SOC 2 감사,
ISMS-P 인증, 조직 보안정책을 대체하지 않고 `llmgate` 운영자가 반드시 외부 증적으로 남겨야
하는 항목만 적는다.

## Result Event

- result event는 원문 prompt/response를 durable event로 내보내므로 NATS stream 보관/파기 기준이 확정된 환경에서만 쓴다.
- result event 원격 publish를 켜는 변경은 운영 change ticket, 데이터 목적, 보관기간, downstream consumer, 파기 방법을 남긴다.
- `llm.result.finalized` stream 보관기간은 stdout audit/call log보다 길게 잡지 않는다. 원문 export 목적이 끝나면 stream purge 또는 subject-level delete 절차를 실행한다.

## Management Surface

- `/metrics`는 `LLMGATE_METRICS_ENABLED=true`일 때만 열린다. 기본값은 disabled 다.
- `LLMGATE_ENVIRONMENT != local`에서 result event 원격 publish를 켜면 `LLMGATE_LLMRESULT_NATS_URL`은 `tls://`만 허용한다.
- 같은 조건에서 `LLMGATE_LLMRESULT_NATS_USER`와 `LLMGATE_LLMRESULT_NATS_PASSWORD`도 필수다.
- `/metrics`, NATS client port, NATS monitoring port, Prometheus, Grafana는 public internet ingress에 직접 노출하지 않는다.
- `docker-compose.yaml`은 local hand-test 전용이다. compose의 NATS, Prometheus, Grafana port publish와 Grafana anonymous admin 설정은 prod 금지다.

## Audit And Access Logs

- `audit`는 인증 실패, 정책 거부, vendor 호출 성공/실패를 request 단위로 남긴다.
- `access`는 HTTP 전송 사실과 함께 `remote_addr`, `user_agent`, `request_id`를 남긴다.
- prompt, response, tool payload, raw Authorization header, raw API key, raw upstream error body는 stdout 로그에 남기지 않는다.
- 법정 개인정보처리시스템 접속기록 충족 여부는 운영 환경에서 별도 판단한다. `llmgate`의 audit/access log는 그 판단에 사용할 수 있는 보조 증적이다.
- audit/access log를 접속기록 보조 증적으로 사용하는 운영 환경은 최소 1년 보관을 기본값으로 둔다.
- 5만명 이상 정보주체, 고유식별정보/민감정보 처리, 기간통신사업자 등 강화 조건에 해당하면 최소 2년 보관 기준을 적용한다.
- 운영자는 점검 주기, 점검 방법, 이상 징후 사후조치 절차를 내부 관리계획에 남긴다.

## Vendor Registry

운영자는 upstream LLM vendor별로 다음 항목을 별도 증적으로 관리한다.

- vendor name, model ids, endpoint base URL
- 처리 목적과 전송 데이터 범위
- 처리 국가, 국외 이전 여부, 재위탁 여부
- prompt/response 보관 여부와 보관기간
- 학습 사용 여부와 opt-out 설정
- DPA, 약관, 보안 문서 링크와 확인일

## Change Evidence

- `catalog/` 변경은 어떤 alias/model/vendor가 추가·삭제·변경되었는지 리뷰 증거를 남긴다.
- `consumers/` 변경은 호출 앱, owner, 목적, 허용 alias, unrestricted 여부, 발급/회전/폐기 일자를 남긴다.
- result event 원격 publish, metrics exposure 변경, NATS credential 변경은 보안 변경으로 취급한다.
