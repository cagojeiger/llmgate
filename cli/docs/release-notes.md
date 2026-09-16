Apple Silicon macOS 15+용 Qwen3 Embedding·Qwen3 ASR 파일 STT 워커입니다.

- 압축 파일과 SHA256SUMS를 함께 받아 `shasum -a 256 -c SHA256SUMS`로 검사합니다.
- Python·MLX·모델은 최초 start에서 별도로 내려받습니다. API 키·JWT·모델은 배포 압축에 포함하지 않습니다.
- 이 release는 검토용 draft입니다. 게시 전 서명·공증 여부와 실제 모델 검증 결과를 확인하고 이 문장을 최종 결과로 교체합니다.
- 모델 메모리 목표는 관리 home당 합산 최근 60초 평균 4 GiB입니다. 임베딩과 STT는 각각 한 건씩 동시 처리하며 비상 종료 보호선은 6 GiB입니다. 입력·동시성 제한과 250ms 감시를 사용하며 OS 강제 상한은 아닙니다.
