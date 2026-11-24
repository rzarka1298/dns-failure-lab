# Synthetic DNS benchmark

The benchmark applies known synthetic scenarios, records probe observations, and scores the workbench's **evidence-based diagnosis** only after the run. It does not measure real-user impact or prove production reliability.

## Run and artifacts

```sh
python3 scripts/benchmark.py run --url http://localhost:8080 --samples 8 --concurrency 4 --output benchmark-run.json
python3 scripts/benchmark.py report benchmark-run.json --output benchmark-report.json
```

The runner also writes `benchmark-labels.json` beside the run file. `benchmark-run.json` contains public probe observations and no injected scenario labels; `benchmark-labels.json` contains the applied scenario for each stage and its expected alert. The scorer joins them by a shared run ID. The two files are separate artifacts, not an access-control boundary. Keep labels away from any code that generates diagnoses. The server's `event.diagnosis.class` is generated before the scorer reads labels.

The first measured local run is saved under `artifacts/2026-11-23-verified/` with its observations, held-back labels, report, smoke probe, metrics, and Compose logs. It measured 56 synthetic probes and is one run of this fixed corpus, not a reliability estimate for other environments.

The default sequence is `healthy`, `slow`, `timeout`, `wrong_answer`, `stale_answer`, `bad_release`, `healthy`. Each stage starts after the control API acknowledges the change. The runner allows 1–32 concurrent probes and 1–1,000 samples per stage; defaults are 4 and 8. A failed stage application writes an **incomplete** observations artifact with the error and exits nonzero, preserving evidence from earlier stages. It attempts to restore `healthy` after failure. A missing detection or recovery stays `null`.

Use `--labels-output` with `run` and `--labels` with `report` to choose a different private label path. Migrate a historical schema v1 run with:

```sh
python3 scripts/benchmark.py migrate old-run.json --output benchmark-run.json
```

Migration strips injected labels from saved observations. Historical events without a diagnosis remain `unknown`; migration does not invent one.

## Scoring

| Metric | Definition |
| --- | --- |
| Positive label | An injected fault stage (`scenario != healthy`) in the private labels file. |
| Predicted alert | A probe's observed `outcome != ok`. |
| Precision | True positives / (true positives + false positives). |
| Recall | True positives / (true positives + false negatives). |
| False-positive rate | False positives / all healthy probe events. |
| Diagnosis accuracy | `event.diagnosis.class` matching the hidden scenario / returned probe events. |
| Citation coverage | Diagnoses whose `evidence` includes their event ID / returned probe events. |
| Detection time | Client-observed milliseconds from successful fault application to the first completed non-`ok` probe. |
| Recovery time | Client-observed milliseconds from successful healthy reapplication to the first completed `ok` probe. |

The report includes an alert confusion matrix, a diagnosis confusion matrix, event IDs and evidence lists for every misdiagnosis, and per-stage detection/recovery times. Client request errors are excluded from accuracy denominators and counted separately. Unknown diagnosis classes map to `unknown`; a stage with no returned events has `null` diagnosis accuracy. Classification accuracy measures this lab's fixed fault corpus, not independent root-cause inference. A wrong DNS answer may look like a generic connection failure without authoritative comparison, so ambiguous evidence should produce `unknown` rather than a fabricated cause.

## Validation and limits

```sh
python3 -m unittest scripts/test_benchmark.py -v
```

The scorer tests verify that labels are absent from observations, mismatched run IDs are rejected, and a correct transport outcome cannot substitute for a diagnosis. Run artifacts should be retained with the report. The 30-second stale cache window can expire during large or slow runs; interpret those samples as recovery or increase sample scheduling discipline. All measurements are synthetic.
