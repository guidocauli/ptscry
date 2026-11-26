#!/usr/bin/env bash
# ptscry-gui build script - Guido Cauli <guido.cauli@gmail.com>
#
# Builds the graphical session multiplexer (gui/) and the ptscry command it
# uses for the "senza server" profile, and packs them per platform in dist-gui/.
#
#   ./build-gui.sh [options] [target...]
#
#   targets:  linux   native build (needs cgo and the development packages below)
#             windows amd64 and arm64, cross-compiled, no cgo, no other tools
#             macos   only on a Mac (Gio needs cgo and Apple's frameworks there)
#             all     linux + windows (+ macos when run on a Mac); the default
#   options:  -n  do not pack, leave the binaries in dist-gui/<os>-<arch>/
#             -t  run the tests first (go vet + go test -race of gui/)
#             -c  check the build dependencies and stop
#             -I  no installers (by default, when the tools are there: a Windows
#                 setup .exe with makensis, .deb and .rpm with nfpm, a .dmg on a Mac)
#             -h  this help
#
# Linux build dependencies (the first run says which one is missing):
#   Debian/Ubuntu: gcc pkg-config libwayland-dev libxkbcommon-dev libx11-xcb-dev
#                  libxkbcommon-x11-dev libx11-dev libxcursor-dev libxfixes-dev
#                  libegl1-mesa-dev libvulkan-dev
#   RedHat/Alma:   gcc pkgconf-pkg-config wayland-devel libxkbcommon-devel
#                  libxkbcommon-x11-devel libX11-devel libXcursor-devel
#                  libXfixes-devel mesa-libEGL-devel vulkan-loader-devel
#   Arch:          gcc pkgconf wayland libxkbcommon libxkbcommon-x11 libx11
#                  libxcursor libxfixes mesa vulkan-headers
# Installers: Windows ptscry-gui-setup-<arch>.exe (NSIS: apt install nsis), Linux
# .deb and .rpm (nfpm: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest),
# macOS .dmg with ptscry-gui.app (hdiutil, only on a Mac).
# Every package contains its installer (install.sh, or install.bat / install.ps1 on
# Windows) and the icons; the Windows exe has the icon embedded (gui/winres, rebuilt
# with go-winres: https://github.com/tc-hib/go-winres).
# At run time on Linux: libwayland-client, libxkbcommon, libEGL (X11 or Wayland).
# The first build downloads the Go modules (network needed once).

set -euo pipefail
cd "$(dirname "$0")"

OUT=dist-gui
LDFLAGS="-s -w"
PACK=1
TEST=0
CHECK=0
INST=1

usage() { sed -n '2,/^set -e/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while getopts "ntchI" o; do
  case $o in
    I) INST=0 ;;
    n) PACK=0 ;;
    t) TEST=1 ;;
    c) CHECK=1 ;;
    h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
TARGETS=("${@:-all}")

