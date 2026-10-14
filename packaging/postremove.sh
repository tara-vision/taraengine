#!/bin/sh
# Runs after the deb or rpm removed the binary. A Debian purge removes the configuration and the data as well; the
# system user stays, as Debian keeps system users. rpm has no purge and keeps both.
set -e
if [ "${1:-}" = "purge" ]; then
	rm -rf /etc/taraengine /var/lib/taraengine
fi
exit 0
