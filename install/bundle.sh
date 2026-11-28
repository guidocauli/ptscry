#!/usr/bin/env bash
# ptscry - Guido Cauli <guido.cauli@gmail.com>
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# bundle.sh: groups the release packages by system, one archive per system with
# every architecture inside:
#
#   ptscry-VERSION-gui-linux.tar.gz    ptscry-VERSION/amd64/ ... ptscry-VERSION/arm64/ ...
#   ptscry-VERSION-gui-windows.zip     ptscry-VERSION/amd64/ ... ptscry-VERSION/arm64/ ...
#   ptscry-VERSION-gui-macos.zip       ptscry-VERSION/amd64/ ... ptscry-VERSION/arm64/ ...
#   ptscry-VERSION-server-linux.tar.gz ptscry-VERSION/amd64/ ... ptscry-VERSION/arm64/ ...
#
# Each architecture directory holds that architecture's packages (installer,
# deb, rpm, dmg, portable archive) and their SHA256SUMS; the base directory a
# LEGGIMI.txt. A file of unknown system or architecture stops the script.
#
#   install/bundle.sh VERSION INPUT_DIR OUTPUT_DIR

set -euo pipefail
[ $# -eq 3 ] || { sed -n '2,/^set -e/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2; exit 2; }
VER=$1 IN=$2 OUT=$3
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

arch_of() {
  case "$1" in
    *amd64* | *x86_64*) echo amd64 ;;
    *arm64* | *aarch64*) echo arm64 ;;
    *) echo "" ;;
  esac
}

system_of() { # gui-linux, gui-windows, gui-macos, server-linux
  local f=$1
  case "$f" in
    ptscry-gui*windows* | ptscry-gui-setup-windows*) echo gui-windows ;;
    ptscry-gui*macos*) echo gui-macos ;;
    ptscry-gui*) echo gui-linux ;;
    ptscry-server-linux* | ptscry_*.deb | ptscry-[0-9]*.rpm) echo server-linux ;;
    *) echo "" ;;
  esac
}

n=0
for p in "$IN"/*; do
  [ -f "$p" ] || continue
  f=$(basename "$p")
  case "$f" in SHA256SUMS | *-src.tar.gz) continue ;; esac
  sys=$(system_of "$f") arch=$(arch_of "$f")
  [ -n "$sys" ] && [ -n "$arch" ] || { echo "bundle.sh: $f: system or architecture unknown" >&2; exit 1; }
  mkdir -p "$WORK/$sys/ptscry-$VER/$arch"
  cp "$p" "$WORK/$sys/ptscry-$VER/$arch/"
  n=$((n + 1))
done
[ "$n" -gt 0 ] || { echo "bundle.sh: no packages in $IN" >&2; exit 1; }

readme() { # readme <system>
  printf 'ptscry %s - Guido Cauli <guido.cauli@gmail.com>\r\n\r\n' "$VER"
  case "$1" in
    gui-linux) printf 'ptscry-gui per Linux (con il server ptscry e ptscry-tui).\r\n.deb: sudo apt install ./ptscry-gui_*.deb   .rpm: sudo dnf install ./ptscry-gui-*.rpm\r\nportabile: tar xzf ptscry-gui-linux-*.tar.gz, poi ./install.sh\r\n' ;;
    gui-windows) printf 'ptscry-gui per Windows.\r\nInstaller: ptscry-gui-setup-windows-*.exe   portabile: ptscry-gui-windows-*.zip (install.bat)\r\n' ;;
    gui-macos) printf 'ptscry-gui per macOS (arm64 = Apple Silicon, amd64 = Intel).\r\n.dmg: trascinare ptscry-gui.app in Applicazioni   portabile: ptscry-gui-macos-*.tar.gz, poi ./install.sh\r\n' ;;
    server-linux) printf 'Server ptscry per Linux.\r\n.deb: sudo apt install ./ptscry_*.deb   .rpm: sudo dnf install ./ptscry-*.rpm\r\naltri: ptscry-server-linux-*.tar.gz\r\n' ;;
  esac
  printf '\r\nUna directory per architettura: amd64 (x86_64), arm64 (aarch64); SHA256SUMS in ognuna.\r\n'
  printf 'Software fornito "cosi'"'"' com'"'"'e'"'"'", senza alcuna garanzia: vedi LICENSE e NOTICE.md.\r\n'
}

for d in "$WORK"/*; do
  sys=$(basename "$d") base="$d/ptscry-$VER"
  for a in "$base"/*/; do
    (cd "$a" && sha256sum -- * > SHA256SUMS)
  done
  readme "$sys" > "$base/LEGGIMI.txt"
  case "$sys" in
    gui-windows | gui-macos)
      name="ptscry-$VER-$sys.zip"
      (cd "$d" && zip -qr -X "$name" "ptscry-$VER")
      ;;
    *)
      name="ptscry-$VER-$sys.tar.gz"
      tar -C "$d" --owner=0 --group=0 -czf "$d/$name" "ptscry-$VER"
      ;;
  esac
  mv "$d/$name" "$OUT/"
  echo "$name:"
  (cd "$d" && find "ptscry-$VER" -type f | sort | sed 's/^/    /')
done
