#!/usr/bin/env bash
# Sync tests/e2e/fixtures/models/ with the catalog model id set. ADR 006.
#   --status   (default) — diff catalog vs fixture set, no mutation
#   --prune              — delete fixture dirs for models removed from catalog
#   --record             — call upstream and capture missing fixtures
#                          (json + sse per model). Skips files that exist
#                          and returns nonzero if any capture fails.
#                          Reads vendor key from .env. Defaults to
#                          LLMGATE_E2E_VENDOR=opencode; set to all or a
#                          comma-separated vendor list to record more.

set -euo pipefail

cd "$(dirname "$0")/.."
if [ -f .env ]; then
    set -a; source .env; set +a
fi

CATALOG_DIR="catalog/models"
FIXTURES_DIR="tests/e2e/fixtures/models"
RECORD_PROMPT="Count 1 to 5, one number per line."
RECORD_MAX_TOKENS=2048
VENDOR_FILTER="${LLMGATE_E2E_VENDOR:-opencode}"

mode="${1:---status}"
mkdir -p "$FIXTURES_DIR"

yaml_field_in_file() { grep -m1 "^$2:" "$1" 2>/dev/null | awk '{print $2}'; }

vendor_selected() {
    local vendor="$1" selected="$2" part
    [ "$selected" = "all" ] && return 0
    IFS=',' read -ra parts <<< "$selected"
    for part in "${parts[@]}"; do
        part="$(echo "$part" | xargs)"
        [ "$vendor" = "$part" ] && return 0
    done
    return 1
}

catalog_ids_for_vendor() {
    local path id vendor
    for path in "$CATALOG_DIR"/*.yaml; do
        [ -f "$path" ] || continue
        id=$(yaml_field_in_file "$path" id)
        vendor=$(yaml_field_in_file "$path" vendor)
        [ -n "$id" ] || continue
        vendor_selected "$vendor" "$VENDOR_FILTER" || continue
        echo "$id"
    done | sort -u
}

all_catalog_ids() {
    local path id
    for path in "$CATALOG_DIR"/*.yaml; do
        [ -f "$path" ] || continue
        id=$(yaml_field_in_file "$path" id)
        [ -n "$id" ] && echo "$id"
    done | sort -u
}

model_file_for_id() {
    local id="$1" path
    for path in "$CATALOG_DIR"/*.yaml; do
        [ -f "$path" ] || continue
        [ "$(yaml_field_in_file "$path" id)" = "$id" ] && { echo "$path"; return 0; }
    done
    return 1
}

# Catalog model id set for the selected vendor scope, sorted unique.
catalog_ids=$(catalog_ids_for_vendor)
catalog_ids_all=$(all_catalog_ids)

# Preserve namespaced model IDs when finding recorded fixtures.
fixture_ids=$(find "$FIXTURES_DIR" -type f -name 'chat-completion.json' \
    -not -path '*/.*' -exec dirname {} \; 2>/dev/null \
    | sed "s|^$FIXTURES_DIR/||" | sort -u || true)

added=$(comm -23 <(echo "$catalog_ids") <(echo "$fixture_ids" || echo ""))
removed=$(comm -13 <(echo "$catalog_ids_all") <(echo "$fixture_ids" || echo ""))

yaml_field() {
    local file
    file=$(model_file_for_id "$1") || return 1
    yaml_field_in_file "$file" "$2"
}

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

    local -a headers=(-H "$hdr" -H "Content-Type: application/json")
    if [ "$vendor" = "opencode" ]; then
        local session_id
        # OpenCode Go requires affinity. Share one session across both captures
        # for this model, without reusing it for unrelated models or runs.
        session_id=$(python3 -c 'import uuid; print(uuid.uuid4())') || return 1
        headers+=(-H "X-OpenCode-Session: $session_id")
    fi

    local url="${base_url%/}$path" target="$FIXTURES_DIR/$id"
    mkdir -p "$target" || return 1
    local body='{"model":"'$id'","messages":[{"role":"user","content":"'$RECORD_PROMPT'"}],"max_tokens":'$RECORD_MAX_TOKENS'}'
    local stream_body='{"model":"'$id'","messages":[{"role":"user","content":"'$RECORD_PROMPT'"}],"max_tokens":'$RECORD_MAX_TOKENS',"stream":true}'

    local json_path="$target/chat-completion.json"
    if [ ! -f "$json_path" ]; then
        if curl -fsS -X POST "$url" "${headers[@]}" \
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
        if curl -fsSN -X POST "$url" "${headers[@]}" \
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
        if [ -z "$added" ] && [ -z "$removed" ]; then
            echo "fixtures in sync with catalog vendor filter '$VENDOR_FILTER' ($(echo "$catalog_ids" | wc -l | tr -d ' ') models)"
        fi
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
        failed=0
        while IFS= read -r id; do
            [ -z "$id" ] && continue
            echo "recording $id:"
            record_one "$id" || failed=1
        done <<< "$catalog_ids"
        exit "$failed"
        ;;
    *)
        echo "usage: $0 [--status|--prune|--record]" >&2
        exit 2
        ;;
esac
