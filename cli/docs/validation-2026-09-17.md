# CLI UX 검증 — 2026-09-17

| 항목 | 확인 결과 |
| --- | --- |
| 시작 완료 | 실제 embedding/STT의 start 반환 직후 runtime=ready·publish=active |
| 준비 대기 | 모델 준비와 Relay 공개 중 하나라도 미완료면 성공하지 않음 |
| 대기 중단 | 시간 초과·Ctrl+C는 실패 반환, 기존 background 워커는 유지 |
| profile 선택 | start --all은 등록된 profile만 대상 |
| 로컬 전용 | Relay 공개를 성공했다고 표시하지 않음 |
| 상태 | 기본 표·status --json NDJSON, 이전 ready 기록을 현재 서빙으로 표시하지 않음 |
| 오류·로그 | 401/403/404별 다음 행동, supervisor와 모델 로그, 빈 로그 안내 |
| 설치 | tar.gz checksum·INSTALL.md 동봉·임시 PATH 설치·--version 확인 |
| 실제 API | 로컬 Compose Go/Caller → TLS Gateway → MLX embedding float/base64·파일 STT JSON/SSE |
| 복구·호환 | JWT 만료 후 Gateway 재시작 복구, OpenClaw memory index/search·audio transcribe |

Rust fmt/build/test/clippy, Python 기존 경계·수명주기 9개와 UX 9개 테스트를 검증했다.
실제 API 테스트는 패키지에서 추출한 CLI로 실행했고 down/unregister·Compose 종료까지 확인했다.
사용자 안내 수정 후 패키지는 재빌드하여 checksum·PATH 설치·첫 실행 오류를 다시 검사했다.

검증 코드: tests/test_ux.py, tests/test_lifecycle.py, scripts/e2e_macos.py.
start 성공은 Mac 모델·Relay Listener의 준비 확인이다. 운영 catalog·API까지 자동 검사하지 않는다.
운영 배포·정식 Release 게시·여러 물리 Mac·Developer ID 서명/공증은 이번 UX 검증 범위가 아니다.
