#!/bin/sh
# SessionStart: report what this session can and cannot run, so a missing tool
# shows up before a command fails rather than after. Local checks only: no
# network calls, so it stays fast and gives the same answer offline.
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0

missing=""
for tool in go gofmt golangci-lint goreleaser gh jq git-lfs; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done

hidden=$(find . -maxdepth 1 -name '.*' ! -name . ! -name .git | sed 's|^\./||' | sort | tr '\n' ' ')

echo "plumbline preflight:"
if [ -n "$missing" ]; then
  echo "- missing tools:$missing (commands that need them will fail with 'command not found')"
else
  echo "- all project tools found"
fi
if command -v go >/dev/null 2>&1; then
  echo "- $(go version)"
fi
echo "- hidden entries in the project root (not shown by plain ls): $hidden"
echo "- certificate (x509) errors in the sandbox are not worked around: hand the user the command instead"
echo "- git push, tags, releases and gh write commands ask first"
