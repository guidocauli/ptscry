# ptscry-gui - Guido Cauli <guido.cauli@gmail.com>

# Guida all'uso di ptscry-gui

Guida pratica, a passi, per chi usa la finestra grafica. Il riferimento completo (profili, impostazioni, compilazione, limiti) e'
in [GUI.md](GUI.md); per installare e configurare il **server** vedi [MANUALE.md](MANUALE.md); per la riga di comando `man ptscry`.
Questa guida si legge anche con `man ptscry-gui` (sintesi).

## 1. Che cos'e' in due righe

Ogni **tab** e' una *sessione* ptscry: un terminale che continua a vivere anche se chiudi la finestra, esci dal desktop o cade la
rete. Alla riapertura il tab si riattacca e ritrovi tutto com'era (programmi compresi, finche' il computer o il server restano accesi).

## 2. Installare

Il modo piu' semplice e' l'**installer** del proprio sistema:

| Sistema | File | Come |
|---|---|---|
| Windows | `ptscry-gui-setup-windows-amd64.exe` (o `-arm64`) | doppio clic; si sceglie "solo per me" (senza amministratore) o "per tutti"; voce nel menu Start, icona sul desktop e PATH facoltativi; si disinstalla da Impostazioni → App |
| Debian, Ubuntu | `ptscry-gui_VERSIONE-1_amd64.deb` | `sudo apt install ./ptscry-gui_*.deb` (rimozione: `sudo apt remove ptscry-gui`) |
| Fedora, Alma, RHEL, openSUSE | `ptscry-gui-VERSIONE-1.x86_64.rpm` | `sudo dnf install ./ptscry-gui-*.rpm` (o `zypper install`) |
| macOS | `ptscry-gui-macos-ARCH.dmg` | si apre e si trascina ptscry-gui in Applicazioni |

I pacchetti Linux mettono il programma in `/usr/lib/ptscry-gui`, il comando `ptscry-gui` in `/usr/bin`, la voce nel menu,
le icone, la pagina di manuale (`man ptscry-gui`) e questa guida in `/usr/share/doc/ptscry-gui`.

In alternativa, senza privilegi, dagli archivi `.tar.gz` / `.zip`:

| Sistema | Per te soltanto | Per tutti gli utenti |
|---|---|---|
| Linux | `./install.sh` | `sudo ./install.sh --system` |
| macOS | `./install.sh` (crea ptscry-gui.app) | `sudo ./install.sh --system` |
| Windows | doppio clic su `install.bat` | `install.bat -System` |

Per togliere tutto: stesso comando con `--uninstall` (Windows: `-Uninstall`). Le impostazioni in `~/.config/ptscry` non si toccano mai.

## 3. Primo avvio

1. Lancia **ptscry-gui** dal menu delle applicazioni (o da terminale).
2. Si apre un tab con il profilo predefinito: **senza server** su Linux/macOS, cioe' una sessione locale persistente che non richiede root ne' SSH; su Windows **terminale locale**, una shell (PowerShell o cmd) che non sopravvive alla chiusura del tab. Per sessioni persistenti su Windows collegati a un server ptscry (capitolo 5).
3. Scrivi come in un terminale qualunque. Il pulsante **+** (o `Ctrl+Maiusc+T`) apre un altro tab.
4. Chiudi la finestra: le sessioni restano vive. Rilancia ptscry-gui e i tab tornano da soli.

## 4. Operazioni di ogni giorno

