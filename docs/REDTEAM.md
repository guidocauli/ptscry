# ptscry — analisi red team

ptscry - Guido Cauli <guido.cauli@gmail.com>

Analisi del 27-11-2026 su ptscry 0.7.2 (+ correzioni per 0.7.3): server (monitor, host, launcher, front-end SSH),
ptscry-gui 0.9.0-gui, ptscry-tui 0.7.2-tui. Gli attacchi locali sono **eseguiti davvero** da utenti senza privilegi
contro un monitor di prova: `sudo PTSCRY_BIN=./ptscry python3 tests/e2e_redteam.py` (ogni controllo è PASS quando
l'attacco fallisce). Gli attacchi di rete al front-end sono quelli di `tests/e2e_frontend.py`.

## 1. Chi attacca

| Attaccante | Cosa ha | Cosa vuole |
|---|---|---|
| A1 utente locale di ptscry | una shell, è in `access_group` | sessioni o dati di altri, root, nascondere ciò che fa (audit, registrazioni), bloccare il servizio |
| A2 amministratore di ptscry | è in `admin_group`, niente sudo | creare account di sistema, toccare root, decidere chi amministra ptscry |
| A3 rete | raggiunge la porta del front-end | entrare senza chiave/TOTP, scoprire utenti, inoltri, esaurire risorse |
| A4 server ostile | un server a cui si collega la GUI/TUI | eseguire codice o leggere dati sul computer di chi si collega |

## 2. Superfici

- Socket del monitor (`/run/ptscry/monitor.sock`, 0666: decide `SO_PEERCRED`): JSON in frame con lunghezza (max 1 MiB).
- Canale di controllo monitor ↔ host (socketpair, mai raggiungibile da fuori); l'host è un processo **dell'utente**.
- Launcher root (`ptscry launch`): PAM, `setuid`, ambiente della sessione.
- Front-end SSH (utente dedicato, seccomp): chiavi, certificati, FIDO2, TOTP, ticket del monitor.
- `admin setup` (gpasswd/useradd e file delle chiavi, fatti dal monitor per conto di `admin_group`).
- Client: emulatore della GUI (charmbracelet/x/vt), TUI, client a riga di comando, `known_hosts`, import di RexDesk e
  `~/.ssh/config`, disposizioni salvate sul server, OSC 7799.

## 3. Risultati degli attacchi

### 3.1 Utente locale (A1) — tutti respinti
| Attacco | Esito |
|---|---|
| hello con `proxy` falso (agire come un altro utente), `fe-ticket`/`fe-totp`/`fe-report` | `access denied`, `fe_denied` nell'audit |
| `admin-list/kill/stop/reexec/setup` senza essere amministratore | rifiutati (`access_denied`) |
| attach/kill/send-keys alla sessione di un altro (`secret`, `../rt_vic/secret`, `rt_vic/secret`, `rt_vic:secret`) | ogni richiesta va solo al proprio host: nessun effetto |
| registrazioni e loro cartella | root 0600 / 0700, non elencabili |
| nomi di sessione `a/b`, `../../etc/passwd`, sequenze ESC/OSC 52, 65 caratteri, `-rf`, a capo | rifiutati (`nameRe`) |
| frame che dichiara 2 GiB, JSON annidato 200 000 livelli | connessione chiusa / errore, monitor vivo |
| ambiente del client `LD_PRELOAD`, `BASH_ENV`, `PATH`, `TERM` con a capo | filtrati (solo TERM, COLORTERM, LANG, LANGUAGE, LC_*) |
| 200 connessioni aperte senza hello | limite per uid (32): gli altri utenti serviti in <2 s |
| ptrace sul proprio host in esecuzione, lettura di `/proc/HOST/fd` | EPERM (host non ispezionabile) |
| ptrace sull'host **mentre parte** (C, 15 tentativi, `ptrace_scope` assente = 0) | 0/15, vedi R1 |
| SIGSTOP al proprio host + 600 richieste in 60 s | solo l'attaccante resta bloccato; altri utenti, `admin list/kill/stop` e lo spegnimento del monitor funzionano, vedi R2 |

### 3.2 Amministratore di ptscry (A2)
| Attacco | Esito |
|---|---|
| `users add -create`, `-shell`, `root`, `daemon` (UID < UID_MIN), opzione dopo il nome | rifiutati |
| chiavi per root; `users add -admin` (anche dopo il nome o `--admin=true`); togliere un altro amministratore | rifiutati |
| chiavi per `..`, `keys list ..`, file di chiave `/etc/shadow` | rifiutati (il file lo legge il client) |
| chiavi per un membro di `sudo` o per chi ha `NOPASSWD: ALL` in sudoers | accettate **per scelta di progetto** (R3) |

### 3.3 Rete (A3) — `tests/e2e_frontend.py`
Respinti: codice TOTP sbagliato, riuso di un codice, 5 errori ⇒ blocco (anche con il codice giusto), utente senza
segreto, certificati scaduti/di altri/CA sbagliata, account vietati, comandi fuori dall'elenco (`touch`, `sh -c`,
`$(...)`, `;`), `ptscry admin`/`monitor` via ssh, port forwarding (direct-tcpip), X11, shell senza terminale, richieste
dirette al monitor, flood da un indirizzo (limiti per IP, handshake e globali, timeout di autenticazione). Il blocco
TOTP non è sfruttabile da chi non ha già la chiave: il TOTP è il secondo fattore.

