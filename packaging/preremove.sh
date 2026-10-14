#!/bin/sh
# Runs before the deb or rpm removes the binary. On a removal (deb: remove; rpm: 0), not on an upgrade, the service goes
# with it; the user, the configuration and the data stay. A service that is already gone does not stop the removal.
# TARAENGINE_BIN is for the tests.
case "${1:-}" in
remove | 0) "${TARAENGINE_BIN:-/usr/bin/taraengine}" service uninstall || true ;;
esac
exit 0
