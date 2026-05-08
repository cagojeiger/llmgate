# ADR 002: 카탈로그 — 데이터 디렉토리, 별명이 제어 단위

- Status: Accepted
- Date: 2026-05-02
- 관련: 003

## 문제

vendor 단위 yaml 한 장에 endpoints / models / aliases / fallback 다 들어가면 모델 추가 PR 과 alias 변경 PR 이 충돌하고, "fallback policy" 가 사실 셋 (적격 / 회로 / 디폴트) 을 묶어 catalog 패키지가 데이터·로더·정책을 동시에 든다.

## 결정

- `catalog/models/<id>.yaml` = 모델 1 개. 6 필드: `id` / `vendor` / `protocol` / `base_url` / `auth_env` (생략 시 `LLMGATE_<VENDOR>_API_KEY`) / `auth_scheme`. `protocol` 은 `llmtypes.Protocol` 닫힌 enum.
- `catalog/aliases/<name>.yaml` = chain. raw model id 호출은 chain 길이 1 → 폴백 발동 자체 없음 (별명이 *실제 제어 단위*).
- 정책 (`LLMGATE_FALLBACK_ON` / circuit / timeouts) 은 env. yaml 에 없음.
- 모델 메타 (cost / context window) 안 가짐 — 미지원 항목 architecture.md "의도적 미지원" 참조.
- schema flat. apiVersion / kind 헤더 안 둠.
- strict 파싱 — 모르는 필드 → 부팅 fail.
- `LLMGATE_CATALOG=<dir>` 또는 cwd 의 `./catalog`. hot-reload 없음, 재시작 적용.

## 이유

- 모델 추가 = 파일 1 개. PR diff 명확, 동시 PR 충돌 사라짐.
- yaml = 모델·별명, env = 정책, 코드 = 알고리즘. 운영자 시점이 단순.
- catalog 패키지 책임 = "yaml → 데이터" 1 개.

## 대가

- 같은 vendor N 모델이 base_url 등 N 번 반복. sync 도구가 자동 생성 가정.
- 별명별 정책 (alias 마다 다른 OnKinds) 표현 안 됨. 신호 뜨면 alias yaml override 로 확장.
