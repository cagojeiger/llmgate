# 00. Security standards

- 조사 기준일: 2026-05-26
- 범위: `llmgate` 바이너리, 컨테이너 이미지, `catalog/`, `consumers/`, stdout 로그, Prometheus metrics, 선택적 `llm.result.finalized` NATS/JetStream 발행 경로
- 결정 근거: [ADR-007: 서비스 보안 설계의 상위 참조 기준 채택](../adr/007-security-standards.md)
- 결론: LLM 요청/응답에 개인정보가 포함될 수 있음을 전제로 **개인정보 보호법령 및 개인정보의 안전성 확보조치 기준**을 우선 참조한다. **ISMS-P 보호대책 요구사항**은 운영 통제 프레임으로 채택한다. **SOC 2**는 법정 의무가 아니라 고객/외부 감사 대응 기준이므로 Security와 Confidentiality를 기본으로 삼고, Availability/Privacy/Processing Integrity는 운영 약속에 따라 추가한다.

본 문서는 SOC 2, ISMS-P, 개인정보 보호법령 및 안전성 확보조치 기준을 서비스 보안 설계의 상위 참조 기준으로 사용한다.

다만 본 문서는 조직 운영, 인증 심사, 내부 감사, 상시 보안 운영 체계 전체를 다루지 않는다. 대신 각 기준에서 요구하는 통제 목표를 서비스 기능, 데이터 흐름, 접근통제, 감사로그, 설정 변경, 개인정보 처리 방식에 어떻게 반영할지 정의한다.

이 문서는 법률 자문이 아니다. 운영자가 어느 법인이고, 누구의 데이터를 어떤 목적으로 처리하는지에 따라 법정 의무와 인증 의무는 달라진다. 다만 이 리포의 기능상 LLM 요청/응답 원문, 호출자 식별자, API 키, 운영 감사기록을 다루므로 보안 기준의 하한은 높게 잡는다.

## Service boundary

`llmgate`는 OpenAI wire-compatible LLM router다. 호출자는 `Authorization: Bearer <key>`와 chat completion payload를 보내고, 게이트웨이는 alias/fallback chain에 따라 upstream LLM vendor를 호출한다.

보안 기준이 적용되는 주요 데이터는 다음과 같다.

| 데이터 | 저장/전송 위치 | 기준상 취급 |
|---|---|---|
| 호출자 bearer key | 요청 헤더, 런타임 메모리 | 인증정보/비밀정보. 원문 저장 금지, 회전/폐기/접근통제 필요 |
| vendor API key | 환경변수, upstream 요청 헤더 | 비밀정보. secret manager/KMS, 최소권한, 노출 감사 필요 |
| prompt/message/tool payload | inbound request body, upstream request | 개인정보/기밀정보가 포함될 수 있음 |
| LLM response body | client response, optional result event | prompt와 동일하게 개인정보/기밀정보 가능 |
| audit/call log | stdout JSON, external log sink | 운영 감사기록. 원문 key/body 배제 원칙 필요 |
| metrics | Prometheus `/metrics` | 낮은 cardinality 운영 신호. 식별자/본문 label 금지 |
| `llm.result.finalized` | NATS JetStream | 원문 request/response를 포함할 수 있는 고위험 export 경계 |

핵심 판단:

- stdout audit/call log는 운영 감사기록 기준에 가깝다.
- `llm.result.finalized`는 원문 request/response를 durable event로 내보낼 수 있으므로, 운영자가 이 기능을 켜면 NATS/JetStream, downstream consumer, 보관기간, 재처리 파이프라인까지 개인정보/기밀정보 통제 범위에 포함한다.
- 내부용 gateway라도 프롬프트에 개인정보가 들어갈 수 있는 구조이므로 “개인정보 필드를 만들지 않는다”만으로 개인정보 보호법령 적용 가능성을 배제하지 않는다.

## Standards decision

