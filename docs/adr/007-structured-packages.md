# 007. 구조화된 패키지 레이아웃

## 상태

수락됨

## 맥락

Go 프로젝트는 보통 패키지를 얕고 평평하게 두다가, 패키지가 충분히 커졌을 때 나눈다.
llmgate 는 이 관성을 의도적으로 따르지 않는다. 패키지 경로는 Go 컴파일 단위이기 전에
시스템 경계와 소유권을 먼저 드러내야 한다.

현재 게이트웨이는 얕은 패키지 안에서 쉽게 섞일 수 있는 세 가지 관심사를 가진다.

- 도메인 규칙과 계약 (`llmtypes`, 라우팅, finalized result schema)
- 플랫폼 adapter (HTTP, NATS, Prometheus, upstream HTTP/SSE)
- 애플리케이션 조립 (부팅, 설정 조립, shutdown)

NATS 결과 발행 작업이 이 불일치를 드러냈다. NATS publisher 는 generic transport
추상화가 아니라 finalized result delivery boundary 뒤에 붙는 플랫폼 기반 sink 다.

## 결정

목표 방향으로 명시적인 세 밴드 패키지 구조를 사용한다.

```text
internal/domain/     도메인 계약, 라우팅 규칙, durable event 모델
internal/platform/   HTTP, NATS, Prometheus, upstream network adapter
internal/app/        부팅 조립, provider 생성, shutdown
```

기존 패키지를 한 번에 모두 옮기지 않는다. 리팩토링은 경계가 분명한 component 하나씩 이동하고,
동작을 보존하며, import 와 테스트를 같은 PR 에서 함께 갱신한다.

## 결과

- 소유권이 더 명확해진다면 디렉토리 깊이가 늘어나는 것을 허용한다.
- "Go니까 평평하게 둔다"는 이 프로젝트의 컨벤션이 아니다.
- 패키지 이동 자체는 코드 다이어트로 보지 않는다. 읽는 사람이 봐야 할 범위가 줄거나,
  잘못된 소유권 이름이 사라질 때만 의미 있는 다이어트로 본다.
- 새 인프라 기반 구현은 순수 도메인 코드가 아닌 한 `platform/*` 아래에 둔다.
- 새 비즈니스 규칙과 안정적인 wire-shaped contract 는 adapter 전용 코드가 아닌 한
  `domain/*` 아래에 둔다.

## 마이그레이션 순서

1. 컨벤션을 문서화하고 낡은 구조 문서를 지운다.
2. delivery 코드를 `internal/platform/http/*` 쪽으로 이동한다.
3. LLM result schema / assembly 를 `internal/domain/llmresult/*` 쪽으로 이동한다.
4. NATS result publisher 를 `internal/platform/nats/llmresult` 쪽으로 이동한다.
5. 런타임 조립 코드를 `internal/app` 으로 옮겨 `cmd/llmgate` 를 얇게 만든다.
