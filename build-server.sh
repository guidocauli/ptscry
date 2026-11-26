#!/usr/bin/env bash
# ptscry server packages - Guido Cauli <guido.cauli@gmail.com>
#
# Builds the ptscry server for Linux (cgo, so that users and groups resolve
# through NSS/SSSD and PAM works) and packs it in dist-server/:
#   ptscry_VERSION-1_ARCH.deb       Debian 12+, Ubuntu 22.04+            (amd64, arm64)
#   ptscry-VERSION-1.ARCH.rpm       RHEL/Alma/Rocky 9+, Fedora, openSUSE  (x86_64, aarch64)
#   ptscry-server-linux-ARCH.tar.gz binary, contrib and docs, for "make install"-style setups
# The binary needs glibc 2.34 or later. Packages need nfpm
# (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest). arm64 from an
# amd64 machine needs the cross compiler (Debian/Ubuntu: gcc-aarch64-linux-gnu
# libc6-dev-arm64-cross; Fedora: gcc-aarch64-linux-gnu sysroot-aarch64-fc*-glibc).
#
#   ./build-server.sh [-t] [arch...]    -t: run the tests first; arch: amd64 arm64 (default: both)
#
# The packages install /usr/bin/ptscry, the systemd unit (not enabled), the
# configuration in /etc/ptscry (kept on upgrade and removal), PAM, logrotate,
# tmpfiles, man pages and docs; they create the groups ptscry and
# ptscry-admins when missing. They never start or restart the monitor
# (that would end the sessions): after an upgrade, "ptscry admin reexec".

set -euo pipefail
cd "$(dirname "$0")"
OUT=dist-server
TEST=0
[ "${1:-}" = -t ] && { TEST=1; shift; }
ARCHES=("${@:-amd64 arm64}")
# shellcheck disable=SC2206
ARCHES=(${ARCHES[*]})
say()  { printf '==> %s\n' "$*"; }
fail() { printf 'build-server: %s\n' "$*" >&2; exit 1; }

[ "$(go env GOOS)" = linux ] || fail "the server is built on Linux"
HOSTARCH=$(go env GOARCH)
VER=$(sed -n 's/.*progVersion *= *"\(.*\)".*/\1/p' version.go | head -1)
NFPM=$(command -v nfpm || echo "$(go env GOPATH)/bin/nfpm")
[ -x "$NFPM" ] || fail "nfpm not found: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"

if [ "$TEST" = 1 ]; then say "tests"; go vet . ./internal/...; go test ./...; fi
rm -rf "$OUT"; mkdir -p "$OUT"

cross_cc() { # the C compiler for an architecture, or nothing
  case $1 in
    "$HOSTARCH") echo "${CC:-cc}" ;;
    arm64) command -v aarch64-linux-gnu-gcc || true ;;
    amd64) command -v x86_64-linux-gnu-gcc || true ;;
  esac
}

build_arch() { # build_arch <goarch>
  local ARCH=$1 cc STAGE need f pam T
  cc=$(cross_cc "$ARCH")
  if [ -z "$cc" ]; then say "skipping $ARCH: no C compiler for it (see the head of this script)"; return 0; fi
  STAGE=$(mktemp -d)
  say "ptscry $VER linux/$ARCH (cgo, $cc)"
  CC=$cc CGO_ENABLED=1 GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$STAGE/ptscry" .
  say "ptscry-tui linux/$ARCH (panes on a text terminal)"
  (cd tui && CGO_ENABLED=0 GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$STAGE/ptscry-tui" .)
  need=$(readelf -V "$STAGE/ptscry" 2>/dev/null | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1 || true)
  case "$need" in GLIBC_2.3[5-9]|GLIBC_2.[4-9]*) fail "the $ARCH binary needs $need: it would not run on RHEL/Alma 9 (glibc 2.34)";; esac

  # files adapted to /usr/bin (the sources say /usr/local, as "make install")
  sed -e 's|/usr/local/bin/ptscry|/usr/bin/ptscry|g; s|/usr/local/share/doc/ptscry|/usr/share/doc/ptscry|g' \
    contrib/systemd/ptscry-monitor.service > "$STAGE/ptscry-monitor.service"
  mkdir -p "$STAGE/man" "$STAGE/doc"
  gzip -9n -c man/ptscry.1 > "$STAGE/man/ptscry.1.gz"
  gzip -9n -c man/ptscry.conf.5 > "$STAGE/man/ptscry.conf.5.gz"
  gzip -9n -c man/ptscry-tui.1 > "$STAGE/man/ptscry-tui.1.gz"
  cp README.md LICENSE NOTICE.md docs/MANUALE.md docs/ISO27001.md docs/SIEM.md contrib/client.conf "$STAGE/doc/"
  sed -e 's|/usr/local/bin/ptscry|/usr/bin/ptscry|g' contrib/sshd_config.d/50-ptscry.conf > "$STAGE/doc/sshd-50-ptscry.conf"
  cp contrib/rsyslog.d/50-ptscry.conf "$STAGE/doc/rsyslog-50-ptscry.conf"
  cp -r contrib/wazuh contrib/elastic contrib/pam.d "$STAGE/doc/"
  chmod -R u=rwX,go=rX "$STAGE"

  for f in deb rpm; do
    pam=contrib/pam.d/ptscry.debian; [ $f = rpm ] && pam=contrib/pam.d/ptscry.rhel
    sed -e "s|\${PKG_STAGE}|$STAGE|g; s|\${PKG_PAM}|$pam|g; s|\${PKG_VERSION}|$VER|g; s|\${PKG_ARCH}|$ARCH|g" \
      install/server/nfpm.yaml > "$STAGE/nfpm.yaml"
    say "package: .$f ($ARCH)"
    "$NFPM" package -f "$STAGE/nfpm.yaml" -p $f -t "$OUT/" >/dev/null
  done

  say "archive: ptscry-server-linux-$ARCH.tar.gz"
  T="$STAGE/ptscry-$VER"
  mkdir -p "$T"
  cp "$STAGE/ptscry" "$STAGE/ptscry-tui" install/server/ptscry-setup-frontend README.md LICENSE NOTICE.md "$T/"
  cp -r man contrib docs "$T/"
  tar -C "$STAGE" -czf "$OUT/ptscry-server-linux-$ARCH.tar.gz" "ptscry-$VER"
  rm -rf "$STAGE"
}

for a in "${ARCHES[@]}"; do build_arch "$a"; done
(cd "$OUT" && sha256sum ./* > SHA256SUMS)
say "done: $OUT/"
ls -1 "$OUT" | sed 's/^/    /'
