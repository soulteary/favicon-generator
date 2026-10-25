#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
mkdir "$test_dir/bin"
cat > "$test_dir/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${GOTOOLCHAIN:-}" != local ]]; then
  printf 'The version gate must disable automatic toolchain switching.\n' >&2
  exit 1
fi
if [[ "$*" == 'env GOVERSION' ]]; then
  printf '%s\n' "$TEST_GO_VERSION"
  exit 0
fi
case "$1" in
  test|vet|build) printf '%s\n' "$*" >> "$TEST_GO_LOG" ;;
  *) printf 'Unexpected Go command: %s\n' "$*" >&2; exit 1 ;;
esac
GO
chmod +x "$test_dir/bin/go"

while read -r version expected; do
  : > "$test_dir/commands"
  if PATH="$test_dir/bin:$PATH" GOTOOLCHAIN=auto TEST_GO_VERSION="$version" \
      TEST_GO_LOG="$test_dir/commands" "$repo_root/scripts/test.sh" > "$test_dir/output" 2>&1; then
    status=0
  else
    status=$?
  fi
  if [[ "$expected" == accept ]]; then
    if [[ "$status" != 0 || ! -s "$test_dir/commands" ]]; then
      printf 'Expected %s to run the project checks.\n' "$version" >&2
      cat "$test_dir/output" >&2
      exit 1
    fi
  else
    output="$(cat "$test_dir/output")"
    if [[ "$status" == 0 || -s "$test_dir/commands" || "$output" != *'Go 1.27.3 or later is required'* ]]; then
      printf 'Expected %s to be rejected before running any checks.\n' "$version" >&2
      cat "$test_dir/output" >&2
      exit 1
    fi
  fi
done <<'CASES'
go1.26.9 reject
go1.27.2 reject
go1.27rc1 reject
go1.27.3 accept
go1.27.4 accept
go1.28.0 accept
go1.28rc1 accept
unknown reject
CASES

printf 'Go version gate checks passed.\n'
