#!/usr/bin/env bash
# Installs the pinned runtime into a temporary data directory and runs the integration test against the built
# binary. Build the binary first, or run `make integration`, which does. Needs network once, for the runtime from
# GitHub and the one-megabyte test model from Hugging Face. The temporary directory is removed on exit. To keep the
# runtime between runs, set TARAENGINE_INTEGRATION_DIR; that directory is never removed. Extra arguments go to
# go test, for example -run TestCatalogNameWithAColon.
#
# The script never lets the test skip for want of its environment: it stops before go test when there is no runtime
# directory to hand over, and the test itself fails instead of skipping where the variable CI is set.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
bin="$root/taraengine"
if [ ! -x "$bin" ]; then
  echo "integration: $bin is missing; run make build first" >&2
  exit 1
fi
if [ -n "${TARAENGINE_INTEGRATION_DIR:-}" ]; then
  tmp="$TARAENGINE_INTEGRATION_DIR"
  mkdir -p "$tmp"
else
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
fi

# The commands run with a configuration directory of their own, so that the configuration of whoever runs this
# cannot change what is installed where.
engine() { XDG_CONFIG_HOME="$tmp/config" TARAENGINE_API_KEY="" TARAENGINE_HF_TOKEN="" "$bin" "$@"; }

engine runtime install --data-dir "$tmp" --no-cudart
engine runtime status --data-dir "$tmp"
export TARAENGINE_BIN="$bin"
TARAENGINE_RUNTIME_DIR=$(engine runtime status --data-dir "$tmp" --dir-only)
export TARAENGINE_RUNTIME_DIR
if [ -z "$TARAENGINE_RUNTIME_DIR" ] || [ ! -d "$TARAENGINE_RUNTIME_DIR" ]; then
  echo "integration: no runtime directory to test against (runtime status printed \"$TARAENGINE_RUNTIME_DIR\")" >&2
  exit 1
fi
cd "$root"
go test -tags integration -count=1 -v -timeout 15m "$@" ./internal/integration/
