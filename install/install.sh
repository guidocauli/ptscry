#!/usr/bin/env bash
# ptscry-gui installer (Linux and macOS) - Guido Cauli <guido.cauli@gmail.com>
#
#   ./install.sh [options]            install (run it from the unpacked package)
#   ./install.sh --uninstall [opts]   remove what a previous install put there
#
#   --user       for the current user only: no root needed (the default when not root)
#                  Linux: ~/.local/lib/ptscry-gui, launcher in ~/.local/bin,
#                         menu entry and icons in ~/.local/share
#                  macOS: ~/Applications/ptscry-gui.app
#   --system     for every user of the computer (the default when root; or use sudo)
#                  Linux: /usr/local/lib/ptscry-gui, launcher in /usr/local/bin,
#                         menu entry and icons in /usr/local/share
#                  macOS: /Applications/ptscry-gui.app
#   --prefix D   Linux: install under D instead of /usr/local or ~/.local
#   --uninstall  remove (the settings in ~/.config/ptscry are never touched)
#   -h           this help
#
# "ptscry" (the command used by the "senza server" profile) is put next to
# ptscry-gui, in the private directory, so it never replaces a ptscry you
# already have in the PATH.

set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
MODE=""
PREFIX=""
UNINST=0

usage() { sed -n '2,/^set -e/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }
say()  { printf '==> %s\n' "$*"; }
fail() { printf 'install: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --user) MODE=user ;;
    --system) MODE=system ;;
    --prefix) shift; [ $# -gt 0 ] || fail "--prefix needs a directory"; PREFIX=$1 ;;
    --uninstall) UNINST=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done
if [ -z "$MODE" ]; then
  if [ "$(id -u)" = 0 ]; then MODE=system; else MODE=user; fi
fi
if [ "$MODE" = system ] && [ "$(id -u)" != 0 ]; then
  fail "--system needs root: run it with sudo (or use --user)"
fi
OS=$(uname -s)

# where the files are in the package: next to this script, or in its parent (source tree)
find_file() { # find_file name...
  local n d
  for n in "$@"; do
    for d in "$HERE" "$HERE/.." "$HERE/../gui/assets" "$HERE/assets" "$HERE/../man" "$HERE/../docs"; do
      [ -f "$d/$n" ] && { printf '%s\n' "$d/$n"; return 0; }
    done
  done
  return 1
}

# ---------------------------------------------------------------- Linux ----
linux_dirs() {
  if [ -n "$PREFIX" ]; then
    BASE=$PREFIX
  elif [ "$MODE" = system ]; then
    BASE=/usr/local
  else
    BASE=${HOME}/.local
  fi
  LIB=$BASE/lib/ptscry-gui
  BIN=$BASE/bin
  if [ "$MODE" = user ] && [ -z "$PREFIX" ]; then
    SHARE=${XDG_DATA_HOME:-$HOME/.local/share}
  else
    SHARE=$BASE/share
  fi
}

refresh_caches() {
  command -v update-desktop-database >/dev/null && update-desktop-database "$SHARE/applications" 2>/dev/null || true
  command -v gtk-update-icon-cache >/dev/null && gtk-update-icon-cache -q -t -f "$SHARE/icons/hicolor" 2>/dev/null || true
}

linux_install() {
  linux_dirs
  local gui cli svg f
  gui=$(find_file ptscry-gui) || fail "ptscry-gui not found next to this script (unpack the whole package)"
  cli=$(find_file ptscry) || fail "ptscry not found next to this script"
  say "installing ($MODE) in $LIB"
  mkdir -p "$LIB" "$BIN" "$SHARE/applications" "$SHARE/icons/hicolor/scalable/apps"
  install -m 0755 "$gui" "$LIB/ptscry-gui"
  install -m 0755 "$cli" "$LIB/ptscry"
  ln -sf "$LIB/ptscry-gui" "$BIN/ptscry-gui"
  local tui=""
  if tui=$(find_file ptscry-tui); then
    install -m 0755 "$tui" "$LIB/ptscry-tui"
    # not over the ptscry-tui of the server package
    if [ ! -e "$BIN/ptscry-tui" ] || [ -L "$BIN/ptscry-tui" ]; then ln -sf "$LIB/ptscry-tui" "$BIN/ptscry-tui"; fi
  fi
  local s f
  for s in 16 24 32 48 64 128 256 512; do
    if f=$(find_file "ptscry-gui-$s.png"); then
      mkdir -p "$SHARE/icons/hicolor/${s}x${s}/apps"
      install -m 0644 "$f" "$SHARE/icons/hicolor/${s}x${s}/apps/ptscry-gui.png"
    fi
  done
  if svg=$(find_file ptscry-gui.svg); then
    install -m 0644 "$svg" "$SHARE/icons/hicolor/scalable/apps/ptscry-gui.svg"
  fi
  local m d
  if m=$(find_file ptscry-gui.1); then
    mkdir -p "$SHARE/man/man1"
    install -m 0644 "$m" "$SHARE/man/man1/ptscry-gui.1"
  fi
  if [ -n "$tui" ] && m=$(find_file ptscry-tui.1); then
    mkdir -p "$SHARE/man/man1"
    install -m 0644 "$m" "$SHARE/man/man1/ptscry-tui.1"
  fi
  mkdir -p "$SHARE/doc/ptscry-gui"
  for d in GUIDA-GUI.md GUI.md LICENSE NOTICE.md; do
    if f=$(find_file "$d"); then install -m 0644 "$f" "$SHARE/doc/ptscry-gui/$d"; fi
  done
  cat > "$SHARE/applications/ptscry-gui.desktop" <<DESK
[Desktop Entry]
Type=Application
Version=1.0
Name=ptscry-gui
GenericName=Terminal multiplexer
GenericName[it]=Multiplexer di terminale
Comment=Persistent terminal sessions, local and remote - Guido Cauli <guido.cauli@gmail.com>
Comment[it]=Sessioni di terminale persistenti, locali e remote - Guido Cauli <guido.cauli@gmail.com>
TryExec=$LIB/ptscry-gui
Exec=$LIB/ptscry-gui
Icon=ptscry-gui
Terminal=false
Categories=System;TerminalEmulator;
Keywords=terminal;terminale;ssh;session;sessione;multiplexer;
StartupWMClass=ptscry-gui
DESK
  chmod 0644 "$SHARE/applications/ptscry-gui.desktop"
  if [ -n "$tui" ]; then
    cat > "$SHARE/applications/ptscry-tui.desktop" <<DESK
[Desktop Entry]
Type=Application
Version=1.0
Name=ptscry-tui
GenericName=Terminal multiplexer (text)
GenericName[it]=Multiplexer di terminale (testo)
Comment=ptscry sessions in panes on a text terminal - Guido Cauli <guido.cauli@gmail.com>
Comment[it]=Sessioni ptscry in pannelli su un terminale di testo - Guido Cauli <guido.cauli@gmail.com>
TryExec=$LIB/ptscry-tui
Exec=$LIB/ptscry-tui
Icon=ptscry-gui
Terminal=true
Categories=System;TerminalEmulator;
Keywords=terminal;terminale;tmux;panes;pannelli;session;sessione;multiplexer;
DESK
    chmod 0644 "$SHARE/applications/ptscry-tui.desktop"
  fi
  refresh_caches
  say "done: run ptscry-gui, or find it in the applications menu"
  case ":$PATH:" in *":$BIN:"*) ;; *) say "note: $BIN is not in your PATH (the menu entry works anyway)" ;; esac
}

