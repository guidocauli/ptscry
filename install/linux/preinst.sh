#!/bin/sh
# ptscry-gui - Guido Cauli <guido.cauli@gmail.com>: before installing or upgrading.
# The package contains the server and takes the place of the "ptscry" package;
# the removal script of ptscry 0.7.0/0.7.1 stops the monitor. Remember whether
# it was running (and enabled) so that the end of the installation restarts it.
rm -rf /run/ptscry-pkg 2>/dev/null || true # what an interrupted installation may have left
mkdir -p /run/ptscry-pkg 2>/dev/null || true
if command -v systemctl >/dev/null 2>&1; then
  systemctl is-active --quiet ptscry-monitor 2>/dev/null && : > /run/ptscry-pkg/was-active
  systemctl is-enabled --quiet ptscry-monitor 2>/dev/null && : > /run/ptscry-pkg/was-enabled
fi
exit 0
