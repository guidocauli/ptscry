# ptscry - Guido Cauli <guido.cauli@gmail.com>

# Manuale di amministrazione (versione 0.7.0)

Questo manuale e' per chi installa e gestisce ptscry. Per i comandi vedi `man ptscry`, per
le chiavi di configurazione `man ptscry.conf`, per il modello di sicurezza e la mappatura
ISO 27001 `docs/ISO27001.md`.

## 0. Avvio rapido (copia e incolla)

Percorso piu' semplice (e' la strada B del cap. 3.0, da leggere dopo): server Linux, utenti locali, ingresso dal front-end SSH di ptscry sulla porta 2222 con
chiavi semplici (senza TOTP, FIDO2 o CA: per rafforzarlo vedi la tabella al cap. 3.5). Ogni blocco si puo' incollare
cosi' com'e', e si puo' rilanciare senza danni (non sovrascrive nulla di esistente). Servono i sorgenti, Go >= 1.24,
systemd e un utente con `sudo`. `UTENTE` e' l'unico valore da cambiare.

**1. Sul server: compila e installa**
```
make build
sudo make install          # binario (in /usr/local/bin, con link in /usr/bin), configurazione, servizio, gruppi, sysctl
sudo make frontend-user    # account ptscry-fe, chiave host SSH, directory delle chiavi
```

**2. Sul server: abilita il front-end e avvia**
```
sudo sed -i 's/^#frontend = yes$/frontend = yes/' /etc/ptscry/ptscry.conf
sudo make enable           # avvia ptscry-monitor e lo abilita al boot
sudo ptscry check          # niente CRIT; i WARN sulla protezione debole sono attesi
```

**3. Sul tuo computer: crea la chiave** (deve essere **ed25519**: RSA ed ECDSA sono rifiutate)
```
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519      # salta se hai gia' una chiave ed25519
cat ~/.ssh/id_ed25519.pub                       # copia questa riga
```

**4. Sul server: abilita l'utente** (stesso nome utente del server; incolla la riga copiata al posto di `CHIAVE_PUBBLICA`)
```
U=UTENTE
sudo usermod -aG ptscry "$U"
echo 'CHIAVE_PUBBLICA' | sudo tee /etc/ptscry/authorized_keys.d/"$U" >/dev/null
sudo chown root:root /etc/ptscry/authorized_keys.d/"$U"; sudo chmod 0644 /etc/ptscry/authorized_keys.d/"$U"
sudo ptscry check -u "$U"  # deve dire OK sull'accesso; se no, dice cosa correggere
```
L'utente deve avere una shell interattiva (non `nologin`). Con FreeIPA il gruppo si assegna in IPA, non con `usermod`.

**5. Dal tuo computer: entra**
```
ssh -p 2222 UTENTE@IP_DEL_SERVER                # oppure il client ptscry: ptscry -H UTENTE@IP_DEL_SERVER list
```
Dentro la sessione `Ctrl-B d` stacca, `ptscry attach` (o un nuovo accesso) riattacca.

Se qualcosa non va:

| Sintomo | Causa e rimedio |
|---|---|
| `unable to authenticate` | chiave non ed25519, nome utente diverso dal file in `authorized_keys.d`, riga incollata incompleta: `sudo ptscry check -u UTENTE` |
| `frontend_host_key: ... no such file` | manca la chiave host: `sudo make frontend-user` e `sudo systemctl restart ptscry-monitor` |
| `access denied` | utente fuori dal gruppo `ptscry`, shell `nologin` o `enabled = no`: `sudo ptscry check -u UTENTE` |
| niente sulla porta 2222 | `frontend = yes` non attivo o firewall: `ss -tlnp \| grep 2222`, `journalctl -u ptscry-monitor -n 30` |

Dopo questo, il resto del manuale serve per rafforzare l'accesso (FIDO2, TOTP, CA), per usare sshd al posto del
front-end, per i limiti e l'audit.

## 1. Come funziona (in 2 minuti)

```
 utente ──ssh (autenticazione forte)──► sshd ──► ptscry (client)
                                                    │ socket Unix, il kernel garantisce l'uid
                                                    ▼
                                        MONITOR (root): policy, audit, registrazioni
                                                    │ avvia, con l'uid dell'utente
                                                    ▼
                                         HOST (uno per utente): PTY e sessioni
                                                    ▼
                                              shell / comandi
```

- **Una sessione** e' un terminale che continua a vivere anche senza client. `Ctrl-B d` stacca (come tmux; prefisso configurabile, cap. 4.1),
  `ptscry attach` riattacca; al ritorno si rivede l'output recente.
- **Il monitor** e' l'unico a scrivere audit e registrazioni; gli utenti non possono toccarli.
- **L'autenticazione** non e' di ptscry: e' di sshd (certificati, FIDO2, TOTP, HBAC).

## 2. Installazione

### 2.1 Con i pacchetti (consigliato)

`./build-server.sh` (serve nfpm) produce in `dist-server/` i pacchetti del server, separati da quelli della GUI:

| Sistema | Pacchetto | Comando |
|---|---|---|
| Debian 12+, Ubuntu 22.04+ | `ptscry_VERSIONE-1_amd64.deb` / `_arm64.deb` | `sudo apt install ./ptscry_*.deb` |
| RHEL/Alma/Rocky 9+, Fedora, openSUSE | `ptscry-VERSIONE-1.x86_64.rpm` / `.aarch64.rpm` | `sudo dnf install ./ptscry-*.rpm` |
| altri (stile `make install`) | `ptscry-server-linux-amd64.tar.gz` / `-arm64` | binario, `contrib/`, man page e documenti |

Per arm64 da un PC amd64 serve il compilatore incrociato (`gcc-aarch64-linux-gnu libc6-dev-arm64-cross`); senza, lo script salta arm64. Il binario e' compilato con cgo (utenti FreeIPA/SSSD e PAM) e richiede glibc 2.34 o successiva (RHEL 8 e Debian 11 no:
li' si compila sul posto con `make build`). Il pacchetto installa `/usr/bin/ptscry`, la unit `ptscry-monitor.service`
(non abilitata), `/etc/ptscry/ptscry.conf`, `/etc/pam.d/ptscry` (variante Debian o RHEL), logrotate, tmpfiles, man page
e documenti in `/usr/share/doc/ptscry` (snippet sshd, rsyslog, Wazuh, Elastic). Crea i gruppi `ptscry` e `ptscry-admins` se
mancano e imposta `kernel.yama.ptrace_scope = 1` solo se il valore attuale e' piu' basso. **Non avvia e non riavvia mai il
monitor** (un riavvio chiuderebbe le sessioni): dopo un aggiornamento si passa alla nuova versione con `ptscry admin reexec`.
La configurazione e' conservata negli aggiornamenti e nella rimozione; i log e le registrazioni in `/var/log/ptscry` restano
sempre (sono evidenze). Poi:

```
sudoedit /etc/ptscry/ptscry.conf             # access_group, admin_group, limiti
sudo usermod -aG ptscry UTENTE               # i pacchetti non aggiungono mai utenti ai gruppi
sudo ptscry-setup-frontend                   # solo con il front-end SSH (= make frontend-user)
sudo systemctl enable --now ptscry-monitor
sudo ptscry check -u UTENTE
```