say()  { printf '==> %s\n' "$*"; }
fail() { printf 'build-gui: %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null || fail "go not found (the Go toolchain is needed: https://go.dev/dl/)"
need=$(awk '/^go /{print $2; exit}' gui/go.mod)
have=$(go env GOVERSION | sed 's/^go//')
if [ "$(printf '%s\n%s\n' "${need%.*}" "${have%.*}" | sort -V | head -1)" != "${need%.*}" ]; then
  fail "Go $need or newer is needed, found $have"
fi

HOSTOS=$(go env GOHOSTOS)
HOSTARCH=$(go env GOHOSTARCH)
VER_CLI=$(sed -n 's/.*progVersion *= *"\(.*\)".*/\1/p' version.go | head -1)
VER_GUI=$(sed -n 's/.*guiVersion *= *"\(.*\)".*/\1/p' gui/conn.go | head -1)

check_linux() {
  local missing=() lib
  command -v gcc >/dev/null || command -v cc >/dev/null || missing+=("a C compiler (gcc)")
  if ! command -v pkg-config >/dev/null; then
    missing+=("pkg-config")
  else
    for lib in wayland-client xkbcommon xkbcommon-x11 x11 x11-xcb xcursor xfixes egl vulkan; do
      pkg-config --exists "$lib" 2>/dev/null || missing+=("$lib (pkg-config)")
    done
  fi
  if [ ${#missing[@]} -gt 0 ]; then
    printf 'build-gui: missing build dependencies:\n' >&2
    printf '  - %s\n' "${missing[@]}" >&2
    cat >&2 <<'EOF'

  Debian/Ubuntu: apt install gcc pkg-config libwayland-dev libxkbcommon-dev libx11-xcb-dev \
                 libxkbcommon-x11-dev libx11-dev libxcursor-dev libxfixes-dev libegl1-mesa-dev libvulkan-dev
  RedHat/Alma:   dnf install gcc pkgconf-pkg-config wayland-devel libxkbcommon-devel libxkbcommon-x11-devel \
                 libX11-devel libXcursor-devel libXfixes-devel mesa-libEGL-devel vulkan-loader-devel
  Arch:          pacman -S gcc pkgconf wayland libxkbcommon libxkbcommon-x11 libx11 libxcursor libxfixes mesa vulkan-headers
EOF
    return 1
  fi
}

pack() { # pack <dir-with-binaries> <archive-name-without-extension> <tar|zip>
  local dir=$1 name=$2 kind=$3
  [ "$PACK" = 1 ] || return 0
  cp README.md "$dir/README.md" 2>/dev/null || true
  cp LICENSE NOTICE.md "$dir/"
  [ -f docs/GUI.md ] && cp docs/GUI.md "$dir/GUI.md"
  [ -f docs/GUIDA-GUI.md ] && cp docs/GUIDA-GUI.md "$dir/GUIDA-GUI.md"
  # installer and icons
  case $name in
    *windows*) cp install/install.bat install/install.ps1 gui/assets/ptscry-gui.ico "$dir/" ;;
    *)         cp man/ptscry-gui.1 man/ptscry-tui.1 "$dir/"
               cp install/install.sh gui/assets/ptscry-gui.svg gui/assets/ptscry-gui-*.png "$dir/"
               case $name in *macos*) cp gui/assets/ptscry-gui.icns "$dir/" ;; esac ;;
  esac
  if [ "$kind" = tar ]; then
    tar -C "$dir" -czf "$OUT/$name.tar.gz" .
  else
    (cd "$dir" && python3 - "$PWD/../$name.zip" <<'PY'
import os, sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w", zipfile.ZIP_DEFLATED) as z:
    for f in sorted(os.listdir(".")):
        z.write(f)
PY
    )
  fi
}

