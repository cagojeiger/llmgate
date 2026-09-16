# 로컬 Mac 임베딩 pool (초기 버전)

`POST /v1/embeddings`에 기존 consumer 인증, allowed_aliases, audit-always,
call telemetry와 선택적 result sink를 적용한다. 입력은 문자열 또는 최대 128개의
비어 있지 않은 문자열 배열이다. token ID 배열은 지원하지 않는다.
`encoding_format`은 float/base64, `dimensions`는 upstream 전달 및 응답 검증용이다.
임베딩 응답은 개수, index, 차원, 유한한 수를 확인하고 최대 32 MiB로 제한한다.

[Mac worker CLI](../cli/README.md)로 MLX를 설치·공개한다. 로컬 통합 검증은 CLI의 Compose·caller probe를 사용한다.
모든 worker와 connect bridge는 같은 Gateway 인스턴스에 연결한다.

카탈로그에 다음 파일을 추가하고 LLMGate를 재시작한다. 이 예제 파일은 기본
운영 catalog에 자동 등록하지 않는다.

```yaml
# catalog/models/qwen3-embedding-0.6b.yaml
id: qwen3-embedding-0.6b
vendor: local-mlx
protocol: openai
api: embeddings
base_url: http://127.0.0.1:18080/v1
```

```yaml
# catalog/aliases/embed.yaml
alias: embed
chain:
  - qwen3-embedding-0.6b
```

```sh
curl --fail http://127.0.0.1:8080/v1/embeddings \
  -H "Authorization: Bearer $LLMGATE_CONSUMER_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"embed","input":["안녕하세요","hello"],"encoding_format":"float"}'
```

필요하면 consumer의 `allowed_aliases`에 `embed`를 추가한다. 외부 upstream이
인증을 요구하면 `auth_env`와 `auth_scheme`을 명시한다. 명시한 credential이 없으면
시작을 거부한다. 로컬 unauthenticated 모델은 둘 다 생략한다.

embedding alias는 target 하나만 가진다. 다른 모델로 fallback하면 기존 인덱스의
vector space가 달라질 수 있으므로 다중 target alias는 시작 시 거부한다.
같은 모델의 worker 선택은 RelayGate가 담당한다. HTTP/1 연결을 요청마다 닫아
새 Pipe를 만들며, 요청 replay/자동 retry는 없다. embedding 오류에도 기존 breaker
설정을 적용한다.

CLI의 Qwen3 0.6B 8-bit adapter는 1024차원을 사용한다. `dimensions`는 생략하거나 1024를 지정한다.
문서당 2048·요청 합계 8192토큰을 실제 tokenizer로 검사하고 초과 입력을 거부한다.
query instruction/문서 전처리·청킹은 호출자가 소유한다.

역할별 파일:

```text
internal/domain/llmtypes/embedding.go          요청·응답·provider 계약
internal/domain/routing/embedding.go           단일 모델 선택·timeout·breaker
internal/platform/providers/openai/
  embedding.go                                upstream HTTP 전송
  embedding_response.go                       vector 응답 검증
internal/platform/http/embeddings/
  handler.go                                  인증·정책·감사 기록
  request.go                                  bounded JSON 입력
  complete.go                                 HTTP 응답
internal/domain/llmresult/schema/embedding.go   선택적 원문 결과 기록
internal/app/gateway/embedding.go              catalog → adapter 조립
```

CLI는 모델 장애 시 공개를 철회하고 제한된 재시작을 수행하며 SDK token source에서 JWT를 갱신한다.
실제 여러 Mac/Gateway의 분산·장애 부하 검증과 launchd 운영은 후속이다. 모델별 처리량/정확도 비교도 별도다.