| 기준 | 이 리포에 대한 결론 | 따라야 하는 이유 | 기준 시점/유효성 |
|---|---|---|---|
| 개인정보 보호법령 및 안전성 확보조치 기준 | **개인정보 포함 가능성을 전제로 한 설계 baseline** | prompt/response/user field/tool payload에 살아 있는 개인을 식별할 수 있는 정보가 들어가면 개인정보 처리에 해당할 수 있다. 법 제29조는 내부관리계획, 접속기록 보관 등 기술적/관리적/물리적 안전조치를 요구한다. 이 리포는 그중 서비스가 직접 강제하거나 보조 증적으로 생성할 수 있는 항목만 다룬다. | 개인정보 보호법: [시행 2025-10-02, 법률 제20897호](https://www.law.go.kr/LSW/lsInfoP.do?ancYnChk=0&chrClsCd=010202&efYd=20251002&lsiSeq=270351&urlMode=lsInfoP). 안전성 확보조치 기준: [시행 2025-10-31, 개인정보보호위원회고시 제2025-9호](https://law.go.kr/LSW/admRulLsInfoP.do?admRulId=73493&efYd=0). 안내서: [2025-11 현재 안내서](https://www.pipc.go.kr/np/cop/bbs/selectBoardArticle.do?bbsId=BS217&mCode=D010030020&nttId=11641). 법령/고시는 다음 개정 전까지 적용되는 현행 기준으로 본다. |
| ISMS-P | **운영 통제 프레임으로 채택. 인증 의무는 운영자별 판단** | KISA 기준은 관리체계 수립/운영, 보호대책, 개인정보 처리단계별 요구사항을 포괄한다. 이 리포는 인증/권한, 접근통제, 암호화, 개발보안, 운영관리, 사고대응, 재해복구 요구사항과 직접 맞닿아 있다. | KISA 제도 페이지는 ISMS-P가 정보보호 및 개인정보보호 조치가 인증기준에 적합함을 증명하는 제도라고 설명하고, 기준을 1.관리체계(16), 2.보호대책(64), 3.개인정보 처리단계(21)로 제시한다: [KISA ISMS-P 제도소개](https://isms.kisa.or.kr/main/ispims/intro/). KISA 자료실의 인증기준 안내서는 [2023-11-23 수정게시](https://isms.kisa.or.kr/main/ispims/notice/?boardId=bbs_0000000000000014&cntId=21&mode=view). 인증을 취득하면 [3년 유효, 매년 1회 이상 사후심사](https://isms.kisa.or.kr/main/ispims/request)가 기준이다. |
| SOC 2 | **외부 고객/감사 대응 기준으로 채택 가능. 기본은 Security + Confidentiality** | SOC 2는 서비스 조직의 시스템 통제 설계/운영 효과성을 고객과 파트너에게 설명하는 감사 보고 프레임이다. `llmgate`가 내부 도구를 넘어 고객-facing 서비스 또는 third-party risk review 대상이 되면 적합하다. | AICPA의 2017 Trust Services Criteria with revised points of focus 2022는 Security, Availability, Processing Integrity, Confidentiality, Privacy 기준을 제시한다: [AICPA TSC resource](https://www.aicpa-cima.com/resources/download/2017-trust-services-criteria-with-revised-points-of-focus-2022?Jid=CppDev20110217). AICPA SOC 2 guide는 2022-10-15 기준으로 업데이트되었다: [SOC 2 guide](https://www.aicpa-cima.com/cpe-learning/publication/soc-2-reporting-on-an-examination-of-controls-at-a-service-organization-relevant-to-security-availability-processing-integrity-confidentiality-or-privacy-OPL). SOC 2 보고서는 고정 유효기간보다 감사 대상 기간/시점에 대한 보고서로 관리한다. |

## Repo baseline and operations responsibility

### 개인정보 보호법령 / 안전성 확보조치

- 개인정보 처리 여부 판단: `messages`, `user`, tool payload, upstream response, `llm.result.finalized.request/response`에 개인정보가 포함될 수 있음을 전제로 데이터 분류를 한다.
- 리포가 직접 강제하는 항목: bearer 인증, consumer별 model allowlist, raw key 미저장, 원문 prompt/response stdout 로그 금지, 낮은 cardinality metrics label, result event 원격 publish fail-fast 설정.
- 운영 조직이 별도 증적으로 남겨야 하는 항목: 개인정보 보호 조직/책임자, 개인정보취급자 교육, 내부관리계획, 정기 권한검토, 악성프로그램 방지, 취약점 점검, 사고대응, 위험관리, 수탁자 관리.
- 접근권한 관리: `consumers/`, provider secret, NATS subject/stream, log sink, metrics endpoint, deployment secret에 대해 최소권한과 계정 회전을 적용한다. 정기 리뷰 증적은 운영 책임이다.
- 접근통제: `/v1/chat/completions`는 bearer key 필수, `allowed_aliases`로 모델 접근을 제한한다. `/metrics`, NATS monitoring, Grafana는 별도 네트워크/인증 통제로 막아야 한다.
- 암호화: client-to-gateway, gateway-to-vendor, gateway-to-NATS, 로그/JetStream 저장소의 전송 및 저장 암호화를 운영 기준으로 둔다. local compose의 `nats://`와 placeholder password는 local-only로만 허용한다.
- 접속기록 보조 증적: auth success/failure, policy denial, upstream attempt, result sink publish/drop/failure를 `request_id`로 조인 가능하게 보관한다. 이 로그가 법정 개인정보처리시스템 접속기록을 단독 충족하는지는 운영 환경에서 별도 판단한다. 원문 prompt/response 로그 금지 정책은 유지한다.
- 보관/파기: audit/call log와 `llm.result.finalized`는 보관 목적, 기간, 파기 방식을 분리한다. 특히 result event는 원문을 포함하므로 로그보다 짧은 보관기간 또는 별도 승인을 둔다.
- 위탁/제3자 제공/국외 이전: upstream LLM vendor로 prompt/response가 전송된다. vendor별 처리 목적, 국가, 재위탁, 보관, 학습 사용 여부를 운영자가 문서화해야 한다.
- 침해사고 대응: bearer key/vendor key 유출, prompt/response 유출, 잘못된 alias allowlist, NATS stream 노출, upstream vendor 오전송을 사고 시나리오에 포함한다.

### ISMS-P

| ISMS-P 영역 | 적용 이유 | 리포 관점의 요구 |
|---|---|---|
| 1.1 관리체계 기반 마련 | 인증/비밀/개인정보 처리 경계가 운영자 설정에 의존 | 인증범위, 자산 목록, 책임자, 정책 문서화 |
| 1.2 위험관리 | LLM prompt는 내용 예측이 어렵고 upstream/vendor가 바뀔 수 있음 | prompt/response/result event 위험평가, vendor risk register |
| 1.3 관리체계 운영 | catalog/consumer/env 변경이 곧 운영 통제 변경 | 변경승인, 배포 전 검토, secret rotation 절차 |
| 1.4 점검 및 개선 | 보안 통제가 코드와 운영 sink에 나뉨 | 로그 샘플 점검, NATS 권한 점검, 취약점/구성 점검 |
| 2.1 정책/조직/자산 | catalog, consumers, env, NATS, logs가 별도 자산 | 자산 등급과 소유자 지정 |
| 2.3 외부자 보안 | upstream LLM vendor와 downstream analytics consumer가 있음 | vendor 계약/보안검토, downstream 접근 승인 |
| 2.5 인증 및 권한관리 | bearer key, provider key, NATS user/password | 키 발급/회전/폐기, shared key 최소화, short key id 감사 |
| 2.6 접근통제 | API, metrics, NATS, Grafana 경계 | network policy, ingress auth, management endpoint 제한 |
| 2.7 암호화 적용 | prompt/response/API key 보호 | TLS, secret encryption, JetStream at-rest encryption |
| 2.8 정보시스템 도입 및 개발 보안 | gateway가 security boundary | 보안 요구사항, 테스트, dependency/container scan |
| 2.9 시스템 및 서비스 운영관리 | stdout/NATS/Prometheus 운영 의존 | 백업, 모니터링, 운영 절차, 장애 대응 |
| 2.10 시스템 및 서비스 보안관리 | 인터넷 노출 시 공격면 발생 | HTTP hardening, request size limit, panic recovery, secure headers 검토 |
| 2.11 사고 예방 및 대응 | key/prompt/result 유출 가능 | 탐지, 통지, 격리, 재발방지 절차 |
| 2.12 재해복구 | gateway 장애는 LLM 호출 경로 장애 | 복구 목표, config/secret 복구, NATS stream 복구 |
| 3. 개인정보 처리단계별 요구사항 | prompt/response에 개인정보 가능 | 수집 최소화, 이용 목적, 제공/국외이전, 파기, 정보주체 권리 대응은 운영자 서비스 정책에 반영 |

인증 자체는 운영자가 ISMS-P 의무대상자인지에 따라 달라진다. 이 리포만으로 의무대상 여부를 확정할 수 없다. 다만 KISA는 의무대상자가 아니어도 자율 신청자가 인증심사를 받을 수 있다고 안내한다: [KISA 인증대상](https://isms.kisa.or.kr/main/ispims/target/).

### SOC 2

| Category | 적용 | 판단 |
|---|---|---|
| Security | 필수 | gateway 인증, secret, 접근통제, 변경관리, 취약점 대응의 공통 기준 |
| Confidentiality | 필수 | prompt/response, vendor key, consumer key, result event가 기밀정보 |
| Availability | 조건부 필수 | 운영자가 LLM routing SLA, 장애 대응, fallback/circuit breaker 효과를 고객에게 약속하면 포함 |
| Privacy | 조건부 필수 | prompt/response/result event에 개인정보가 포함되거나 고객에게 개인정보 처리자로 설명하면 포함 |
| Processing Integrity | 조건부 | alias/fallback, model allowlist, audit correctness, result event completeness를 고객에게 보장해야 하면 포함 |

## Source register

- AICPA & CIMA, [2017 Trust Services Criteria with revised points of focus 2022](https://www.aicpa-cima.com/resources/download/2017-trust-services-criteria-with-revised-points-of-focus-2022?Jid=CppDev20110217), resource page dated 2023-09-30. Used as SOC 2 criteria source. Valid until AICPA supersedes the criteria.
- AICPA & CIMA, [SOC 2 Reporting guide](https://www.aicpa-cima.com/cpe-learning/publication/soc-2-reporting-on-an-examination-of-controls-at-a-service-organization-relevant-to-security-availability-processing-integrity-confidentiality-or-privacy-OPL), updated as of 2022-10-15. Used as SOC 2 engagement/reporting context.
- KISA, [ISMS-P 제도소개](https://isms.kisa.or.kr/main/ispims/intro/), checked 2026-05-26. Used for certification purpose, legal basis, and criteria count.
- KISA, [ISMS-P 인증기준 안내서(2023.11) 수정게시](https://isms.kisa.or.kr/main/ispims/notice/?boardId=bbs_0000000000000014&cntId=21&mode=view), posted 2023-11-23. Used as latest located certification criteria guide.
- KISA, [ISMS-P 신청절차](https://isms.kisa.or.kr/main/ispims/request), checked 2026-05-26. Used for certification validity: 3 years with annual surveillance review.
- KISA, [ISMS-P 인증대상](https://isms.kisa.or.kr/main/ispims/target/), checked 2026-05-26. Used for mandatory-vs-voluntary certification note.
- 국가법령정보센터, [개인정보 보호법](https://www.law.go.kr/LSW/lsInfoP.do?ancYnChk=0&chrClsCd=010202&efYd=20251002&lsiSeq=270351&urlMode=lsInfoP), effective 2025-10-02, Law No. 20897, amended 2025-04-01. Used for statutory baseline.
- 국가법령정보센터, [개인정보의 안전성 확보조치 기준](https://law.go.kr/LSW/admRulLsInfoP.do?admRulId=73493&efYd=0), effective 2025-10-31, PIPC Notice No. 2025-9. Used for current safety measures baseline.
- 개인정보보호위원회, [개인정보의 안전성 확보조치 기준 안내서(2025.11)](https://www.pipc.go.kr/np/cop/bbs/selectBoardArticle.do?bbsId=BS217&mCode=D010030020&nttId=11641), posted 2025-11-28. Used as current explanatory guide.