linux_uninstall() {
  linux_dirs
  say "removing ($MODE) from $BASE"
  rm -f "$BIN/ptscry-gui" "$SHARE/applications/ptscry-gui.desktop" "$SHARE/applications/ptscry-tui.desktop"
  [ -L "$BIN/ptscry-tui" ] && rm -f "$BIN/ptscry-tui" "$SHARE/icons/hicolor/scalable/apps/ptscry-gui.svg"
  local s
  for s in 16 24 32 48 64 128 256 512; do rm -f "$SHARE/icons/hicolor/${s}x${s}/apps/ptscry-gui.png"; done
  rm -f "$SHARE/man/man1/ptscry-gui.1" "$SHARE/man/man1/ptscry-tui.1" "$SHARE/doc/ptscry-gui/GUIDA-GUI.md" "$SHARE/doc/ptscry-gui/GUI.md" \
    "$SHARE/doc/ptscry-gui/LICENSE" "$SHARE/doc/ptscry-gui/NOTICE.md"
  rmdir "$SHARE/doc/ptscry-gui" 2>/dev/null || true
  rm -f "$LIB/ptscry-gui" "$LIB/ptscry" "$LIB/ptscry-tui"
  rmdir "$LIB" 2>/dev/null || true
  refresh_caches
  say "done (the settings in ~/.config/ptscry were left alone)"
}

# ---------------------------------------------------------------- macOS ----
mac_app() {
  if [ -n "$PREFIX" ]; then APPDIR=$PREFIX
  elif [ "$MODE" = system ]; then APPDIR=/Applications
  else APPDIR=$HOME/Applications; fi
  APP=$APPDIR/ptscry-gui.app
}

mac_install() {
  mac_app
  local gui cli icns ver
  gui=$(find_file ptscry-gui) || fail "ptscry-gui not found next to this script"
  cli=$(find_file ptscry) || fail "ptscry not found next to this script"
  icns=$(find_file ptscry-gui.icns) || fail "ptscry-gui.icns not found"
  ver=$("$gui" -version 2>/dev/null | awk "{print \$2}") || true
  say "installing ($MODE) $APP"
  rm -rf "$APP"
  mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
  install -m 0755 "$gui" "$APP/Contents/MacOS/ptscry-gui"
  install -m 0755 "$cli" "$APP/Contents/MacOS/ptscry"
  local tui
  if tui=$(find_file ptscry-tui); then install -m 0755 "$tui" "$APP/Contents/MacOS/ptscry-tui"; fi
  install -m 0644 "$icns" "$APP/Contents/Resources/ptscry-gui.icns"
  cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>ptscry-gui</string>
  <key>CFBundleDisplayName</key><string>ptscry-gui</string>
  <key>CFBundleIdentifier</key><string>org.ptscry.gui</string>
  <key>CFBundleExecutable</key><string>ptscry-gui</string>
  <key>CFBundleIconFile</key><string>ptscry-gui</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${ver:-0.9.1-gui}</string>
  <key>CFBundleVersion</key><string>${ver:-0.9.1-gui}</string>
  <key>NSHumanReadableCopyright</key><string>Guido Cauli &lt;guido.cauli@gmail.com&gt;</string>
  <key>LSMinimumSystemVersion</key><string>10.13</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
</dict></plist>
PLIST
  # unsigned binaries copied from elsewhere may carry the quarantine flag
  xattr -dr com.apple.quarantine "$APP" 2>/dev/null || true
  say "done: open it from $APPDIR (Launchpad / Finder)"
}

mac_uninstall() {
  mac_app
  say "removing $APP"
  rm -rf "$APP"
  say "done (the settings in ~/.config/ptscry were left alone)"
}

case "$OS" in
  Linux)  if [ $UNINST = 1 ]; then linux_uninstall; else linux_install; fi ;;
  Darwin) if [ $UNINST = 1 ]; then mac_uninstall; else mac_install; fi ;;
  *) fail "unsupported system: $OS (on Windows use install.bat)" ;;
esac
