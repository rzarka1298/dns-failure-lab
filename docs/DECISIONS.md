# Architecture decisions

## ADR 001: Go standard library for the first slice

**Decision:** Use one Go binary with no external dependencies. Its resolver sends real UDP DNS queries to an embedded controllable DNS server. The service also hosts the upstream, probe, metrics, and UI.

**Reason:** A single runnable artifact keeps the first experiment reproducible while exercising actual DNS packet exchange and HTTP dependency behavior. Later experiments can split the processes without changing the probe event contract.

## ADR 002: Evidence before explanation

**Decision:** Store each probe with trace ID, timestamp, scenario, DNS and HTTP spans, durations, outcome, and error class. The incident draft is deterministic and cites event IDs. It never asserts real-user impact.

**Reason:** A useful diagnosis must be auditable. An AI summarizer can be added only after its claims can be checked against the same evidence model.

## ADR 003: Synthetic, isolated faults

**Decision:** Fault controls only affect `api.lab.test` inside this lab. The wrong answer is a loopback address with no listener. Compose uses its internal DNS to locate the upstream container; the experiment never queries a production target.

**Reason:** Experiments should be repeatable and have a clear boundary.

## ADR 004: Deployment staging

**Decision:** Deliver a local Docker deployment first. AWS preview/production stacks and rollback workflow are source-controlled but require account setup, cost boundary, and live validation before deployment.

**Reason:** The environment has no existing repository, infrastructure, or account configuration to safely build against.

## ADR 005: Restrict the cloud fault injector

**Decision:** AWS load balancers accept HTTPS only from a narrow operator/runner CIDR. Preview and production use separate stacks and repositories. Production promotion uses the preview-tested image digest, and rollback names a known-good digest.

**Reason:** Fault controls have no application authentication, so network access is the current boundary. Immutable image identity makes release and recovery auditable.

## ADR 006: Keep AWS deployment as code for now

**Decision:** Prepare and validate the AWS infrastructure and pipeline as repository files. Do not create AWS resources yet.

**Reason:** The user chose code-only delivery for the AWS milestone on 2026-11-23. Account, repository, budget, and access details can be supplied when live deployment is desired.

## ADR 007: Hold labels out of diagnosis

**Decision:** Public probe and state JSON contain observable DNS/HTTP evidence and an evidence-derived diagnosis, not the injected scenario label. The benchmark stores injected labels in a separate file and joins them to observations only during scoring.

**Reason:** The diagnosis should be evaluated against facts it did not receive as input. The separate label file is an experiment boundary, not a security boundary.

## ADR 008: Persist local incident reviews

**Decision:** Create a draft at the onset of an observed alert and store its evidence snapshot, decision, reviewer name, and timestamps as atomic JSON files. Compose mounts a named volume for the records.

**Reason:** A reviewer can inspect and approve a draft after a workbench restart. Reviewer identity is self-asserted until authentication is added; approval does not publish externally.
