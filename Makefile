# ptscry - Guido Cauli <guido.cauli@gmail.com>

BIN     := ptscry
LDFLAGS := -s -w
PREFIX  ?= /usr/local

.PHONY: server-dist gui gui-test gui-dist groups frontend frontend-user enable sysctl build build-static test e2e install clean cross dist

# Production build: cgo, so that users/groups resolve through NSS (SSSD/FreeIPA).
build:
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

# Static binary (no FreeIPA/SSSD users): development, standalone mode, containers.
build-static:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test: cross
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

# Static binaries of the remote client for macOS and Windows (and Linux arm64), in dist/.
dist: cross
	mkdir -p dist
	for t in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=; test $$os = windows && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$$os-$$arch$$ext . || exit 1; \
	done
	cd dist && sha256sum * > SHA256SUMS

# The portable part (no server) must keep compiling, statically, for macOS and Windows.
cross:
	for t in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/arm64; do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go vet . ./internal/... && \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -trimpath -ldflags "$(LDFLAGS)" -o /dev/null . || exit 1; \
	done

# Needs python3; the managed test must run as root (creates and removes test users).
e2e: build
	PTSCRY_BIN=./$(BIN) python3 tests/e2e_standalone.py
	PTSCRY_BIN=./$(BIN) python3 tests/e2e_managed.py
	PTSCRY_BIN=./$(BIN) python3 tests/e2e_frontend.py

# Installs binary and example files; does not enable or start anything and never
# overwrites an existing configuration.
# Creates access_group and admin_group only when they do not exist anywhere
# (getent also sees FreeIPA/SSSD groups). It never adds users to them.
groups:
	@for g in ptscry ptscry-admins; do \
	  if getent group $$g >/dev/null; then echo "group $$g: already exists"; \
	  else groupadd -r $$g && echo "group $$g: created"; fi; \
	done

# Optional, for the SSH front-end: its dedicated account (no login shell), the SSH host key and the
# directory for plain public keys (the starting setup: see contrib/ptscry.conf). Idempotent.
# TOTP, FIDO2 and the certificate CA need nothing here: see docs/MANUALE.md 3.5.
frontend frontend-user:
	@getent passwd ptscry-fe >/dev/null && echo "user ptscry-fe: already exists" || \
	  { useradd -r -M -s /usr/sbin/nologin -c "ptscry ssh front-end" ptscry-fe && echo "user ptscry-fe: created"; }
	@install -d -o root -g root -m 0755 /etc/ptscry /etc/ptscry/authorized_keys.d
	@if [ -e /etc/ptscry/ssh_host_ed25519_key ]; then echo "host key: already exists"; \
	  else ssh-keygen -q -t ed25519 -N '' -C "ptscry front-end" -f /etc/ptscry/ssh_host_ed25519_key \
	    && chown root:root /etc/ptscry/ssh_host_ed25519_key* && chmod 0600 /etc/ptscry/ssh_host_ed25519_key \
	    && echo "host key: created (fingerprint: $$(ssh-keygen -lf /etc/ptscry/ssh_host_ed25519_key.pub | cut -d' ' -f2))"; fi
	@echo "Now: set 'frontend = yes' in /etc/ptscry/ptscry.conf, put each user's public key in"
	@echo "  /etc/ptscry/authorized_keys.d/USER (one per line), then: systemctl reload ptscry-monitor && ptscry check"

