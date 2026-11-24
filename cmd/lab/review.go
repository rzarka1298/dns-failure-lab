package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const syntheticImpact = "Synthetic probe affected; real-user impact is unknown and not measured."

type incidentReview struct {
	Decision string    `json:"decision"`
	Reviewer string    `json:"reviewer"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
}

type incidentRecord struct {
	ID             string           `json:"id"`
	Status         string           `json:"status"`
	Summary        string           `json:"summary"`
	Impact         string           `json:"impact"`
	Evidence       []string         `json:"evidence"`
	EvidenceEvents []event          `json:"evidence_events"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Reviews        []incidentReview `json:"reviews"`
}

type incidentStore struct {
	dir     string
	mu      sync.Mutex
	records map[string]incidentRecord
}

func newIncidentStore(dir string) (*incidentStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("incident data directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create incident directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read incident directory: %w", err)
	}
	store := &incidentStore{dir: dir, records: make(map[string]incidentRecord)}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "inc-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		var record incidentRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if !validIncidentID(record.ID) || entry.Name() != record.ID+".json" || !validIncidentStatus(record.Status) || !validEvidence(record.Evidence, record.EvidenceEvents) {
			return nil, fmt.Errorf("invalid persisted incident %s", entry.Name())
		}
		store.records[record.ID] = record
	}
	return store, nil
}

func validIncidentID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "inc-") {
		return false
	}
	for _, ch := range id[4:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validIncidentStatus(status string) bool {
	return status == "draft" || status == "approved" || status == "rejected"
}

func (s *incidentStore) CreateDraft(draft brief, evidenceEvents []event) (incidentRecord, error) {
	if len(draft.Evidence) == 0 || strings.TrimSpace(draft.Summary) == "" {
		return incidentRecord{}, errors.New("draft requires summary and evidence event IDs")
	}
	if !validEvidence(draft.Evidence, evidenceEvents) {
		return incidentRecord{}, errors.New("draft evidence IDs must match observable event snapshots")
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return incidentRecord{}, fmt.Errorf("generate incident ID: %w", err)
	}
	now := time.Now().UTC()
	record := incidentRecord{
		ID:             "inc-" + hex.EncodeToString(idBytes),
		Status:         "draft",
		Summary:        draft.Summary,
		Impact:         syntheticImpact,
		Evidence:       append([]string(nil), draft.Evidence...),
		EvidenceEvents: copyEvidenceEvents(evidenceEvents),
		CreatedAt:      now,
		UpdatedAt:      now,
		Reviews:        []incidentReview{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(record); err != nil {
		return incidentRecord{}, err
	}
	s.records[record.ID] = record
	return record, nil
}

func (s *incidentStore) List() []incidentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]incidentRecord, 0, len(s.records))
	for _, item := range s.records {
		items = append(items, copyIncident(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items
}

func (s *incidentStore) Get(id string) (incidentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found := s.records[id]
	return copyIncident(item), found
}

func copyIncident(item incidentRecord) incidentRecord {
	item.Evidence = append([]string(nil), item.Evidence...)
	item.EvidenceEvents = copyEvidenceEvents(item.EvidenceEvents)
	item.Reviews = append([]incidentReview(nil), item.Reviews...)
	return item
}

func validEvidence(ids []string, events []event) bool {
	if len(ids) == 0 || len(ids) != len(events) {
		return false
	}
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] || events[i].ID != id || events[i].TraceID == "" || len(events[i].Spans) == 0 {
			return false
		}
		seen[id] = true
	}
	return true
}

func copyEvidenceEvents(events []event) []event {
	copied := make([]event, len(events))
	for i, source := range events {
		copied[i] = source
		// The injected scenario is ground truth, not an observation. Never
		// persist it in a reviewable evidence snapshot.
		copied[i].Scenario = ""
		copied[i].Spans = append([]span(nil), source.Spans...)
	}
	return copied
}

func (s *incidentStore) Review(id, reviewer, decision, note string) (incidentRecord, error) {
	if !validIncidentID(id) {
		return incidentRecord{}, errors.New("invalid incident ID")
	}
	reviewer = strings.TrimSpace(reviewer)
	note = strings.TrimSpace(note)
	if reviewer == "" || len(reviewer) > 120 {
		return incidentRecord{}, errors.New("reviewer name is required and must be at most 120 characters")
	}
	if decision != "approve" && decision != "reject" {
		return incidentRecord{}, errors.New("decision must be approve or reject")
	}
	if len(note) > 2000 {
		return incidentRecord{}, errors.New("review note must be at most 2000 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found := s.records[id]
	if !found {
		return incidentRecord{}, os.ErrNotExist
	}
	if item.Status != "draft" {
		return incidentRecord{}, errors.New("incident has already been reviewed")
	}
	now := time.Now().UTC()
	item.Reviews = append(item.Reviews, incidentReview{Decision: decision, Reviewer: reviewer, Note: note, At: now})
	item.UpdatedAt = now
	if decision == "approve" {
		item.Status = "approved"
	} else {
		item.Status = "rejected"
	}
	if err := s.write(item); err != nil {
		return incidentRecord{}, err
	}
	s.records[id] = item
	return copyIncident(item), nil
}

func (s *incidentStore) write(item incidentRecord) error {
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return fmt.Errorf("encode incident: %w", err)
	}
	temporary, err := os.CreateTemp(s.dir, ".incident-*.tmp")
	if err != nil {
		return fmt.Errorf("create incident temp file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.dir, item.ID+".json")); err != nil {
		return fmt.Errorf("persist incident: %w", err)
	}
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func mountIncidentReview(mux *http.ServeMux, store *incidentStore) {
	mux.HandleFunc("GET /api/incidents", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, store.List())
	})
	mux.HandleFunc("GET /api/incidents/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, found := store.Get(r.PathValue("id"))
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, item)
	})
	mux.HandleFunc("POST /api/incidents/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reviewer string `json:"reviewer"`
			Decision string `json:"decision"`
			Note     string `json:"note"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid review JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "review JSON must contain one object", http.StatusBadRequest)
			return
		}
		item, err := store.Review(r.PathValue("id"), body.Reviewer, body.Decision, body.Note)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			if err.Error() == "incident has already been reviewed" {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, item)
	})
}
