# ptscry-gui - Guido Cauli <guido.cauli@gmail.com>

# Manuale di ptscry-gui (multiplexer grafico di sessioni)

`ptscry-gui` e' un terminale grafico con schede (tab) che usa le sessioni di ptscry: ogni tab e' una sessione
persistente. Chiudere la finestra, uscire dal desktop o perdere la rete **non** ferma i programmi: restano nella
sessione, e alla riapertura il tab si riattacca. Si usa sullo stesso computer o su un server remoto, ed e' un'applicazione
nativa Wayland e X11 (anche su window manager a tasselli come Niri, Hyprland e Sway), Windows e macOS.

E' un programma a parte (`gui/`, modulo `ptscry/gui`) che condivide con il client a riga di comando il codice di
connessione (`internal/rclient`). Il server non cambia, salvo un campo in piu' (`cwd`) nell'elenco delle sessioni.

## 1. Installazione

Compilazione (serve Go; la prima volta scarica i moduli, quindi la rete):

```
./build-gui.sh            # Linux (e Windows in cross-compilazione); su un Mac: macOS e Windows
./build-gui.sh linux      # un solo target: linux, windows, macos
./build-gui.sh -t all     # prima i test (go vet + go test -race)
./build-gui.sh -c         # solo controllo delle dipendenze
make gui                  # solo ptscry-gui nella cartella corrente
```

Con gli strumenti presenti, `build-gui.sh` prepara anche gli installer: `ptscry-gui-setup-windows-<arch>.exe` (NSIS, `apt install nsis`), `.deb` e `.rpm` (nfpm), `.dmg` (solo su un Mac); `-I` li salta. I pacchetti finiscono in `dist-gui/` (`ptscry-gui-linux-amd64.tar.gz`, `ptscry-gui-windows-amd64.zip`, `...-arm64.zip`,
`SHA256SUMS`) e contengono `ptscry-gui`, il comando `ptscry` e la documentazione.

**Dipendenze di compilazione su Linux** (la GUI usa cgo per Wayland, X11 e OpenGL/Vulkan):

| Distribuzione | Pacchetti |
|---|---|
| Debian/Ubuntu | `gcc pkg-config libwayland-dev libxkbcommon-dev libx11-xcb-dev libxkbcommon-x11-dev libx11-dev libxcursor-dev libxfixes-dev libegl1-mesa-dev libvulkan-dev` |
| RedHat/Alma | `gcc pkgconf-pkg-config wayland-devel libxkbcommon-devel libxkbcommon-x11-devel libX11-devel libXcursor-devel libXfixes-devel mesa-libEGL-devel vulkan-loader-devel` |
| Arch | `gcc pkgconf wayland libxkbcommon libxkbcommon-x11 libx11 libxcursor libxfixes mesa vulkan-headers` |

