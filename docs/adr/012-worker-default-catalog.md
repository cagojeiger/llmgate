# ADR 012: worker profile에서 기본 catalog와 Caller route를 구성한다

- Status: Accepted
- Date: 2026-09-17
- Supersedes: ADR 011. ADR 002의 catalog 데이터·라우팅 정책 분리는 유지한다.

| 책임 | 결정 |
| --- | --- |
| 관리자 | issuer/Secret, Gateway 공개키 신뢰, 서버 Caller 배포, consumer 권한을 최초 구성 |
| Go LLMGate | 인증·인가·감사·publish JWT 발급. 설정된 profile의 고정 모델/alias를 시작 시 메모리에 구성 |
| 서버 Caller | 같은 workers.json으로 loopback route 구성, exact dial JWT 발급, SDK reconnect·전송 |
| Mac CLI | 기존 API 키로 register → start. 모델 설치·실행·publish. catalog 수정 없음 |
| 기본 profile | embedding → qwen3-embedding-0.6b, stt → qwen3-asr-0.6b. alias는 profile 이름 |
| 주소 | embedding 127.0.0.1:18081, stt 127.0.0.1:18082. profile caller_address로 변경, numeric loopback만 허용 |
| 충돌 | 기본 모델/alias와 기존 catalog 경로가 다르면 시작 실패. 기존 설정을 조용히 덮어쓰지 않음 |
| 권한 | allowed_worker_profiles와 allowed_aliases는 독립. 등록으로 호출 권한을 자동 확대하지 않음 |

일반 catalog YAML의 책임은 그대로 데이터이며 worker 기본값도 gateway 조립 단계가 주입하는 데이터다. YAML 없이 worker만 구성할 때도 models 디렉터리는 있어야 한다. profile/주소 변경은 서버 재시작으로 적용한다. Mac 추가·제거는 RelayGate의 현재 Binding으로 처리하므로 catalog hot reload나 동적 저장소가 필요 없다.

ADR 011의 전송·한도·배포 결정은 유지한다: 서버 Secret만 서명 키를 읽고 CLI에는 전달하지 않는다. Caller는 모든 route 합산 연결 상한·연결 수명·종료 drain 5초를 적용한다. 워커 없음은 unavailable이며 다음 요청이 새 dial을 한다. 전달 후 자동 replay는 없고 Linux worker·실시간 음성 입력은 범위 밖이다.
