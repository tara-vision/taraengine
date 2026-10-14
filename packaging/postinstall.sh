#!/bin/sh
# Runs after the deb or rpm put the binary in place, on an install and on an upgrade (deb: configure; rpm: any count):
# the binary installs or updates its own system service, which an upgrade restarts on the new binary. On a system
# without systemd, such as a container, it says so and succeeds. TARAENGINE_BIN is for the tests.
set -e
case "${1:-}" in
configure | [0-9]*) "${TARAENGINE_BIN:-/usr/bin/taraengine}" service install ;;
esac
exit 0
