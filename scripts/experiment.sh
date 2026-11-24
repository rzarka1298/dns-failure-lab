#!/bin/sh
set -eu
base="${LAB_URL:-http://localhost:8080}"
for scenario in healthy slow timeout wrong_answer bad_release healthy; do
  curl --fail --silent --show-error -X POST "$base/api/scenario" -H 'Content-Type: application/json' -d "{\"scenario\":\"$scenario\"}" >/dev/null
  curl --fail --silent --show-error -X POST "$base/api/probe" -H 'Content-Type: application/json' -d '{}'
  printf '\n'
done
