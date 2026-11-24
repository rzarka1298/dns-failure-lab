#!/bin/sh
set -eu
url="${1:?usage: smoke.sh https://host}"
case "$url" in https://*) ;; *) echo "HTTPS URL required" >&2; exit 2;; esac
curl --fail --silent --show-error --max-time 15 "$url/api/state" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert "events" in d and "scenario" in d'
curl --fail --silent --show-error --max-time 15 -X POST "$url/api/scenario" -H 'Content-Type: application/json' -d '{"scenario":"healthy"}' >/dev/null
curl --fail --silent --show-error --max-time 15 -X POST "$url/api/probe" -H 'Content-Type: application/json' -d '{}' | python3 -c 'import json,sys; e=json.load(sys.stdin); assert e["outcome"] == "ok", e; assert [s["name"] for s in e["spans"]] == ["dns.resolve", "http.upstream"], e; print("smoke probe:", e["id"], e["outcome"])'
