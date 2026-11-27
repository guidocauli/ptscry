#!/usr/bin/env bash
# ptscry - Guido Cauli <guido.cauli@gmail.com>
# SPDX-License-Identifier: AGPL-3.0-or-later
# stage.sh STAGE: the files of the server packaged under /usr (the sources say
# /usr/local, as "make install"): systemd unit, man pages, documentation.
# Used by build-server.sh and by build-gui.sh (the GUI packages contain the server).
set -euo pipefail
STAGE=${1:?usage: stage.sh STAGE}
cd "$(dirname "$0")/../.."
mkdir -p "$STAGE/man" "$STAGE/doc"
sed -e 's|/usr/local/bin/ptscry|/usr/bin/ptscry|g; s|/usr/local/share/doc/ptscry|/usr/share/doc/ptscry|g' \
  contrib/systemd/ptscry-monitor.service > "$STAGE/ptscry-monitor.service"
gzip -9n -c man/ptscry.1 > "$STAGE/man/ptscry.1.gz"
gzip -9n -c man/ptscry.conf.5 > "$STAGE/man/ptscry.conf.5.gz"
gzip -9n -c man/ptscry-tui.1 > "$STAGE/man/ptscry-tui.1.gz"
gzip -9n -c man/ptscry-gui.1 > "$STAGE/man/ptscry-gui.1.gz"
cp README.md LICENSE NOTICE.md docs/MANUALE.md docs/ISO27001.md docs/SIEM.md contrib/client.conf "$STAGE/doc/"
sed -e 's|/usr/local/bin/ptscry|/usr/bin/ptscry|g' contrib/sshd_config.d/50-ptscry.conf > "$STAGE/doc/sshd-50-ptscry.conf"
cp contrib/rsyslog.d/50-ptscry.conf "$STAGE/doc/rsyslog-50-ptscry.conf"
cp -r contrib/wazuh contrib/elastic contrib/pam.d "$STAGE/doc/"
chmod -R u=rwX,go=rX "$STAGE"
