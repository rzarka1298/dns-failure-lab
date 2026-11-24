import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import benchmark


def sample(event_id, outcome, elapsed, diagnosis=None):
    event = {"id": event_id, "outcome": outcome}
    if diagnosis is not None:
        event["diagnosis"] = {"class": diagnosis, "evidence": [event_id]}
    return {"elapsed_ms": elapsed, "event": event}


class BenchmarkReportTests(unittest.TestCase):
    def test_accuracy_detection_and_recovery(self):
        run = {
            "schema_version": 1,
            "kind": "synthetic_dns_benchmark",
            "started_at": "2026-01-01T00:00:00Z",
            "configuration": {"samples_per_stage": 2},
            "stages": [
                {"index": 0, "scenario": "healthy", "phase": "baseline", "expected_alert": False, "samples": [sample("p1", "ok", 10, "healthy"), sample("p2", "slow", 20, "slow")]},
                {"index": 1, "scenario": "timeout", "phase": "fault", "expected_alert": True, "samples": [sample("p3", "dns_timeout", 850, "timeout"), sample("p4", "ok", 900, "healthy")]},
                {"index": 2, "scenario": "healthy", "phase": "recovery", "expected_alert": False, "samples": [sample("p5", "ok", 15, "healthy"), sample("p6", "ok", 30, "healthy")]},
            ],
        }
        report = benchmark.report(run)
        self.assertEqual(report["confusion_matrix"], {"true_positive": 1, "true_negative": 3, "false_positive": 1, "false_negative": 1})
        self.assertEqual(report["precision"], 0.5)
        self.assertEqual(report["recall"], 0.5)
        self.assertEqual(report["false_positive_rate"], 0.25)
        self.assertEqual(report["detection_time_ms"]["median"], 850)
        self.assertEqual(report["recovery_time_ms"]["median"], 15)
        self.assertEqual(report["stages"][1]["evidence_event_ids"], ["p3", "p4"])
        self.assertEqual(report["diagnosis"]["accuracy"], 0.6667)
        self.assertEqual(report["diagnosis"]["confusion_matrix"], {
            "healthy": {"healthy": 3, "slow": 1},
            "timeout": {"timeout": 1, "healthy": 1},
        })
        self.assertEqual(
            [item["event_id"] for item in report["diagnosis"]["misclassified_events"]],
            ["p2", "p4"],
        )
        self.assertEqual(report["diagnosis"]["citation_coverage"], 1)
        self.assertIn("real-user impact was not measured", report["impact"])

    def test_unmeasured_stays_null_and_client_error_excluded(self):
        run = {
            "schema_version": 1,
            "kind": "synthetic_dns_benchmark",
            "started_at": "2026-01-01T00:00:00Z",
            "configuration": {},
            "stages": [{"index": 0, "scenario": "timeout", "phase": "fault", "expected_alert": True, "samples": [{"elapsed_ms": 100, "client_error": "connection refused"}, sample("p1", "ok", 200)]}],
        }
        report = benchmark.report(run)
        self.assertEqual(report["excluded_client_errors"], 1)
        self.assertIsNone(report["precision"])
        self.assertEqual(report["recall"], 0)
        self.assertIsNone(report["detection_time_ms"]["median"])
        self.assertIsNone(report["recovery_time_ms"]["median"])
        self.assertEqual(report["diagnosis"]["accuracy"], 0)

    def test_classification_unmeasured_and_unknown_signature(self):
        run = {
            "schema_version": 1,
            "kind": "synthetic_dns_benchmark",
            "started_at": "2026-01-01T00:00:00Z",
            "configuration": {},
            "stages": [{"index": 0, "scenario": "wrong_answer", "phase": "fault", "expected_alert": True, "samples": [{"elapsed_ms": 100, "client_error": "connection refused"}]},
                       {"index": 1, "scenario": "stale_answer", "phase": "fault", "expected_alert": True, "samples": [sample("p1", "upstream_error", 200)]}],
        }
        result = benchmark.report(run)
        self.assertIsNone(result["stages"][0]["diagnosis"]["accuracy"])
        self.assertEqual(result["diagnosis"]["accuracy"], 0)
        self.assertEqual(result["diagnosis"]["confusion_matrix"]["stale_answer"], {"unknown": 1})
        self.assertEqual(result["diagnosis"]["misclassified_events"][0]["event_id"], "p1")
        self.assertEqual(result["diagnosis"]["misclassified_events"][0]["predicted_diagnosis"], "unknown")

    def test_bounds_and_atomic_output(self):
        with self.assertRaises(ValueError):
            benchmark.run_experiment("http://localhost:8080", ["healthy"], 1001, 1)
        with self.assertRaises(ValueError):
            benchmark.run_experiment("http://localhost:8080", ["healthy"], 1, 33)
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "nested" / "run.json"
            benchmark.write_json(target, {"a": 1})
            self.assertEqual(json.loads(target.read_text()), {"a": 1})
            self.assertFalse(target.with_name(target.name + ".tmp").exists())

    def test_hidden_labels_are_separate_from_observations(self):
        legacy = {
            "schema_version": 1, "kind": "synthetic_dns_benchmark", "run_id": "run-1",
            "started_at": "2026-01-01T00:00:00Z", "status": "complete",
            "configuration": {"scenario_order": ["timeout"], "samples_per_stage": 1},
            "stages": [{"index": 0, "scenario": "timeout", "phase": "fault", "expected_alert": True,
                        "samples": [{"elapsed_ms": 800, "event": {"id": "p1", "scenario": "timeout",
                                      "outcome": "dns_timeout", "diagnosis": {"class": "timeout", "evidence": ["p1"]},
                                      "injected_scenario": "timeout"}}]}],
        }
        observations, labels = benchmark.split_run(legacy)
        self.assertNotIn("scenario_order", observations["configuration"])
        self.assertNotIn("scenario", json.dumps(observations))
        self.assertEqual(labels["stage_labels"][0]["scenario"], "timeout")
        joined = benchmark.join_run(observations, labels)
        self.assertEqual(benchmark.report(joined)["diagnosis"]["accuracy"], 1)
        labels["run_id"] = "other-run"
        with self.assertRaises(ValueError):
            benchmark.join_run(observations, labels)

    def test_outcome_cannot_be_used_as_diagnosis(self):
        run = {
            "schema_version": 1, "kind": "synthetic_dns_benchmark",
            "started_at": "2026-01-01T00:00:00Z", "configuration": {},
            "stages": [{"index": 0, "scenario": "timeout", "phase": "fault", "expected_alert": True,
                        "samples": [sample("p1", "dns_timeout", 700, "wrong_answer")]}],
        }
        scored = benchmark.report(run)
        self.assertEqual(scored["recall"], 1)
        self.assertEqual(scored["diagnosis"]["accuracy"], 0)
        self.assertEqual(scored["diagnosis"]["misclassified_events"][0]["event_id"], "p1")

    def test_failed_stage_keeps_partial_evidence_and_restores_healthy(self):
        calls = []

        def fake_post(_base, path, body, _timeout):
            calls.append((path, body))
            if path == "/api/scenario" and body["scenario"] == "timeout":
                raise OSError("control unavailable")
            return {"scenario": body["scenario"]}

        with patch.object(benchmark, "post_json", side_effect=fake_post), patch.object(
            benchmark, "probe", return_value=sample("p1", "ok", 1, "healthy")
        ):
            internal = benchmark.run_experiment("http://local", ["healthy", "timeout", "healthy"], 1, 1)
        self.assertEqual(internal["status"], "incomplete")
        self.assertEqual(len(internal["stages"]), 1)
        self.assertEqual(internal["errors"][0]["stage_index"], 1)
        self.assertEqual(calls[-1], ("/api/scenario", {"scenario": "healthy"}))
        observations, labels = benchmark.split_run(internal)
        scored = benchmark.report(benchmark.join_run(observations, labels))
        self.assertFalse(scored["completed_all_stages"])


if __name__ == "__main__":
    unittest.main()
