# ptscry - Guido Cauli <guido.cauli@gmail.com>

Sessioni PTY persistenti e **auditate**: detach, reattach, replay dell'output, registrazione e
audit a prova di manomissione. Nome: **pt**(y) + **scry** (divinare, osservare a distanza).

Versione 0.7.2 (GUI 0.9.0): la finestra della GUI come quella di RexDesk (barra dei server con gruppi, Preferiti e sessioni, import da RexDesk e ~/.ssh/config, Benvenuto, pannello Attività, impostazioni a schede) e Gestione ptscry riorganizzata. Dalla 0.7.1: cursore dei terminali come lo chiede il programma, Niri. Dalla 0.7.0: comandi stile tmux/zellij (`rename`, `notify`, `send-keys`, layout salvati sul server, tasti `Ctrl-B s ( ) L c $ f m w`), pannelli nella GUI 0.8.0 e su terminale con **ptscry-tui**. Modalita' di produzione con **monitor root + host per utente**, dietro OpenSSH oppure con il **front-end SSH integrato** (processo non privilegiato confinato da seccomp, FIDO2 + certificati a breve durata + TOTP, `docs/MANUALE.md` 3.5). Le shell
nascono da un **launcher root con sessione PAM** (`pam_loginuid`, `pam_limits`, HBAC FreeIPA sul servizio `ptscry`).
Dalla 0.6.0 il prefisso e' Ctrl-B (configurabile). Dalla 0.5: `ptscry run [-d]`/`wait`/`status` (job con exit code), limiti per utente con blocchi `Match` e `rate_limit`.
Manuale di amministrazione (installazione, scenari, **audit: conservazione e analisi**) in
[docs/MANUALE.md](docs/MANUALE.md); man page: `man ptscry`, `man ptscry.conf`.
Integrazione con Wazuh ed Elastic/Kibana: [docs/SIEM.md](docs/SIEM.md).
Modello di sicurezza e mappatura ISO/IEC 27001:2022 in [docs/ISO27001.md](docs/ISO27001.md).
Analisi red team (attacchi eseguiti, problemi corretti, rischi residui): [docs/REDTEAM.md](docs/REDTEAM.md).

## In breve

- `ptscry new -A -n main`: crea o riattacca la sessione `main` (come `tmux new-session -A -s main`).
  Detach: **Ctrl-B** poi **d** (come tmux; `Ctrl-B Ctrl-B` invia un Ctrl-B letterale; il tasto si cambia, vedi sotto). Piu' sessioni per utente; limite `max_sessions` per utente/gruppo,
  oppure `unlimited`.
- **Autenticazione forte** = quella di sshd (certificati SSH a breve durata, FIDO2 `verify-required`,
  TOTP via PAM, FreeIPA/HBAC): configurazioni pronte in `contrib/sshd_config.d`, verifica con
  `ptscry check`. ptscry non reimplementa l'autenticazione.
- **Audit** scritto solo dal monitor (root): JSON con numero di sequenza e hash del record
  precedente, `ptscry audit-verify`, copia su syslog per l'inoltro fuori host. Se l'audit non e'
  scrivibile il monitor non opera.
- **Registrazioni** asciicast v2 scritte dal monitor (root:root 0600), mai dall'utente.
- Modalita' **bastion**: `ForceCommand ptscry login` confina un gruppo dentro ptscry.

## Interfaccia grafica (ptscry-gui)

