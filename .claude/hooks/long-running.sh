#!/bin/sh
# PreToolUse (Bash): stop commands that usually outlast the Bash tool's default
# 2-minute limit unless they run in the background or with a timeout of at
# least 5 minutes. Exit 2 blocks the call and feeds stderr back to Claude.
# Fails open (allows the call) when jq is missing or the input is unreadable.
command -v jq >/dev/null 2>&1 || exit 0
input=$(cat)
cmd=$(printf '%s' "$input" | jq -r '.tool_input.command // ""' 2>/dev/null) || exit 0
bg=$(printf '%s' "$input" | jq -r '.tool_input.run_in_background // false' 2>/dev/null)
timeout=$(printf '%s' "$input" | jq -r '.tool_input.timeout // 0 | floor' 2>/dev/null)

case "$cmd" in
  *"go test"*-race* | *"go test"*-fuzz* | *govulncheck* | *goreleaser* | \
  *"make check"* | *"make race"* | *"make fuzz"* | *"make vuln"* | *"make snapshot"*) ;;
  *) exit 0 ;;
esac

if [ "$bg" = true ] || [ "${timeout:-0}" -ge 300000 ]; then
  exit 0
fi

echo "Long-running command: re-run it with run_in_background: true, or with timeout of at least 300000 (max 600000), so it is not cut off at the 2-minute default." >&2
exit 2
