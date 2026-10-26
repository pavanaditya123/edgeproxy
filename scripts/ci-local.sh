#!/bin/sh
# Runs the same checks as CI's lint, unit and e2e jobs. Works anywhere Docker
# runs (including Windows, where the race detector isn't otherwise available):
#
#   docker run --rm -v "$PWD:/src" -w /src golang:1.26 sh scripts/ci-local.sh
set -eu

echo "== gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "These files need gofmt:"
  echo "$unformatted"
  exit 1
fi

echo "== go vet"
go vet -tags e2e ./...

echo "== unit tests (race detector)"
go test -race -count=1 ./...

echo "== fuzz the config parser"
go test -run='^$' -fuzz=FuzzParse -fuzztime="${FUZZTIME:-15s}" ./internal/config/

echo "== end-to-end tests (race-enabled binaries)"
E2E_RACE=1 go test -tags e2e -count=1 ./e2e/

echo "== all checks passed"
