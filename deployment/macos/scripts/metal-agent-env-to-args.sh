#!/usr/bin/env bash
# Translate ~/.config/llmkube/metal-agent/env into command-line flags for
# llmkube-metal-agent. Each line in the env file is either:
#   --flag=value
#   --flag value
#   # comment (ignored)
#   (blank line, ignored)
#
# This lets operators keep per-node config in a plain text file that survives
# `brew upgrade`, instead of hand-editing the launchd plist every release.

set -euo pipefail

ENV_FILE="${METAL_AGENT_ENV_FILE:-$HOME/.config/llmkube/metal-agent/env}"

if [ ! -f "$ENV_FILE" ]; then
  echo "metal-agent: env file not found at $ENV_FILE" >&2
  echo "Create it with: mkdir -p ~/.config/llmkube/metal-agent && touch ~/.config/llmkube/metal-agent/env" >&2
  exit 1
fi

ARGS=()
while IFS= read -r line || [ -n "$line" ]; do
  # Skip comments and blank lines.
  [[ "$line" =~ ^[[:space:]]*# ]] && continue
  [[ -z "${line// /}" ]] && continue

  # Strip leading whitespace.
  line="${line#"${line%%[![:space:]]*}"}"

  # Split on first whitespace to handle both --flag=value and --flag value.
  if [[ "$line" =~ ^--([a-zA-Z0-9_-]+)=(.+)$ ]]; then
    ARGS+=("--${BASH_REMATCH[1]}=${BASH_REMATCH[2]}")
  elif [[ "$line" =~ ^--([a-zA-Z0-9_-]+)[[:space:]]+(.+)$ ]]; then
    ARGS+=("--${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}")
  else
    echo "metal-agent: invalid line in env file: $line" >&2
    exit 1
  fi
done < "$ENV_FILE"

exec /opt/homebrew/bin/llmkube-metal-agent "${ARGS[@]}"