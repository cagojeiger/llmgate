# OpenClaw 연결

OpenClaw 2026.9.4의 실제 CLI로 LLMGate 경유 memory index/search와 파일 STT를 검증했다.
LLMGate alias `embedding`, `stt`가 각각 해당 모델을 가리키고 API 키의 allowed_aliases에 포함돼 있어야 한다.

```json
{
  "memory": {
    "search": {
      "provider": "openai-compatible",
      "model": "embedding",
      "fallback": "none",
      "remote": {
        "baseUrl": "https://llmgate.example/v1",
        "apiKey": "${LLMGATE_API_KEY}"
      }
    }
  },
  "models": {
    "providers": {
      "openai": {
        "baseUrl": "https://llmgate.example/v1",
        "api": "openai-completions",
        "apiKey": "${LLMGATE_API_KEY}",
        "models": [{"id": "stt", "name": "File STT"}]
      }
    }
  },
  "tools": {
    "media": {
      "audio": {"enabled": true},
      "models": [{
        "provider": "openai", "model": "stt",
        "baseUrl": "https://llmgate.example/v1",
        "capabilities": ["audio"], "timeoutSeconds": 180
      }]
    }
  }
}
```

기존 OpenClaw 설정에 병합한다. 이미 openai provider로 채팅을 사용 중이라면 이 예제로 전체 provider 설정을 덮어쓰지 않는다. 오디오 전용 auth profile과 media entry를 사용한다.
localhost 테스트에서는 격리한 OpenClaw config의 해당 provider에만 `request.allowPrivateNetwork=true`를 넣었다. 운영 public HTTPS 연결에는 이 예외가 필요하지 않다.

```sh
openclaw config validate
openclaw memory index --force
openclaw memory search --query '검색할 내용' --json
openclaw infer audio transcribe --file speech.wav --model openai/stt --json
```

인덱스를 다시 만들기 전 기존 embedding 모델·차원 변경 여부를 확인한다. 입력 제한에 걸리는 문서는 나누어 인덱싱한다.
재현 스크립트: [verify_openclaw.py](../scripts/verify_openclaw.py). API 키는 테스트 프로세스 환경으로만 넘기며 결과 파일에 기록하지 않는다.