### 2.2 Dai sorgenti

Requisiti: Linux, Go >= 1.24 per compilare, systemd, OpenSSH >= 8.7. Per utenti FreeIPA/SSSD
la build **deve** usare cgo (`make build`), altrimenti si vedono solo gli utenti di `/etc/passwd`.

```
make build
sudo make install                       # binario, config, unit, logrotate, tmpfiles, man page, gruppi, sysctl, daemon-reload
sudo make groups     # (lo fa gia' make install) crea ptscry e ptscry-admins solo se non esistono, anche in FreeIPA
sudo usermod -aG ptscry UTENTE      # make non aggiunge mai utenti ai gruppi; root compreso, se deve usare ptscry
sudo make frontend-user             # solo con il front-end SSH: account ptscry-fe senza shell
sudoedit /etc/ptscry/ptscry.conf        # access_group, admin_group, limiti
# make install ha gia' impostato kernel.yama.ptrace_scope = 1 (/etc/sysctl.d/60-ptscry.conf; non abbassa mai un valore piu' severo)
# e fatto daemon-reload. Dopo aver modificato la configurazione:
sudo make enable                    # systemctl enable --now ptscry-monitor (oppure: sudo make install ENABLE=1)
sudo ptscry check -u UTENTE   # senza -u controlla l'utente corrente (con sudo: chi ha lanciato sudo)
```

Con FreeIPA i gruppi `ptscry` e `ptscry-admins` si creano in IPA (`ipa group-add`), non in locale,
e si associano con regole HBAC al servizio `sshd`.

