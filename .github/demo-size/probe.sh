#!/usr/bin/env bash
# probe.sh <out>: where a build's time and disk go on this image store: today's builder, claims-portal three times.
set -uo pipefail
export OUT=$1 TRIAL_OUT=$1; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../.." && pwd); cd "$repo"
cp "$here/probe_test.go.txt" executor/probe_test.go
go test -tags e2e -run '^$' ./executor || exit 1
mkdir -p "$RUNNER_TEMP/wrapper"; cp "$here/pack" "$RUNNER_TEMP/wrapper/pack"; chmod +x "$RUNNER_TEMP/wrapper/pack"; export PATH=$RUNNER_TEMP/wrapper:$PATH
go test -tags e2e -count=1 -timeout 45m -run '^TestProbe$' -v ./executor 2>&1 | tee "$OUT/probe.txt"
exit 0
