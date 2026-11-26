# ptscry - Guido Cauli <guido.cauli@gmail.com>

# Integrazione con Wazuh ed Elastic/Kibana

L'audit di ptscry e' JSON, una riga per evento (vedi `docs/MANUALE.md`, cap. 6). Non serve
nessun parser dedicato: bastano un decoder JSON e qualche regola. Qui ci sono due percorsi,
che si possono usare insieme:

```
                    ┌─► rsyslog (TLS) ─► collettore ─► Wazuh manager / Logstash / Elastic
server con ptscry ──┤      (copia "sicura": e' quella che resiste a chi diventa root sul server)
 audit.log + syslog └─► agent (Wazuh agent / Filebeat) legge il file in tempo reale
```

- Il **percorso syslog** (`audit_syslog = yes`, `contrib/rsyslog.d`) e' quello che conta per l'integrita'.
- L'**agent sul file** da' analisi in tempo reale, campi gia' pronti e, con Wazuh, il controllo di integrita'
  dei file di configurazione. Chi diventa root potrebbe fermare l'agent, ma l'evento che lo dimostra
  (`monitor_*`, assenza di eventi) e il percorso syslog restano.

Tutto quanto segue e' sintatticamente verificato (XML/JSON/sh/python), ma non l'ho potuto provare contro un
Wazuh o un Elasticsearch reali: **fai un giro di prova** (sezione 3) prima di fidarti degli allarmi.

## 1. Wazuh

File in `contrib/wazuh/`:

| File | Dove va | A cosa serve |
|---|---|---|
| `agent-ossec.conf.snippet` | `ossec.conf` dell'agent (o `agent.conf` di gruppo) | legge `/var/log/ptscry/audit.log` come JSON; FIM in tempo reale su `/etc/ptscry`, binario, unit systemd, `sshd_config.d` |
| `ptscry_rules.xml` | manager, `/var/ossec/etc/rules/` | regole 100100-100120 |
| `ptscry_decoders.xml` | manager, `/var/ossec/etc/decoders/` | **solo** se l'audit arriva via syslog invece che dall'agent |

Passi:
1. Sull'agent: inserisci lo snippet, `systemctl restart wazuh-agent`.
2. Sul manager: copia le regole, `chown wazuh:wazuh`, `/var/ossec/bin/wazuh-analysisd -t`, `systemctl restart wazuh-manager`.
3. Prova con `/var/ossec/bin/wazuh-logtest`: incolla una riga di audit (es. un `access_denied`) e controlla che scatti la regola 100101.

Regole principali:

