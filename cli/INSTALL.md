# Mac에 설치하고 시작하기

Apple Silicon(M1 이상) · macOS 15+가 필요합니다. Python·Rust는 설치하지 않아도 됩니다.
관리자가 worker 기능과 Caller를 배포한 LLMGate URL과, embedding/STT worker 권한이 있는 API 키를 준비하세요.
서버는 worker 설정으로 기본 모델과 `embedding`·`stt` alias를 자동 구성합니다. Mac마다 catalog 파일을 추가하거나 서버를 재시작하지 않습니다. API 호출 권한을 제한한 키라면 관리자가 최초에 이 두 alias도 허용해야 합니다.

## 1. 다운로드·설치

[CLI 0.1.1 Release](https://github.com/cagojeiger/llmgate/releases/tag/cli-v0.1.1)에서
`llmgate-cli-0.1.1-aarch64-apple-darwin.tar.gz`와 `SHA256SUMS`를 받습니다.

두 파일이 있는 디렉터리에서 실행하세요.

```sh
shasum -a 256 -c SHA256SUMS
tar -xzf llmgate-cli-0.1.1-aarch64-apple-darwin.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 755 llmgate-cli-0.1.1-aarch64-apple-darwin/llmgate-cli "$HOME/.local/bin/llmgate-cli"
export PATH="$HOME/.local/bin:$PATH"
llmgate-cli --version
```

새 터미널에서도 사용하려면 기본 macOS 셸인 zsh의 `~/.zshrc`에 다음 줄을 한 번 추가하세요.
다른 셸을 사용하면 해당 셸의 설정 파일에 추가합니다.

```sh
export PATH="$HOME/.local/bin:$PATH"
```

현재 Developer ID 서명·공증은 미검증입니다. macOS가 실행을 차단하면 배포자에게 서명된 패키지를 요청하세요.
CLI 교체 전 실행 중인 워커는 `llmgate-cli down --all`로 종료합니다. 키와 모델 캐시는 유지됩니다.

## 2. 등록 → 시작 → 확인

`https://llmgate.example`을 관리자가 준 서버 URL로 바꾸세요. API 키는 명령에 넣지 않고 숨김 입력합니다.

```sh
llmgate-cli register --url https://llmgate.example --profiles embedding stt
llmgate-cli start --all
llmgate-cli status
```

등록은 권한 확인과 키 저장입니다. 아직 모델을 실행하지 않습니다.
처음 `start`는 전용 Python·모델을 다운로드합니다. 네트워크와 디스크 공간이 필요하며 완료 시간은 환경에 따라 다릅니다.
`start --all`은 등록된 profile만 시작합니다. 설치 후 모델 준비와 Relay 연결이 끝나면 **서빙 준비 완료**를 표시합니다.
기본 대기 시간은 profile마다 설치 이후 660초입니다. `--wait-timeout 120`처럼 바꿀 수 있습니다.

시간 초과 또는 Ctrl+C로 **백그라운드 시작 대기**를 중단해도 워커는 계속 실행·재연결합니다.
`status`로 확인하거나 `down --all`로 종료하세요. `--foreground`에서는 Ctrl+C·준비 시간 초과 시 워커도 종료합니다.
`--local-only`는 로컬 모델 준비까지만 확인하며 LLMGate에 공개하지 않습니다.
Relay 연결 완료는 Mac의 모델·Listener 준비 확인입니다. 이후 `/v1/embeddings`에 `model=embedding`, `/v1/audio/transcriptions`에 `model=stt`로 호출합니다. start는 실제 API 결과까지 검사하지는 않습니다.

## 3. 문제가 생기거나 종료할 때

```sh
llmgate-cli logs embedding  # 시작·인증·감독 로그 + 모델 로그
llmgate-cli logs stt
llmgate-cli down --all      # 실행 환경 삭제, 모델·package 캐시 유지
llmgate-cli cache list
llmgate-cli cache clean     # 모두 종료한 뒤 캐시 삭제
```

`down` 이후에는 다음 `start`에서 캐시를 사용해 실행 환경을 재설치합니다.
등록 정보와 Keychain의 키까지 지우려면 `unregister`를 실행하세요. 서버 API 키 자체는 폐기하지 않습니다.

| 표시되는 문제 | 다음 행동 |
| --- | --- |
| HTTP 401 | API 키를 확인하고 다시 등록 |
| HTTP 403 | 관리자에게 키의 `allowed_worker_profiles` 권한 요청 |
| HTTP 404 | 서버 URL 및 worker 기능 배포 여부 확인 |
| 모델 준비 중 | 다운로드·로드를 기다리거나 해당 profile의 logs 확인 |
| Relay 연결 대기 | 네트워크·Gateway 상태·서버 인증 설정 확인 |
| 로그 없음 | 아직 시작하지 않았거나 설치 단계에서 실패한 상태. start 출력 확인 |

자동화는 `llmgate-cli status --json`을 사용합니다. 기존 profile별 NDJSON 형식을 유지합니다.
