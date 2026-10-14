#!/usr/bin/env bash
# Stops a release whose run lacks a secret that signs or notarizes the macOS binary. goreleaser skips the notarize step
# when the certificate is missing, so without this check a tag would publish an unsigned binary and the cask would
# hand it to every Mac, where Gatekeeper refuses it. `make snapshot` and `goreleaser check` need no secrets and do not
# run this.
#
# Usage: scripts/require-signing.sh, with the five secrets in the environment.
set -euo pipefail
for name in MACOS_SIGN_P12 MACOS_SIGN_PASSWORD MACOS_NOTARY_ISSUER_ID MACOS_NOTARY_KEY_ID MACOS_NOTARY_KEY; do
  if [ -z "${!name:-}" ]; then
    echo "release: $name is not set; a release ships only a signed and notarized macOS binary" >&2
    exit 1
  fi
done
