# ADR 010: 기존 consumer 키로 worker publish 권한을 발급한다

- Status: Accepted
- Date: 2026-09-16
- Supersedes: ADR 003 (인증 경계). 기존 caller API 키·해시 저장·alias 정책은 유지한다.

| 책임 | 결정 |
| --- | --- |
| caller 인증 | 기존 Bearer API 키. OAuth·caller JWT 추가 없음 |
| worker 권한 | consumer의 allowed_worker_profiles. 빈 값은 거부 |
| 발급 API | POST /v1/workers/token. backend가 exact Destination의 publish JWT만 발급 |
| 키 | 개인키는 backend 파일 Secret, 공개키는 RelayGate namespace 설정 |
| 장비 | 중앙 등록 DB·관리자 웹·개별 장비 credential은 추가하지 않음 |
| 회수 | token은 admission-only. 기존 Binding·Pipe 즉시 회수 보장 없음 |

LLMGate는 local runtime을 관리하지 않는다. 모델 설치·health·종료는 Mac agent가 소유한다.
issuer는 선택 기능이며 LLMGATE_WORKERS_CONFIG가 없으면 route를 설치하지 않는다.
caller's dial 권한은 별도 내부 공급 경로의 책임이며 worker API로 발급하지 않는다.
