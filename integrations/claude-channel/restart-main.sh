#!/usr/bin/env bash
# Start (or restart) the main Claude Code session for mode claude.
#
# Stops every running session with the main session's name in the main
# directory first, by id: only one session can hold the bridge's main
# attachment, and a leftover one keeps it while the new one stays passive.
# The main directory comes from mahamantri's discovery file, so start
# mahamantri first.
set -euo pipefail

name=${SAMRAJYA_SESSION_NAME:-samrajya}
rc_name=${SAMRAJYA_RC_NAME:-Samrajya}
discovery=${SAMRAJYA_BRIDGE_FILE:-${XDG_STATE_HOME:-$HOME/.local/state}/samrajya/bridge.json}

[[ -r $discovery ]] || { echo "mahamantri is not running (no $discovery)" >&2; exit 1; }
dir=$(jq -r '.mainDirectory // empty' "$discovery")
[[ -n $dir ]] || { echo "$discovery names no mainDirectory" >&2; exit 1; }
dir=$(realpath "$dir")

claude agents --json | jq -r --arg name "$name" '.[] | select(.name == $name) | "\(.id)\t\(.cwd)"' |
  while IFS=$'\t' read -r id cwd; do
    [[ $(realpath -m "$cwd") == "$dir" ]] || continue
    echo "stopping old main session $id"
    claude stop "$id"
  done

cd "$dir"
exec claude --bg -n "$name" --channels plugin:samrajya@samrajya --remote-control "$rc_name"
