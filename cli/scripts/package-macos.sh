#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || { echo 'Apple Silicon macOS is required' >&2; exit 1; }
export MACOSX_DEPLOYMENT_TARGET=15.0
cargo build --release --locked
version=$(awk -F '"' '/^version = / {print $2; exit}' Cargo.toml)
name="llmgate-cli-${version}-aarch64-apple-darwin"
mkdir -p dist
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir "$stage/$name"
install -m 755 target/aarch64-apple-darwin/release/llmgate-cli "$stage/$name/llmgate-cli"
cp README.md "$stage/$name/README.md"
cp INSTALL.md "$stage/$name/INSTALL.md"
if [[ -n "${APPLE_SIGNING_IDENTITY:-}" ]]; then
  codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$stage/$name/llmgate-cli"
  codesign --verify --strict --verbose=2 "$stage/$name/llmgate-cli"
fi
"$stage/$name/llmgate-cli" --version
COPYFILE_DISABLE=1 tar -C "$stage" -czf "dist/$name.tar.gz" "$name"
(cd dist && shasum -a 256 "$name.tar.gz" > SHA256SUMS && shasum -a 256 -c SHA256SUMS)
printf 'Package: dist/%s.tar.gz\n' "$name"
