package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func reviewEvidence(id string) []event {
	return []event{{ID: id, TraceID: id, Scenario: timeout, Target: "api.lab.test", Outcome: "dns_timeout", Spans: []span{{Name: "dns.resolve", Outcome: "error", Detail: "dns_timeout", DurationMS: 701}}}}
}

func TestIncidentReviewPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := newIncidentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateDraft(brief{
		Status:   "draft_for_human_review",
		Summary:  "Synthetic probe detected DNS timeout.",
		Evidence: []string{"probe-000007"},
		Impact:   "Untrusted impact claim supplied by caller",
	}, reviewEvidence("probe-000007"))
	if err != nil {
		t.Fatal(err)
	}
	if !validIncidentID(created.ID) || created.Status != "draft" || created.Impact != syntheticImpact {
		t.Fatalf("unexpected draft: %+v", created)
	}
	if created.EvidenceEvents[0].Scenario != "" || created.EvidenceEvents[0].Spans[0].Detail != "dns_timeout" {
		t.Fatalf("evidence snapshot leaked label or lost spans: %+v", created.EvidenceEvents)
	}
	approved, err := store.Review(created.ID, "  on-call engineer  ", "approve", "Confirmed against trace probe-000007")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "approved" || len(approved.Reviews) != 1 || approved.Reviews[0].Reviewer != "on-call engineer" || approved.Reviews[0].At.IsZero() || approved.UpdatedAt.Before(created.CreatedAt) {
		t.Fatalf("unexpected approval: %+v", approved)
	}
	reopened, err := newIncidentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, found := reopened.Get(created.ID)
	if !found || restored.ID != created.ID || restored.Status != "approved" || restored.Evidence[0] != "probe-000007" || restored.EvidenceEvents[0].Spans[0].Detail != "dns_timeout" || restored.Reviews[0].Decision != "approve" {
		t.Fatalf("review was not persisted: %+v found=%v", restored, found)
	}
	data, err := os.ReadFile(dir + "/" + created.ID + ".json")
	if err != nil || strings.Contains(string(data), "\"scenario\": \"timeout\"") {
		t.Fatalf("persisted evidence leaked injected label: err=%v data=%s", err, data)
	}
	if _, err := reopened.Review(created.ID, "second reviewer", "reject", ""); err == nil {
		t.Fatal("already approved incident was reviewed again")
	}
	second, err := reopened.CreateDraft(brief{Summary: "Another synthetic probe failed.", Evidence: []string{"probe-000008"}}, reviewEvidence("probe-000008"))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == created.ID {
		t.Fatal("incident ID reused across restart")
	}
}

func TestIncidentReviewRejectsInvalidApproval(t *testing.T) {
	store, err := newIncidentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDraft(brief{Summary: "Unsupported", Evidence: []string{"probe-000002"}}, reviewEvidence("probe-000001")); err == nil {
		t.Fatal("accepted evidence snapshot with a different event ID")
	}
	item, err := store.CreateDraft(brief{Summary: "Synthetic failure", Evidence: []string{"probe-000001"}}, reviewEvidence("probe-000001"))
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct{ reviewer, decision string }{
		{"", "approve"},
		{"engineer", "publish"},
		{"engineer", ""},
	} {
		if _, err := store.Review(item.ID, attempt.reviewer, attempt.decision, ""); err == nil {
			t.Fatalf("accepted invalid review: %+v", attempt)
		}
	}
	if got, found := store.Get(item.ID); !found || got.Status != "draft" || len(got.Reviews) != 0 {
		t.Fatalf("invalid review changed draft: %+v", got)
	}
}

func TestIncidentReviewHTTP(t *testing.T) {
	store, err := newIncidentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.CreateDraft(brief{Summary: "Synthetic failure", Evidence: []string{"probe-000001"}}, reviewEvidence("probe-000001"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mountIncidentReview(mux, store)
	request := httptest.NewRequest(http.MethodPost, "/api/incidents/"+item.ID+"/review", strings.NewReader(`{"reviewer":"Alice","decision":"reject","note":"Evidence insufficient"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("review HTTP status %d: %s", w.Code, w.Body.String())
	}
	var reviewed incidentRecord
	if err := json.Unmarshal(w.Body.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	if reviewed.Status != "rejected" || reviewed.Reviews[0].Note != "Evidence insufficient" {
		t.Fatalf("unexpected review response: %+v", reviewed)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/incidents/"+item.ID+"/review", strings.NewReader(`{"reviewer":"Alice","decision":"reject"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("repeat review status %d", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), item.ID) {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
}
