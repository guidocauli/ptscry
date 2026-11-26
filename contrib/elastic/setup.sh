#!/bin/sh
# ptscry - Guido Cauli <guido.cauli@gmail.com>
# Crea pipeline, ILM e template in Elasticsearch. Uso:
#   ES=https://elastic.example.lan:9200 ES_API_KEY=... ./setup.sh
# (la chiave serve con diritto manage_pipeline, manage_ilm, manage_index_templates)
set -eu
: "${ES:?set ES}" ; : "${ES_API_KEY:?set ES_API_KEY}"
here=$(dirname "$0")
put() { curl -fsS --cacert "${ES_CA:-/etc/pki/elastic-ca.pem}" -X PUT "$ES/$1" \
        -H "Authorization: ApiKey $ES_API_KEY" -H 'Content-Type: application/json' --data-binary "@$here/$2"; echo; }
put _ingest/pipeline/ptscry            ingest-pipeline.json
put _ilm/policy/ptscry-audit           ilm-policy.json
put _index_template/ptscry-audit       index-template.json
