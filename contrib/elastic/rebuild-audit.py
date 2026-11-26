#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com>
# Ricostruisce il file di audit di un host dalla copia in Elasticsearch, cosi' si puo'
# verificarlo con `ptscry audit-verify` e confrontarlo con quello locale (se differiscono,
# il file locale e' stato toccato). Usa il campo `message`, che conserva la riga ORIGINALE.
#
#   ES=https://elastic.example.lan:9200 ES_API_KEY=... ES_CA=/etc/pki/elastic-ca.pem \
#   ./rebuild-audit.py HOSTNAME [seq_da] > audit-remoto.log
#   ptscry audit-verify audit-remoto.log
#   cmp audit-remoto.log /var/log/ptscry/audit.log        # stesso intervallo di record
#
# Solo libreria standard. Nota: richiede la chiave con diritto di lettura su ptscry-audit-*.
import json, os, re, ssl, sys, urllib.request

es, key = os.environ["ES"].rstrip("/"), os.environ["ES_API_KEY"]
host = sys.argv[1]
seq_from = int(sys.argv[2]) if len(sys.argv) > 2 else 0
ctx = ssl.create_default_context(cafile=os.environ.get("ES_CA"))

def search(body):
    req = urllib.request.Request(f"{es}/ptscry-audit-*/_search", json.dumps(body).encode(),
                                 {"Authorization": f"ApiKey {key}", "Content-Type": "application/json"})
    with urllib.request.urlopen(req, context=ctx) as r:
        return json.load(r)["hits"]["hits"]

after = None
while True:
    body = {"size": 5000, "sort": [{"ptscry.seq": "asc"}], "_source": ["message", "ptscry.seq"],
            "query": {"bool": {"filter": [{"term": {"ptscry.host": host}},
                                          {"range": {"ptscry.seq": {"gte": seq_from}}}]}}}
    if after:
        body["search_after"] = after
    hits = search(body)
    if not hits:
        break
    for h in hits:
        m = re.search(r"\{.*\}", h["_source"]["message"])   # toglie l'eventuale prefisso syslog
        print(m.group(0) if m else "")
    after = hits[-1]["sort"]
