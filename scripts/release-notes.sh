#!/usr/bin/env bash
# Prints the CHANGELOG section of one release, without its heading, for use as the notes of the GitHub release.
#
# Usage: scripts/release-notes.sh v0.1.0 [CHANGELOG.md]
#
# The section runs from the heading "## [0.1.0] - date" to the next "## [" heading, or to the link references that
# close a Keep a Changelog file. Blank lines at its start and end are dropped. The script fails when there is no
# such section or when it holds no text, so that a release never goes out with empty notes. Without a second
# argument it reads the CHANGELOG.md next to the scripts directory.
set -euo pipefail
tag="${1:-}"
if [ -z "$tag" ]; then
  echo "usage: release-notes.sh <tag, for example v0.1.0> [CHANGELOG.md]" >&2
  exit 2
fi
changelog="${2:-$(cd "$(dirname "$0")/.." && pwd)/CHANGELOG.md}"
if [ ! -f "$changelog" ]; then
  echo "release-notes: $changelog does not exist" >&2
  exit 1
fi
version="${tag#v}"

notes=$(awk -v v="$version" '
  /^## \[/ { if (found) exit; found = index($0, "[" v "]") > 0; next }
  found && /^\[[^]]*\]: / { exit }
  found { print }
' "$changelog" | sed '/./,$!d')

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  echo "release-notes: $changelog has no text under a heading for $version" >&2
  exit 1
fi
printf '%s\n' "$notes"
