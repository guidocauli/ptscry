#!/bin/sh
# ptscry - Guido Cauli <guido.cauli@gmail.com>: after installing or upgrading the server package.
# Never enables, starts or restarts the monitor: a restart would end every session.
# deb: postinst configure <old-version>   rpm: %post $1 (1 = install, 2 = upgrade)
set -e
for g in ptscry ptscry-admins; do
  getent group "$g" >/dev/null 2>&1 || groupadd -r "$g" || true
done
command -v systemd-tmpfiles >/dev/null 2>&1 && systemd-tmpfiles --create /usr/lib/tmpfiles.d/ptscry.conf || true
# kernel.yama.ptrace_scope >= 1, never lowering a stricter value (as "make sysctl")
cur=$(cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null || true)
if [ -n "$cur" ] && [ "$cur" -lt 1 ] && [ ! -e /etc/sysctl.d/60-ptscry.conf ]; then
  printf '# ptscry - Guido Cauli <guido.cauli@gmail.com>\nkernel.yama.ptrace_scope = 1\n' > /etc/sysctl.d/60-ptscry.conf
  sysctl -q -p /etc/sysctl.d/60-ptscry.conf || true
fi
command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload >/dev/null 2>&1 || true
upgrade=0
case "${1:-}" in
  configure) [ -n "${2:-}" ] && upgrade=1 ;;
  2) upgrade=1 ;;
esac
if [ "$upgrade" = 1 ]; then
  if systemctl is-active --quiet ptscry-monitor 2>/dev/null; then
    echo "ptscry: the running monitor is still the old version; to switch without closing the sessions: ptscry admin reexec"
  fi
else
  cat <<'MSG'
ptscry installed. Next:
  1. edit /etc/ptscry/ptscry.conf (access_group, ...)
  2. add the users: ptscry setup (wizard), or: ptscry setup users add [-admin] USER
  3. SSH: see /usr/share/doc/ptscry/sshd-50-ptscry.conf; or the built-in front-end: ptscry-setup-frontend,
     then the users' public keys: ptscry setup keys add USER FILE
  4. systemctl enable --now ptscry-monitor && ptscry check
Manual: /usr/share/doc/ptscry/MANUALE.md, man ptscry
MSG
fi
exit 0
