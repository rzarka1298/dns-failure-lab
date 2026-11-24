package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProbeScenariosAndIncidentEvidence(t *testing.T) {
	l, err := newLab()
	if err != nil {
		if isBindDenied(err) && os.Getenv("LAB_REQUIRE_NETWORK_TEST") != "1" {
			t.Skipf("sandbox disallows loopback bind: %v", err)
		}
		t.Fatal(err)
	}
	defer l.close()
	tests := []struct {
		mode    scenario
		outcome string
		spans   int
	}{
		{healthy, "ok", 2},
		{slow, "slow", 2},
		{wrongAnswer, "connection_failed", 2},
		{staleAnswer, "connection_failed", 3},
		{badRelease, "dns_error", 1},
		{timeout, "dns_timeout", 1},
		{healthy, "ok", 2},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			if err := l.setScenario(test.mode); err != nil {
				t.Fatal(err)
			}
			e := l.probe()
			if e.Outcome != test.outcome {
				t.Fatalf("outcome = %q; spans = %+v", e.Outcome, e.Spans)
			}
			if len(e.Spans) != test.spans {
				t.Fatalf("spans = %+v", e.Spans)
			}
			if e.Spans[0].Name != "dns.resolve" {
				t.Fatal("DNS resolution was not first")
			}
			if e.Diagnosis.Class != string(test.mode) || len(e.Diagnosis.Evidence) != 1 || e.Diagnosis.Evidence[0] != e.ID {
				t.Fatalf("diagnosis is not supported by event: %+v", e.Diagnosis)
			}
			if test.mode == staleAnswer {
				if e.Spans[0].Outcome != "cache_hit" || e.Spans[0].Detail != "127.0.0.2" || e.Spans[0].TTLRemainingMS <= 0 || e.Spans[1].Name != "dns.authoritative_check" || e.Spans[1].Outcome != "mismatch" || e.Spans[1].Detail != "127.0.0.1" {
					t.Fatalf("stale evidence = %+v", e.Spans)
				}
			}
			s := l.snapshot()
			if test.outcome == "ok" {
				if s.Alert != nil {
					t.Fatal("healthy probe did not clear alert")
				}
				return
			}
			if s.Alert == nil || s.Alert.EventID != e.ID || s.Brief == nil || len(s.Brief.Evidence) != 1 || s.Brief.Evidence[0] != e.ID {
				t.Fatalf("uncited alert: %+v", s)
			}
			if !strings.Contains(s.Brief.Impact, "real-user impact is unknown") {
				t.Fatal("impact boundary missing")
			}
			if test.mode == staleAnswer {
				l.scenarioMu.Lock()
				l.cache.expires = time.Now().Add(-time.Second)
				l.scenarioMu.Unlock()
				if recovered := l.probe(); recovered.Outcome != "ok" {
					t.Fatalf("expired cache did not recover: %+v", recovered)
				}
			}
		})
	}
}

func TestScenarioRejectsInvalidValue(t *testing.T) {
	l := &lab{current: healthy}
	w := httptest.NewRecorder()
	l.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/scenario", strings.NewReader(`{"scenario":"external"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	if l.snapshot().Scenario != healthy {
		t.Fatal("scenario changed")
	}
}

