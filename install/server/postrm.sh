#!/bin/sh
# ptscry - Guido Cauli <guido.cauli@gmail.com>: after removing the server package.
# The configuration (/etc/ptscry), the groups, the audit log and the recordings
# (/var/log/ptscry) are kept: they are evidence, removing them is the administrator's call.
command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload >/dev/null 2>&1 || true
exit 0
