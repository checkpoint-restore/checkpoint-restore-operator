#!/usr/bin/env bash
# Generate an ephemeral armored GPG key pair for forensic signature e2e tests.
# The private key is written to test/data/forensic-signing-key.asc and must never
# be used outside CI/local operator testing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${SCRIPT_DIR}/data/forensic-signing-key.asc"
OUT_PUB="${SCRIPT_DIR}/data/forensic-signing-key.pub.asc"

if [ -f "$OUT" ] && [ -f "$OUT_PUB" ]; then
  exit 0
fi

if ! command -v gpg >/dev/null 2>&1; then
  echo "gpg is required to generate forensic signing test keys" >&2
  exit 1
fi

mkdir -p "${SCRIPT_DIR}/data"
GNUPGHOME="$(mktemp -d)"
chmod 700 "$GNUPGHOME"
export GNUPGHOME

cleanup() {
  rm -rf "$GNUPGHOME"
}
trap cleanup EXIT

gpg --batch --passphrase '' --quick-generate-key \
  "FSC E2E Test <fsc-e2e@test.local>" ed25519 sign

KEY="$(gpg --list-secret-keys --with-colons "fsc-e2e@test.local" | awk -F: '/^fpr:/ {print $10; exit}')"
if [ -z "$KEY" ]; then
  echo "failed to resolve generated signing key fingerprint" >&2
  exit 1
fi

gpg --armor --export-secret-keys "$KEY" >"$OUT"
gpg --armor --export "$KEY" >"$OUT_PUB"