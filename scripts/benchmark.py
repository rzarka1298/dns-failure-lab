#!/usr/bin/env python3
"""Run labeled synthetic DNS experiments and report detector performance.

This uses only the lab's HTTP API and Python's standard library. It does not
measure real users or infer their impact.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


DEFAULT_SCENARIOS = [
    "healthy",
    "slow",
    "timeout",
    "wrong_answer",
    "stale_answer",
    "bad_release",
    "healthy",
]
KNOWN_SCENARIOS = set(DEFAULT_SCENARIOS)
OBSERVED_EVENT_KEYS = {"id", "trace_id", "at", "release", "target", "outcome", "total_ms", "spans", "diagnosis"}


def observable_event(event: dict[str, Any]) -> dict[str, Any]:
    """Allowlist public evidence; injected labels never enter the observation file."""
    return {key: event[key] for key in OBSERVED_EVENT_KEYS if key in event}


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def post_json(base_url: str, path: str, body: dict[str, Any], timeout: float) -> dict[str, Any]:
    request = urllib.request.Request(
        base_url.rstrip("/") + path,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        value = json.load(response)
    if not isinstance(value, dict):
        raise ValueError(f"{path} returned a non-object JSON response")
    return value


def probe(base_url: str, stage_start: float, timeout: float) -> dict[str, Any]:
    started_at = utc_now()
    try:
        event = observable_event(post_json(base_url, "/api/probe", {}, timeout))
        if not isinstance(event.get("outcome"), str) or not isinstance(event.get("id"), str):
            raise ValueError("probe response lacks event ID or outcome")
        return {
            "started_at": started_at,
            "observed_at": utc_now(),
            "elapsed_ms": round((time.perf_counter() - stage_start) * 1000, 3),
            "event": event,
        }
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        return {
            "started_at": started_at,
            "observed_at": utc_now(),
            "elapsed_ms": round((time.perf_counter() - stage_start) * 1000, 3),
            "client_error": str(exc),
        }


def run_experiment(
    base_url: str,
    scenarios: list[str],
    samples_per_stage: int,
    concurrency: int,
    timeout: float = 10.0,
) -> dict[str, Any]:
    if samples_per_stage < 1 or samples_per_stage > 1000:
        raise ValueError("samples per stage must be between 1 and 1000")
    if concurrency < 1 or concurrency > 32:
        raise ValueError("concurrency must be between 1 and 32")
    if not scenarios or any(s not in KNOWN_SCENARIOS for s in scenarios):
        raise ValueError("scenarios must be known lab scenario names")
    if scenarios[0] != "healthy" or scenarios[-1] != "healthy":
        raise ValueError("the first and last stages must be healthy")

    run: dict[str, Any] = {
        "schema_version": 1,
        "kind": "synthetic_dns_benchmark",
        "run_id": uuid.uuid4().hex,
        "started_at": utc_now(),
        "status": "complete",
        "errors": [],
        "configuration": {
            "base_url": base_url,
            "scenario_order": scenarios,
            "samples_per_stage": samples_per_stage,
            "max_concurrency": concurrency,
            "request_timeout_seconds": timeout,
        },
        "stages": [],
    }

    for index, scenario in enumerate(scenarios):
        try:
            applied = post_json(base_url, "/api/scenario", {"scenario": scenario}, timeout)
            if applied.get("scenario") != scenario:
                raise ValueError(f"lab did not apply {scenario!r}")
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            run["status"] = "incomplete"
            run["errors"].append({"stage_index": index, "operation": "apply_scenario", "error": str(exc)})
            break
        stage_start = time.perf_counter()
        stage: dict[str, Any] = {
            "index": index,
            "scenario": scenario,
            "expected_alert": scenario != "healthy",
            "phase": "recovery" if scenario == "healthy" and index > 0 else ("baseline" if index == 0 else "fault"),
            "applied_at": utc_now(),
            "samples": [],
        }
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(probe, base_url, stage_start, timeout) for _ in range(samples_per_stage)]
            for future in as_completed(futures):
                stage["samples"].append(future.result())
        stage["samples"].sort(key=lambda sample: sample["elapsed_ms"])
        stage["completed_at"] = utc_now()
        run["stages"].append(stage)
    if run["status"] != "complete":
        try:
            post_json(base_url, "/api/scenario", {"scenario": "healthy"}, timeout)
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            run["errors"].append({"operation": "restore_healthy", "error": str(exc)})
    run["completed_at"] = utc_now()
    return run


def safe_div(numerator: int, denominator: int) -> float | None:
    return round(numerator / denominator, 4) if denominator else None


def split_run(run: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    """Migrate a v1 in-memory or saved run into public evidence and private labels."""
    if run.get("schema_version") != 1 or run.get("kind") != "synthetic_dns_benchmark":
        raise ValueError("expected a schema version 1 benchmark run")
    run_id = run.get("run_id") or uuid.uuid4().hex
    observations = {
        "schema_version": 2, "kind": "synthetic_dns_observations", "run_id": run_id,
        "started_at": run["started_at"], "completed_at": run.get("completed_at"),
        "status": run.get("status", "complete"), "errors": run.get("errors", []),
        "configuration": {key: value for key, value in run.get("configuration", {}).items() if key != "scenario_order"},
        "stages": [],
    }
    labels = {
        "schema_version": 2, "kind": "synthetic_dns_hidden_labels", "run_id": run_id,
        "scenario_order": run.get("configuration", {}).get("scenario_order", [stage["scenario"] for stage in run["stages"]]),
        "stage_labels": [],
    }
    for stage in run["stages"]:
        labels["stage_labels"].append({key: stage[key] for key in ("index", "scenario", "phase", "expected_alert")})
        observations["stages"].append({
            "index": stage["index"], "applied_at": stage.get("applied_at"), "completed_at": stage.get("completed_at"),
            "samples": [{**{key: value for key, value in sample.items() if key != "event"},
                         **({"event": observable_event(sample["event"])} if "event" in sample else {})}
                        for sample in stage["samples"]],
        })
    return observations, labels


def join_run(observations: dict[str, Any], labels: dict[str, Any]) -> dict[str, Any]:
    """Join only inside the evaluator, after diagnoses have already been produced."""
    if observations.get("schema_version") != 2 or observations.get("kind") != "synthetic_dns_observations":
        raise ValueError("expected schema version 2 observations; use migrate for legacy runs")
    if labels.get("schema_version") != 2 or labels.get("kind") != "synthetic_dns_hidden_labels":
        raise ValueError("expected schema version 2 hidden labels")
    if not observations.get("run_id") or observations["run_id"] != labels.get("run_id"):
        raise ValueError("observation and label run IDs differ")
    stages = observations.get("stages", [])
    by_index = {label["index"]: label for label in labels.get("stage_labels", [])}
    if len(by_index) != len(labels.get("stage_labels", [])):
        raise ValueError("duplicate hidden stage index")
    joined = {"schema_version": 1, "kind": "synthetic_dns_benchmark",
              "run_id": observations["run_id"], "started_at": observations["started_at"],
              "status": observations.get("status", "unknown"), "configuration": observations["configuration"],
              "expected_stage_count": len(labels.get("scenario_order", [])), "stages": []}
    for stage in stages:
        label = by_index.get(stage["index"])
        if label is None:
            raise ValueError(f"missing hidden label for stage {stage['index']}")
        for sample in stage["samples"]:
            if "scenario" in sample.get("event", {}) or "injected_scenario" in sample.get("event", {}):
                raise ValueError("observation event leaks injected scenario label")
        joined["stages"].append({**stage, **label})
    return joined


def report(run: dict[str, Any]) -> dict[str, Any]:
    if run.get("schema_version") != 1 or run.get("kind") != "synthetic_dns_benchmark":
        raise ValueError("not a schema version 1 DNS benchmark run")
    stages = run.get("stages")
    if not isinstance(stages, list) or not stages:
        raise ValueError("benchmark run has no stages")
    tp = tn = fp = fn = client_errors = 0
    classification_correct = classification_incorrect = uncited_diagnoses = 0
    classification_matrix: dict[str, dict[str, int]] = {}
    misclassified_events: list[dict[str, str]] = []
    stage_reports: list[dict[str, Any]] = []
    for stage in stages:
        expected = stage["expected_alert"]
        scenario = stage["scenario"]
        if scenario not in KNOWN_SCENARIOS:
            raise ValueError(f"unknown scenario label: {scenario!r}")
        detections: list[float] = []
        recovery: list[float] = []
        outcome_counts: dict[str, int] = {}
        evidence_ids: list[str] = []
        stage_classification_correct = stage_classification_incorrect = 0
        for sample in stage["samples"]:
            event = sample.get("event")
            if event is None:
                client_errors += 1
                continue
            outcome = event["outcome"]
            outcome_counts[outcome] = outcome_counts.get(outcome, 0) + 1
            evidence_ids.append(event["id"])
            diagnosis = event.get("diagnosis")
            if not isinstance(diagnosis, dict):
                diagnosis = {}
            predicted_scenario = diagnosis.get("class", "unknown")
            if not isinstance(predicted_scenario, str) or predicted_scenario not in KNOWN_SCENARIOS:
                predicted_scenario = "unknown"
            citations = diagnosis.get("evidence", [])
            if not isinstance(citations, list):
                citations = []
            if event["id"] not in citations:
                uncited_diagnoses += 1
            row = classification_matrix.setdefault(scenario, {})
            row[predicted_scenario] = row.get(predicted_scenario, 0) + 1
            if predicted_scenario == scenario:
                classification_correct += 1
                stage_classification_correct += 1
            else:
                classification_incorrect += 1
                stage_classification_incorrect += 1
                misclassified_events.append(
                    {
                        "event_id": event["id"],
                        "expected_scenario": scenario,
                        "observed_outcome": outcome,
                        "predicted_diagnosis": predicted_scenario,
                        "diagnosis_evidence": citations,
                    }
                )
            detected = outcome != "ok"
            if expected and detected:
                tp += 1
                detections.append(sample["elapsed_ms"])
            elif expected:
                fn += 1
            elif detected:
                fp += 1
            else:
                tn += 1
                if stage["phase"] == "recovery":
                    recovery.append(sample["elapsed_ms"])
        stage_reports.append(
            {
                "index": stage["index"],
                "scenario": stage["scenario"],
                "phase": stage["phase"],
                "expected_alert": expected,
                "observed_outcomes": outcome_counts,
                "diagnosis": {
                    "correct": stage_classification_correct,
                    "incorrect": stage_classification_incorrect,
                    "accuracy": safe_div(stage_classification_correct, stage_classification_correct + stage_classification_incorrect),
                },
                "first_detection_ms": min(detections) if detections else None,
                "first_recovery_ms": min(recovery) if recovery else None,
                "evidence_event_ids": evidence_ids,
            }
        )
    detection_times = [s["first_detection_ms"] for s in stage_reports if s["first_detection_ms"] is not None]
    recovery_times = [s["first_recovery_ms"] for s in stage_reports if s["first_recovery_ms"] is not None]
    return {
        "kind": "synthetic_dns_benchmark_report",
        "schema_version": 2,
        "source_run_id": run.get("run_id"),
        "source_status": run.get("status", "unknown"),
        "completed_all_stages": run.get("status", "unknown") == "complete" and len(stages) == run.get("expected_stage_count", len(stages)),
        "source_started_at": run["started_at"],
        "configuration": run["configuration"],
        "confusion_matrix": {"true_positive": tp, "true_negative": tn, "false_positive": fp, "false_negative": fn},
        "precision": safe_div(tp, tp + fp),
        "recall": safe_div(tp, tp + fn),
        "false_positive_rate": safe_div(fp, fp + tn),
        "excluded_client_errors": client_errors,
        "diagnosis": {
            "correct": classification_correct,
            "incorrect": classification_incorrect,
            "accuracy": safe_div(classification_correct, classification_correct + classification_incorrect),
            "confusion_matrix": classification_matrix,
            "misclassified_events": misclassified_events,
            "uncited_events": uncited_diagnoses,
            "citation_coverage": safe_div(classification_correct + classification_incorrect - uncited_diagnoses, classification_correct + classification_incorrect),
        },
        "detection_time_ms": {
            "measured_stages": len(detection_times),
            "median": statistics.median(detection_times) if detection_times else None,
            "maximum": max(detection_times) if detection_times else None,
        },
        "recovery_time_ms": {
            "measured_stages": len(recovery_times),
            "median": statistics.median(recovery_times) if recovery_times else None,
            "maximum": max(recovery_times) if recovery_times else None,
        },
        "stages": stage_reports,
        "impact": "Synthetic probes only; real-user impact was not measured.",
    }


def write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.replace(temporary, path)


def default_labels_path(observations_path: Path) -> Path:
    return observations_path.with_name("benchmark-labels.json")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    run_parser = commands.add_parser("run", help="run a labeled synthetic experiment")
    run_parser.add_argument("--url", default="http://localhost:8080")
    run_parser.add_argument("--samples", type=int, default=8, help="probes per stage, 1–1000")
    run_parser.add_argument("--concurrency", type=int, default=4, help="maximum parallel probes, 1–32")
    run_parser.add_argument("--timeout", type=float, default=10.0, help="HTTP timeout in seconds")
    run_parser.add_argument("--scenarios", nargs="+", default=DEFAULT_SCENARIOS)
    run_parser.add_argument("--output", type=Path, default=Path("benchmark-run.json"))
    run_parser.add_argument("--labels-output", type=Path)
    report_parser = commands.add_parser("report", help="score a saved run")
    report_parser.add_argument("run", type=Path)
    report_parser.add_argument("--labels", type=Path)
    report_parser.add_argument("--output", type=Path, default=Path("benchmark-report.json"))
    migrate_parser = commands.add_parser("migrate", help="split a legacy v1 run into v2 observations and labels")
    migrate_parser.add_argument("legacy_run", type=Path)
    migrate_parser.add_argument("--output", type=Path, default=Path("benchmark-run.json"))
    migrate_parser.add_argument("--labels-output", type=Path)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        if args.command == "run":
            if not math.isfinite(args.timeout) or args.timeout <= 0:
                raise ValueError("timeout must be a positive, finite number")
            internal = run_experiment(args.url, args.scenarios, args.samples, args.concurrency, args.timeout)
            observations, labels = split_run(internal)
            labels_path = args.labels_output or default_labels_path(args.output)
            write_json(args.output, observations)
            write_json(labels_path, labels)
            print(f"Saved {len(observations['stages'])} observed stages to {args.output}; hidden labels to {labels_path}")
            return 0 if observations["status"] == "complete" else 1
        if args.command == "migrate":
            legacy = json.loads(args.legacy_run.read_text(encoding="utf-8"))
            observations, labels = split_run(legacy)
            labels_path = args.labels_output or default_labels_path(args.output)
            write_json(args.output, observations)
            write_json(labels_path, labels)
            print(f"Migrated v1 run to {args.output} and {labels_path}; absent diagnoses remain unknown")
            return 0
        observations = json.loads(args.run.read_text(encoding="utf-8"))
        labels_path = args.labels or default_labels_path(args.run)
        labels = json.loads(labels_path.read_text(encoding="utf-8"))
        result = report(join_run(observations, labels))
        write_json(args.output, result)
        print(json.dumps({k: result[k] for k in ("completed_all_stages", "confusion_matrix", "precision", "recall", "false_positive_rate", "diagnosis", "detection_time_ms", "recovery_time_ms", "excluded_client_errors")}, indent=2))
        print(f"Saved report to {args.output}")
        return 0
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError, urllib.error.URLError) as exc:
        print(f"benchmark: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
