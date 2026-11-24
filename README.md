# DNS failure lab

A local, reproducible DNS incident workbench. `go run` starts the DNS server, upstream HTTP service, probe, and incident view in one process. Docker Compose runs the upstream, DNS, and workbench as three separate containers. Every probe resolves `api.lab.test` through the lab DNS server before calling the upstream service.

## Run

```sh
go run ./cmd/lab
# open http://localhost:8080
```

Or run the container deployment:

```sh
docker compose up --build
```

Compose exposes only the workbench on `127.0.0.1:8080`; its DNS and upstream services stay on the internal Compose network.

The dashboard can set `healthy`, `slow`, `timeout`, `wrong_answer`, `stale_answer`, or `bad_release` and trigger a probe. `stale_answer` primes a wrong DNS record with a 30-second TTL, restores the authoritative answer, and shows the cached/authoritative mismatch. The rollback control restores `healthy` and verifies it with a probe. A background probe runs every 5 seconds. Scenarios apply to synthetic traffic only. Probe and state JSON expose observed outcomes and evidence-based diagnoses; the injected scenario labels stay in the experiment controller.

```sh
curl -s -X POST localhost:8080/api/scenario -H 'Content-Type: application/json' -d '{"scenario":"timeout"}'
curl -s -X POST localhost:8080/api/probe
curl -s localhost:8080/api/state
curl -s localhost:8080/metrics
curl -s -X POST localhost:8080/api/rollback
```

To run the ordered synthetic experiment and capture its JSON event records:

```sh
sh scripts/experiment.sh > experiment.jsonl
```

`LAB_ADDR` changes the dashboard address (default `:8080`); `LAB_PROBE_INTERVAL` changes probe frequency (default `5s`). A timeout adds about 700 ms to each probe. `/api/state` includes the recent event timeline, DNS and HTTP spans, alert, and deterministic incident draft. The draft cites event IDs and says nothing about real users. The experiment script captures per-probe latency, outcomes, and trace IDs for later comparison.

For a bounded labeled load experiment and a synthetic detection/recovery report:

```sh
python3 scripts/benchmark.py run --output benchmark-run.json
python3 scripts/benchmark.py report benchmark-run.json --output benchmark-report.json
```

The runner saves `benchmark-run.json` (observable evidence) and `benchmark-labels.json` (held-back experiment labels). The report uses both to score alert detection, diagnosis accuracy, and recovery time. See [benchmark definitions](docs/BENCHMARK.md) and the [measured local report](artifacts/2026-11-23-verified/benchmark-report.json).

Incident drafts are saved at alert onset. `GET /api/incidents` lists drafts and reviews; `POST /api/incidents/{id}/review` accepts `{"reviewer":"name","decision":"approve","note":"..."}` or `reject`. Compose persists these records in a named volume across workbench restarts. Reviewer names are self-asserted, and approval is a local record, not external publication.

AWS preview/production templates, release gates, and digest rollback instructions are in [infra/README.md](infra/README.md). No AWS deployment has been run yet.

## Verify

```sh
go test ./...
go test -race ./...
python3 -m unittest scripts/test_benchmark.py -v
```

See [project plan](docs/PLAN.md) and [decisions](docs/DECISIONS.md) for scope, milestones, and safety boundaries.
