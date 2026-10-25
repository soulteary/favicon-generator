#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# Use an installed toolchain and cached dependencies. Populate the module cache
# separately with `go mod download` in a network-enabled environment if needed.
export GOTOOLCHAIN=local
export GOWORK=off
export GOPROXY=off

go version
go test -mod=readonly -run '^$' -bench "${BENCHMARK_PATTERN:-^Benchmark(Pipeline|ArtifactPipeline)$}" \
  -benchmem -benchtime "${BENCHTIME:-1s}" -count "${BENCHCOUNT:-3}" \
  -timeout 10m "$@" .
