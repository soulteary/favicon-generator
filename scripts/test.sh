#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Validate the installed toolchain instead of allowing Go to download a newer
# one while loading go.mod. Keep the same toolchain for every check below.
export GOTOOLCHAIN=local
export GOWORK=off
go_version="$(go env GOVERSION)"
if [[ ! "$go_version" =~ ^go([0-9]+)\.([0-9]+)(\.([0-9]+)|(beta|rc)([0-9]+))?$ ]]; then
  printf 'Go 1.27.3 or later is required (found %s).\n' "$go_version" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[4]:-0}"
if (( major < 1 || (major == 1 && (minor < 27 || (minor == 27 && patch < 3))) )); then
  printf 'Go 1.27.3 or later is required (found %s).\n' "$go_version" >&2
  exit 1
fi

# Test each module independently: ./... at the root excludes nested go.mod files.

while IFS= read -r -d '' modfile; do
  module_dir="${modfile%/go.mod}"
  module_name="${module_dir#"$repo_root"}"
  printf '\nChecking %s\n' "${module_name:-/}"
  (
    cd "$module_dir"
    go test -mod=readonly -race -count=1 -timeout=5m ./...
    go vet -mod=readonly ./...
    go build -mod=readonly -o /dev/null ./...
  )
done < <(find "$repo_root" -type d \( -name .git -o -name vendor \) -prune -o -name go.mod -type f -print0)
