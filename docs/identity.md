# 정체성 — 상태가 어디 사는가 + 의도적 미지원

← [architecture.md](architecture.md) 로 돌아가기

게이트웨이가 *무엇을 안 하는가* 가 정체성의 절반. 풀-피처 LLM 게이트웨이의 운영 면적을
*한 사람 머리에 들어오는 작은 컴포넌트* 로 좁힌 결과로서의 두 표.

## 상태가 어디 사는가

| 데이터 | 위치 | 수명 |
|---|---|---|
| 모델 / 별명 | `catalog/` yaml | 외부 갱신 시 재시작 |
| 호출자 등록 (해시만) | `consumers/` yaml | 외부 갱신 시 재시작 |
| 호출자 raw 키 | **gateway 보관 안 함** (호출자 측 vault) | — |
| 라우팅 정책 + 서버 런타임 | env → Server config | 프로세스 수명 |
| 회로 차단 상태 | `llmrouter.Service` breakerStore (per-process) | 프로세스 수명 |
| 호출자 lookup | consumers Store (per-process) | 프로세스 수명 |
| 요청별 시도 이력 | Result → Record | 요청 1 회 |
| 감사 record | Sink 정책 따라 | Sink 정책 |
| 비용 / 한도 / 단가 | **gateway 보관 안 함** | 후처리 시스템 |

## 의도적 미지원

V1 에서 다음을 *지원하지 않는다*. 디폴트는 거절 — 외부 요청에 같은 잣대로 응답한다.

| 항목 | 거절 근거 |
|---|---|
| **mid-stream 폴백** | HTTP 시맨틱 + SDK 호환 + record 무결성 셋 동시 위반. 첫 chunk 송출 후 vendor 교체 안 함 ([ADR 004](adr/004-fallback-policy.md)) |
| **capability matching / `/v1/models` discovery** | 게이트웨이가 모델 능력을 *판정* = 파생 값 생성. *사실만 발행* 위반. 능력은 호출자가 안다 |
| **hot-reload** | yaml 갱신 실시간 반영 시 *언제 반영됐나* 가 흐려지고 같은 호출이 부분 적용된 catalog 를 보는 race. 재시작 = 적용 |
| **모델 메타 (가격 / context window / capabilities)** | yaml 에 안 박음. destination 없음 — 가격은 후처리 시스템이 record 받아 계산 |
| **multi-key smart distribution** | 키별 사용량 상태 = *상태 없음* 위반. 같은 vendor model 을 다른 yaml 두 장에 다른 인증으로 등록하면 catalog 에서 별개 모델 → alias chain 자연 단위 ([ADR 002](adr/002-catalog-shape.md) 부수 효과) |
| **사전 한도 차단 (pre-call quota)** | 외부 상태 조회 = 상태 없음 위반. 한도는 후처리 시스템이 record 모아 판정 |
| **k8s · CRD 인지 / 게이트웨이 안 operator** | *환경 의존* 이 게이트웨이 정체성에 새로 들어옴. operator 가 필요해지면 *외부* 컴포넌트가 yaml 굴림 — 게이트웨이는 mount path 만 본다 |

"이 정도면 가벼우니" 의 누적 압력 — 항목이 늘면 본 문서 갱신으로 응답.
