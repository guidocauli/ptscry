#!/bin/sh
# ptscry - Guido Cauli <guido.cauli@gmail.com>: before removing the server package.
# Only on a real removal (not an upgrade) the monitor is stopped: that ends the sessions.
# deb: prerm remove|upgrade   rpm: %preun $1 (0 = removal, 1 = upgrade)
case "${1:-}" in
  remove|0)
    if command -v systemctl >/dev/null 2>&1; then
      systemctl disable --now ptscry-monitor >/dev/null 2>&1 || true
    fi ;;
esac
exit 0
