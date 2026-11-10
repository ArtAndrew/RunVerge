#!/bin/sh
# SPDX-License-Identifier: Elastic-2.0
set -eu
umask 022

model=/models/pilot-qwen.gguf
digest=6a1a2eb6d15622bf3c96857206351ba97e1af16c30d7a74ee38970e434e9407e
verify() {
  printf '%s  %s\n' "$digest" "$1" | sha256sum -c -
}
if [ -f "$model" ]; then
  verify "$model"
  exit
fi
partial=$(mktemp /models/.pilot-qwen.XXXXXXXX)
trap 'rm -f "$partial"' EXIT
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 15 --max-time 720 --max-filesize 1117320736 \
  --output "$partial" \
  'https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/91cad51170dc346986eccefdc2dd33a9da36ead9/qwen2.5-1.5b-instruct-q4_k_m.gguf'
verify "$partial"
chmod 0444 "$partial"
mv "$partial" "$model"
