# llmgate-cli

Apple Silicon macOS 15+에서 Qwen3 Embedding·Qwen3 ASR 파일 STT를 LLMGate에 연결한다.
LLMGate와 같은 저장소에서 리뷰하되 Rust 바이너리·Python 실행 환경은 Go 서버와 독립적이다.
RelayGate SDK 0.5.1은 crates.io에서 가져온다. CLI 자체는 crates.io에 게시하지 않는다.

## 모델과 제한

| profile | 고정 모델 | 제한 |
| --- | --- | --- |
| embedding | Qwen3 Embedding 0.6B 8bit | 문서당 2,048토큰, 요청 합계 8,192토큰, 최대 128개 문자열, 1024차원 |
| stt | Qwen3 ASR 0.6B 4bit | 파일 5 MiB·60초, prompt 256토큰, 출력 합계 1,024토큰 |

- 임베딩은 실제 tokenizer로 특수 토큰까지 계산한다. 초과 입력을 자동으로 자르지 않는다. float/base64를 지원한다.
- 파일 STT는 JSON 또는 `stream=true` SSE다. 실시간 음성 입력은 제외한다. `response_format=json`, `temperature=0`을 지원한다.
- STT는 30초 단위로 순차 추론한다. 출력 한도에 도달하면 불완전한 결과를 성공으로 처리하지 않는다.
- 음성은 miniaudio 지원 형식을 받으며 WAV로 실증했다. 모델 규격의 query instruction은 호출자가 구성한다.

## 사용

처음 사용하는 Mac에서는 [다운로드·PATH 설정·등록 안내](INSTALL.md)를 먼저 따라간다.

```sh
llmgate-cli register --url https://llmgate.example --profiles embedding stt
llmgate-cli start --all
llmgate-cli status
llmgate-cli logs stt
llmgate-cli down stt
llmgate-cli cache list
llmgate-cli cache clean
```

API 키는 숨김 입력 또는 `register --key-stdin`으로 받고 Keychain에 저장한다. JWT는 내부에서 발급·갱신해 메모리에만 둔다.
HTTPS+RelayGate TLS가 기본이며 `--allow-loopback-http`는 로컬 테스트용이다.
`start` 성공은 모델 준비·Relay 공개까지 확인했다는 뜻이다. `logs`는 supervisor와 모델 로그를 함께 보여준다.
등록 전 서버의 [worker 권한과 issuer 설정](../docs/worker-registration.md)이 필요하다.

| start 옵션 | 의미 |
| --- | --- |
| embedding / stt / --all | 선택 profile 또는 등록된 모든 profile 설치·시작 |
| --port N | 포트 지정. 기본 자동 선택, 충돌 시 기존 프로세스 보존 |
| --max-connections N | Relay Pipe 상한, 기본 8. 추론 동시성과 별개 |
| --connection-timeout SECONDS | Pipe 수명, 기본/최대 3600초 |
| --wait-timeout SECONDS | 설치 이후 준비·Relay 공개 대기, 기본 660초. 초과 시 오류를 반환하며 background 워커는 계속 실행 |
| --foreground | 터미널에서 감독. 기본 background supervisor |
| --local-only | 등록 없이 로컬 엔진만 실행 |
| --home PATH | 전용 관리 root 변경. 기본 ~/.llmgate |

## 메모리와 종료