install: build groups
	install -o root -g root -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)
	@# On RHEL-like systems /usr/local/bin is not in root's or sudo's PATH: link the binary into /usr/bin (never over a real file).
	@if [ "$(PREFIX)" != /usr ]; then \
	  if [ -e /usr/bin/$(BIN) ] && [ ! -L /usr/bin/$(BIN) ]; then echo "/usr/bin/$(BIN) exists and is not a link: left alone"; \
	  else ln -sfn $(PREFIX)/bin/$(BIN) /usr/bin/$(BIN) && echo "link: /usr/bin/$(BIN) -> $(PREFIX)/bin/$(BIN)"; fi; fi
	install -d -o root -g root -m 0755 /etc/ptscry
	test -e /etc/ptscry/ptscry.conf || install -o root -g root -m 0644 contrib/ptscry.conf /etc/ptscry/ptscry.conf
	test -e /etc/pam.d/ptscry || install -o root -g root -m 0644 contrib/pam.d/ptscry.$$(test -e /etc/debian_version && echo debian || echo rhel) /etc/pam.d/ptscry
	install -o root -g root -m 0644 contrib/systemd/ptscry-monitor.service /etc/systemd/system/ptscry-monitor.service
	install -o root -g root -m 0644 contrib/logrotate.d/ptscry /etc/logrotate.d/ptscry
	install -o root -g root -m 0644 contrib/tmpfiles.d/ptscry.conf /etc/tmpfiles.d/ptscry.conf
	install -d -o root -g root -m 0755 $(PREFIX)/share/doc/ptscry
	install -o root -g root -m 0644 README.md docs/ISO27001.md docs/MANUALE.md contrib/client.conf $(PREFIX)/share/doc/ptscry/
	install -o root -g root -m 0644 contrib/sshd_config.d/50-ptscry.conf $(PREFIX)/share/doc/ptscry/sshd-50-ptscry.conf
	install -o root -g root -m 0644 contrib/rsyslog.d/50-ptscry.conf $(PREFIX)/share/doc/ptscry/rsyslog-50-ptscry.conf
	install -d -o root -g root -m 0755 $(PREFIX)/share/man/man1 $(PREFIX)/share/man/man5
	install -o root -g root -m 0644 man/ptscry.1 $(PREFIX)/share/man/man1/ptscry.1
	install -o root -g root -m 0644 man/ptscry.conf.5 $(PREFIX)/share/man/man5/ptscry.conf.5
	install -o root -g root -m 0644 man/ptscry-gui.1 $(PREFIX)/share/man/man1/ptscry-gui.1
	install -o root -g root -m 0644 docs/GUI.md docs/GUIDA-GUI.md $(PREFIX)/share/doc/ptscry/
	cp -r contrib/wazuh contrib/elastic contrib/pam.d docs/SIEM.md $(PREFIX)/share/doc/ptscry/
	systemd-tmpfiles --create /etc/tmpfiles.d/ptscry.conf || true
	$(MAKE) --no-print-directory sysctl
	-systemctl daemon-reload
	@if [ "$(ENABLE)" = "1" ]; then $(MAKE) --no-print-directory enable; else \
	  echo "Add the users to the group: usermod -aG ptscry USER (admins also to ptscry-admins)."; \
	  echo "Next: edit /etc/ptscry/ptscry.conf (access_group, ...), apply the sshd snippet from $(PREFIX)/share/doc/ptscry, then:"; \
	  echo "  sudo make enable        (enable and start the monitor; or: make install ENABLE=1)"; \
	  echo "  ptscry check"; fi

# kernel.yama.ptrace_scope >= 1, persistent. It never lowers a stricter value and
# does nothing on kernels without Yama.
sysctl:
	@cur=$$(cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null); \
	if [ -z "$$cur" ]; then echo "sysctl: no Yama on this kernel, skipped (ptscry check will warn)"; \
	elif [ "$$cur" -ge 1 ]; then echo "sysctl: kernel.yama.ptrace_scope is already $$cur, left as it is"; \
	else install -o root -g root -m 0644 contrib/sysctl.d/60-ptscry.conf /etc/sysctl.d/60-ptscry.conf \
	  && sysctl -q -p /etc/sysctl.d/60-ptscry.conf && echo "sysctl: kernel.yama.ptrace_scope = 1 (persistent, /etc/sysctl.d/60-ptscry.conf)"; fi

# Starts the monitor with the configuration in /etc/ptscry/ptscry.conf: edit it first.
enable:
	systemctl daemon-reload
	systemctl enable --now ptscry-monitor
	@echo "ptscry-monitor enabled and started. Check: ptscry check"

clean:
	rm -f $(BIN)

# Graphical session multiplexer (separate module, needs network the first time
# for Gio; on Linux: libwayland-dev libxkbcommon-dev libx11-xcb-dev
# libxkbcommon-x11-dev libvulkan-dev libegl1-mesa-dev libxcursor-dev libxfixes-dev)
gui:
	cd gui && go build -trimpath -o ../ptscry-gui .

# Packages for Linux (native) and Windows (cross); macOS only on a Mac. See build-gui.sh -h.
gui-dist:
	./build-gui.sh -t all

# Server packages for Linux (.deb, .rpm, .tar.gz) in dist-server/. See build-server.sh.
server-dist:
	./build-server.sh -t

gui-test:
	cd gui && go vet ./... && go test -race ./...
