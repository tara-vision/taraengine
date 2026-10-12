#!/usr/bin/env bash
# Fails when a gated package is below its coverage floor. Run from the repo root.
set -euo pipefail

gates="
./internal/cli:90
./internal/logging:90
./internal/config:90
./internal/registry:90
./internal/pull:90
./internal/runtime:90
./internal/catalog:90
./internal/gguf:90
./internal/state:90
./internal/backend:90
./internal/backend/llamaserver:90
./internal/llamatest:90
./internal/measure:90
./internal/machine:90
./internal/api:90
./pkg/client:90
./tools/pinregistry:90
"

status=0
for entry in $gates; do
  pkg="${entry%%:*}"
  floor="${entry##*:}"
  [ -d "$pkg" ] || continue
  line=$(go test -cover "$pkg" 2>&1 | tail -1) || true
  pct=$(printf '%s' "$line" | sed -n 's/.*coverage: \([0-9.]*\)%.*/\1/p')
  if [ -z "$pct" ]; then
    echo "FAIL $pkg: no coverage line: $line"
    status=1
    continue
  fi
  if awk -v p="$pct" -v f="$floor" 'BEGIN { exit !(p + 0 < f + 0) }'; then
    echo "FAIL $pkg: ${pct}% is below the ${floor}% floor"
    status=1
  else
    echo "ok   $pkg: ${pct}%"
  fi
done
exit $status
