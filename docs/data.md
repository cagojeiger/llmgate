# 카탈로그 / 호출자 yaml

← [architecture.md](architecture.md) 로 돌아가기

운영자가 게이트웨이를 다루는 *데이터 평면* 두 갈래 — vendor 등록 (`catalog/`) 과 호출자
등록 (`consumers/`). 둘 다 yaml, 코드 0줄, 부재시 부팅 fail.

## 디렉토리 형태

```
catalog/                              consumers/
├── models/<id>.yaml                  └── <name>.yaml
│      id + vendor + protocol            name +
│      + base_url + auth_env             key_hashes (sha256:hex64)
│      + auth_scheme                     [raw 키는 디스크 미존재]
└── aliases/<name>.yaml
       alias + chain
```

## 샘플

```yaml
# catalog/models/deepseek-v4-flash.yaml
id: deepseek-v4-flash
vendor: opencode
protocol: openai
base_url: https://api.opencode.example/v1
auth_env: LLMGATE_OPENCODE_API_KEY
auth_scheme: bearer
```

```yaml
# catalog/aliases/smart.yaml
alias: smart
chain:
  - kimi-k2.6
  - deepseek-v4-pro
  - deepseek-v4-flash
```

```yaml
# consumers/acme-prod.yaml
name: acme-prod
key_hashes:
  - sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

## 정책

데이터 / 정책 / 코드가 세 자리에 산다 — yaml = 운영 데이터, env = 인프라·시크릿, 코드 = 알고리즘.

- **catalog**: 별명 호출만 chain 폴백 (raw model id 호출은 chain 길이 1, 폴백 발동 자체 없음). 모르는 필드 → 부팅 fail. 결정 근거 [ADR 002](adr/002-catalog-shape.md).
- **consumers**: `scripts/gen-consumer.sh` 가 raw 키 발급 → sha256 만 yaml 에 박음. multi-key 활성 가능 (회전 윈도우). 부재 / 빈 디렉토리 → 부팅 fail (닫힘 default). 결정 근거 [ADR 003](adr/003-consumers.md).
