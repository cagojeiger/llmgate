#!/usr/bin/env bash
#
# Generate one client registration: random raw key + sha256 hash + yaml.
# The raw key is printed once on stdout; the gateway never sees it again.
# Operator is responsible for delivering the raw key to the caller via a
# secret channel (vault / 1Password / encrypted DM).
#
# Usage:
#   scripts/gen-client.sh <name>
#
# The yaml is written to ${LLMGATE_CLIENTS:-clients}/<name>.yaml. Refuses
# to overwrite an existing file so a second invocation does not silently
# rotate a live caller's key.
#
# Naming rule (must match internal/clients/clients.go):
#   ^[a-z0-9][a-z0-9_-]{0,63}$
#
# Requires: openssl (or `head -c 32 /dev/urandom`) and shasum (or sha256sum).

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $(basename "$0") <name>" >&2
    exit 2
fi
NAME="$1"

if ! [[ "$NAME" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]]; then
    echo "error: name must match ^[a-z0-9][a-z0-9_-]{0,63}\$ (got: $NAME)" >&2
    exit 2
fi

DIR="${LLMGATE_CLIENTS:-clients}"
mkdir -p "$DIR"
OUT="$DIR/$NAME.yaml"
if [[ -e "$OUT" ]]; then
    echo "error: $OUT already exists; refusing to overwrite. Edit by hand to rotate keys." >&2
    exit 1
fi

# 256-bit random raw key (64 hex chars). openssl is preferred; fall back
# to /dev/urandom + xxd if openssl is missing.
if command -v openssl >/dev/null 2>&1; then
    RAW=$(openssl rand -hex 32)
else
    RAW=$(head -c 32 /dev/urandom | xxd -p -c 32)
fi

# sha256 of the raw key. shasum (BSD/macOS) and sha256sum (GNU) both work
# and emit the digest as the first column.
if command -v shasum >/dev/null 2>&1; then
    HASH=$(printf '%s' "$RAW" | shasum -a 256 | cut -d' ' -f1)
else
    HASH=$(printf '%s' "$RAW" | sha256sum | cut -d' ' -f1)
fi

cat > "$OUT" <<EOF
# 발급일: $(date +%Y-%m-%d)
name: $NAME
key_hashes:
  - sha256:$HASH
EOF

echo "✓ wrote $OUT"
echo
echo "Raw key (give to caller, gateway never sees it again):"
echo "  $RAW"
echo
echo "Test:"
echo "  curl -H 'Authorization: Bearer $RAW' http://localhost:8080/v1/chat/completions ..."
