#!/usr/bin/env bash
# Repository hygiene, the same in CI and by hand. Run it from anywhere inside the repository:
#   bash scripts/hygiene.sh
# It checks the tracked files and the new files that git does not ignore, and prints every offender:
#   1. No lab hostnames or addresses: the repository is public.
#   2. No Go file over 800 lines.
#   3. No en dash, em dash or curly quote: plain hyphens and straight quotes only.
# The exit status is 1 when anything was found, and 2 when git itself failed.
set -euo pipefail
cd "$(dirname "$0")/.."

status=0

# scan <message> <git grep arguments>: prints the matching lines and fails the run when there are any. git grep
# answers 1 for "no match", which is the good case, and more than 1 for a failure, which must not pass as one.
scan() {
  local message=$1 rc=0
  shift
  git grep --untracked --exclude-standard -n -I "$@" -- . || rc=$?
  case $rc in
    0) echo "hygiene: $message" >&2; status=1 ;;
    1) ;;
    *) echo "hygiene: git grep failed with status $rc" >&2; exit 2 ;;
  esac
}

# The brackets in the first pattern keep this file from matching its own words.
scan "lab hostnames and addresses must not appear in the public repository." \
  -E 'tara\.lab|[a]ramis|[p]orthos|192\.168\.|10\.0\.0\.'

oversized=$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 -r wc -l |
  awk '$2 != "total" && $1 > 800')
if [ -n "$oversized" ]; then
  printf '%s\n' "$oversized"
  echo "hygiene: Go files must stay under 800 lines." >&2
  status=1
fi

# The UTF-8 bytes of U+2013, U+2014, U+2018, U+2019, U+201C and U+201D, matched as bytes so that the check does not
# depend on the locale or on PCRE support in git.
fancy=$(printf '\xe2\x80\x93|\xe2\x80\x94|\xe2\x80\x98|\xe2\x80\x99|\xe2\x80\x9c|\xe2\x80\x9d')
LC_ALL=C scan "use plain hyphens and straight quotes." -E "$fancy"

exit $status