### 3.4 Server ostile (A4)
- L'emulatore della GUI risponde solo a richieste di stato (DA, DSR, posizione, modi, colori): niente report del titolo
  (`CSI 21 t`), niente lettura degli appunti (OSC 52), niente DECRQSS ⇒ nessuna iniezione di input.
- OSC 8 non apre nulla; OSC 7799 (split/disposizione) solo dal pannello con la tastiera, una volta al secondo, mai dopo
  un replay, mai su altre connessioni; nessun comando eseguito.
- Avvisi (`notify`) ripuliti anche dal client (`cleanNotice`), nomi di sessione filtrati (`validSessions`).
- Chiave del server: TOFU con domanda esplicita, chiave cambiata ⇒ rifiuto; impronta fissa possibile.
- Import: `Include` con profondità 8 e senza cicli, file limitati, `ProxyCommand`/`Match` ignorati.
- **Titolo della finestra** (OSC 0/2) passava intero (controlli, inversione bidi, lunghezza) → R4.

## 4. Problemi trovati e correzioni

| # | Gravità | Problema | Correzione |
|---|---|---|---|
| R1 | Media (solo con `ptrace_scope = 0`) | tra l'exec dell'host e `prctl(PR_SET_DUMPABLE, 0)` (avvio del runtime Go) un processo dello stesso utente poteva agganciarlo con ptrace e falsare audit/registrazioni | costruttore ELF (`harden_cgo.go`) che rende host, launcher e monitor non ispezionabili prima del runtime; `ptscry check` continua a segnalare `ptrace_scope = 0` |
| R2 | Bassa | gli invii del monitor a un host non avevano scadenza: un host fermato con SIGSTOP poteva tenere occupati goroutine di root (kill, revoca, spegnimento) | scadenza di 10 s su ogni invio al canale di controllo di un host |
| R3 | Decisione di progetto | un membro di `admin_group` può aggiungere una chiave a un utente con diritti sudo (per gruppo o per nome in sudoers) ed entrare come lui ⇒ root. Un primo controllo (gruppi privilegiati, `sudo -l -U`) è stato tolto: aggiungeva una politica che Unix non ha | come in Unix, chi è in sudo/docker/`admin_group` lo decide l'amministratore del sistema; `admin_group` non può più decidere chi amministra ptscry (`-admin`, togliere amministratori): lo fanno root e i nuovi **`admin_root_groups`** (es. un gruppo FreeIPA), con l'autorità di root; `admin_setup = users` toglie le chiavi ad `admin_group` |
| R4 | Bassa | il titolo impostato da un programma arrivava intero alla scheda e alla finestra (falsificazione con U+202E, lunghezza) | `cleanTitle`: niente controlli né caratteri di formato, massimo 128 byte |
| R5 | Informativa | i nomi utente `.` e `..` erano validi (percorsi nella cartella delle chiavi; solo errori di lettura) | esclusi da `feUserRe` |

