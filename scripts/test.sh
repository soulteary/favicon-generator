#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Test each module independently: ./... at the root excludes nested go.mod files.
export GOWORK=off

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
