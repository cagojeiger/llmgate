# ADR 011: 서버 Caller sidecar가 dial과 토큰 발급을 소유한다

- Status: Superseded by ADR 012 (공유 profile 설정으로 기본 route·catalog 구성)
- Date: 2026-09-16
- Complements: ADR 010. worker API는 계속 publish만 허용한다.

| 책임 | 결정 |
| --- | --- |
| Go LLMGate | API 키 인증·권한·감사, worker publish JWT 발급 |
| 서버 Caller | loopback HTTP TCP → SDK Pipe, exact dial JWT를 매 admission마다 발급 |
| 서명 키 | 서버 Secret만 읽는다. CLI·모델에는 전달하지 않는다 |
| 설정 | Go와 같은 workers.json, 별도 caller.json의 profile → loopback port 매핑 |
| 장애 | Gateway 최초 연결 재시도, 이후 SDK reconnect. worker 없음은 503, 다음 요청에서 다시 dial |
| 한도 | 모든 route 합산 연결 상한, SDK live/pending 한도, 연결 수명, 종료 drain 5초 |
| 배포 | caller/ 별도 Rust package·서버 sidecar image. 서버 VERSION으로 Go image와 함께 빌드 |

공유 pod/network namespace 안의 loopback만 listen한다. 외부 HTTP 인증은 Go가 담당한다.
수신 payload를 해석하지 않으며 Pipe로 전달한 요청은 자동 재전송하지 않는다.
Mac CLI·MLX 모델의 Linux 지원을 추가하는 결정은 아니다.
