#!/bin/sh
# ptscry - Guido Cauli <guido.cauli@gmail.com>: before removing the server package.
# Only on a real removal (not an upgrade) the monitor is stopped: that ends the sessions.
# Not when another package takes its place: ptscry-gui contains the server too
# (deb: "remove in-favour <package>"; rpm: the other package already owns the unit).
# deb: prerm remove|upgrade   rpm: %preun $1 (0 = removal, 1 = upgrade)
unit=/usr/lib/systemd/system/ptscry-monitor.service
kept=0
case "${2:-}" in in-favour) kept=1 ;; esac
if command -v dpkg >/dev/null 2>&1 && dpkg -S "$unit" 2>/dev/null | grep -q ','; then kept=1; fi
if command -v rpm >/dev/null 2>&1 && [ "$(rpm -qf --qf '%{NAME}\n' "$unit" 2>/dev/null | sort -u | wc -l)" -gt 1 ]; then kept=1; fi
case "${1:-}" in
  remove|0)
    if [ "$kept" = 0 ] && command -v systemctl >/dev/null 2>&1; then
      systemctl disable --now ptscry-monitor >/dev/null 2>&1 || true
    fi ;;
esac
exit 0
