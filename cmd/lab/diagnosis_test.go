package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestDiagnosisUsesEvidenceWithoutScenarioLabel(t *testing.T) {
	tests := []struct {
		name  string
		event event
		want  string
	}{
		{"healthy", event{ID: "p1", Outcome: "ok", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "ok"}, {Name: "http.upstream", Outcome: "ok"}}}, "healthy"},
		{"slow", event{ID: "p2", Outcome: "slow", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "ok", DurationMS: 350}, {Name: "http.upstream", Outcome: "ok"}}}, "slow"},
		{"timeout", event{ID: "p3", Outcome: "dns_timeout", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "error", Detail: "dns_timeout", DurationMS: 700}}}, "timeout"},
		{"wrong", event{ID: "p4", Outcome: "connection_failed", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "ok", Detail: "127.0.0.2"}, {Name: "http.upstream", Outcome: "error", Detail: "connection_failed"}}}, "wrong_answer"},
		{"stale", event{ID: "p5", Outcome: "connection_failed", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "cache_hit", Detail: "127.0.0.2", TTLRemainingMS: 20000}, {Name: "dns.authoritative_check", Outcome: "mismatch", Detail: "127.0.0.1"}, {Name: "http.upstream", Outcome: "error", Detail: "connection_failed"}}}, "stale_answer"},
		{"release", event{ID: "p6", Outcome: "dns_error", Target: "api.invalid.lab", Release: "v2-bad", Spans: []span{{Name: "dns.resolve", Outcome: "error", Detail: "dns_error"}}}, "bad_release"},
		{"unknown", event{ID: "p7", Outcome: "connection_failed", Target: "api.lab.test", Spans: []span{{Name: "dns.resolve", Outcome: "ok", Detail: "10.0.0.5"}, {Name: "http.upstream", Outcome: "error", Detail: "connection_failed"}}}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := diagnose(tc.event)
			if first.Class != tc.want || len(first.Evidence) != 1 || first.Evidence[0] != tc.event.ID || !strings.Contains(first.Summary, tc.event.ID) {
				t.Fatalf("diagnosis = %+v", first)
			}
			tc.event.Scenario = badRelease
			second := diagnose(tc.event)
			if second.Class != first.Class || second.Summary != first.Summary {
				t.Fatalf("scenario changed diagnosis: %+v -> %+v", first, second)
			}
		})
	}
}

func TestPublicEvidenceDoesNotExposeScenario(t *testing.T) {
	e := event{ID: "p1", Scenario: wrongAnswer, Outcome: "connection_failed"}
	e.Diagnosis = diagnose(e)
	b, err := json.Marshal(snapshot{Scenario: wrongAnswer, Events: []event{e}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"scenario"`) || strings.Contains(string(b), "wrong_answer") {
		t.Fatalf("public state leaked label: %s", b)
	}
}

func TestTimeoutTransitionDoesNotContaminateWrongAnswer(t *testing.T) {
	l, err := newLab()
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	if err := l.setScenario(timeout); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() { defer group.Done(); l.probe() }()
	}
	group.Wait()
	if err := l.setScenario(wrongAnswer); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		e := l.probe()
		if e.Outcome != "connection_failed" || e.Diagnosis.Class != "wrong_answer" {
			t.Fatalf("contaminated transition: %+v", e)
		}
	}
}
