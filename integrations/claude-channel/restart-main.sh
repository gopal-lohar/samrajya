#!/usr/bin/env bash
# Start, restart or keep running the main Claude Code session for mode claude.
#
#   restart-main.sh           stop the main session and resume its conversation
#   restart-main.sh --fresh   stop it and start a new conversation
#   restart-main.sh --ensure  start it only if none is running (mahamantri runs
#                             this when no main session is attached)
#
# Only one session can hold the bridge's main attachment, and a leftover one
# keeps it while a new one stays passive, so every session with the main
# session's name in the main directory is stopped first, by id. The last main
# session's id is kept in the state directory: Claude Code ends a background
# session after hours idle, and resuming it keeps the conversation instead of
# starting from nothing. The main directory comes from mahamantri's discovery
# file, so start mahamantri first.
#
# Like managed mode's sainiks, the session runs without permission prompts: it
# works unattended, and a prompt would leave a ping blocked until someone
# answered it. It posts on Linear through Samrajya's tools, as Senapati, so the
# Linear connector's own write tools are disallowed: the connector may belong
# to someone else.
set -euo pipefail

mode=${1:-restart}
case $mode in restart | --fresh | --ensure) ;; *)
  echo "usage: $0 [--fresh | --ensure]" >&2
  exit 2
  ;;
esac

name=${SAMRAJYA_SESSION_NAME:-samrajya}
rc_name=${SAMRAJYA_RC_NAME:-Samrajya}
state=${XDG_STATE_HOME:-$HOME/.local/state}/samrajya
discovery=${SAMRAJYA_BRIDGE_FILE:-$state/bridge.json}
last=$state/main-session

[[ -r $discovery ]] || { echo "mahamantri is not running (no $discovery)" >&2; exit 1; }
dir=$(jq -r '.mainDirectory // empty' "$discovery")
[[ -n $dir ]] || { echo "$discovery names no mainDirectory" >&2; exit 1; }
dir=$(realpath "$dir")

# Running main sessions: this name, this directory, a live process.
running() {
  claude agents --json | jq -r --arg name "$name" '.[] | select(.name == $name and .pid != null) | "\(.id)\t\(.pid)\t\(.cwd)"' |
    while IFS=$'\t' read -r id pid cwd; do
      [[ $(realpath -m "$cwd") == "$dir" ]] && kill -0 "$pid" 2>/dev/null && echo "$id"
    done
}

ids=$(running)
if [[ $mode == --ensure && -n $ids ]]; then
  exit 0
fi
for id in $ids; do
  echo "stopping main session $id"
  claude stop "$id"
done

cd "$dir"
flags=(-n "$name" --dangerously-skip-permissions
  --disallowedTools mcp__claude_ai_Linear__save_comment mcp__claude_ai_Linear__delete_comment
  --channels plugin:samrajya@samrajya --remote-control "$rc_name")
start() {
  local out id
  out=$(claude --bg "$@" "${flags[@]}" 2>&1) || { echo "$out" >&2; return 1; }
  echo "$out"
  # "backgrounded · <id> · <name> ..."
  id=$(sed -n 's/^backgrounded · \([0-9a-f]*\).*/\1/p' <<<"$out" | head -1)
  [[ -n $id ]] && echo "$id" >"$last"
  return 0
}
# Resuming with options makes a copy of the conversation that runs with
# them; the old session is stopped above.
if [[ $mode != --fresh && -s $last ]]; then
  start --resume "$(cat "$last")" && exit 0
  echo "could not resume $(cat "$last"); starting a new conversation" >&2
fi
start
