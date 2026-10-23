#!/usr/bin/env bash
# Apply the repository settings that cannot live in files.
#
# Dry run by default: reads current state and prints every change it would make.
# Run again with --apply to make them. Safe to re-run: the branch ruleset is
# updated in place, never duplicated.
#
# Needs the GitHub CLI (https://cli.github.com), logged in with admin rights.
set -euo pipefail

RULESET_NAME="protect-default-branch"

usage() {
  cat <<'EOF'
usage: scripts/github-setup.sh [--apply] [--require-checks] [--repo OWNER/NAME]

  --apply           make the changes (default: print them only)
  --require-checks  also require a pull request and passing CI before anything
                    lands on the default branch (blocks direct pushes, including yours)
  --repo            target repository (default: the one this checkout points to)

Always applied:
  - delete head branches after merge
  - private vulnerability reporting
  - Dependabot alerts and security updates
  - secret scanning with push protection
  - default branch cannot be deleted or force-pushed
EOF
}

apply=false
require_checks=false
repo=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) apply=true ;;
    --require-checks) require_checks=true ;;
    --repo)
      [ $# -ge 2 ] || { echo "--repo needs OWNER/NAME" >&2; exit 2; }
      repo="$2"
      shift
      ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

command -v gh >/dev/null 2>&1 || { echo "the GitHub CLI (gh) is required: https://cli.github.com" >&2; exit 2; }
if [ -z "$repo" ]; then
  repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)" ||
    { echo "could not detect the repository; push it first or pass --repo OWNER/NAME" >&2; exit 2; }
fi
if ! [[ "$repo" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]]; then
  echo "not an OWNER/NAME repository: $repo" >&2
  exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# change prints a mutating call and runs it only with --apply.
change() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if $apply; then "$@" >/dev/null; fi
}

# payload writes a JSON body to a file, shows it, and prints the file path.
payload() {
  local file="$tmp/$1.json"
  cat >"$file"
  sed 's/^/    /' "$file" >&2
  printf '%s' "$file"
}

if $apply; then echo "Applying settings to $repo"; else echo "Dry run for $repo (nothing changes; add --apply)"; fi

echo "# Repository: delete head branches after merge"
change gh api --method PATCH "repos/$repo" -F delete_branch_on_merge=true

echo "# Private vulnerability reporting (the route SECURITY.md points to)"
change gh api --method PUT "repos/$repo/private-vulnerability-reporting"

echo "# Dependabot alerts and security updates"
change gh api --method PUT "repos/$repo/vulnerability-alerts"
change gh api --method PUT "repos/$repo/automated-security-fixes"

echo "# Secret scanning with push protection"
body="$(payload security <<'EOF'
{"security_and_analysis": {
  "secret_scanning": {"status": "enabled"},
  "secret_scanning_push_protection": {"status": "enabled"}
}}
EOF
)"
change gh api --method PATCH "repos/$repo" --input "$body"

# Status check names must match the job names in .github/workflows/ci.yml.
checks_rules=""
if $require_checks; then
  checks_rules=',
    {"type": "pull_request", "parameters": {
      "required_approving_review_count": 0,
      "dismiss_stale_reviews_on_push": false,
      "require_code_owner_review": false,
      "require_last_push_approval": false,
      "required_review_thread_resolution": false}},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": false,
      "required_status_checks": [
        {"context": "test (ubuntu-latest, go minimum)"},
        {"context": "test (ubuntu-latest, go stable)"},
        {"context": "test (macos-latest, go minimum)"},
        {"context": "test (macos-latest, go stable)"},
        {"context": "test (windows-latest, go minimum)"},
        {"context": "test (windows-latest, go stable)"},
        {"context": "lint"},
        {"context": "vulnerabilities"},
        {"context": "fuzz"}]}}'
fi

echo "# Branch ruleset \"$RULESET_NAME\""
existing="$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name == \"$RULESET_NAME\") | .id" 2>/dev/null || true)"
body="$(payload ruleset <<EOF
{"name": "$RULESET_NAME",
 "target": "branch",
 "enforcement": "active",
 "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
 "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"}$checks_rules
 ]}
EOF
)"
if [ -n "$existing" ]; then
  echo "  exists (id $existing): updating in place"
  change gh api --method PUT "repos/$repo/rulesets/$existing" --input "$body"
else
  echo "  not found: creating"
  change gh api --method POST "repos/$repo/rulesets" --input "$body"
fi

if $apply; then echo "Done."; else echo "Dry run complete. Re-run with --apply to make these changes."; fi
