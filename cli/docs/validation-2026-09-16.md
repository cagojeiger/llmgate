# 로컬 MLX 검증 — 2026-09-16

LLMGate 소스의 Docker Compose build → 서버 Caller sidecar → TLS RelayGate → native MLX 경로를 실제 호출했다.
범위는 Qwen3 Embedding·Qwen3 ASR 파일 STT다.

| 항목 | 환경 / 결과 |
| --- | --- |
| 호스트 | Apple Silicon, macOS 26.6.2, 메모리 36 GiB |
| CLI | 배포 tar.gz에서 추출한 Rust release arm64, published relaygate-sdk 0.5.1 |
| LLMGate | local-mlx-validation, 로컬 소스 Docker build |
| embedding float / base64 | 각각 2개 × 1024차원, 유한한 float 확인 |
| 파일 STT JSON / SSE | 영어 WAV에서 “Hello. This is a local speech recognition test.”, SSE 10 delta |
| OpenClaw 2026.9.4 | 실제 config validate, memory index/search, infer audio transcribe 통과 |
| embedding 경계 | 2048토큰 성공 / 2049 거부, batch 8192 성공 / 10240 거부 |
| STT 경계 | 60초 성공 / 61초 3회 연속 400, prompt 257토큰 400 |
| 공유 추론 lock | embedding·STT 각각 다른 추론 점유 시 429 |
| 메모리 | 두 모델·Rust supervisor 합산 peak 2,955,710,400 bytes ≈ 2.75 GiB (경계값 테스트) |
| 연결·복구 | max_connections=1의 추가 Pipe 거부, TTL 60초 만료 후 호출·Gateway 재시작 후 재공개/호출 성공 |
| 종료 | down 후 runtime 삭제·port 종료, unregister 후 credential·등록 설정 제거 |

메모리는 Darwin physical footprint의 250ms 샘플링이다. Docker·설치·OpenClaw 테스트 도구는 제외했다.
4 GiB는 관측 기반 보호 목표이며 순간 초과를 막는 OS 강제 상한이 아니다.
60초 경계는 짧은 영어 WAV를 반복한 fixture이며 자연스러운 장시간 발화 품질·최대 출력 길이의 실증을 뜻하지 않는다.
OpenClaw는 임시 state/config를 사용했고 WAV 파일을 호출했다. OGG/Opus·한국어 정확도는 검증하지 않았다.

## 인증·수명주기

- Keychain의 기존 API 키 → Go publish JWT → Gateway 승인 → Listener 공개를 확인했다.
- 잘못된 API 키 401, dial action 주입 400을 확인했다. 단위 테스트는 caller-only 등록 거부·서명·scope를 검사한다.
- 전용 uv/Python·고정 dependency hash lock·model revision으로 설치했다.
- 앞선 같은 Mac 검증에서 중복 start 보존·실행 중 cache clean 거부를 확인했다.
- 운영 Caller image로 exact dial JWT를 매 admission마다 발급했다. 두 worker의 최초 publish 이후 62초를 기다린 뒤 Gateway를 재시작했고, Caller·worker 프로세스를 재시작하지 않고 호출이 회복됐다.
- 64 KiB 전송 버퍼로 큰 음성 요청의 frame 폭증을 줄였다. 61초 거부를 3회 반복하고 다음 prompt 거부 응답까지 확인했다.
- 테스트 후 Compose와 Gateway/caller를 종료했다. 모델·package cache와 로그는 유지했다.

## 자동 검증

- Go: go test -race ./..., go vet ./..., golangci-lint 0 issues, govulncheck 호출 가능한 취약점 없음.
- Cassette E2E: 25 passed, 29 skipped. 모델 유형별 skip이며 전부 실행한 것으로 계산하지 않는다.
- Rust: fmt/check/test/clippy, published SDK probe build/clippy.
- Python: 기존 경계 5개 + 실제 CLI 프로세스의 강제 종료·lease 보존·busy/proxy readiness·legacy receipt 보존 4개, 합계 9개 테스트.
- 배포 package: arm64 release build, --version, tar.gz와 SHA256SUMS 확인. 압축에서 추출한 release 바이너리로 실제 추론·경계값·OpenClaw·종료를 재검증했다.

실행 코드: [e2e_macos.py](../scripts/e2e_macos.py), [verify_limits.py](../scripts/verify_limits.py), [verify_openclaw.py](../scripts/verify_openclaw.py).
실행 결과는 git 제외 경로 `.local/e2e-embedding-stt/{results,limits-results,openclaw-results}.json`에 저장한다.

운영 namespace/issuer/Secret의 실제 배포, 여러 물리 Mac/Gateway, launchd 자동 시작, Developer ID 서명·notarization은 이 검증에 포함하지 않는다.
서버 Caller는 구현·Compose 검증에 포함한다. supervisor SIGKILL은 모델의 생존 pipe로 종료하며, guardian이 멈춘 경우 상속 lease로 재시작·설치·삭제를 차단한다. 오래된 unclean receipt는 자동 덮어쓰지 않는다.

검증한 tar.gz SHA-256: `f9e3ccf495c2befbc80f6f89c98aecf2a365de5a42f9ce4646e18a656f89d238`. CI에서 새로 빌드한 파일은 별도 SHA256SUMS를 따른다.