`ptscry check` (si chiamava `doctor`, il vecchio nome funziona ancora; senza `-u` controlla l'utente corrente) e' il primo posto dove guardare, e se un utente riceve "access denied" dice perche' (non e' nell'`access_group`: CRIT; `admin_group`: solo vero/falso; ptscry disabilitato; nessuna shell interattiva): ogni riga e' **OK** (verde), **WARN** (giallo) o **CRIT** (rosso; colori solo su terminale, `NO_COLOR` o `-color=never` li tolgono) e riporta il controllo ISO a cui si riferisce
e dice cosa correggere. Va rilanciato dopo ogni modifica di sshd, della configurazione o del sistema.

## 3. Scenari

### 3.0 Come si entra: sshd, bastion o front-end (leggere prima)

ptscry **non** e' un programma che si raggiunge da solo: l'utente deve arrivare al server in qualche modo, e ci sono due
strade. Sono alternative, ma possono convivere sulla stessa macchina. In entrambe l'utente finisce in una sessione ptscry
**su quel server**: nessuna delle due fa da proxy verso altri host.

```
 STRADA A - con sshd            utente ──► sshd (porta 22) ──► ptscry ──► sessione
                                           autentica sshd

 STRADA B - front-end           utente ──► ptscry (porta 2222) ──► sessione
                                           autentica ptscry (chiave/certificato, TOTP)
```

**Strada A: sshd (capp. 3.1, 3.2, 3.3).** L'utente entra con ssh normale sulla porta 22 e poi usa `ptscry`. L'autenticazione
e' quella di sshd, quindi ne eredita punti di forza e debolezze: certificati, FIDO2, OTP e HBAC di FreeIPA valgono solo se
li hai configurati in sshd (`ptscry check` ne mostra la policy effettiva). Ha due varianti:
- **sshd "normale"**: l'utente ha la sua shell via ssh e usa ptscry quando vuole. Aggiunge audit e sessioni persistenti
  a un accesso che gia' esiste.
- **Bastion (3.2)**: ancora sshd, ma con `Match Group ptscry` e `ForceCommand ptscry login`. Chi e' nel gruppo non ha una
  shell: puo' solo entrare in ptscry (niente scp, sftp, comandi liberi, port forwarding). "Bastion" non e' un programma a
  parte: e' questa configurazione di sshd.

**Strada B: front-end integrato (3.5).** ptscry ascolta da solo sulla porta 2222 e parla SSH, senza sshd. Entra solo chi ha una
chiave in `/etc/ptscry/authorized_keys.d/UTENTE` o un certificato della CA di ptscry; il TOTP, se attivo, lo verifica il
monitor (root), non il front-end. L'utente **non ha mai una shell sul server**: puo' solo aprire, staccare e riattaccare
sessioni ptscry. Il processo che ascolta gira senza privilegi (account `ptscry-fe`, senza shell).

| | Strada A: sshd (anche bastion) | Strada B: front-end |
|---|---|---|
| Porta | 22 (o quella di sshd) | 2222 (`frontend_listen`) |
| Chi autentica | sshd | ptscry |
| Chi puo' entrare | chi sshd ammette (utenti, gruppi, HBAC) | chi ha una chiave in `authorized_keys.d` o un certificato della CA di ptscry |
| Shell sul server | si', salvo bastion | mai |
| Dove si configura la sicurezza | `sshd_config` | `ptscry.conf` (`frontend_*`) |
| Si rompe ptscry: posso ancora entrare? | si', con ssh normale (non nel bastion) | no, solo da sshd se e' attivo |
| Utenti in FreeIPA | si', con HBAC e certificati gia' esistenti | si', ma le chiavi/CA le gestisce ptscry |

**Quale scegliere**
- *Hai gia' sshd con FreeIPA, CA e FIDO2 ben configurati*: strada A. Non cambi nulla per gli utenti, aggiungi audit e sessioni.
- *Vuoi che certi utenti non abbiano mai una shell, e non vuoi toccare sshd*: strada B.
- *Vuoi il massimo controllo e l'unico ingresso*: bastion (A) oppure B, e sshd limitato agli amministratori.
- **Consigliato nel caso tipico**: lasciare sshd sulla 22 per gli amministratori e per l'emergenza, e dare agli utenti
  ordinari il front-end sulla 2222. Se ptscry ha un problema, gli amministratori entrano comunque.

Le due strade usano gli stessi gruppi (`ptscry`, `ptscry-admins`), la stessa policy e lo stesso audit: cambia solo
**come** l'utente arriva alla sessione. Il client remoto `ptscry -H` (Windows e macOS, 3.6) e' pensato per la strada B (porta 2222 di default).


### 3.1 Server di amministrazione con FreeIPA (consigliato)
1. sshd con CA a breve durata + FIDO2 + OTP: `contrib/sshd_config.d/50-ptscry.conf` (profilo `strong`
   per i sysadmin). Verifica con `ptscry check -u mario` (mostra la policy sshd *effettiva* dopo i `Match`).
2. HBAC in IPA per chi puo' entrare; `access_group = ptscry` in ptscry per chi puo' avere sessioni. Root puo' usare ptscry
   sul server anche senza essere nel gruppo (`allow_root = yes`, default; l'audit registra comunque il loginuid);
   `allow_root = no` se gli amministratori devono lavorare solo con l'account personale.
3. Gruppo `sysadmins`: `max_sessions = unlimited`, `session_lifecycle = persistent_per_user`, `record = output`.
4. Audit inoltrato a un collettore (cap. 6.4).

### 3.2 Bastion: l'unico modo di entrare e' ptscry
In `sshd_config.d`: `Match Group ptscry` con `ForceCommand /usr/local/bin/ptscry login`, senza port
forwarding. Il login apre o riattacca la sessione `main`; `ssh -t host ptscry list|attach|new|kill` e' ammesso,
tutto il resto (scp, sftp, shell, comandi) e' rifiutato. Va combinato con la registrazione (`record = output`)
per chi entra dal bastion.

### 3.3 Server non in dominio
Funziona uguale: utenti locali in `/etc/passwd`, gruppi in `/etc/group`, autenticazione con le chiavi/certificati
di sshd. Un binario statico (`make build-static`) basta se tutti gli utenti sono locali.

### 3.5 Front-end SSH integrato (porta TCP propria)
Alternativa a sshd: ptscry ascolta da solo su una porta TCP (di default `:2222`) e parla SSH. Serve quando non si vuole
(o non si puo') dare ai sysadmin una shell via sshd, oppure si vuole un unico punto d'ingresso con regole proprie.

```
 ssh -p 2222 host ──► ptscry frontend (utente ptscry-fe, senza privilegi)
                       │ verifica chiave/certificato (FIDO2 con PIN)
                       ▼
                     monitor (root): policy, TOTP, ticket, audit ──► host dell'utente ──► shell
```
**Cosa fa chi.** Il front-end verifica il possesso della chiave o del certificato e inoltra il terminale; **non** legge file
di configurazione ne' segreti (tranne la chiave host SSH, che gli passa il monitor) e non puo' toccare l'audit. Il
**monitor** applica la policy, verifica il **TOTP** (i segreti sono `root:root 0600`: un front-end compromesso non puo'
saltare il secondo fattore) e rilascia un **ticket** legato all'utente e di 12 ore: senza un ticket del monitor il front-end
non puo' aprire alcuna sessione. Ogni azione e' nell'audit con indirizzo remoto, impronta della chiave, id/serial del certificato.

**Messa in esercizio, in breve.** `sudo make frontend-user` (account `ptscry-fe` senza shell, chiave host, directory
`authorized_keys.d`), poi in `ptscry.conf` `frontend = yes`, la chiave pubblica di ogni utente in
`/etc/ptscry/authorized_keys.d/UTENTE`, `systemctl reload ptscry-monitor`, `ptscry check`. La configurazione di
partenza (`contrib/ptscry.conf`) e' **chiavi semplici, senza TOTP, FIDO2 o CA**: e' la piu' semplice e anche la piu'
debole (una chiave rubata basta), e `ptscry check` la segnala con WARN. Per rafforzarla si cambia **una riga**:

| Voglio | Riga in `ptscry.conf` | Poi |
|---|---|---|
| chiavi FIDO2 con PIN/biometria | `frontend_require_fido2 = yes` | gli utenti usano `ssh-keygen -t ed25519-sk -O verify-required` |
| secondo fattore TOTP | `frontend_totp = yes` | `ptscry totp-init UTENTE` (crea da solo la directory dei segreti) |
| certificati di una CA a breve durata | `frontend_ca_keys = /etc/ptscry/frontend_ca.pub` | copiare la chiave pubblica della CA nel file; si possono tenere anche le chiavi semplici |

(Nel codice i default restano quelli prudenti: FIDO2 e TOTP attivi, CA in `/etc/ptscry/frontend_ca.pub`. Il file di
esempio li disattiva in modo esplicito, cosi' chi ha un file di configurazione senza queste righe non perde protezioni.)

**Passi nel dettaglio**
1. Account dedicato (lo fa `make frontend-user`): `useradd -r -s /usr/sbin/nologin -M ptscry-fe` (il monitor rifiuta un account con shell di login,
   in `access_group` o in `admin_group`).
2. Chiave host (anche questa la crea `make frontend-user`): `ssh-keygen -t ed25519 -N '' -f /etc/ptscry/ssh_host_ed25519_key` (root, 0600).
3. Chi puo' entrare, a scelta (o entrambi):
   - **CA a breve durata**: la chiave pubblica della CA in `/etc/ptscry/frontend_ca.pub`; certificato con il nome utente tra i
     principals e validita' di poche ore (`ssh-keygen -s ca -I id -n mario -V +8h -O source-address=10.0.0.0/8 mario.pub`).
     Il front-end rifiuta certificati senza scadenza o piu' lunghi di `frontend_cert_max_age`, di host, con `force-command`
     o con altre opzioni critiche, e firmati da CA non elencate. Senza estensione `permit-pty` non concede il terminale.
   - **Chiavi semplici**: file `/etc/ptscry/authorized_keys.d/UTENTE` (root, non scrivibile da altri), una chiave per riga.
     Opzioni supportate: `from="..."` (solo indirizzi: `10.0.0.0/8,!10.9.0.0/16`, `192.168.1.*`; i nomi host no),
     `expiry-time="AAAAMMGG[HHMM[SS]][Z]"`, `no-pty`/`pty`/`restrict`, `no-port-forwarding`, `no-agent-forwarding`,
     `no-X11-forwarding`, `no-user-rc`, `verify-required`. **Qualunque altra opzione** (`command=`, `environment=`,
     `principals=`, `cert-authority`, `no-touch-required`, forwarding abilitati...) rende la riga inutilizzabile: non
     concede nulla, mai "ignora la restrizione e fa entrare". `ptscry check` elenca le righe inutilizzabili.
4. FIDO2: con `frontend_require_fido2 = yes` (default del codice; nel file di esempio e' `no`) passano solo chiavi `sk-ssh-ed25519` e le firme devono avere
   *presenza* e *verifica utente* (PIN/biometria): `ssh-keygen -t ed25519-sk -O verify-required`.
5. TOTP: `ptscry totp-init mario` stampa il segreto e l'URI `otpauth://` per l'app. Un utente senza segreto non entra.
   Un codice vale una volta sola; 5 codici sbagliati bloccano l'account per 15 minuti (`fe_totp_locked`).
6. In `ptscry.conf`: `frontend = yes` e le chiavi `frontend_*` (vedi `contrib/ptscry.conf`); `systemctl reload ptscry-monitor`
   (modificare le impostazioni riavvia solo il front-end, non le sessioni). Verifica con `ptscry check`.
7. Rete: apri solo la porta scelta ai soli client ammessi (firewall). Le porte sotto 1024 non sono possibili
   (il processo non e' privilegiato): usa 2222 o un reindirizzamento.

**Uso.** `ssh -t -p 2222 mario@host` apre o riattacca la sessione `main` (Ctrl-B d per staccarsi, o il prefisso scelto con `detach_prefix`);
`ssh -p 2222 mario@host ptscry list|attach|new|kill|stop ...` come con `ptscry login`. Niente comandi arbitrari, niente
forwarding di porte, X11 o agent, niente sftp/scp.
**ProxyJump / `ssh -J` / `-W` / `-L` / `-R` non sono ammessi *attraverso* ptscry** (richiedono canali `direct-tcpip`, che sono rifiutati di proposito: sarebbero traffico non registrato verso altre macchine). Si puo' invece usare ptscry *come destinazione* dietro un altro salto: `ssh -J bastion -p 2222 mario@host` funziona, perche' il salto e' un altro server e ptscry vede solo la connessione finale. Se la connessione cade la sessione resta viva.

**Difese del listener.** 30 nuove connessioni al minuto per indirizzo (raffica 10, `frontend_rate`), 4 connessioni
ancora in autenticazione e 16 totali per indirizzo, 64 handshake contemporanei, timeout di autenticazione 30 s, 3 tentativi per
connessione, 8 sessioni per connessione, keepalive. I tentativi falliti non finiscono uno per uno nell'audit: il front-end
manda un riepilogo al minuto (`fe_auth_failures`, con i 5 indirizzi peggiori).

**Sandbox (seccomp, `frontend_seccomp = yes`, default; solo Linux x86-64).** Avviato il listener, il front-end si
applica un filtro seccomp-bpf (tutti i thread). Un processo front-end compromesso **non puo'**: eseguire alcun programma
(`execve`: niente shell ne' strumenti; il processo viene terminato), `ptrace`, montare filesystem, creare namespace,
caricare moduli o programmi eBPF, usare i keyring; **non puo' creare, modificare o cancellare file** (EPERM) ne' aprirne
uno in scrittura; puo' aprire solo socket unix/IPv4/IPv6 (niente raw/packet/netlink/vsock). **Non** limita le
connessioni di rete in uscita: aggiungi una regola firewall sull'uid di `frontend_user` (ammetti solo il monitor via socket
unix e le risposte ai client). Su altre architetture parte senza filtro e lo scrive su stderr (`journalctl -u ptscry`).
Se il kernel rifiuta il filtro il front-end non parte (fail closed): `frontend_seccomp = no` solo per diagnosi.

**Rischi residui (da conoscere).** Chi compromette il processo front-end ottiene la chiave host SSH (puo' impersonare il
server) e, **se `frontend_totp = no`**, puo' agire come qualunque utente consentito; con il TOTP attivo non puo' farlo senza
un codice valido. Non c'e' revoca di certificati oltre alla scadenza breve: tieni `frontend_cert_max_age` basso. Un
attacco distribuito da molti indirizzi puo' saturare i 64 handshake (usa il firewall / un limitatore a monte).

### 3.6 Client per macOS, Windows e Linux remoto (dalla 0.6.1)

Un solo binario statico (nessun cgo, nessuna libreria) che parla con il front-end SSH del cap. 3.5. Su macOS e Windows
e' tutto il programma; su Linux e' `ptscry -H ...`, accanto al client locale.

    ptscry -H mario@server.example.org new -A -n lavoro     crea o riattacca "lavoro"
    ptscry -H mario@server.example.org                      come sopra, sessione "main"
    ptscry -H mario@server list | attach lavoro | run -d -n job make | wait job | status job | kill lavoro

Opzioni: `-H [utente@]host[:porta]` (porta 2222 se omessa; oppure `server =` in `client.conf`, oppure `PTSCRY_SERVER`),
`-i chiave`, `-c certificato`, `-F SHA256:...` (impronta della chiave del server). Un esempio completo di `client.conf`
e' in `contrib/client.conf` (dove sta il file: cap. 4.1).

- **Chiavi.** Senza `-i` usa le chiavi di **ssh-agent** (Linux/macOS: `SSH_AUTH_SOCK`; Windows: l'agent OpenSSH),
  comprese le chiavi FIDO2 `sk-ssh-ed25519`: la chiave hardware resta gestita dall'agent e dal suo OpenSSH, ptscry non
  ne ha bisogno. Con `-i` legge un file (se cifrato chiede la passphrase). Con `-c` usa il certificato con la sua chiave.
- **TOTP.** Il codice si digita sul terminale (non e' un'opzione, per non lasciarlo nella cronologia).
- **Chiave del server.** Al primo collegamento mostra l'impronta e chiede `yes`; la ricorda in `known_hosts`
  (accanto a `client.conf`). Se cambia il collegamento e' **rifiutato prima dell'autenticazione**. Per la prima volta
  si puo' confrontare l'impronta con quella dell'amministratore o darla con `-F`.
- **Rete che cade.** Se il collegamento si interrompe durante una sessione con terminale, il client si ricollega da
  solo (fino a 20 tentativi, chiedendo di nuovo il codice TOTP) e riattacca la stessa sessione, che sul server
  non e' mai stata chiusa. Non si ricollega se il nome della sessione non e' noto (`new` senza `-n`).
- **Codici di uscita.** Quelli del comando remoto (`run`, `wait`); 255 per errori di connessione o autenticazione.
- **Limiti.** Gli argomenti ammessi sono quelli che il front-end accetta (lettere, cifre e `_ . / : = @ + , -`);
  niente `admin` e `check` da remoto. Windows 10 1809 o successivo (sequenze VT della console). Non provato su Mac e
  Windows reali: il codice specifico delle due piattaforme e' compilato in cross-compilazione ma non eseguito.

### 3.4 Solo per prove (standalone)
`PTSCRY_STANDALONE=1 ptscry new`: nessun monitor, nessun privilegio. **Non** e' a prova di manomissione
(l'utente scrive il proprio audit): usalo solo per sviluppo.

## 4. Uso quotidiano

```
ptscry new -A -n lavoro        crea o riattacca "lavoro"
ptscry new -d -n build make -j8    sessione in background con un comando
ptscry list                    le tue sessioni
ptscry attach -r lavoro        sola lettura (per affiancare un collega)
ptscry kill lavoro
ptscry run -n job make -j8     esegue il comando come sessione, ne mostra l'output e termina con il SUO codice
                               di uscita; la sessione si chiude da sola quando il comando finisce
ptscry run -k ...              come sopra, ma se il client viene interrotto uccide il comando
ptscry wait job                segue (sola lettura, senza tty) una sessione esistente e ne restituisce l'exit code
```
`new -d CMD` avvia il comando in background e la sessione termina con esso, ma non restituisce l'esito:
per "lancia, attendi e chiudi" usare `run` (e `wait` per agganciarsi in seguito). Se la connessione cade durante
`run` il comando **continua** (senza `-k`). Funzionano anche via sshd o front-end: `ssh host ptscry run -n j cmd`.
Per catturare l'output di comandi velocissimi tenere `replay_bytes` > 0.

**Job staccati (per controllori come GoREX).** `ptscry run -d -n job cmd` avvia il comando e torna subito stampando
il nome; alla fine l'host conserva **codice di uscita e coda dell'output** (`replay_bytes`) per `job_retention`
(default 1h, `0` = non conservare, max 7d; al massimo 64 risultati per utente, i piu' vecchi escono per primi).
Poi, anche a distanza di ore e da un'altra connessione:
```
ptscry status job        running | finished exit=N | unknown (uscita 4)     (-j: JSON)
ptscry wait -q job       segue il job se gira, altrimenti consegna output e exit code salvati
ptscry wait -q -x job    come sopra e scarta il risultato
ptscry list -j           {"sessions":[...],"jobs":[...]} per gli script
ptscry kill job          su un job finito ne scarta il risultato
```
I risultati stanno solo nella memoria dell'host dell'utente (non sono su disco, non sopravvivono a `stop` o al
riavvio del monitor) e non contano come sessioni; un host `exit_on_last_shell` resta attivo finche' ci sono
risultati da ritirare. Un nuovo job con lo stesso nome sostituisce il vecchio risultato. Il ritiro e' in audit.

Limite di sessioni per utente/gruppo, con sintassi a blocchi `Match`:
```
max_sessions 8                      # default globale
Match User guido
    MaxParallelSessions 5
Match User rex-agent,svc-deploy
    MaxParallelSessions 16
Match Group sysadmins
    MaxParallelSessions unlimited
```
(la vecchia sintassi `[user:x]` / `chiave = valore` resta valida). Resta il limite globale `max_total_sessions`.

Rate limit delle richieste al monitor: default 10 richieste/s per utente (burst = 4x). Un controllore che lancia
molti job al secondo va alzato con `RateLimit N` (1..1000) in un `Match User`; ogni richiesta e' un record di
audit con fsync, quindi valori alti costano I/O. Il valore vale dalla prima richiesta dopo il reload.
Stato di uscita: quando la shell termina il client stampa il codice di uscita. Se la connessione cade la sessione
**resta viva**: riattaccati.

**Comandi sulle sessioni (stile tmux, dalla 0.7.0).** Funzionano in locale, con `ptscry -H` e attraverso il front-end
SSH (`ssh host ptscry ...`); agiscono solo sulle sessioni del proprio utente e sono in audit.
```
ptscry rename vecchio nuovo         rinomina una sessione (il nome di creazione resta riservato finche' vive)
ptscry notify lavoro "build finita" mostra un messaggio sui terminali collegati a "lavoro"
ptscry send-keys lavoro 'make' Enter  digita nella sessione: testo e nomi di tasti (Enter, Tab, Escape, Space,
                                    C-c, M-x, Up, F1...; -l = tutto letterale; senza tasti: standard input)
ptscry layout list | show NOME | save NOME [FILE|-] | rm NOME
                                    disposizioni (pannelli e sessioni) salvate sul server per la GUI e il client
```
- `notify`: testo ripulito da caratteri di controllo e sequenze di escape (anche bidirezionali), max 1024 byte e
  10 avvisi ogni 10 s per sessione. Sul terminale appare come riga a se' (come `write`); sopra un programma a tutto
  schermo copre l'ultima riga per 4 s, poi il programma viene invitato a ridisegnarsi.
- `send-keys`: rifiutato via front-end per le chiavi senza terminale (`no-pty`, `restrict`): sarebbe una shell
  interattiva per altra via. Max 4096 byte per volta; l'audit registra **solo la lunghezza** (cio' che si digita puo' essere una
  password). Se la sessione non legge l'input, una seconda richiesta viene rifiutata finche' la prima e' in attesa.
- `layout`: un oggetto JSON di al massimo 16 KiB, al massimo 256 per utente, in
  `~/.local/state/ptscry/layouts/` (cartella 0700 dell'utente; li scrive l'host dell'utente, mai root). Sopravvivono
  all'host: se non e' attivo viene avviato per leggerli. Via ssh il contenuto arriva dallo standard input
  (`ssh host ptscry layout save web < web.json`).
- Dopo `rename` la variabile `PTSCRY_SESSION` delle shell gia' aperte mantiene il nome originale.

### 4.1 Il tasto di comando (prefisso)

Dentro una sessione il prefisso e' **Ctrl-B** (come tmux, dalla 0.6.0; prima era Ctrl-]): `Ctrl-B d` stacca,
`Ctrl-B Ctrl-B` invia un Ctrl-B letterale alla shell (utile perche' in bash Ctrl-B sposta il cursore a sinistra),
`Ctrl-B h` (o `Ctrl-B ?`) mostra l'elenco dei tasti e dei comandi utilizzabili fuori dalla sessione (vale anche
attraverso il front-end SSH; Ctrl-L ridisegna lo schermo).

Gli altri tasti (dalla 0.7.0, anche attraverso il front-end SSH):

| Tasti | Effetto |
|---|---|
| `Ctrl-B s` | elenco delle sessioni: si scrive per filtrare, frecce, Invio per passarvi, Esc per tornare |
| `Ctrl-B (` / `Ctrl-B )` | sessione precedente / successiva (ordine alfabetico, ricomincia dall'inizio) |
| `Ctrl-B L` | la sessione in cui si era prima |
| `Ctrl-B c` | nuova sessione (nome automatico `sN`; `Ctrl-B $` per rinominarla) |
| `Ctrl-B $` | rinomina la sessione corrente |
| `Ctrl-B f` | congela / scongela la sessione corrente (come `ptscry freeze/thaw`) |
| `Ctrl-B m` / `Ctrl-B M` | avvisi si/no: attivita' / 30 s di silenzio nelle altre sessioni |

Ogni cambio di sessione e' uno stacco e un riattacco (`detach`/`attach` in audit); i comandi sono richieste
normali dell'utente, con gli stessi controlli. Con `attach -r` (sola lettura) restano solo gli spostamenti tra
sessioni (che restano in sola lettura); `c`, `$`, `f` e `w` sono rifiutati. I nomi di sessione non possono iniziare con `-`. Mentre l'elenco o la domanda sono a schermo l'output della
sessione viene trattenuto (fino a 512 KiB, oltre si chiede al programma di ridisegnarsi).

`Ctrl-B w` (tastiera condivisa): con piu' client sulla stessa sessione (lo stesso utente da due postazioni) chi lo
preme prende la tastiera e gli altri guardano soltanto (input e ridimensionamento ignorati, con un avviso); di nuovo per
restituirla; se il client si stacca la tastiera torna libera. In audit `keyboard_take` / `keyboard_release`.

### 4.2 Pannelli sul terminale: ptscry-tui (dalla 0.7.0)

`ptscry tui [sessione...]` (o `ptscry-tui`, anche `-H server` per un server remoto, `ptscry attach -l NOME` per una
disposizione salvata) divide il terminale in pannelli come tmux: ogni pannello e' una sessione ptscry a se'.
Tasti dopo il prefisso: `%` e `"` dividono, frecce/`o`/`;` spostano, `Alt`/`Ctrl`+frecce ridimensionano, `z` zoom,
`Spazio` disposizioni pronte, `x` chiude (stacca), `S` input sincronizzato, `[` modalita' copia (ricerca con `/`,
copia negli appunti del terminale con OSC 52), `W`/`O` salvano/aprono una disposizione sul server, `d` stacca tutto.
Gli altri (`s ( ) L c $ f m M w`) arrivano al client della sessione del pannello. Barra di stato e mouse si tolgono con
`-s`/`-m` o con `status = off` / `mouse = off` in client.conf. Il cursore e' quello che chiede il programma del pannello attivo
(forma e lampeggio, DECSCUSR) passato al terminale; se non chiede niente resta quello del terminale. `cursor = blink` o `steady`
in client.conf forza il lampeggio. Dettagli: `man ptscry-tui`. Da dentro un pannello (anche
nella GUI) `ptscry split [-v]` apre un pannello e `ptscry layout apply NOME` una disposizione.

Si puo' cambiare in due modi:

- **Sul client**, in `client.conf` con una riga `prefix = ctrl-a`:
  Linux `~/.config/ptscry/client.conf` (rispetta `XDG_CONFIG_HOME`) e, per tutti, `/etc/ptscry/client.conf`;
  macOS `~/Library/Application Support/ptscry/client.conf`; Windows `%APPDATA%\ptscry\client.conf`.
  Vince il file dell'utente, poi quello di sistema. Un file di un altro utente o scrivibile da altri, o con
  un valore non valido, e' ignorato con un avviso e si usa il tasto della policy.
- **Dal server**, con `detach_prefix` in `ptscry.conf` (anche per utente, gruppo o `Match`): vale per chi non ha un
  `client.conf`. Con il front-end SSH il tasto lo interpreta il server: il client di ptscry (`ptscry -H`, cap. 3.6)
  gli comunica il proprio prefisso a inizio sessione (richiesta `ptscry-prefix`, solo tasti ammessi), mentre con un
  `ssh` qualunque vale il `detach_prefix` del server.

Formati: `ctrl-b`, `C-b`, `^B`, `ctrl-]`. Sono ammessi solo i tasti di controllo; sono rifiutati quelli che renderebbero
la sessione inutilizzabile (Ctrl-C, Ctrl-D, Ctrl-Z, Invio, Tab, Backspace, Esc, NUL).

## 5. Gestione

**Utenti e chiavi: `ptscry setup`.** Senza argomenti apre una procedura guidata testuale (menu numerati); ogni operazione
esiste anche come comando diretto, utile negli script:

```
sudo ptscry setup                                  # procedura guidata: utenti, chiavi, front-end
sudo ptscry setup frontend                         # attiva il front-end SSH (account, chiave host, frontend = yes, reload)
sudo ptscry setup users list                       # membri di access_group e admin_group, con UID e numero di chiavi
sudo ptscry setup users add -create mario          # crea l'account locale se manca e lo mette in access_group
sudo ptscry setup users add -admin anna            # anche in admin_group
sudo ptscry setup users remove mario               # lo toglie dai gruppi (l'account resta; le sessioni vengono chiuse)
sudo ptscry setup keys add mario mario.pub         # chiave pubblica per il front-end SSH (anche "-" = stdin)
sudo ptscry setup keys add mario -home             # importa ~mario/.ssh/authorized_keys
sudo ptscry setup keys list [mario]                # chiavi con tipo, impronta, commento, opzioni
sudo ptscry setup keys remove mario 2              # per numero o impronta SHA256:...
```

`ptscry setup frontend` fa in un passo cio' che serve perche' la GUI (o `ptscry -H`) si colleghi da un altro computer: crea
l'account `ptscry-fe` e la chiave host, mette `frontend = yes` in `[global]` (la configurazione nuova viene verificata prima
di sostituire la vecchia, che resta come `.bak`), ricarica il monitor e dice quale porta aprire nel firewall. Il messaggio
"connection refused" / "actively refused" nel client significa proprio che il front-end non e' attivo (o la porta e' un'altra).

I gruppi locali si cambiano con `gpasswd`; se `access_group` viene da FreeIPA/SSSD il comando non lo tocca e dice il comando
`ipa group-add-member` da usare. Le chiavi vanno in `frontend_authorized_keys_dir` (file root 0644, scrittura atomica):
vengono rifiutate le chiavi private, i doppioni e le righe con opzioni che il front-end non saprebbe rispettare.

| Operazione | Comando |
|---|---|
| Vedere chi ha sessioni | `ptscry admin list` |
| Chiudere una sessione | `ptscry admin kill UTENTE SESSIONE` |
| Fermare tutto di un utente | `ptscry admin stop UTENTE` |
| Applicare la configurazione | `systemctl reload ptscry-monitor` |
| Installare una nuova versione / riciclare il monitor **senza chiudere le sessioni** | `make install && sudo ptscry admin reexec` (o `kill -USR2` al monitor), cap. 8.2 |
| Controllare config e installazione senza avviare | `ptscry monitor -t` |
| Sospendere un account | `[user:mario] enabled = no` + reload (le sessioni in corso terminano) |
| Verificare la salute | `ptscry check` |
| Registrare il TOTP di un utente (front-end) | `ptscry totp-init UTENTE` (`-f` per sostituirlo) |

Note operative:
- **Riavviare o fermare il monitor termina tutte le sessioni** (scelta voluta: nessuna sessione senza audit).
  Pianifica le finestre di manutenzione e avvisa gli utenti. `reload` invece non le tocca.
- I limiti sul numero di sessioni valgono subito; ciclo di vita, `idle_timeout` e `record` si applicano agli host
  avviati dopo (`ptscry admin stop UTENTE` per forzare).
- La policy e' riapplicata alle sessioni in corso a ogni reload e ogni minuto: utente disabilitato, tolto da `access_group`
  o cancellato = sessioni chiuse (evento `policy_revoked`). Un account disabilitato **solo in IPA** non e' visibile
  al monitor: usa `max_lifetime` o `admin stop`.
- Limiti globali di protezione (vedi `man ptscry.conf`): `max_total_sessions`, rate limit per uid, 32 client per
  sessione, `record_max_bytes`.

## 6. Audit: cosa viene registrato, dove, come si conserva e si analizza

### 6.1 Principio
Ogni cosa rilevante genera un **record JSON su una riga** in `/var/log/ptscry/audit.log`. Lo scrive **solo il
monitor** (root): i processi dell'utente non possono ne' scriverlo ne' alterarlo (l'host dell'utente e' non
ispezionabile con ptrace e l'identita' nei record la mette il monitor, non l'host). Se l'audit non e' scrivibile il
monitor **rifiuta nuove richieste** e mette in coda (in memoria, limitata) quanto accade, scrivendolo in ordine appena
possibile.

### 6.2 Anatomia di un record
```json
{"ts":"2026-10-08T18:16:28.9Z","seq":108,"prev":"ccabd3…f99f4d","event":"session_new","host":"adm01",
 "uid":30033,"user":"mario","src":"host","host_pid":19027,"session":"lavoro","pid":19034,
 "cmd":"/bin/bash","record":"output","record_file":"/var/log/ptscry/rec/30033-mario/lavoro-….cast","req":69}
```
- `seq` e `prev`: numero progressivo e SHA-256 della riga precedente (catena, cap. 6.5).
- `uid`, `user`, `host_pid`, `src`: aggiunti dal monitor; `src:"host"` = evento relativo a una sessione,
  `src:"monitor"` = deciso dal monitor.
- `req`: numero della richiesta, **collega** la richiesta (`request`) a tutto cio' che ne segue.
- Nei `request`: `peer_uid`, `peer_pid`, `op`, `loginuid` e `audit_session` (impostati dal kernel al login: dicono
  *chi si e' autenticato davvero*, anche dopo `su`/`sudo`; `4294967295` = non impostato), `via_sshd` (verificato: il chiamante
  discende da un sshd di root), `tty`, `client_exe`, `ssh_connection_claimed` (**dichiarato** dall'utente: non e' una prova).

### 6.3 Catalogo degli eventi
| Evento | Significato |
|---|---|
| `request` | richiesta ricevuta (operazione, chi, da dove) |
| `access_denied` | rifiutata: fuori da `access_group`, `enabled=no`, nessuna shell, `allow_command=no`, non admin |
| `limit_reached` | `max_sessions` (o `scope:"total"` = `max_total_sessions`) |
| `rate_limited` | troppe richieste da un uid (riassunto, `refused` = quante) |
| `host_spawn`, `host_exit`, `host_spawn_failed` | avvio/fine dell'host di un utente |
| `session_new`, `session_end` | nascita e fine di una sessione (comando, pid, `exit_code`, `reason`, `duration_s`) |
| `attach`, `detach` | un client si collega/scollega (`readonly`, `clients`, `reason`) |
| `kill`, `stop_requested`, `idle_kill`, `lifetime_kill` | chiusure volute dall'utente o dai limiti |
| `rename`, `notify`, `send_keys` | comandi sulle sessioni (`from` = nome precedente; `bytes` = lunghezza, mai il contenuto) |
| `layout_set`, `layout_del` | disposizioni salvate o rimosse (`layout`, `bytes`) |
| `job_collect`, `job_discard` | ritiro (`wait`) o scarto (`wait -x`, `kill`) del risultato di un job staccato (`exit_code`) |
| `admin_kill`, `admin_stop` | azioni amministrative (`admin`, `target_user`) |
| `policy_revoked` | sessioni chiuse perche' la policy non consente piu' l'utente |
| `record_open`, `record_close`, `record_error` | registrazione (file, modalita', errori: limite, disco) |
| `config_reloaded`, `config_reload_failed` | reload della configurazione (anche rifiutata) |
| `monitor_start`, `monitor_shutdown` | ciclo di vita del monitor (versione, config, ptrace_scope) |
| `audit_reopen`, `audit_close`, `audit_probe` | rotazione/chiusura/recupero dopo un errore del disco |
| `audit_events_lost` | la coda in memoria e' traboccata durante un guasto prolungato (`count`) |
| `host_flood`, `host_event_unknown`, `host_event_invalid` | comportamenti anomali di un host |
| `session_launch` | la shell e' partita: `pid`, `loginuid` e `audit_session` letti dal kernel, servizio PAM |
| `launch_failed` | avvio rifiutato; `stage` = `account` (PAM/HBAC), `session`, `setcred`, `spawn`... |
| `launch_exit` | il launcher ha concluso la sessione (codice di uscita) |

### 6.4 Conservazione
Quattro punti, i primi tre sull'audit:

1. **Copia fuori dall'host (indispensabile).** Con `audit_syslog = yes` ogni record e' inviato anche a syslog
   (`authpriv`, programma `ptscry`, il messaggio e' il JSON identico alla riga del file). `contrib/rsyslog.d/50-ptscry.conf`
   lo inoltra a un collettore in TLS con coda su disco. Il collettore deve essere gestito da altre persone/credenziali:
   se chi diventa root qui potesse cambiare anche il collettore, la copia non servirebbe a nulla. Meglio ancora, archiviazione
   non riscrivibile (WORM / object lock / sola aggiunta).
2. **Chi puo' leggere il file locale.** Di default solo root (0600): il log contiene l'attivita' di tutti gli utenti, quindi
   non va reso leggibile ai membri di `access_group`. Con `audit_group = ptscry-admins` (impostato nella configurazione
   di esempio) il file e la sua directory diventano `root:ptscry-admins` 0640/0750, anche dopo logrotate e `reload`:
   gli amministratori fanno `ptscry audit-verify` e indagini senza root, ma non possono scrivere. `ptscry check` segnala
   CRIT se `audit_group` e' l'`access_group`.
3. **File locale ruotato.** `contrib/logrotate.d/ptscry`: settimanale, 104 rotazioni (2 anni), `create 0600 root root`,
   `SIGHUP` al monitor (che riapre il file, **continua la catena** e rimette i permessi di `audit_group`); non usare `copytruncate`.
4. **Registrazioni** (`/var/log/ptscry/rec/UID-utente/*.cast`) con una conservazione piu' breve, perche' contengono il
   contenuto delle sessioni (dati personali, e con `record = all` anche password digitate).

Il periodo di conservazione non lo decide il software: va fissato nella vostra politica (ISO A.8.15) tenendo conto di
obblighi di legge, GDPR (informativa, minimizzazione, accesso limitato) e dello spazio. Consiglio di partenza: audit 12-24 mesi,
registrazioni 3-6 mesi, copie cifrate a riposo.

### 6.5 Integrita': cosa garantisce la catena (e cosa no)
Ogni record contiene l'hash SHA-256 della riga precedente. `ptscry audit-verify` rilegge i file e scopre:
record modificati, cancellati, riordinati, buchi nella sequenza. Piu' file si verificano come una catena sola:

```
zcat /var/log/ptscry/audit.log.3.gz > /root/audit-a3          # i file compressi vanno prima decompressi
ptscry audit-verify /root/audit-a3 /var/log/ptscry/audit.log.1 /var/log/ptscry/audit.log
```
Un intervallo contiguo qualsiasi si puo' verificare da solo (il primo record non e' confrontato con uno precedente).

**Limite onesto:** la catena rende la manomissione *rilevabile*, non impossibile. Chi ha root sul server puo' riscrivere il
file e ricalcolare tutti gli hash. La difesa e' la copia esterna: quella catena, gia' fuori, non si puo' piu' modificare.
Verifica periodica consigliata: ricostruisci il file dal collettore, esegui `audit-verify` e confronta con il locale
(`cmp`): una differenza = il file locale e' stato toccato.

### 6.6 Analisi
Ogni riga e' JSON: `jq` basta per le indagini; per l'analisi continua mandalo a un SIEM.

```bash
A=/var/log/ptscry/audit.log
# Tutto cio' che ha fatto un utente, in ordine
jq -c 'select(.user=="mario")' $A
# Una richiesta e tutte le sue conseguenze
jq -c 'select(.req==69)' $A
# Sessioni concluse, con durata ed esito
jq -r 'select(.event=="session_end") | [.ts,.user,.session,.duration_s,.exit_code,.reason] | @tsv' $A
# Accessi negati e perche'
jq -r 'select(.event=="access_denied") | [.ts,.user,.reason] | @tsv' $A
# Richieste NON arrivate da sshd (accesso locale/console o scorciatoie)
jq -c 'select(.event=="request" and .via_sshd==false)' $A
# Chi si e' autenticato davvero (loginuid) e chi ha agito (uid): differiscono dopo su/sudo
jq -r 'select(.event=="request" and .loginuid!="4294967295" and (.loginuid|tonumber)!=.peer_uid) | [.ts,.loginuid,.peer_uid,.op] | @tsv' $A
# Chi ha toccato le sessioni altrui
jq -c 'select(.event|test("^admin_"))' $A
# Quali registrazioni esistono per una sessione
jq -r 'select(.event=="record_open" and .user=="mario") | .file' $A
# Riproduzione di una registrazione (formato asciicast v2)
asciinema play /var/log/ptscry/rec/30033-mario/lavoro-1791483240-984439.cast
```

Correlazione con altri log: `loginuid`/`audit_session` coincidono con `auid`/`ses` di **auditd** e con il login in `journalctl`
di sshd; l'orario (`ts`, UTC) con l'ora di sshd. Tenete gli orologi sincronizzati (`check` controlla NTP): senza, la correlazione perde valore.

### 6.7 Cosa far scattare come allarme
| Segnale | Perche' |
|---|---|
| `monitor_start` / `monitor_shutdown` non pianificati | riavvio inatteso = sessioni perse, possibile manomissione |
| raffica di `access_denied` dallo stesso `user`/`peer_uid` | tentativi di accesso non autorizzato |
| `rate_limited`, `host_flood` | abuso o malfunzionamento di un client |
| `limit_reached` ripetuto con `scope:"total"` | capacita' esaurita |
| `record_error`, `audit_probe`, `audit_events_lost` | disco pieno o guasto: l'audit e' in pericolo |
| `config_reload_failed` | qualcuno ha toccato la configurazione (o l'ha rotta) |
| `policy_revoked`, `admin_kill`, `admin_stop` | azioni sull'accesso di altri: da rivedere |
| assenza di eventi/heartbeat dal collettore | il monitor o l'inoltro e' fermo |
| `audit-verify` che fallisce | integrita' compromessa: incidente |

### 6.8 Inoltro verso un SIEM (Wazuh, Elastic/Kibana: vedi `docs/SIEM.md`)
Sul collettore (rsyslog) il messaggio e' gia' JSON: `mmjsonparse` o Filebeat/Fluent Bit con parser JSON lo trasformano in campi
(`event`, `user`, `req`...). Indicizza `event`, `user`, `uid`, `req`, `session` e imposta come ora dell'evento il campo `ts`.

## 7. Problemi frequenti

| Sintomo | Causa e rimedio |
|---|---|
| `cannot reach the ptscry monitor` | servizio fermo: `systemctl status ptscry-monitor`; il client non ha fallback |
| `refusing to talk to … not served by root` | il socket e' stato creato da un altro utente; controlla `monitor_socket` e i permessi della directory |
| `access denied` | non in `access_group`, `enabled=no`, shell `nologin`: vedi `access_denied` nell'audit (`reason`) |
| `audit log unavailable: refusing to operate` | disco pieno o file non scrivibile; libera spazio, il monitor si riprende da solo alla richiesta successiva |
| `too many requests, slow down` | rate limit (10/s, raffica 40): script troppo aggressivo |
| `session limit reached` / `server-wide session limit` | `max_sessions` / `max_total_sessions` |
| il monitor non parte: `… writable by …` | binario, config o loro directory scrivibili da altri: correggi i permessi |
| utenti FreeIPA non riconosciuti | binario compilato senza cgo: `make build`; `check` lo segnala |
| `command line too long` | piu' di 32 KiB di argomenti in `ptscry new` |
| `audit-verify` segnala "sequence gap" | file mancanti o in ordine sbagliato nella lista |

## 8. Aggiornamento
Sostituire il binario (`make install`) non tocca i processi in corso: il monitor e i suoi host continuano con la versione
caricata. Per usare la nuova versione **dalla 0.5.4** non serve riavviare: `ptscry admin reexec` (cap. 8.2) sostituisce il programma del monitor sul posto e le sessioni restano. Un `systemctl restart` invece termina tutte le sessioni (fail closed). Controlla `ptscry version` e `check`.

### 8.2 Aggiornare o riciclare il monitor senza chiudere le sessioni (0.5.4)
```
make install                 # sostituisce il binario (rename: i processi in corso non sono toccati)
ptscry monitor -t            # facoltativo: config e installazione accettate dal nuovo programma?
ptscry admin reexec          # solo root; equivale a: kill -USR2 $(systemctl show -p MainPID --value ptscry-monitor)
```
Il monitor **si rilancia come lo stesso processo** (stesso pid: systemd non vede alcun riavvio) con il programma
nuovo. Host, launcher e front-end sono suoi figli e restano tali; i canali che li legano a lui (socket di controllo, socket
di ascolto, registrazioni aperte) sono ereditati dal nuovo programma, quindi non c'e' nessun punto di incontro da
ricostruire ne' di cui fidarsi. Passano anche i ticket del front-end e i blocchi TOTP, quindi **le connessioni SSH
aperte continuano a funzionare**; i client in coda sul socket aspettano qualche decina di millisecondi. L'audit
registra `monitor_reexec`, poi `monitor_start` con `resumed:true` e `monitor_resumed` (host, sessioni, launcher,
front-end ripresi); la catena di hash prosegue senza interruzioni.

Prima di rilanciarsi il monitor verifica che il nuovo programma giri (`version`) e accetti la configurazione
(`monitor -t`): se no scrive `monitor_reexec_failed` e **continua com'era**. Rifiuta anche se un'altra chiusura e' in
corso (si riprova dopo qualche secondo) o se lo stato non puo' stare in un filesystem in memoria. Dettagli da sapere:
- gli **host** (e le shell) continuano a girare con il **programma vecchio** finche' non terminano: il protocollo di
  controllo monitor-host deve restare compatibile tra versioni adiacenti (nelle note di rilascio se non lo e');
- il front-end resta quello vecchio fino al prossimo riavvio per cambio di configurazione;
- se il monitor muore per altre cause (crash, `restart`, SIGKILL) le sessioni terminano come prima: non c'e' nessuna
  sessione senza audit;
- il rate limit per uid si azzera; i record di audit del microsecondo tra la chiusura del file e l'`exec` possono perdersi.

### 8.1 Passando alla 0.3.0 (sessioni PAM)
`make install` crea `/etc/pam.d/ptscry` se manca (modelli in `contrib/pam.d/`, per Debian o RHEL). Con FreeIPA
crea il servizio HBAC `ptscry` e assegnalo alle regole di accesso, altrimenti `launch_failed` con `stage=account`.
Il binario deve essere compilato con cgo (`make build`); un binario statico rifiuta `pam_service` e si usa
`pam_service = off`. `ptscry check` controlla libpam, il file PAM, `pam_loginuid`/`pam_limits` e `/proc/self/loginuid`.
Limiti: i demoni lanciati con `setsid` non vengono chiusi con la sessione; niente utmp/wtmp.

## 9. Domande frequenti
- **Posso usarlo senza SSH?** Oggi solo in locale o dietro sshd. Il front-end TCP integrato e' previsto dopo.
- **Un utente puo' cancellare le proprie registrazioni o l'audit?** No: sono di root, 0600, in directory di root (l'audit puo' essere reso leggibile, non scrivibile, a un gruppo di amministratori con `audit_group`).
- **Un utente puo' evitare ptscry?** Se ha una shell normale, si. Per imporlo serve la modalita' bastion.
- **Le password digitate vengono registrate?** Solo con `record = all`. Con `output` si registra cio' che appare a schermo
  (le password non vengono mostrate).
- **E se il collettore e' irraggiungibile?** rsyslog accoda su disco (`queue.filename`); il file locale continua a essere scritto.
