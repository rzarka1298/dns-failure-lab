# DNS failure lab plan

## Goal and scope

Demonstrate a DNS-dependent backend and the reliability workflow around it: controlled faults, detection, evidence correlation, a human-reviewed incident draft, safe release and recovery, and measured experiments. All traffic and impact measurements are synthetic. AWS is the selected cloud target.

## Architecture

```text
workbench + synthetic probe -> custom Go resolver -> lab UDP DNS -> A answer
                            -> upstream HTTP /data
                            -> event store -> metrics + alert + timeline + incident draft
```

The probe records DNS and HTTP durations and outcomes under one trace ID. The DNS server controls answer delay, dropped responses, and incorrect answers. The upstream provides a tiny service whose availability depends on resolution. Local `go run` uses one process; Compose separates DNS, upstream, and workbench into three containers. The workbench's in-process event store is ephemeral. Its JSON model is the contract for later durable storage and telemetry export.

## Milestones

1. **Local vertical slice (verified on this host):** Go service and three-container Docker deployment; healthy, slow, timeout, incorrect answer, and bad release scenarios; synthetic probes; structured logs, counters, trace spans, threshold alert, workbench timeline, and evidence-cited draft. Live UDP and Compose smoke tests pass.
2. **Faithful DNS experiments (verified on this host):** Compose separates DNS and upstream services; explicit TTL and cache behavior for stale answers and bounded labeled experiments work. A timeout-to-wrong-answer transition bug was found in the first live benchmark and fixed. Recorded experiment seeds remain.
3. **Preview and production on AWS (templates ready, not deployed):** isolated preview and production CloudFormation stacks, smoke and bounded benchmark gates, digest-pinned promotion, and rollback workflow are written. Add durable telemetry storage and OpenTelemetry export. Set a cost boundary before provisioning.
4. **Benchmark and validation (one measured local run):** hidden labels are separate from observations; binary alert precision/recall, diagnosis accuracy, citation coverage, detection latency, and recovery time are reported in `artifacts/2026-11-23-verified/benchmark-report.json`. Broader fault corpus and incident-draft quality review remain.

## Acceptance criteria

- A healthy probe performs DNS resolution before the upstream call and records both spans.
- Fault probes produce distinct DNS/HTTP outcomes and an alert with traceable evidence.
- The workbench shows an ordered timeline and a draft that cites stored event IDs.
- The first slice runs with `go run` and `docker compose`; tests exercise real UDP resolution.
- Later milestones must use separate preview and production configuration and provide a tested rollback path before claiming deployment readiness.

## Current limits

The current service uses three Compose containers (or one process under `go run`) and synthetic traffic. Staleness is simulated by an explicit 30-second resolver cache whose wrong record is compared with the corrected authoritative answer. Incident reviews persist in a Compose volume; probe event history remains memory-only. The AWS pipeline is source-only and has not been deployed. One local benchmark is measured, but no production availability or real-user impact claim follows from it.
