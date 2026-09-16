# 빌드·다운로드

| 단계 | 산출물 |
| --- | --- |
| PR | macOS CLI CI의 `llmgate-cli-macos-arm64` artifact |
| `cli-vVERSION` tag | GitHub draft Release에 tar.gz·SHA256SUMS 첨부 |
| 게시 | draft의 검증 결과·서명 상태 확인 후 운영자가 게시 |

서버 VERSION·Docker release와 CLI Cargo.toml 버전·cli-v 태그는 독립적이다. CLI는 crates.io에 게시하지 않는다.

```sh
cd cli
scripts/package-macos.sh
# dist/llmgate-cli-0.1.0-aarch64-apple-darwin.tar.gz
# dist/SHA256SUMS
```

GitHub Release에서 두 파일을 같은 디렉터리에 내려받은 뒤:

```sh
shasum -a 256 -c SHA256SUMS
tar -xzf llmgate-cli-0.1.0-aarch64-apple-darwin.tar.gz
mkdir -p ~/.local/bin
install -m 755 llmgate-cli-0.1.0-aarch64-apple-darwin/llmgate-cli ~/.local/bin/llmgate-cli
~/.local/bin/llmgate-cli --version
```

Rust·Python 사전 설치는 사용자에게 요구하지 않는다. 최초 모델 설치는 네트워크와 디스크 공간이 필요하다.

서명은 APPLE_SIGNING_IDENTITY와 인증서/암호 Secret이 구성되면 CI에서 적용한다. 현재 로컬에는 Developer ID가 없어 서명·공증 완료를 주장하지 않는다. 공증된 사용자 배포를 위해서는 별도 Apple 계정 자격 증명과 공증 검증이 필요하다. 서명 설정이 없으면 검토용 unsigned draft만 생성한다.