A runtime su Linux servono `libwayland-client`, `libxkbcommon` e `libEGL` (c'e' gia' su ogni desktop). Su Windows non serve
nulla (nessun cgo). Su macOS va compilata su un Mac (cgo e framework Apple): copia i sorgenti e lancia
`./build-gui.sh macos`.

## 2. Dove ci si collega: i profili

Ogni tab appartiene a un **profilo**. Ce ne sono tre tipi:

| Profilo | Cosa fa | Serve |
|---|---|---|
| **senza server** (predefinito su Linux/macOS) | esegue `ptscry` in modo standalone su questo computer | solo il comando `ptscry` accanto a `ptscry-gui` o nel PATH |
| **terminale locale** (predefinito su Windows) | apre una shell di questo computer su una pseudoconsole: PowerShell 7 (`pwsh`), Windows PowerShell o `cmd` su Windows (serve Windows 10 1809 o successivo), la shell di login su Linux/macOS. La shell si sceglie in Impostazioni (predefinita) e per ogni tab nel dialogo del "+": l'elenco mostra quelle installate (Linux/macOS: `/etc/shells`, cioe' bash, zsh, fish...; Windows: PowerShell 7, Windows PowerShell, cmd, Git Bash, MSYS2, Cygwin, WSL) piu' "Un altro comando" (es. `/usr/bin/fish -l`) | niente. **Non e' persistente**: chiudere il tab termina la shell (niente distacco ne' congelamento) |
| **locale** | si collega al front-end SSH di questo computer, `127.0.0.1:2222` | server ptscry con `frontend = yes` (manuale 3.5) |
| **remoto** | si collega a `[utente@]host[:porta]` | un server ptscry raggiungibile |

Per avere sessioni che sopravvivono alla finestra serve un server ptscry (profilo locale o remoto): su Windows, per esempio, si collega la GUI a un server Linux. **Nuova sessione** (pulsante "+", `Ctrl+Maiusc+T`) chiede prima il tipo, con quattro pulsanti: **Terminale locale** (una shell; su
Linux e macOS con "Sessione persistente" diventa una sessione ptscry senza server), **Server locale** (front-end di questo computer),
**SSH** (terminale su un server SSH qualsiasi: indirizzo, porta, utente; accesso con agente, file di chiave o password; nelle opzioni
avanzate jump host come `ssh -J` e comando da eseguire) e **ptscry** (server ptscry remoto). Per i tipi con sessioni si sceglie
"Nuova sessione" (nome, variabili d'ambiente) o "Collegati a una sessione esistente", con l'elenco letto dal server. Le connessioni
SSH e ptscry si salvano con un nome e si ritrovano in un elenco ricercabile (si scrive per filtrare, ▾ lo apre); "Elimina" toglie
quella scelta. Password, passphrase e codici TOTP non vengono mai salvati. Il predefinito si sceglie nelle impostazioni. In passato "Un altro server" nel dialogo creava e salvava un
profilo remoto, con eventuale file della chiave privata (vuoto = ssh-agent, comprese le chiavi FIDO2).

**Senza server.** Nessun root, nessun SSH, nessuna porta aperta: la GUI lancia `ptscry new -A -n NOME` su un terminale locale con
`PTSCRY_STANDALONE=1`; l'host per utente si avvia da solo e tiene le sessioni, che sopravvivono alla finestra. Configurazione in
`~/.config/ptscry/local.conf`, dati (socket, audit, registrazioni) in `~/.local/state/ptscry`. L'audit lo scrive l'utente
stesso: non e' a prova di manomissione (vedi manuale 3.4), quindi e' per uso personale. Non disponibile su Windows.

**Locale e remoto** usano il front-end SSH: autenticazione con chiave (file o ssh-agent), certificati e TOTP, che la GUI chiede in
un dialogo. La chiave del server si accetta al primo uso (come SSH) e si salva in `~/.config/ptscry/known_hosts`; se cambia il
collegamento viene rifiutato. Una chiave diversa dalla salvata non viene mai accettata in silenzio.

## Installazione

Ogni pacchetto contiene l'installatore e le icone (l'icona e' anche dentro `ptscry-gui.exe`).

| Sistema | Per l'utente (senza privilegi) | Per tutti gli utenti |
|---|---|---|
| Linux | `./install.sh` (in `~/.local`: programma in `lib/ptscry-gui`, comando in `bin`, voce di menu e icone in `share`) | `sudo ./install.sh --system` (in `/usr/local`) |
| macOS | `./install.sh` (crea `~/Applications/ptscry-gui.app`) | `sudo ./install.sh --system` (`/Applications`) |
| Windows | doppio clic su `install.bat` (in `%LOCALAPPDATA%\Programs\ptscry-gui`, menu Start dell'utente) | `install.bat -System` (chiede i privilegi di amministratore; `Program Files`, menu Start di tutti) |

Disinstallare: `./install.sh --uninstall [--user|--system]`, `install.bat -Uninstall [-System]`, oppure da "App e funzionalita'" di Windows.
Le impostazioni dell'utente non vengono mai toccate. Su Linux `--prefix DIR` sceglie un'altra destinazione; su Windows
`-AddToPath` mette la cartella nel PATH. Il comando `ptscry` usato dal profilo "senza server" resta accanto a `ptscry-gui`, in
una cartella privata, e non sostituisce un `ptscry` che hai gia' nel PATH.

## Disconnessione e congelamento

Valgono per ogni sessione: locale senza server, locale con server e remota.

- **Disconnetti** stacca il tab dalla sessione senza chiuderlo: la sessione resta viva e `Invio` la riattacca. Chiudere il tab o la
  finestra stacca allo stesso modo.
- **Congela** ferma (SIGSTOP) tutti i processi della sessione dove si trovano; **Scongela** li fa ripartire (SIGCONT). Ci si puo'
  disconnettere e riconnettere mentre e' congelata: lo stato resta (`[II]` nel titolo, `[congelata]` in Sessioni). Un "Termina" la
  scongela prima di chiuderla. Alla ripartenza la shell puo' scrivere `Stopped` per il programma in primo piano: e' solo la sua notifica.
  Sui server serve ptscry 0.6.11 o piu' recente (`ptscry freeze|thaw NOME` si usa anche da riga di comando).

## 3. Uso

| Azione | Come |
|---|---|
| Nuova sessione | pulsante **+**, `Ctrl+Maiusc+T`, menu File |
| Riconoscere i tab | le sessioni ptscry hanno il prefisso **[PTS]** e una linea azzurra alla base, le connessioni SSH **[SSH]** e una linea rossa, i terminali di questo computer il nome della shell (`bash`, `zsh`, `pwsh`...; con la sessione persistente anche il suo nome) e una linea verde |
| Chiudere il tab | **x** (la sessione resta viva, si stacca soltanto; con piu' pannelli `Ctrl+Maiusc+W` chiude solo quello attivo) |
| Cambiare tab | clic, `Ctrl+PagGiu` / `Ctrl+PagSu`, `Ctrl+Tab` / `Ctrl+Maiusc+Tab` |
| Copiare / incollare | selezionare col mouse (copia da solo), `Ctrl+Maiusc+C` / `Ctrl+Maiusc+V`, `Ctrl+Ins` / `Maiusc+Ins`, click centrale, menu col tasto destro |
| Scorrere lo storico | rotella, `Maiusc+PagSu` / `PagGiu`, `Maiusc+Home` / `Fine` |
| Ingrandire / ridurre / ripristinare il carattere | `Ctrl` (anche con `Maiusc`) e `+` / `-` / `0`, `Ctrl`+rotella; funziona con qualunque tastiera (anche quella italiana) |
| Menu a tendina | barra dei menu in alto, visibile di default: **File**, **Tab**, **Sessione**, **Modifica**, **Vista**, **Finestra**, **Aiuto**, con le scorciatoie accanto (con la barra nascosta resta il pulsante **☰**, che li riunisce tutti) |
| Sessioni del server | menu, `Ctrl+Maiusc+S` |
| Workspace | menu, `Ctrl+Maiusc+O` |
| Variabili d'ambiente del tab | menu, `Ctrl+Maiusc+E` |
| Rinominare il tab | `Ctrl+Maiusc+R` |
| Disconnettersi dalla sessione (il tab resta, la sessione continua; Invio riattacca) | menu, `Ctrl+Maiusc+D` |
| Congelare / scongelare la sessione (ferma tutti i suoi processi, `[II]` nel titolo del tab) | menu, `Ctrl+Maiusc+F`, pulsante **Congela** in Sessioni |
| Impostazioni | menu |
| Ridurre a icona / ingrandire-ripristinare / chiudere la finestra | i tre pulsanti a destra della barra dei tab (su tutte le piattaforme, quando la finestra non ha la barra del titolo di sistema), oppure il menu; doppio clic sulla zona libera della barra = ingrandisci/ripristina, trascinarla = sposta la finestra |
| Schermo intero | menu |

Il distacco e' quello di ptscry: `Ctrl-B` poi `d` (o il prefisso di `client.conf`). Dopo un distacco o la fine della sessione il
tab mostra un messaggio: **Invio** riattacca, `Ctrl+Maiusc+W` chiude. Se la connessione cade il tab si ricollega da solo (fino a
20 tentativi) e riattacca la stessa sessione. Doppio clic seleziona una parola, triplo una riga, `Alt`+trascinamento un blocco
rettangolare. Con un programma che usa il mouse (vim, htop...) i clic vanno a lui; con **Maiusc** premuto si seleziona come al solito.

Tutti i campi di testo hanno il menu contestuale (tasto destro): Taglia, Copia, Incolla, Seleziona tutto.

### 3.1 Pannelli (dalla 0.8.0)

Un tab si divide in pannelli come in tmux o zellij; ogni pannello e' un terminale a se', con la sua sessione e la sua
connessione: i pannelli di un tab possono stare anche su server diversi (**Dividi con un'altra connessione…**).

| Azione | Come |
|---|---|
| Dividi a destra / sotto | `Alt+Maiusc++` / `Alt+Maiusc+-`, menu **Pannelli**, tasto destro nel terminale; il nuovo pannello apre una nuova sessione sulla stessa connessione, nella stessa cartella |
| Spostarsi | clic, `Alt`+frecce, `Alt+Maiusc+O` (successivo) |
| Ridimensionare | trascinare la linea di divisione, `Alt+Maiusc`+frecce |
| Ingrandire il pannello a tutto il tab e tornare | `Alt+Maiusc+Z` |
| Disposizioni pronte (affiancati, impilati, principale a sinistra/in alto, griglia) | `Alt+Maiusc+Spazio` |
| Input sincronizzato (cio' che si scrive va a tutti i pannelli) | `Alt+Maiusc+S`; bordi rossi e **INPUT SINCRONIZZATO** nella barra di stato; i pannelli in sola lettura sono esclusi; non viene mai ripristinato all'avvio |
| Chiudere un pannello | `Ctrl+Maiusc+W`: solo distacco, la sessione continua |
| Disposizioni sul server | `Alt+Maiusc+L`: salva i pannelli del tab sul server (`ptscry layout`) e li riapre, anche da un altro computer o dal client a riga di comando |
| Avvisi di attivita' / silenzio (30 s) | menu Pannelli, per il terminale attivo: un punto **●** sul tab e un messaggio quando quel terminale, non in vista, scrive (o tace) |

La disposizione dei pannelli si salva con il workspace. Un programma nella sessione puo' chiedere un pannello con
`ptscry split` (o una disposizione salvata con `ptscry layout apply NOME`): la richiesta viaggia nell'output (OSC 7799),
quindi la GUI la accetta solo dal pannello che ha la tastiera (mai da un altro pannello o tab, e senza mai spostare
la tastiera o portare un tab in primo piano), fa solo cio' che si potrebbe fare con un tasto (nuova sessione sulla
stessa connessione; disposizione dal proprio server, senza i pannelli di altre connessioni, al massimo una ogni 10 s),
al massimo una richiesta al secondo, mai nei primi secondi dopo un riattacco (il server la toglie anche dal replay) e
mai eseguendo comandi; al massimo 16 pannelli per tab. L'annuncio del client ptscry che ha cambiato sessione
(`Ctrl-B s` e simili) vale solo poco dopo che si e' premuto il prefisso in quel terminale.

La **barra di stato** in basso mostra connessione, sessione e pannello attivo, gli avvisi (input sincronizzato, zoom,
congelata, sola lettura) e l'ora; **Vista → Nascondi la barra di stato** la toglie.

## 4. Gestire le sessioni

**Sessioni** elenca le sessioni del server del profilo scelto: nome, dimensione, quanti client sono agganciati e la directory.
Per ognuna: **Apri** (apre un tab, o passa a quello gia' aperto), **Sola lettura** (`attach -r`: il tab e' marcato `[ro]`, si guarda
senza poter digitare, per affiancare un collega), **Termina** (chiede conferma con un secondo clic ed equivale a `ptscry kill`).
**Aggiorna** rilegge l'elenco. Si possono vedere e terminare solo le sessioni dell'utente collegato.

**Workspace.** Alla chiusura la GUI salva i tab aperti (profilo, sessione, directory, nome, variabili) e al successivo avvio li
riapre, se l'opzione "Riapri i tab dell'ultima volta" e' attiva. In piu' si possono salvare **workspace con nome**: "Salva i tab
attuali con il nome", poi **Apri** (i tab aperti ora si staccano, le loro sessioni continuano) o **Elimina**. Un workspace e' un
elenco di tab, non il contenuto delle sessioni: quello e' nel server.

**Rinomina tab** cambia soltanto l'etichetta nella GUI; il nome della sessione sul server resta lo stesso.

## 5. Directory e variabili d'ambiente

**Directory.** Il server comunica la directory di lavoro di ogni sessione (`ptscry list -j`, campo `cwd`, letta da `/proc`: solo Linux). La GUI
la aggiorna ogni 20 secondi e alla chiusura, e la salva col workspace. Quando un tab si riattacca e la sessione **non esiste piu'**
(server riavviato, sessione terminata, `exit`), la GUI ne crea una nuova e vi scrive ` cd -- 'directory'`.

**Variabili d'ambiente per sessione.** Ogni tab ha le proprie, impostabili nel dialogo del "+" (elenco scorrevole di righe
NOME/valore, "+ Aggiungi variabile", "x" per togliere) e modificabili poi con **Ambiente**. Nessuna variabile = ambiente standard.
Il nome segue le regole della shell (lettere, cifre, `_`, non iniziale cifra), il valore e' su una sola riga, al massimo 100 variabili.
Il valore e' preso alla lettera (spazi, apici e `$` compresi). **Salva** le tiene per quando la sessione verra' creata; **Salva e applica
ora** le scrive anche nella shell aperta: usalo al prompt, non dentro un programma.

**Comandi di avvio.** Nelle impostazioni, "Comandi da scrivere in ogni sessione creata di nuovo" (uno per riga: alias, `export`, ecc.).

Sia la directory sia le variabili e i comandi di avvio sono **scritti nella shell come se li digitassi** (con uno spazio iniziale, che
tiene la riga fuori dalla cronologia delle shell che lo prevedono): funzionano con le shell POSIX (bash, zsh, sh), non con fish o csh.
Valgono solo per le sessioni create dalla GUI; non toccano quelle che esistono gia'.

**Cosa non si puo' ripristinare.** I processi in esecuzione non tornano dopo un riavvio del server: tornano la sessione, la directory
e l'ambiente, non i programmi. Finche' il server resta acceso, invece, tutto continua a girare.

## 6. Impostazioni

Dimensione e nome del carattere (a spaziatura fissa; vuoto = Go Mono incluso; i simboli mancanti li prende dai font di sistema; **Scegli dall'elenco...** mostra i font a spaziatura fissa installati (OpenType/TrueType: i font Type 1 e bitmap non si possono usare), con filtro e **anteprima** disegnata con il file del font stesso, quindi uguale su Linux, Windows e macOS; il font scelto viene caricato dal suo file, non cercato per nome; lo stesso elenco e' in **Vista → Scegli il carattere del terminale...**, che applica la scelta subito a tutti i tab),
righe di scrollback (1000, 10000, 100000, illimitato = un milione), barra del titolo, profilo predefinito, comandi di avvio, riapertura dei tab.
Il file e' `~/.config/ptscry/gui.json` (Windows: `%AppData%\ptscry`, macOS: `~/Library/Application Support/ptscry`), permessi 0600.
**Non contiene password ne' codici TOTP.** Le impostazioni non valide vengono corrette alla lettura. Piu' finestre aperte insieme
condividono il file: vale quello dell'ultima che si chiude.

## 7. Wayland, X11 e window manager a tasselli

La GUI parla direttamente con Wayland (xdg-shell) e X11. Su Niri, Hyprland, Sway, i3 e simili rileva il window manager
(`NIRI_SOCKET`, `HYPRLAND_INSTANCE_SIGNATURE`, `SWAYSOCK`, `XDG_CURRENT_DESKTOP`...) e toglie la propria barra del titolo, che
il compositor gestisce da se': la finestra riempie la tassella e i tab, il menu e le righe/colonne seguono ogni ridimensionamento,
comunicato anche al server (`stty size`). Su Linux la barra del titolo si regola nelle impostazioni (automatica, sempre, mai; su Windows e macOS c'e' sempre quella di sistema e le due opzioni non compaiono); i pulsanti riduci/ingrandisci/chiudi nella barra dei tab ("solo senza barra del titolo", sempre, mai) pure, e si applicano subito.
L'identificativo dell'applicazione (`app_id`) e', di norma, il nome del file (`ptscry-gui`), utile per le regole di finestra.

La scala viene dal sistema: scala dello schermo Wayland, `Xft.dpi` su X11 e, su GNOME, `text-scaling-factor`. La dimensione del
carattere nelle impostazioni e' in punti logici e si moltiplica per quella scala; per cambiare solo il testo usa `Ctrl+Maiusc+` `+` / `-`.

## 8. Problemi comuni

| Sintomo | Causa e rimedio |
|---|---|
| "il front-end di questo computer non risponde su 127.0.0.1:2222" | il profilo "locale" richiede il front-end attivo (manuale 3.5): attivalo, oppure usa il profilo "senza server" |
| "il comando ptscry non e' stato trovato" | metti `ptscry` accanto a `ptscry-gui`, nel PATH, oppure indica il percorso nel profilo |
| "ssh: unable to authenticate" | chiave non autorizzata sul server o `ssh-agent` senza chiavi (`ssh-add`); controlla l'utente nel profilo |
| "HAS CHANGED" sulla chiave del server | il server ha cambiato chiave (o non e' lui): verifica l'impronta, poi togli la riga da `known_hosts` |
| tab con "!" o "x" | connessione persa o sessione terminata: Invio riattacca |
| un tab in secondo piano ha una dimensione piu' piccola sul server | si adegua appena lo si attiva |
| compilazione: `Package wayland-client was not found` | mancano i pacchetti della tabella del capitolo 1 |

## 9. Limiti noti

Su macOS la GUI non e' stata provata (cgo: va compilata su un Mac) e su Windows e' solo compilata, non eseguita. La clipboard e' provata
su X11; su Wayland usa le stesse chiamate di Gio ma non e' ancora stata verificata a mano. Provata su sway (Wayland nativo e a tasselli);
Niri e Hyprland non ancora. La directory delle sessioni si legge solo su Linux. Non c'e' ancora un tema scuro/chiaro scelto dalle
impostazioni (la GUI e' scura). Nel modo "senza server" non ci sono audit a prova di manomissione ne' autenticazione: la sicurezza e'
quella dell'account locale.


## Dimensione dell'interfaccia e DPI

I DPI dello schermo sono presi in considerazione: la GUI usa la scala del compositor su Wayland, `Xft.dpi` su X11, il DPI per
monitor su Windows e il fattore Retina su macOS. Sopra a questi, **Impostazioni → Dimensione dell'interfaccia** (60-300 %, 100 =
normale) o **Vista → Ingrandisci / Riduci l'interfaccia** regolano menu, tab e finestre di dialogo. Il carattere del terminale e'
separato (`Ctrl` `+` / `-` / `0`, `Ctrl`+rotella, o Impostazioni).

## Chiavi private

**File → Chiavi private** (o "Elenco delle chiavi…" nel dialogo Nuova sessione, che si apre sopra di esso e poi torna li') e'
un elenco ricercabile delle chiavi, con nome, tipo, impronta e "con passphrase". Sulla chiave scelta: **Usa questa chiave**
(dal dialogo Nuova sessione), **Copia la chiave pubblica** (da incollare sul server: `ptscry setup keys add UTENTE -`),
**Rinomina**, **Rimuovi** (conferma con un secondo clic). Sotto: **Importa da file…** e **Genera una nuova chiave…** (ed25519,
con passphrase facoltativa; viene salvata anche la `.pub`).

### Come era prima

**File → Chiavi private** gestisce le chiavi usate per i server remoti. **Importa una chiave** apre il selettore di file di
sistema (zenity o kdialog su Linux, il pannello nativo su macOS e Windows; senza selettore si scrive il percorso a mano): la chiave
viene controllata (deve essere privata, anche con passphrase) e copiata in `~/.config/ptscry/keys` con modo 0600; l'originale non
si tocca. Nel dialogo "Nuovo tab → Un altro server" **Sfoglia** importa e seleziona la chiave in un passo, e i pulsanti con il nome
delle chiavi gia' importate la scelgono. Una chiave con passphrase la chiede alla connessione.


## Gestione ptscry

**File -> Gestione ptscry** fa dalla finestra tutto quello che fa il comando `ptscry`, sul profilo scelto. Le voci che il profilo non
permette sono nascoste (su un server remoto, via SSH, ptscry ammette solo i comandi sulle sessioni).

| Gruppo | Voci | Profili |
|---|---|---|
| Sessioni e job | Elenca, Stato, Nuova staccata (`new -d`), Esegui job (`run -d`), Risultato del job (`wait`), Congela, Scongela, Termina / scarta | tutti |
| Host | Avvia l'host, Ferma l'host | Avvia: senza server. Ferma: tutti |
| Host | Configurazione (`config`), Verifica (`check`) | questo computer |
| Amministrazione | sessioni di tutti, termina la sessione di un utente, ferma l'host di un utente, Verifica audit (`audit-verify`), verifica di un utente (`check -u`), registra il TOTP (`totp-init`), sostituisci il monitor (`admin reexec`) | "locale", con root o `admin_group` |
| Servizio del monitor | Stato, Avvia, Ferma, Riavvia, Rileggi la config (`systemctl ... ptscry-monitor`; senza root usa `pkexec`) | "locale", Linux |

Il nome della sessione e il comando (parole separate da spazi, senza virgolette) si scrivono nei due campi in alto; l'utente appare
per i profili "locale". Le azioni distruttive chiedono "Sicuro?" al secondo clic. Il risultato compare nel riquadro in basso, da copiare.

## Prestazioni e componenti modificati

Con molto output (`cat` di un file grande, `seq 1 1000000`) l'emulatore e' il collo di bottiglia: lo scorrimento copiava ogni cella
di ogni riga a ogni riga stampata. La GUI usa percio' una copia patchata di `charmbracelet/ultraviolet` (`gui/third_party/ultraviolet`,
licenza originale inclusa; modifica: lo scorrimento sposta le righe invece di copiare le celle, 5 volte piu' veloce) e passa
l'output all'emulatore a pezzi da 4 KB, cosi' la finestra e i tasti restano reattivi anche durante un flusso continuo.

Anche il server ha ora una tolleranza: se un client e' momentaneamente piu' lento dell'output (un `cat` enorme), la shell aspetta fino a
5 secondi invece di scollegarlo subito con "client too slow"; un client fermo piu' a lungo viene comunque staccato.

**Tasti Ctrl.** Nel profilo "senza server" la GUI avvia il client con `PTSCRY_NO_PREFIX=1`: il prefisso di distacco (Ctrl-B, o quello di
`client.conf`) non intercetta nulla e tutti i tasti (Ctrl+A, Ctrl+B, Ctrl+E...) vanno alla shell. Il distacco e' `Ctrl+Maiusc+D`.
