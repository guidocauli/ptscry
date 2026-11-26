#!/bin/sh
# ptscry-gui - Guido Cauli <guido.cauli@gmail.com>: refresh the menu and icon caches
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database -q /usr/share/applications || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
command -v mandb >/dev/null 2>&1 && mandb -q >/dev/null 2>&1 || true
exit 0
