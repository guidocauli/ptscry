#!/bin/sh
# ptscry-gui - Guido Cauli <guido.cauli@gmail.com>: at the end of the installation
# (rpm: %posttrans, after the replaced "ptscry" package is gone; deb: in postinst).
# If the replaced package stopped the monitor, enable and start it again.
if [ -e /run/ptscry-pkg/was-enabled ] && command -v systemctl >/dev/null 2>&1; then
  systemctl enable ptscry-monitor >/dev/null 2>&1 || true
fi
if [ -e /run/ptscry-pkg/was-active ] && command -v systemctl >/dev/null 2>&1 && ! systemctl is-active --quiet ptscry-monitor 2>/dev/null; then
  systemctl start ptscry-monitor >/dev/null 2>&1 || true
  echo "ptscry: the old ptscry package stopped the monitor while being replaced; it has been started again (the sessions it had are closed)"
fi
rm -rf /run/ptscry-pkg 2>/dev/null || true
exit 0
