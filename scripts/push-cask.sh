#!/usr/bin/env bash
# Commits the rendered cask to the tap and pushes it. The release workflow runs it for every stable tag:
#   TAP_TOKEN=... TAP_URL=https://github.com/tara-vision/homebrew-taracode.git bash scripts/push-cask.sh <cask.rb> <tag>
# A cask that is already the tap's is left alone, so a re-run of the job is harmless. TAP_TOKEN may be empty when
# TAP_URL needs none, as a local repository in the tests does.
set -euo pipefail

cask="$1"
tag="$2"
tap=$(mktemp -d)
trap 'rm -rf "$tap"' EXIT

url="$TAP_URL"
if [ -n "${TAP_TOKEN:-}" ]; then
  url="https://x-access-token:${TAP_TOKEN}@${TAP_URL#https://}"
fi
git clone --quiet --depth 1 "$url" "$tap"
mkdir -p "$tap/Casks"
cp "$cask" "$tap/Casks/taraengine.rb"
cd "$tap"
git add Casks/taraengine.rb
if git diff --cached --quiet; then
  echo "push-cask: the tap's cask is already at ${tag}"
  exit 0
fi
git -c user.name="github-actions[bot]" -c user.email="github-actions[bot]@users.noreply.github.com" \
  commit --quiet -m "Update taraengine cask to ${tag}"
git push --quiet origin HEAD
echo "push-cask: pushed the taraengine cask for ${tag}"