- 모델별 load·추론은 한 건씩 실행한다. **임베딩 1건 + STT 1건은 동시에 처리**한다. 모델별 최대 4건(실행 1 + 대기 3)을 수용하고, 초과 요청 또는 5초 대기 만료는 429다. 대기는 모델 실행 슬롯을 늘리지 않는다. Relay Pipe는 기본 8건으로, 모델 수용 4건에 더해 완료된 연결의 비동기 정리 여유를 둔다. Pipe 상한과 추론 동시성은 다르다.
- 두 Python 모델 프로세스와 Rust supervisor의 합산 **평균 4 GiB**를 목표로 한다. Docker 서버·개발 도구·설치 작업은 별도다.
- Darwin physical footprint를 250ms마다 합산하고 최근 60초 표본 평균을 표시한다. 시작 후 60초 미만이면 수집된 기간만 평균한다. 평균 목표 초과만으로 종료하지 않는다.
- 별도 비상 보호선은 5.5 GiB 이상 신규 추론 거부, 6 GiB 초과 관측 시 모델 종료다. MLX cache는 프로세스당 64 MiB다. 이는 OS 강제 상한이나 호스트 전체 메모리 압력 감지가 아니며 샘플 사이 순간 초과는 가능하다.
- `status --json`의 memory에는 현재 합계·평균·관측 peak·target_bytes·stop_bytes·observed_seconds가 나온다. PID 재사용은 시작 시각으로 구분한다.
- `down`은 공개 철회·bounded drain·소유 process group/port 종료 확인 후 전용 Python runtime을 삭제한다.
- cache는 남는다. 실행/설치 중 `cache clean`은 거부하며 `down --all --purge`는 종료 후 cache도 지운다.
- `unregister`는 실행을 멈추고 Keychain·등록 설정을 지운다. 서버의 API 키 자체는 폐기하지 않는다.
- 모델은 engine/cache 잠금을 상속하며 supervisor가 사라지면 생존 pipe의 EOF로 종료한다. 남은 모델이 있으면 start/install/down/cache clean을 거부한다. 이전 버전의 불명확한 crash 기록은 덮어쓰지 않는다.
- 모델 장애는 최대 3회 재시작한다. transport 복구는 SDK 책임이다. 모델 로그는 profile당 10 MiB × 5개로 회전한다. supervisor 시작·종료 로그는 실행마다 회전해 최근 5개를 유지한다.

[동시 처리·메모리 실측](docs/capacity-2026-09-17.md).

## 빌드·검증

```sh
cd cli
cargo fmt --all --check
cargo test --locked
cargo clippy --all-targets -- -D warnings
python3 -m unittest discover -s tests -v
scripts/package-macos.sh
```

[다운로드·release 절차](docs/distribution.md) · [UX 검증](docs/validation-2026-09-17.md). PR CI가 tar.gz·SHA256SUMS를 artifact로 제공한다.

[통합 검증 스크립트](scripts/e2e_macos.py)는 native MLX·TLS RelayGate와 로컬 Go 소스의 Compose build를 사용한다.
테스트 전 Mac의 `say`/`afconvert`, Docker, 별도로 빌드한 RelayGate Gateway가 필요하다. Go와 운영 Caller는 Compose에서 함께 빌드한다.

```sh
cargo build --locked
cargo build --manifest-path tests/relay-bridge/Cargo.toml --locked
# private test venv에 scripts/test-requirements.txt 설치 후:
python scripts/e2e_macos.py --verify-limits --verify-recovery \
  --openclaw-node /path/to/supported/node \
  --openclaw-cli /path/to/openclaw/openclaw.mjs
```

`--cli-binary`로 검증할 CLI를, `--gateway-binary`로 Gateway를 지정할 수 있다. Gateway 경로 기본값은 인접 relaygate checkout의 `target/debug/relaygate-server`다. probe는 실제 worker bridge의 Pipe 상한을 검증하는 테스트 도구다. 요청 경로는 운영 Caller sidecar를 사용한다.
OpenClaw 2026.9.4에서 실제 memory index/search와 audio transcribe를 검증했다. [설정 예](docs/openclaw.md).
운영 namespace/issuer/Secret 배포·여러 물리 Mac·launchd는 이 로컬 검증과 별도다. [서버 Caller](../caller/README.md)의 구현과 Compose 배선은 포함한다.
MLX embedding은 mlx-embeddings 0.0.5, ASR은 MLX Audio 0.5.4를 사용하며 전이 의존성·모델 revision을 고정한다.
