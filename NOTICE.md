# ptscry - Guido Cauli <guido.cauli@gmail.com>

ptscry, ptscry-gui e ptscry-tui  
Copyright (C) 2026 Guido Cauli <guido.cauli@gmail.com>

## Licenza

Questo programma e' software libero: puoi ridistribuirlo e/o modificarlo secondo i termini della
**GNU Affero General Public License** pubblicata dalla Free Software Foundation, versione 3 o (a tua
scelta) qualsiasi versione successiva (`AGPL-3.0-or-later`). Il testo completo e' nel file
[LICENSE](LICENSE). Chi offre una versione modificata come servizio in rete (per esempio un server
ptscry raggiungibile dagli utenti) deve renderne disponibile il sorgente (sezione 13).
Il sorgente di questa versione: <https://github.com/guidocauli/ptscry>.

## Esclusione di responsabilita'

Il software e' fornito **"cosi' com'e'"**, senza garanzie di alcun tipo, esplicite o implicite,
comprese quelle di commerciabilita', di idoneita' a uno scopo particolare e di assenza di violazioni.
In nessun caso l'autore e' responsabile di danni diretti, indiretti, incidentali, speciali o
consequenziali (compresi perdita o alterazione di dati, interruzioni di servizio, violazioni della
sicurezza o accessi non autorizzati) derivanti dall'uso o dall'impossibilita' di usare il software,
anche se avvisato della possibilita' di tali danni.

Installazione, configurazione, uso in produzione e verifica della sicurezza e della conformita'
(comprese ISO/IEC 27001, la normativa sulla protezione dei dati e le regole dell'organizzazione)
restano a carico di chi installa e gestisce il software. I documenti in `docs/` (anche la
mappatura ISO 27001) sono indicazioni, non una certificazione. Si vedano anche le sezioni 15 e 16
della licenza.

## Disclaimer (English)

This program is distributed in the hope that it will be useful, but **WITHOUT ANY WARRANTY**;
without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. In no
event shall the author be liable for any damages (including loss of data, service interruptions,
security breaches or unauthorized access) arising from the use of or inability to use the software.
Installing, configuring, operating it and verifying its security and compliance are the
responsibility of whoever deploys it. See sections 15 and 16 of the GNU AGPL.

## Componenti di terzi

Inclusi nel repository con le loro licenze (compatibili con la AGPL):

| Componente | Dove | Licenza |
|---|---|---|
| golang.org/x/crypto (patch in `third_party/PATCHES.md`) | `third_party/crypto` | BSD-3-Clause |
| golang.org/x/sys | `third_party/sys` | BSD-3-Clause |
| Gio (gioui.org, con patch) | `gui/third_party/gio` | MIT / Unlicense |
| charmbracelet/ultraviolet (con patch) | `gui/third_party/ultraviolet` | MIT |
| Icone Material Design (golang.org/x/exp/shiny/materialdesign/icons) | modulo Go | Apache-2.0 |

Moduli Go scaricati alla compilazione: charmbracelet/x (MIT), creack/pty (MIT), go-text (BSD/MIT),
e i rispettivi indicati in `go.sum` di ciascun modulo.
