#!/usr/bin/env bash
# Prints the CHANGELOG section of one release, without its heading, for use as the notes of the GitHub release.
#
# Usage: scripts/release-notes.sh v0.1.0 [CHANGELOG.md]
#
# The section runs from the heading "## [0.1.0] - date" to the next "## [" heading, or to the link references that
# close a Keep a Changelog file. Blank lines at its start and end are dropped. A pre-release tag such as v0.2.0-rc.1
# takes its own section when that holds text, and the [Unreleased] section otherwise: a release candidate carries what
# the next release will. The script fails when there is no such section or when it holds no text, so that a release
# never goes out with empty notes. Without a second argument it reads the CHANGELOG.md next to the scripts directory.
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

# section prints the text under the "## [$1]" heading, without the blank lines at its start.
section() {
  awk -v v="$1" '
    /^## \[/ { if (found) exit; found = index($0, "[" v "]") > 0; next }
    found && /^\[[^]]*\]: / { exit }
    found { print }
  ' "$changelog" | sed '/./,$!d'
}

# blank reports whether $1 holds no text.
blank() { [ -z "$(printf '%s' "$1" | tr -d '[:space:]')" ]; }

notes=$(section "$version")
heading="$version"
if blank "$notes" && [ "$version" != "${version%%-*}" ]; then
  notes=$(section Unreleased)
  heading="$version or Unreleased"
fi
if blank "$notes"; then
  echo "release-notes: $changelog has no text under a heading for $heading" >&2
  exit 1
fi
printf '%s\n' "$notes"
