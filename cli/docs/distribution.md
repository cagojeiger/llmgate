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
# dist/llmgate-cli-0.1.1-aarch64-apple-darwin.tar.gz
# dist/SHA256SUMS
```

사용자 설치 절차와 현재 다운로드 경로는 [INSTALL.md](../INSTALL.md)에 있다.
패키지에도 INSTALL.md를 포함한다. PATH 설정, 등록·시작·상태 확인 및 오류별 다음 행동을 안내한다.

서명은 APPLE_SIGNING_IDENTITY와 인증서/암호 Secret이 구성되면 CI에서 적용한다. 현재 로컬에는 Developer ID가 없어 서명·공증 완료를 주장하지 않는다. 공증된 사용자 배포를 위해서는 별도 Apple 계정 자격 증명과 공증 검증이 필요하다. 서명 설정이 없으면 검토용 unsigned draft만 생성한다.