build_one() { # build_one <goos> <goarch> <cgo> <gui-ldflags-extra> <ext>
  local os=$1 arch=$2 cgo=$3 extra=$4 ext=$5
  local dir="$OUT/$os-$arch"
  rm -rf "$dir"; mkdir -p "$dir"
  say "ptscry-gui $os/$arch (cgo=$cgo)"
  (cd gui && CGO_ENABLED=$cgo GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS $extra" -o "../$dir/ptscry-gui$ext" .)
  # the command-line client the "senza server" profile runs (not on Windows: no local host there)
  if [ "$os" != windows ]; then
    say "ptscry $os/$arch (static, standalone mode)"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$dir/ptscry$ext" .
    say "ptscry-tui $os/$arch (panes on a text terminal)"
    (cd tui && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "../$dir/ptscry-tui$ext" .)
  else
    say "ptscry $os/$arch (remote client)"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$dir/ptscry$ext" .
  fi
}

# numeric version for package managers: 0.7.5-gui -> 0.7.5
pkgver() { printf '%s' "${VER_GUI%%-*}"; }

nfpm_bin() { command -v nfpm 2>/dev/null || { [ -x "$(go env GOPATH)/bin/nfpm" ] && echo "$(go env GOPATH)/bin/nfpm"; } || true; }

inst_linux() { # inst_linux <arch>
  [ "$PACK" = 1 ] && [ "$INST" = 1 ] || return 0
  local arch=$1 nf man yaml f
  nf=$(nfpm_bin)
  if [ -z "$nf" ]; then say "no nfpm: skipping .deb/.rpm (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest)"; return 0; fi
  man=$(mktemp); yaml=$(mktemp)
  gzip -9n -c man/ptscry-gui.1 > "$man"; chmod 0644 "$man"
  tman=$(mktemp); gzip -9n -c man/ptscry-tui.1 > "$tman"; chmod 0644 "$tman"
  sed -e "s|\${PKG_SRC}|$OUT/linux-$arch|g; s|\${PKG_MAN}|$man|g; s|\${PKG_TUIMAN}|$tman|g; s|\${PKG_VERSION}|$(pkgver)|g; s|\${PKG_ARCH}|$arch|g" \
    install/linux/nfpm.yaml > "$yaml"
  for f in deb rpm; do
    say "installer: .$f"
    "$nf" package -f "$yaml" -p "$f" -t "$OUT/" >/dev/null || fail "nfpm $f failed"
  done
  rm -f "$man" "$yaml"
}

inst_windows() { # inst_windows <arch>
  [ "$PACK" = 1 ] && [ "$INST" = 1 ] || return 0
  local arch=$1
  if ! command -v makensis >/dev/null; then say "no makensis: skipping the Windows setup (apt install nsis)"; return 0; fi
  say "installer: ptscry-gui-setup-windows-$arch.exe"
  cp README.md "$OUT/windows-$arch/README.md" 2>/dev/null || true
  cp docs/GUI.md docs/GUIDA-GUI.md gui/assets/ptscry-gui.ico LICENSE NOTICE.md "$OUT/windows-$arch/"
  # the license page of the setup: the notice (with the disclaimer), then the license, CRLF
  { cat NOTICE.md; printf '\n\n'; cat LICENSE; } | sed 's/$/\r/' > "$OUT/windows-$arch/LICENSE.txt"
  makensis -V1 -DVERSION="$(pkgver)" -DARCH="$arch" -DSRC="$PWD/$OUT/windows-$arch" \
    -DOUT="$PWD/$OUT/ptscry-gui-setup-windows-$arch.exe" install/windows/ptscry-gui.nsi || fail "makensis failed"
}

inst_macos() { # inst_macos <arch>: ptscry-gui.app in a .dmg (on a Mac)
  [ "$PACK" = 1 ] && [ "$INST" = 1 ] || return 0
  local arch=$1 stage
  command -v hdiutil >/dev/null || { say "no hdiutil: skipping the .dmg"; return 0; }
  stage=$(mktemp -d)
  cp "$OUT/darwin-$arch/ptscry-gui" "$OUT/darwin-$arch/ptscry" gui/assets/ptscry-gui.icns install/install.sh "$stage/"
  (cd "$stage" && ./install.sh --prefix "$stage/dmg" >/dev/null) || fail "could not make ptscry-gui.app"
  rm -f "$stage"/ptscry-gui "$stage"/ptscry "$stage"/ptscry-gui.icns "$stage"/install.sh
  ln -s /Applications "$stage/dmg/Applications"
  cp docs/GUIDA-GUI.md "$stage/dmg/"
  say "installer: ptscry-gui-macos-$arch.dmg"
  hdiutil create -quiet -volname "ptscry-gui" -srcfolder "$stage/dmg" -ov -format UDZO "$OUT/ptscry-gui-macos-$arch.dmg" || fail "hdiutil failed"
  rm -rf "$stage"
}

do_linux() {
  [ "$HOSTOS" = linux ] || fail "the Linux build must run on Linux (cgo, system libraries)"
  check_linux || return 1
  build_one linux "$HOSTARCH" 1 "" ""
  pack "$OUT/linux-$HOSTARCH" "ptscry-gui-linux-$HOSTARCH" tar
  inst_linux "$HOSTARCH"
}

do_windows() {
  local a
  for a in amd64 arm64; do
    build_one windows "$a" 0 "-H=windowsgui" ".exe"
    pack "$OUT/windows-$a" "ptscry-gui-windows-$a" zip
    inst_windows "$a"
  done
}

do_macos() {
  [ "$HOSTOS" = darwin ] || fail "the macOS build must run on a Mac (Gio needs cgo and Apple's frameworks); copy the sources there and run: ./build-gui.sh macos"
  build_one darwin "$HOSTARCH" 1 "" ""
  pack "$OUT/darwin-$HOSTARCH" "ptscry-gui-macos-$HOSTARCH" tar
  inst_macos "$HOSTARCH"
}

if [ "$CHECK" = 1 ]; then
  case "$HOSTOS" in linux) check_linux && say "build dependencies: ok" ;; *) say "nothing to check on $HOSTOS" ;; esac
  exit 0
fi

if [ "$TEST" = 1 ]; then
  say "tests"
  (cd gui && go vet ./... && go test -race ./...)
fi

mkdir -p "$OUT"
say "ptscry $VER_CLI, ptscry-gui $VER_GUI"
for t in "${TARGETS[@]}"; do
  case $t in
    linux)   do_linux ;;
    windows) do_windows ;;
    macos)   do_macos ;;
    all)
      case "$HOSTOS" in
        linux)  do_linux; do_windows ;;
        darwin) do_macos; do_windows ;;
        *)      do_windows ;;
      esac ;;
    *) fail "unknown target: $t (linux, windows, macos, all)" ;;
  esac
done

if [ "$PACK" = 1 ]; then
  (cd "$OUT" && ls ./*.tar.gz ./*.zip >/dev/null 2>&1 && sha256sum $(ls ./*.tar.gz ./*.zip ./*.exe ./*.deb ./*.rpm ./*.dmg 2>/dev/null) > SHA256SUMS || true)
fi
say "done: $OUT/"
ls -1 "$OUT" | sed 's/^/    /'
