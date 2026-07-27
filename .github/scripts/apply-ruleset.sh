#!/usr/bin/env bash
#
# Apply a versioned branch ruleset to this repository.
#
# Branch protection is configuration like any other, so it lives in .github/rulesets/*.json and is
# applied from there rather than being clicked into the web UI. That way a change to what protects
# `main` shows up in a diff, gets reviewed, and can be restored after someone edits it by hand.
#
# The script is idempotent: it updates a ruleset with a matching name if one exists, and creates it
# otherwise. It then reads the result back and prints what GitHub actually stored, because the API
# silently accepts some fields it does not apply.
#
# Usage:
#   gh auth login                       # once, interactively
#   .github/scripts/apply-ruleset.sh    # or: make ruleset-apply
#
# Environment:
#   REPO       owner/name. Defaults to the origin remote.
#   RULESET    path to the ruleset JSON. Defaults to .github/rulesets/main-protection.json.
#   DRY_RUN    set to 1 to print what would be sent and exit.

set -euo pipefail

RULESET="${RULESET:-.github/rulesets/main-protection.json}"

if ! command -v gh >/dev/null 2>&1; then
  echo "error: the GitHub CLI (gh) is required. Install it: https://cli.github.com" >&2
  exit 1
fi

if [[ ! -f "$RULESET" ]]; then
  echo "error: ruleset file not found: $RULESET" >&2
  exit 1
fi

# The auth check comes after the dry-run path below would be useful, so it is deferred: inspecting
# what would be sent should not require credentials.
if [[ "${DRY_RUN:-0}" != "1" ]] && ! gh auth status >/dev/null 2>&1; then
  echo "error: gh is not authenticated. Run 'gh auth login' first." >&2
  echo "       The token needs the 'repo' scope to manage rulesets." >&2
  exit 1
fi

# Resolve the repository from the origin remote unless told otherwise, so the script works in a
# fork without editing.
if [[ -z "${REPO:-}" ]]; then
  REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
fi
if [[ -z "$REPO" ]]; then
  echo "error: could not determine the repository. Set REPO=owner/name." >&2
  exit 1
fi

NAME="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$RULESET")"

echo "repository : $REPO"
echo "ruleset    : $NAME  (from $RULESET)"

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  echo "--- dry run, payload below ---"
  cat "$RULESET"
  exit 0
fi

# An existing ruleset with the same name is updated in place. Creating a second one with the same
# name is legal in the API and produces two overlapping rulesets, which is confusing to debug.
EXISTING_ID="$(gh api "repos/$REPO/rulesets" --jq ".[] | select(.name == \"$NAME\") | .id" 2>/dev/null | head -1)"

if [[ -n "$EXISTING_ID" ]]; then
  echo "action     : updating existing ruleset $EXISTING_ID"
  gh api --method PUT "repos/$REPO/rulesets/$EXISTING_ID" --input "$RULESET" >/dev/null
  RULESET_ID="$EXISTING_ID"
else
  echo "action     : creating a new ruleset"
  RULESET_ID="$(gh api --method POST "repos/$REPO/rulesets" --input "$RULESET" --jq .id)"
fi

echo
echo "=== stored by GitHub (ruleset $RULESET_ID) ==="
gh api "repos/$REPO/rulesets/$RULESET_ID" > /tmp/fleetward-ruleset-readback.json

python3 - /tmp/fleetward-ruleset-readback.json <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
print(f"name        : {d['name']}")
print(f"enforcement : {d['enforcement']}")
print(f"targets     : {', '.join(d.get('conditions', {}).get('ref_name', {}).get('include', []))}")

bypass = d.get("bypass_actors") or []
if bypass:
    for b in bypass:
        # The API returns ids rather than names, so the role is spelled out here. If this shows
        # something other than the repository admin, fix the actor_id in the ruleset JSON.
        role = {1: "organization admin", 2: "maintain", 3: "write", 4: "triage", 5: "repository admin"}
        label = role.get(b.get("actor_id"), f"actor_id {b.get('actor_id')}")
        print(f"bypass      : {b.get('actor_type')} / {label} ({b.get('bypass_mode')})")
else:
    print("bypass      : none — nobody can push directly to the target branch")

print("rules       :")
for r in d.get("rules", []):
    t = r["type"]
    p = r.get("parameters") or {}
    if t == "required_status_checks":
        checks = [c["context"] for c in p.get("required_status_checks", [])]
        strict = p.get("strict_required_status_checks_policy")
        print(f"  - {t} (branch must be up to date: {strict})")
        for c in checks:
            print(f"      · {c}")
    elif t == "pull_request":
        print(f"  - {t} (approvals required: {p.get('required_approving_review_count')}, "
              f"merge methods: {', '.join(p.get('allowed_merge_methods', []))})")
    else:
        print(f"  - {t}")
PY

echo
echo "Direct pushes to the default branch are now blocked. Work on a branch and open a pull request:"
echo "  git switch -c feat/my-change && git push -u origin feat/my-change && gh pr create"
