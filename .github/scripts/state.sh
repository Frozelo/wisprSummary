#!/usr/bin/env bash
# Encrypts/decrypts the service state (SQLite + Wispr session) for storage in
# the GitHub Actions cache. The cache of a public repository must never hold
# plaintext tokens.
#
# usage: state.sh pack|unpack
# env:   STATE_KEY (required), DATA_DIR (default data), STATE_FILE (default state.tar.enc)
set -euo pipefail

DATA_DIR="${DATA_DIR:-data}"
STATE_FILE="${STATE_FILE:-state.tar.enc}"
: "${STATE_KEY:?STATE_KEY is not set}"

case "${1:-}" in
  pack)
    tar -C "$DATA_DIR" -czf - . | openssl enc -aes-256-cbc -pbkdf2 -salt -pass env:STATE_KEY -out "$STATE_FILE"
    ;;
  unpack)
    mkdir -p "$DATA_DIR"
    chmod 700 "$DATA_DIR"
    openssl enc -d -aes-256-cbc -pbkdf2 -pass env:STATE_KEY -in "$STATE_FILE" | tar -C "$DATA_DIR" -xzf -
    ;;
  *)
    echo "usage: state.sh pack|unpack" >&2
    exit 2
    ;;
esac
