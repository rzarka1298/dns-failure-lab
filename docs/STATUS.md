# Project status

Updated 2026-11-25. This file is a handoff for future Codex sessions.

## Built

- Go lab service with real UDP DNS exchange, DNS-dependent HTTP probe, controlled healthy/slow/timeout/wrong/stale/bad-release scenarios, structured events, metrics, timeline, and evidence-derived diagnosis. Public probe/state JSON omits injected scenario labels.
- Three-container Docker Compose deployment (upstream, DNS, workbench) and hosted CI end-to-end job. The service has now run locally on this host.
- Bounded synthetic benchmark with separate observation and held-back label artifacts. It scores alert detection, evidence-derived diagnosis, citation coverage, detection time, and recovery time.
- File-backed incident drafts at alert onset, observable evidence snapshots, local approve/reject API and UI, and a Compose volume that preserves reviews across workbench restarts. Reviewer names are self-asserted.
- AWS CloudFormation templates and manual release/rollback workflows for isolated preview and production ECS stacks. Preview smoke and benchmark gates precede digest-pinned promotion. The user chose code-only AWS delivery for now; no AWS resources have been created.

## Verified here

- `git init -b main` succeeded. This folder is now a Git repository.
- `LAB_REQUIRE_NETWORK_TEST=1 GOCACHE=/private/tmp/dns-lab-go-cache go test -race -v ./...` passed, including live UDP healthy, slow, timeout, wrong-answer, stale-answer, bad-release, rollback, and incident-onset tests. `go vet ./...` and build passed.
- `docker compose up --build --detach --wait` brought all three containers to healthy. A live smoke probe resolved the upstream container address and received HTTP 200. The first benchmark attempt failed after timeout-to-wrong-answer transition; its state and logs are in `artifacts/2026-11-23-baseline/`. Resetting the Go resolver on scenario transitions fixed the contamination. A direct timeout → wrong answer → stale answer → recovery check passed and is saved in `artifacts/2026-11-23-transition-check/`.
- `python3 scripts/benchmark.py run --url http://127.0.0.1:8080 --samples 8 --concurrency 4 --output artifacts/2026-11-23-verified/benchmark-run.json` completed all seven stages. `python3 scripts/benchmark.py report artifacts/2026-11-23-verified/benchmark-run.json --output artifacts/2026-11-23-verified/benchmark-report.json` scored 56 returned probes: 40 TP, 16 TN, 0 FP, 0 FN; precision/recall 1.0; diagnosis 56/56 correct against held-back labels; citation coverage 1.0. Median stage detection time 39.214 ms (maximum 733.404 ms); observed recovery time 47.99 ms. These are one synthetic local run, not production reliability estimates.
- A synthetic incident was approved through the API, the workbench container restarted, and the approved record with its evidence snapshot remained available. The before/after records are in `artifacts/2026-11-23-verified/`. Python scorer tests and UI JavaScript syntax check passed.
- The repository is public at `https://github.com/rzarka1298/dns-failure-lab`. The first pushed GitHub Actions run (`45684635777`) passed the Go and hosted Compose end-to-end jobs, including the smoke and benchmark gates.

## Waiting on external environment

- AWS remains code-only by user choice. A live deployment would need an account and cost boundary, Route 53 zone and ACM certificates, OIDC roles, protected GitHub environments, and a deploy runner/operator with a narrow static egress CIDR. See `infra/README.md`. No AWS resources were created, and the manual release and rollback workflows have not been run.

## Remaining engineering work

- Persist telemetry and export standard traces/metrics; probe event history is memory-only and resets on task replacement. Incident reviews persist locally through the Compose volume, but the AWS task template has no durable incident volume.
- Evaluate diagnosis and incident-draft quality against a broader held-back fault corpus; the current perfect score covers only six configured scenarios in one synthetic local run. Reviewer identity is self-asserted and there is no authentication or external publication.
- Rehearse a production image rollback and document observed recovery time. The in-process `/api/rollback` only resets fault controls; it is distinct from the AWS image rollback workflow.
