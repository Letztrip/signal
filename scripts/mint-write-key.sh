#!/bin/sh
# Mint a new write key, hash it with argon2id, and print the line that goes
# into the write-keys Secret Manager secret.
#
# Usage:  scripts/mint-write-key.sh <app-id> [env]
#
# Requirements: openssl, argon2 (Homebrew: `brew install argon2`,
# Debian/Ubuntu: `apt install argon2`).
#
# Output:
#   1. The plaintext key. Hand this to the client. Do NOT store it.
#   2. The storage line (`<app-id>:<phc-hash>`). Append this to the
#      write-keys secret in Secret Manager.
set -e

if [ -z "$1" ]; then
  echo "usage: $0 <app-id> [env]" >&2
  exit 2
fi

APP="$1"
ENV="${2:-prod}"

command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }
command -v argon2  >/dev/null || { echo "argon2 CLI is required" >&2; exit 1; }

# 32 url-safe characters of entropy.
KEY="wk_${ENV}_$(openssl rand -base64 32 | tr -d '/+=' | head -c 32)"

# 16-byte salt, fed verbatim to argon2 (its CLI takes the salt as positional arg).
SALT=$(openssl rand -hex 16)

# m=65536 (16 -> 2^16 KiB = 64 MiB), t=3, p=2, 32-byte hash, PHC-encoded.
PHC=$(printf '%s' "$KEY" | argon2 "$SALT" -id -m 16 -t 3 -p 2 -l 32 -e | tr -d '\n')

echo "Plaintext key (hand to client, store nowhere):"
echo "  $KEY"
echo
echo "Storage line (append to the write-keys secret):"
echo "  $APP:$PHC"