func TestDNSControlAndUpstreamHandlers(t *testing.T) {
	d := &dnsServer{}
	d.mode.Store(healthy)
	controller := dnsControlHandler(d)
	w := httptest.NewRecorder()
	controller.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mode", strings.NewReader(`{"scenario":"slow"}`)))
	if w.Code != http.StatusOK || d.mode.Load().(scenario) != slow {
		t.Fatalf("mode status=%d mode=%v", w.Code, d.mode.Load())
	}
	w = httptest.NewRecorder()
	controller.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mode", strings.NewReader(`{"scenario":"external"}`)))
	if w.Code != http.StatusBadRequest || d.mode.Load().(scenario) != slow {
		t.Fatalf("invalid mode changed state: status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	upstreamHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/data", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("upstream response: %d %s", w.Code, w.Body.String())
	}
}

func TestRollbackRestoresHealthyAndVerifies(t *testing.T) {
	l, err := newLab()
	if err != nil {
		if isBindDenied(err) && os.Getenv("LAB_REQUIRE_NETWORK_TEST") != "1" {
			t.Skipf("sandbox disallows loopback bind: %v", err)
		}
		t.Fatal(err)
	}
	defer l.close()
	if err := l.setScenario(badRelease); err != nil {
		t.Fatal(err)
	}
	if e := l.probe(); e.Outcome != "dns_error" || e.Release != "v2-bad" {
		t.Fatalf("bad release evidence: %+v", e)
	}
	w := httptest.NewRecorder()
	l.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/rollback", nil))
	if w.Code != http.StatusOK || l.snapshot().Alert != nil || l.snapshot().Scenario != healthy {
		t.Fatalf("rollback status=%d state=%+v", w.Code, l.snapshot())
	}
}

func TestIncidentDraftCreatedAtAlertOnset(t *testing.T) {
	l, err := newLab()
	if err != nil {
		if isBindDenied(err) && os.Getenv("LAB_REQUIRE_NETWORK_TEST") != "1" {
			t.Skipf("sandbox disallows loopback bind: %v", err)
		}
		t.Fatal(err)
	}
	defer l.close()
	dir := t.TempDir()
	l.incidents, err = newIncidentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.setScenario(timeout); err != nil {
		t.Fatal(err)
	}
	first := l.probe()
	l.probe()
	items := l.incidents.List()
	if len(items) != 1 || len(items[0].EvidenceEvents) != 1 || items[0].EvidenceEvents[0].ID != first.ID {
		t.Fatalf("incident onset not captured: %+v", items)
	}
	if err := l.setScenario(healthy); err != nil {
		t.Fatal(err)
	}
	if e := l.probe(); e.Outcome != "ok" {
		t.Fatalf("recovery failed: %+v", e)
	}
	if err := l.setScenario(timeout); err != nil {
		t.Fatal(err)
	}
	l.probe()
	if got := len(l.incidents.List()); got != 2 {
		t.Fatalf("incident count = %d, want 2", got)
	}
	reopened, err := newIncidentStore(dir)
	if err != nil || len(reopened.List()) != 2 {
		t.Fatalf("incident persistence failed: %v", err)
	}
}

func isBindDenied(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "listen" && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES))
}

func TestDNSQuestionParsing(t *testing.T) {
	packet := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'p', 'i', 3, 'l', 'a', 'b', 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1}
	name, end, ok := parseQuestion(packet)
	if !ok || name != "api.lab.test" || end != len(packet) {
		t.Fatalf("parse = %q %d %v", name, end, ok)
	}
	if _, _, ok := parseQuestion(packet[:len(packet)-1]); ok {
		t.Fatal("accepted truncated question")
	}
	packet[4] = 0
	packet[5] = 2
	if _, _, ok := parseQuestion(packet); ok {
		t.Fatal("accepted multiple questions")
	}
	if binary.BigEndian.Uint16(packet[0:2]) != 0x1234 {
		t.Fatal("test fixture changed")
	}
}

func TestDNSAnswerVariants(t *testing.T) {
	packet := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'p', 'i', 3, 'l', 'a', 'b', 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1}
	for _, tc := range []struct {
		mode scenario
		ip   []byte
	}{{healthy, []byte{127, 0, 0, 1}}, {wrongAnswer, []byte{127, 0, 0, 2}}} {
		answer := makeDNSResponse(packet, tc.mode)
		if len(answer) < 12 || answer[7] != 1 || !bytes.Equal(answer[len(answer)-4:], tc.ip) {
			t.Fatalf("%s returned %v", tc.mode, answer)
		}
		wantTTL := uint32(1)
		if tc.mode == wrongAnswer {
			wantTTL = 30
		}
		if got := binary.BigEndian.Uint32(answer[len(packet)+6 : len(packet)+10]); got != wantTTL {
			t.Fatalf("%s TTL = %d, want %d", tc.mode, got, wantTTL)
		}
	}
	bad := append([]byte(nil), packet...)
	bad[13] = 'x'
	answer := makeDNSResponse(bad, healthy)
	if answer[3] != 0x83 || answer[7] != 0 {
		t.Fatalf("expected NXDOMAIN: %v", answer)
	}
}

func TestSnapshotCitesOnlyObservedEvent(t *testing.T) {
	l := &lab{current: timeout, events: []event{{ID: "probe-000001", Outcome: "dns_timeout", Target: "api.lab.test"}}}
	s := l.snapshot()
	if s.Alert == nil || s.Alert.EventID != "probe-000001" || s.Brief == nil || s.Brief.Evidence[0] != "probe-000001" {
		t.Fatalf("bad evidence: %+v", s)
	}
	if !strings.Contains(s.Brief.Impact, "unknown") {
		t.Fatal("impact claim is not bounded")
	}
	l.events = append(l.events, event{ID: "probe-000002", Outcome: "ok"})
	if l.snapshot().Alert != nil {
		t.Fatal("healthy probe did not clear alert")
	}
}