Multiplexer grafico di sessioni con tab: terminale accelerato, Wayland nativo e X11 (anche su Niri, Hyprland e Sway), Windows e
macOS; finestra come quella di RexDesk (barra dei server con gruppi, Preferiti, colori e sessioni di ogni server, import da
RexDesk e da `~/.ssh/config`, pagina di benvenuto, pannello Attività, impostazioni a schede); scrollback regolabile o illimitato, workspace con nome, gestore delle sessioni (elenco, sola lettura, termina), directory e
variabili d'ambiente per sessione, menu contestuale e clipboard di sistema. Si usa su questo computer **senza server** (nessun
root), sul front-end locale `127.0.0.1:2222` o su un server remoto. Moduli e sorgenti in `gui/`, manuale in
[docs/GUIDA-GUI.md](docs/GUIDA-GUI.md) (guida all'uso), [docs/GUI.md](docs/GUI.md) (riferimento), `man ptscry-gui`, compilazione con `./build-gui.sh` (o `make gui`, `make gui-dist`, `make gui-test`).
Dipendenze di compilazione su Linux (Debian/Ubuntu, RedHat, Arch) nel capitolo 1 del manuale e nell'intestazione dello script.
Dalla 0.8.0 ogni tab si divide in **pannelli** (anche su server diversi), con input sincronizzato, zoom, disposizioni pronte e
disposizioni salvate sul server.

## Pannelli su terminale (ptscry-tui, dalla 0.7.0)

`ptscry tui [sessione...]`, `ptscry attach -l DISPOSIZIONE` o `ptscry-tui -H server`: pannelli come in tmux su un terminale di
testo (Linux e macOS), ogni pannello una sessione ptscry; copy mode con ricerca e OSC 52, mouse, barra di stato, stesse
disposizioni della GUI. Modulo a parte in `tui/` (il binario del server non contiene un emulatore di terminale); `man ptscry-tui`.

## Client per macOS e Windows (0.6.1)

`ptscry -H utente@server[:2222] [comando]` (macOS, Windows e Linux): client statico verso il front-end SSH, con chiavi
da ssh-agent (anche FIDO2), certificati, TOTP, verifica della chiave del server e riconnessione automatica.
`make dist` produce i binari in `dist/`. Vedi `docs/MANUALE.md` 3.6 e `contrib/client.conf`.

## Installazione

Pacchetti del server (`./build-server.sh`, in `dist-server/`): `sudo apt install ./ptscry_*.deb` oppure
`sudo dnf install ./ptscry-*.rpm`; poi `/etc/ptscry/ptscry.conf`, `usermod -aG ptscry UTENTE`,
`systemctl enable --now ptscry-monitor`, `ptscry check` (manuale 2.1). Il pacchetto della GUI (`ptscry-gui`, deb/rpm)
contiene anche il server e prende il posto del pacchetto `ptscry`. Dai sorgenti:

    make build        # cgo: necessario per utenti FreeIPA/SSSD (NSS)
    sudo make install # binario, config di esempio, unit systemd, logrotate, tmpfiles
    sudoedit /etc/ptscry/ptscry.conf
    # make install crea i gruppi ptscry e ptscry-admins se non esistono (anche in FreeIPA); poi:
    sudo usermod -aG ptscry UTENTE            # chi puo' usare ptscry; gli admin anche in ptscry-admins
    # make install imposta anche kernel.yama.ptrace_scope = 1 (persistente) e fa daemon-reload; poi:
    sudo make enable                          # abilita e avvia il monitor (o: sudo make install ENABLE=1)
    sudo ptscry check

## Comandi

    ptscry new [-d] [-A] [-n nome] [comando...]   crea (e attacca) una sessione
    ptscry attach [-r] [-c] [nome]                attacca (-r sola lettura, -c crea se manca)
    ptscry list | kill <nome> | freeze <nome> | thaw <nome> | stop | config | version
    ptscry admin list | kill <utente> <sessione> | stop <utente>     (root o admin_group)
    ptscry audit-verify [file...]    verifica la catena (piu' file = rotazione, in ordine)
    ptscry check [-u utente]         controlla la distribuzione e un utente (default: tu); vecchio nome: doctor
    ptscry monitor [-c file]         il monitor (servizio systemd)
    ptscry login [-n nome]           ForceCommand di sshd
    ptscry totp-init [-f] utente     registra il TOTP di un utente (front-end SSH)

## Configurazione

`/etc/ptscry/ptscry.conf`, sezioni `[global]`, `[group:X]`, `[user:X]` applicate in
quest'ordine; chiavi sconosciute = errore; il file deve essere di root e non scrivibile da altri.
Riferimento completo e commentato in `contrib/ptscry.conf`:
`access_group`, `admin_group`, `enabled`, `session_lifecycle` (`exit_on_last_shell` |
`persistent_per_user`), `max_sessions` (N | `unlimited`), `idle_timeout`, `max_lifetime`,
`replay_bytes`, `shell`, `allow_command`, `record` (`off` | `output` | `all`), `record_max_bytes`,
`max_total_sessions`, `pam_service`, `frontend*`, `audit_log`,
`audit_syslog`, `record_dir`, `monitor_socket`.

## Architettura

Il monitor (root) autentica i client con `SO_PEERCRED`, applica la policy, avvia **un host per
utente** con le credenziali dell'utente e un ambiente pulito su un canale privato (socketpair
ereditato), e gli **passa la connessione del client** (`SCM_RIGHTS`): non e' mai nel percorso dei
dati. L'host e' non-dumpable (niente ptrace, niente `/proc/PID/mem`), tiene la PTY e il buffer di
replay, e invia al monitor eventi e registrazione; l'identita' negli eventi la scrive il monitor.
Se il canale col monitor cade, l'host termina le sessioni (nessuna sessione senza audit).

La modalita' *standalone* (`PTSCRY_STANDALONE=1`, host avviato dall'utente, audit scritto
dall'utente) resta solo per sviluppo e uso senza privilegi: **non** e' a prova di manomissione.

## Test

    make test    # gofmt, vet, test unitari
    make e2e     # pseudoterminali reali; la parte "managed" va eseguita come root

## Limiti e prossimi passi

Elencati con precisione in docs/ISO27001.md (sez. 5). I principali: i processi che escono dal
gruppo di processi con `setsid` (demoni) sopravvivono alla chiusura della sessione (serve uno scope
`pam_systemd`, non ancora provato); niente utmp/wtmp; riavviare il monitor (`systemctl restart`) termina le sessioni, mentre `ptscry admin reexec` lo sostituisce sul posto senza chiuderle (0.5.4);
solo Linux; non ancora provato con utenti FreeIPA reali.
Dalla 0.5.3 il front-end e' confinato da seccomp (x86-64); dalla 0.5.4 il monitor si aggiorna sul posto senza chiudere le sessioni.

## Struttura del codice (dalla 0.6.0)

Un solo modulo, un solo `package main`, separato per piattaforma con i build tag:

- **Comune a tutte le piattaforme** (compila statico, senza cgo, su Linux, macOS e Windows): `version.go`,
  `keys.go` (prefisso, `client.conf`, filtro dei tasti), `keys_unix.go` / `keys_windows.go` (controllo del file),
  `main_other.go` (punto d'ingresso dei sistemi diversi da Linux).
- **Client remoto** (`remote.go`, `agent_client.go`, `knownhosts.go`, `term_unix.go`, `term_windows.go`): dalla 0.6.1, statico e senza cgo su Linux, macOS e Windows.
- **`tui/`** (modulo `ptscry/tui`, Linux e macOS): ptscry-tui, pannelli su terminale (ultraviolet + x/vt, creack/pty).
- **`gui/`** (modulo `ptscry/gui`): ptscry-gui.
- **Solo Linux** (`//go:build linux`): tutto il resto, cioe' client locale, monitor, host, launcher PAM, front-end SSH,
  seccomp, audit. `make cross` verifica che la parte comune compili per macOS e Windows.

## Release

`git tag vX.Y.Z && git push origin vX.Y.Z` (o **Actions → release → Run workflow**) avvia `.github/workflows/release.yml`:
compila su GitHub server (deb/rpm/tar amd64 e arm64), GUI (Linux, setup Windows amd64/arm64, dmg macOS) e sorgenti, e li
allega a una **bozza** di release, da pubblicare a mano. Le note vengono da `docs/release-notes/<tag>.md`.

## Licenza ed esclusione di responsabilita'

Copyright (C) 2026 Guido Cauli <guido.cauli@gmail.com>. ptscry, ptscry-gui e ptscry-tui sono software libero
sotto **GNU Affero General Public License v3.0 o successiva** ([LICENSE](LICENSE)): chi distribuisce una versione
modificata, o la offre come servizio in rete, deve pubblicarne il sorgente con la stessa licenza.

Il software e' fornito **"cosi' com'e'", senza alcuna garanzia**: l'autore non risponde di danni di alcun tipo
derivanti dal suo uso (compresi perdita di dati, interruzioni di servizio, violazioni della sicurezza). Installazione,
configurazione, uso in produzione e verifica di sicurezza e conformita' sono responsabilita' di chi lo gestisce.
Testo completo, versione inglese e componenti di terzi in [NOTICE.md](NOTICE.md).
Sorgente (anche per la sezione 13 della AGPL, uso via rete): <https://github.com/guidocauli/ptscry>.
