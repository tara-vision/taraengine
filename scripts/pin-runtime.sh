#!/usr/bin/env bash
# Usage: scripts/pin-runtime.sh b11514 internal/runtime/runtime.lock.yaml
#        scripts/pin-runtime.sh b11514 > lock.yaml
# Downloads every pinned asset of the build and renders the lock with fresh checksums. A failed download
# exits 1 before anything is written. With an output path the lock replaces that file in one rename, once
# every checksum succeeded. Without one the lock goes to stdout once, at the end. Redirect that into a
# scratch file: a shell redirect truncates its target before the script starts.
set -euo pipefail
build="${1:?build, for example b11514}"
out="${2:-}"
base="https://github.com/ggml-org/llama.cpp/releases/download/$build"
tmp=$(mktemp -d)
stage=""
trap 'rm -rf "$tmp"; [ -z "$stage" ] || rm -f "$stage"' EXIT

sum() { # sum <asset>
  if ! curl -fsSL --retry 3 -o "$tmp/$1" "$base/$1"; then
    echo "pin-runtime: download failed: $base/$1" >&2
    return 1
  fi
  shasum -a 256 "$tmp/$1" | cut -d' ' -f1
}

entry() { # entry <key> <asset> [untested]
  local digest
  digest=$(sum "$2") || exit 1
  printf '  %s:\n    asset: %s\n    sha256: %s\n' "$1" "$2" "$digest"
  [ "${3:-}" = "untested" ] && printf '    untested: true\n'
  return 0
}

render() {
  echo "# Pinned upstream llama.cpp build: one build per taraengine release. The sha256 values are those of"
  echo "# the GitHub release assets; scripts/pin-runtime.sh <build> internal/runtime/runtime.lock.yaml regenerates this file and CI verifies it."
  echo "build: $build"
  echo "assets:"
  entry darwin/arm64 "llama-$build-bin-macos-arm64.tar.gz"
  entry linux/amd64/cuda13 "llama-$build-bin-ubuntu-cuda-13.4-x64.tar.gz"
  entry linux/amd64/cuda12 "llama-$build-bin-ubuntu-cuda-12.8-x64.tar.gz"
  entry linux/amd64/cpu "llama-$build-bin-ubuntu-x64.tar.gz"
  entry linux/arm64/cuda13 "llama-$build-bin-ubuntu-cuda-13.4-arm64.tar.gz" untested
  entry linux/arm64/cpu "llama-$build-bin-ubuntu-arm64.tar.gz" untested
  echo "cudart:"
  entry linux/amd64/cuda13 "cudart-llama-$build-bin-ubuntu-cuda-13.4-x64.tar.gz"
  entry linux/amd64/cuda12 "cudart-llama-$build-bin-ubuntu-cuda-12.8-x64.tar.gz"
  entry linux/arm64/cuda13 "cudart-llama-$build-bin-ubuntu-cuda-13.4-arm64.tar.gz" untested
}

# Check the output path before the first download, so a bad path costs no network traffic.
if [ -n "$out" ]; then
  if [ -d "$out" ]; then
    echo "pin-runtime: $out is a directory, not a lock file" >&2
    exit 1
  fi
  if [ ! -d "$(dirname -- "$out")" ]; then
    echo "pin-runtime: parent directory of $out does not exist" >&2
    exit 1
  fi
fi

# Every download runs inside render, so a failed download exits before any output is written.
text=$(render) || exit 1
if [ -z "$out" ]; then
  printf '%s\n' "$text"
  exit 0
fi
# The stage file sits beside the output, so the mv is a rename within one filesystem.
stage="$out.tmp.$$"
printf '%s\n' "$text" > "$stage"
mv "$stage" "$out"
stage=""
