#!/usr/bin/env bash
# Sync tests/e2e/fixtures/models/ with the catalog model id set. ADR 006.
#   --status   (default) — diff catalog vs fixture set, no mutation
#   --prune              — delete fixture dirs for models removed from catalog
#   --record             — call upstream and capture missing fixtures
#                          (json + sse per model). Skips files that exist.
#                          Reads vendor key from .env.

set -euo pipefail

cd "$(dirname "$0")/.."
if [ -f .env ]; then
    set -a; source .env; set +a
fi

CATALOG_DIR="catalog/models"
FIXTURES_DIR="tests/e2e/fixtures/models"
RECORD_PROMPT="Count 1 to 5, one number per line."
RECORD_MAX_TOKENS=128

mode="${1:---status}"
mkdir -p "$FIXTURES_DIR"

# Catalog model id set, sorted unique.
catalog_ids=$(grep -h '^id:' "$CATALOG_DIR"/*.yaml | awk '{print $2}' | sort -u)

# Existing fixture dirs (skip dotfiles — .omc scratch etc.).
fixture_ids=$(find "$FIXTURES_DIR" -mindepth 1 -maxdepth 1 -type d \
    -not -name '.*' -exec basename {} \; 2>/dev/null | sort -u || true)

added=$(comm -23 <(echo "$catalog_ids") <(echo "$fixture_ids" || echo ""))
removed=$(comm -13 <(echo "$catalog_ids") <(echo "$fixture_ids" || echo ""))

yaml_field() { grep -m1 "^$2:" "$CATALOG_DIR/$1.yaml" 2>/dev/null | awk '{print $2}'; }

record_one() {
    local id="$1"
    local proto vendor base_url auth_env auth_scheme key
    proto=$(yaml_field "$id" protocol)
    vendor=$(yaml_field "$id" vendor)
    base_url=$(yaml_field "$id" base_url)
    auth_env=$(yaml_field "$id" auth_env)
    auth_scheme=$(yaml_field "$id" auth_scheme)
    [ -z "$auth_env" ] && auth_env="LLMGATE_$(echo "$vendor" | tr 'a-z' 'A-Z')_API_KEY"
    key="${!auth_env:-}"
    [ -z "$key" ] && { echo "  ✗ $id: env $auth_env not set"; return 1; }

    local path hdr
    case "$proto" in
        openai)    path="/chat/completions" ;;
        anthropic) path="/messages" ;;
        *) echo "  ✗ $id: unknown protocol $proto"; return 1 ;;
    esac
    case "$auth_scheme" in
        bearer)    hdr="Authorization: Bearer $key" ;;
        x-api-key) hdr="X-Api-Key: $key" ;;
        *) echo "  ✗ $id: unknown auth_scheme $auth_scheme"; return 1 ;;
    esac

    local url="${base_url%/}$path" target="$FIXTURES_DIR/$id"
    mkdir -p "$target"
    local body='{"model":"'$id'","messages":[{"role":"user","content":"'$RECORD_PROMPT'"}],"max_tokens":'$RECORD_MAX_TOKENS'}'
    local stream_body='{"model":"'$id'","messages":[{"role":"user","content":"'$RECORD_PROMPT'"}],"max_tokens":'$RECORD_MAX_TOKENS',"stream":true}'

    local json_path="$target/chat-completion.json"
    if [ ! -f "$json_path" ]; then
        if curl -fsS -X POST "$url" -H "$hdr" -H "Content-Type: application/json" \
                -d "$body" -o "$json_path"; then
            echo "  ✓ $id non-stream"
        else
            rm -f "$json_path"; echo "  ✗ $id non-stream failed"; return 1
        fi
    else
        echo "  · $id non-stream exists"
    fi

    local sse_path="$target/chat-completion.sse"
    if [ ! -f "$sse_path" ]; then
        if curl -fsSN -X POST "$url" -H "$hdr" -H "Content-Type: application/json" \
                -H "Accept: text/event-stream" -d "$stream_body" -o "$sse_path"; then
            echo "  ✓ $id stream"
        else
            rm -f "$sse_path"; echo "  ✗ $id stream failed"; return 1
        fi
    else
        echo "  · $id stream exists"
    fi
}

case "$mode" in
    --status)
        if [ -n "$added" ]; then
            echo "+ catalog has these models, fixture missing:"
            echo "$added" | sed 's/^/  /'
            echo "  → record: ./scripts/refresh-fixtures.sh --record"
        fi
        if [ -n "$removed" ]; then
            echo "- catalog removed these models, fixture stale:"
            echo "$removed" | sed 's/^/  /'
            echo "  → prune: ./scripts/refresh-fixtures.sh --prune"
        fi
        [ -z "$added" ] && [ -z "$removed" ] && \
            echo "fixtures in sync with catalog ($(echo "$catalog_ids" | wc -l | tr -d ' ') models)"
        ;;
    --prune)
        [ -z "$removed" ] && { echo "no stale fixtures"; exit 0; }
        while IFS= read -r id; do
            [ -z "$id" ] && continue
            target="$FIXTURES_DIR/$id"
            [ -d "$target" ] && { echo "removing $target"; rm -rf "$target"; }
        done <<< "$removed"
        ;;
    --record)
        while IFS= read -r id; do
            [ -z "$id" ] && continue
            echo "recording $id:"
            record_one "$id" || true
        done <<< "$catalog_ids"
        ;;
    *)
        echo "usage: $0 [--status|--prune|--record]" >&2
        exit 2
        ;;
esac