| Voglio... | Faccio cosi' |
|---|---|
| un nuovo terminale | **+**, `Ctrl+Maiusc+T` |
| dare un nome al tab | `Ctrl+Maiusc+R` (cambia solo l'etichetta) |
| chiudere il tab senza uccidere la sessione | **x** (con piu' pannelli `Ctrl+Maiusc+W` chiude solo quello attivo) |
| dividere il tab in pannelli | **Pannelli → Dividi a destra / sotto**, `Alt+Maiusc++` / `Alt+Maiusc+-`; ogni pannello e' una sessione sua |
| spostarmi tra i pannelli | clic, `Alt`+frecce, `Alt+Maiusc+O` |
| allargare un pannello | trascinare la linea di divisione, `Alt+Maiusc`+frecce; `Alt+Maiusc+Z` lo ingrandisce a tutto il tab |
| scrivere in tutti i pannelli insieme | `Alt+Maiusc+S` (bordo rosso e avviso nella barra di stato finche' e' attivo) |
| ritrovare i pannelli su un altro computer | **Pannelli → Disposizioni sul server** (`Alt+Maiusc+L`): Salva, poi da li' Apri |
| riprendere una sessione gia' esistente | **Sessione → Sessioni del server** (`Ctrl+Maiusc+S`) → **Apri** |
| guardare la sessione di un collega senza poter scrivere | Sessioni → **Sola lettura** |
| staccarmi ma lasciare il tab aperto | `Ctrl+Maiusc+D`; **Invio** riattacca |
| fermare tutti i programmi della sessione | `Ctrl+Maiusc+F` (congela, `[II]` nel titolo); di nuovo per scongelare |
| terminare davvero la sessione | Sessioni → **Termina** (chiede conferma) oppure `exit` nella shell |
| copiare / incollare | seleziona col mouse (copia da solo), `Ctrl+Maiusc+C` / `Ctrl+Maiusc+V`, tasto destro |
| leggere l'output passato | rotella, `Maiusc+PagSu` / `PagGiu` |
| testo piu' grande o piu' piccolo | `Ctrl` + `+` / `-` / `0`, oppure `Ctrl`+rotella |
| una shell diversa (bash, zsh, fish, Git Bash, WSL...) | profilo **terminale locale**: nel dialogo del **+** si sceglie la shell del tab; quella predefinita in Impostazioni |
| un altro font per il terminale | **Vista → Scegli il carattere del terminale...**: elenco dei font a spaziatura fissa del sistema, con filtro |
| interfaccia piu' grande (menu e tab) | **Vista → Ingrandisci l'interfaccia** o Impostazioni |
| piu' spazio | **Vista → Nascondi la barra dei menu** (resta ☰), schermo intero |

## 5. Collegarsi a un altro computer

1. **+** (Nuova sessione) → **ptscry** per un server ptscry, oppure **SSH** per un server SSH qualsiasi.
2. Scrivi indirizzo, porta e utente (o scegli una connessione salvata nell'elenco ricercabile in alto).
3. Chiave: scegli dall'elenco, oppure **Importa una chiave…** apre il selettore di file di sistema; vuoto = `ssh-agent` e `~/.ssh`. Per SSH c'e' anche "Password".
4. Per ptscry: **Nuova sessione** o **Collegati a una sessione esistente** (l'elenco arriva dal server).
5. Al primo collegamento controlla l'impronta della chiave del server e accetta: da quel momento una chiave diversa viene rifiutata.
6. Se il server vuole il codice TOTP o la passphrase della chiave, la GUI li chiede in una finestra; non vengono mai salvati.

Il profilo resta salvato: la volta dopo lo scegli dal "+". Il server deve avere ptscry con il front-end SSH attivo (MANUALE.md 3.5).
Per il computer in uso con server ptscry installato scegli invece il profilo **locale**.

## 6. Lavorare a lungo e tornare dopo

- **Workspace**: *Sessione → Workspace* salva con un nome l'insieme dei tab aperti (profilo, sessione, directory, nome, variabili) e li
  riapre con un clic. Alla chiusura la GUI salva comunque i tab aperti e li riapre all'avvio.
- **Variabili d'ambiente**: *Sessione → Variabili d'ambiente* (`Ctrl+Maiusc+E`) per tab; "Salva e applica ora" le scrive anche nella shell aperta.
- **Directory**: se la sessione non esiste piu' (server riavviato), la GUI ne crea una nuova e ci scrive un `cd` alla directory di prima.
  Non tornano i programmi in esecuzione, solo sessione, directory e ambiente.

## 7. Gestire ptscry senza aprire un terminale

**File → Gestione ptscry** esegue dalla finestra i comandi di `ptscry` sul profilo scelto: elenco e stato delle sessioni, job
in background (`run -d` e risultato con `wait`), congela/scongela, host, configurazione e verifica. Sul profilo "locale" con i
privilegi giusti ci sono anche le funzioni di amministrazione e il controllo del servizio `ptscry-monitor`. Le azioni distruttive
chiedono "Sicuro?" al secondo clic. Dettagli in GUI.md ("Gestione ptscry").

## 8. Chiavi private

**File → Chiavi private → Importa una chiave**: scegli il file col selettore di sistema. La chiave viene controllata (privata, anche
con passphrase) e copiata in `~/.config/ptscry/keys` con permessi 0600; l'originale non si tocca. Si puo' eliminare dalla stessa finestra.

## 9. Se qualcosa non va

| Sintomo | Cosa fare |
|---|---|
| "il front-end ... non risponde su 127.0.0.1:2222" | usa il profilo "senza server" oppure attiva il front-end (MANUALE.md 3.5) |
| "il comando ptscry non e' stato trovato" | metti `ptscry` accanto a `ptscry-gui` o nel PATH |
| il tab mostra `!` o `x` | connessione persa o sessione finita: premi **Invio** per riattaccare |
| "HAS CHANGED" sulla chiave del server | verifica con l'amministratore, poi togli la riga da `~/.config/ptscry/known_hosts` |
| testo troppo piccolo/grande su uno schermo HiDPI | Vista → interfaccia, e `Ctrl` + `+`/`-` per il terminale |
| non c'e' la barra del titolo | normale su Niri, Hyprland, Sway: i tre pulsanti sono a destra dei tab |

Ancora in difficolta': GUI.md, capitolo 8, e `man ptscry-gui`.