Test di regressione: `tests/e2e_redteam.py` (R1, R2, R3 nella forma decisa, R5 e tutti gli attacchi del cap. 3.1–3.2),
`gui/review2_test.go:TestCleanTitleRedTeam` (R4).

## 5. Rischi che restano (per progetto)

- **L'utente controlla il proprio host**: può terminarlo o fermarlo (stesso uid). Le sue sessioni finiscono o si
  fermano con lui; l'audit degli eventi decisi dal monitor resta completo. Contro ptrace serve
  `kernel.yama.ptrace_scope >= 1` (consigliato in `ISO27001.md`).
- **Chi è in `admin_group` può entrare come qualunque utente** (tranne root e gli account di sistema) aggiungendogli una
  chiave, anche come chi ha sudo: è il potere che `admin_setup = keys` concede, e chi è in `admin_group` va scelto con la
  stessa fiducia di chi è in sudo. Ogni operazione è nell'audit (`admin_setup` con le impronte). Chi non lo vuole usa
  `admin_setup = users` (le chiavi restano a root e ad `admin_root_groups`) o `no`.
- Il client a riga di comando (`ptscry attach`, `ptscry -H`) passa l'output della sessione al terminale locale come
  fa ssh: un server ostile può usare le funzioni del terminale locale (es. scrivere negli appunti con OSC 52, se il
  terminale lo consente). La GUI e la TUI non hanno questo problema (emulano).
- Una disposizione caricata a mano dal dialogo può aprire pannelli su altri profili con lo stesso nome: è un'azione
  della persona, non del server.
- Tempi di risposta del front-end per utenti esistenti/inesistenti non misurati con precisione statistica.

## 6. Elenco di controllo riutilizzabile

Per analizzare altre applicazioni con lo stesso metodo (base del futuro strumento):

1. **Attaccanti e superfici**: per ogni confine di fiducia (utente→root, utente→utente, rete→servizio,
   server→client) chi sta da ciascun lato e cosa passa.
2. **Identità**: da dove viene (credenziali del kernel, certificati, token)? Si può dichiarare invece che dimostrare?
   Campi "per conto di" accettati solo da chi è autorizzato?
3. **Input**: nomi usati come percorsi (`..`, `/`, NUL, a capo), lunghezze, frame giganti, JSON annidato,
   opzioni (`-x`) passate a comandi esterni, sequenze di escape in ciò che finisce su un terminale o in un titolo.
4. **Ambiente e processi privilegiati**: variabili ereditate (`LD_PRELOAD`, `PATH`, `BASH_ENV`), file aperti come root
   in cartelle dell'utente (link simbolici, `O_NOFOLLOW`, proprietario dopo l'apertura), finestre tra exec e
   protezione (ptrace), descrittori ereditati.
5. **Risorse**: connessioni senza richiesta, richieste per secondo, code verso un processo che l'utente può fermare
   (SIGSTOP), scritture senza scadenza in goroutine/thread privilegiati, disco pieno.
6. **Deleghe di amministrazione**: cosa permette di diventare un altro (chiavi, gruppi, sudoers per nome, gruppi
   equivalenti a root: docker, lxd, libvirt, disk, shadow).
7. **Client**: risposte automatiche dell'emulatore (report del titolo, OSC 52 in lettura, DECRQSS), collegamenti,
   comandi di controllo nell'output, verifica della chiave del server, file importati.
8. **Verifica**: ogni attacco diventa un test che passa quando l'attacco fallisce, eseguito a ogni modifica.