| ID | Livello | Scatta su |
|---|---|---|
| 100101 | 8 | `access_denied` |
| 100102 | 10 | 5 rifiuti dello stesso utente in 120 s |
| 100103 | 4 | richiesta non passata da sshd (rumoroso se si usa root in locale) |
| 100104 / 100105 | 8 / 6 | `rate_limited` / `limit_reached` |
| 100106 | 10 | `host_flood`, `host_event_unknown`, `host_event_invalid` |
| 100112 | 8 | `launch_failed` (HBAC/PAM ha rifiutato la shell) |
| 100113 | 8 | `fe_denied`, `fe_totp_failed` (login SSH rifiutato) |
| 100114 | 12 | `fe_totp_locked` (5 codici sbagliati) |
| 100115 | 6 | `fe_auth_failures` (riepilogo al minuto dei tentativi falliti) |
| 100116 | 10 | `frontend_exit`, `frontend_config_error`, `fe_keys_error` |
| 100121 | 3 | `fe_login` (informativa) |
| 100107 / 100108 | 8 | `admin_kill`/`admin_stop` / `policy_revoked` |
| 100109 | 12 | `record_error`, `audit_probe`, `audit_events_lost` (disco o scrittura in difficolta') |
| 100110 | 10 | `config_reload_failed` |
| 100122 | 5 | `admin_reexec`, `monitor_reexec`, `monitor_resumed` (aggiornamento del monitor sul posto: correlare con la manutenzione) |
| 100123 | 12 | `monitor_reexec_failed`, `monitor_resume_failed` (aggiornamento fallito: verificare sessioni e audit) |
| 100124 | 3 | `job_collect`, `job_discard` (ritiro o scarto di un job staccato; informativa) |
| 100111 | 7 | `monitor_start`, `monitor_shutdown` (un riavvio fa perdere le sessioni) |

Per far notificare per mail/Slack/webhook usa le integrazioni standard di Wazuh con `<level>` o `<rule_id>`.
Il FIM segnala da solo ogni modifica a binario, configurazione, unit e sshd: riconciliala con le tue finestre di
manutenzione.

## 2. Elastic / Kibana

File in `contrib/elastic/`:

| File | A cosa serve |
|---|---|
| `ingest-pipeline.json` | estrae il JSON da `message` (file **o** syslog), imposta `@timestamp` da `ts` e i campi ECS (`event.action`, `user.name`, `user.id`, `event.outcome`) |
| `index-template.json` | mapping tipizzato (`ptscry.seq`, `uid` numerici, `via_sshd` booleano, il resto `keyword`) e pipeline di default |
| `ilm-policy.json` | indici mensili, cancellati dopo 730 giorni (allinea al periodo di conservazione che scegli) |
| `filebeat.yml` | estratto di configurazione di Filebeat sul server con ptscry |
| `setup.sh` | crea pipeline, ILM e template (`ES=… ES_API_KEY=… ./setup.sh`) |
| `rebuild-audit.py` | ricostruisce il file di audit dalla copia in Elastic, per verificarlo (sezione 4) |

Passi:
1. `./setup.sh` (una volta).
2. Filebeat sul server: configurazione di `filebeat.yml`, chiave API **di sola scrittura** su `ptscry-audit-*`.
3. Se usi anche il percorso syslog, fai arrivare i messaggi in Elastic (Logstash o input syslog di Filebeat/Elastic Agent) verso lo
   stesso indice/pipeline: il campo `message` contiene il prefisso syslog, la pipeline lo ignora.
4. In Kibana crea una data view `ptscry-audit-*` con campo ora `@timestamp`.

### Query KQL utili (Discover)
```
ptscry.event : "access_denied"
ptscry.event : "request" and ptscry.via_sshd : false
ptscry.user : "mario" and not ptscry.event : "request"
ptscry.req : 69
ptscry.event : ("admin_kill" or "admin_stop" or "policy_revoked")
ptscry.event : ("record_error" or "audit_probe" or "audit_events_lost" or "config_reload_failed")
ptscry.event : "session_end" and ptscry.duration_s > 28800
ptscry.event : ("fe_denied" or "fe_totp_failed" or "fe_totp_locked")
ptscry.event : "fe_login" and ptscry.remote : 203.0.113.*
```
Colonne consigliate: `@timestamp`, `ptscry.user`, `ptscry.event`, `ptscry.session`, `ptscry.reason`, `ptscry.req`, `ptscry.host`.

### Regole di allerta (Kibana → Rules, tipo "Custom threshold" o "Elasticsearch query"; oppure Security → Detection rules)
| Regola | Condizione |
|---|---|
| Accessi negati ripetuti | `ptscry.event : "access_denied"` conteggio ≥ 5 in 5 min, raggruppato per `ptscry.user` |
| Audit in difficolta' | `ptscry.event : ("record_error" or "audit_probe" or "audit_events_lost")` conteggio ≥ 1 |
| Configurazione toccata | `ptscry.event : "config_reload_failed"` conteggio ≥ 1 |
| Riavvio del monitor | `ptscry.event : ("monitor_start" or "monitor_shutdown")` conteggio ≥ 1 |
| Azioni sull'accesso altrui | `ptscry.event : ("admin_kill" or "admin_stop" or "policy_revoked")` conteggio ≥ 1 |
| Abuso | `ptscry.event : ("rate_limited" or "host_flood")` conteggio ≥ 1 |
| Front-end SSH | `ptscry.event : "fe_totp_locked"` ≥ 1; `ptscry.event : "fe_auth_failures"` con `ptscry.count` alto; `ptscry.event : ("frontend_exit" or "frontend_config_error")` ≥ 1 |
| Monitor | `ptscry.event : ("monitor_reexec_failed" or "monitor_resume_failed")` ≥ 1 |
| Silenzio | nessun documento da `ptscry.host : "X"` per 60 min (in orario di servizio): il monitor o l'inoltro e' fermo |

### Dashboard
Non fornisco un dashboard importabile (scritto a mano sarebbe fragile tra versioni di Kibana). In Lens bastano
quattro pannelli: eventi per `ptscry.event` nel tempo; top 10 `ptscry.user` per sessioni create (`session_new`);
tabella degli `access_denied` con `ptscry.reason`; durata delle sessioni (`session_end`, `ptscry.duration_s`).

## 3. Prova end-to-end
1. Genera eventi: `ptscry new -d -n prova`, `ptscry kill prova`, e da un utente fuori da `access_group` una richiesta (verra' negata).
2. Wazuh: `wazuh-logtest` con le righe, poi controlla la dashboard "Security events" per le regole 100101 e 100120.
3. Elastic: in Discover cerca `ptscry.event : "access_denied"`; controlla che `@timestamp` coincida con `ptscry.ts` e che
   `ptscry.uid` sia numerico.
4. Spegni per un momento il collettore e verifica che i record si accodino (rsyslog `queue.filename`) e arrivino alla riapertura.

## 4. Verifica dell'integrita' dalla copia remota
Il campo `message` conserva la riga originale, identica a quella del file locale:

```
ES=https://elastic.example.lan:9200 ES_API_KEY=... ES_CA=/etc/pki/elastic-ca.pem \
  ./rebuild-audit.py adm01 > /root/audit-remoto.log
ptscry audit-verify /root/audit-remoto.log        # la copia remota e' una catena valida?
cmp /root/audit-remoto.log /var/log/ptscry/audit.log   # (stesso intervallo) il locale e' uguale?
```
Una catena valida in remoto che differisce dal locale significa che il **locale e' stato riscritto**. Se `rebuild-audit.py`
parte da una `seq` diversa dal file locale (rotazioni), confronta gli stessi intervalli. Programmalo (cron/timer, almeno
giornaliero) e fai scattare un allarme se fallisce.

Attenzione: gli utenti di Elastic/Wazuh che possono modificare o cancellare gli indici possono invalidare questa prova.
Limita i diritti (sola scrittura per Filebeat, sola lettura per gli analisti) e usa snapshot o archiviazione non riscrivibile.
