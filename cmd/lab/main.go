package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type span struct {
	Name           string `json:"name"`
	DurationMS     int64  `json:"duration_ms"`
	Outcome        string `json:"outcome"`
	Detail         string `json:"detail,omitempty"`
	TTLRemainingMS int64  `json:"ttl_remaining_ms,omitempty"`
}
type event struct {
	ID        string    `json:"id"`
	TraceID   string    `json:"trace_id"`
	At        time.Time `json:"at"`
	Scenario  scenario  `json:"-"`
	Release   string    `json:"release"`
	Target    string    `json:"target"`
	Outcome   string    `json:"outcome"`
	TotalMS   int64     `json:"total_ms"`
	Spans     []span    `json:"spans"`
	Diagnosis diagnosis `json:"diagnosis"`
}
type alert struct {
	EventID string    `json:"event_id"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}
type brief struct {
	Status   string   `json:"status"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
	Impact   string   `json:"impact"`
	Review   string   `json:"review"`
}
type snapshot struct {
	Scenario scenario `json:"-"`
	Events   []event  `json:"events"`
	Alert    *alert   `json:"alert"`
	Brief    *brief   `json:"brief"`
}
type lab struct {
	dns             *dnsServer
	upstream        net.Listener
	upstreamPort    int
	resolver        *net.Resolver
	dnsAddress      string
	dnsAdminURL     string
	scenarioMu      sync.RWMutex
	cache           *cachedAnswer
	mu              sync.Mutex
	current         scenario
	events          []event
	counts          map[string]int64
	totalDurationMS int64
	dnsDurationMS   int64
	seq             atomic.Uint64
	incidents       *incidentStore
	incidentActive  bool
}

const staleCacheTTL = 30 * time.Second

type cachedAnswer struct {
	ip      net.IP
	expires time.Time
}

func newLab() (*lab, error) {
	dns, err := startDNS()
	if err != nil {
		return nil, err
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		dns.close()
		return nil, err
	}
	l := &lab{dns: dns, upstream: upstream, upstreamPort: upstream.Addr().(*net.TCPAddr).Port, resolver: labResolver(dns.address()), dnsAddress: dns.address(), current: healthy, counts: map[string]int64{}}
	go func() {
		_ = http.Serve(upstream, upstreamHandler())
	}()
	return l, nil
}
func newRemoteLab(dnsAddress, adminURL string, upstreamPort int) *lab {
	return &lab{resolver: labResolver(dnsAddress), dnsAddress: dnsAddress, dnsAdminURL: adminURL, upstreamPort: upstreamPort, current: healthy, counts: map[string]int64{}}
}
func (l *lab) close() {
	if l.dns != nil {
		l.dns.close()
	}
	if l.upstream != nil {
		_ = l.upstream.Close()
	}
}

func upstreamHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"upstream","status":"ok"}`))
	})
	return mux
}

func (l *lab) setDNSMode(s scenario) error {
	if l.dns != nil {
		l.dns.mode.Store(s)
		return nil
	}
	body, _ := json.Marshal(map[string]scenario{"scenario": s})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.dnsAdminURL, "/")+"/mode", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DNS control returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (l *lab) setScenario(s scenario) error {
	l.scenarioMu.Lock()
	defer l.scenarioMu.Unlock()
	if s == staleAnswer {
		// Prime this explicit resolver cache with a real DNS response from the
		// previous, incorrect A record. Then correct the authoritative answer.
		if err := l.setDNSMode(wrongAnswer); err != nil {
			return err
		}
		l.resolver = labResolver(l.dnsAddress)
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		answers, err := l.resolver.LookupIPAddr(ctx, "api.lab.test")
		cancel()
		if err != nil || len(answers) == 0 {
			_ = l.setDNSMode(l.current)
			if err == nil {
				err = errors.New("no A answer")
			}
			return fmt.Errorf("prime stale cache: %w", err)
		}
		if err := l.setDNSMode(healthy); err != nil {
			_ = l.setDNSMode(l.current)
			return err
		}
		l.resolver = labResolver(l.dnsAddress)
		l.cache = &cachedAnswer{ip: append(net.IP(nil), answers[0].IP...), expires: time.Now().Add(staleCacheTTL)}
	} else {
		if err := l.setDNSMode(s); err != nil {
			return err
		}
		// Go's resolver coalesces identical in-flight names. A timed-out lookup
		// may still be in its retry path after callers have returned, so each
		// scenario uses a fresh resolver instance and cannot inherit that work.
		l.resolver = labResolver(l.dnsAddress)
		l.cache = nil
	}
	l.current = s
	return nil
}

func (l *lab) probe() event {
	l.scenarioMu.RLock()
	defer l.scenarioMu.RUnlock()
	mode := l.current
	start := time.Now()
	id := fmt.Sprintf("probe-%06d", l.seq.Add(1))
	target := "api.lab.test"
	if mode == badRelease {
		target = "api.invalid.lab"
	}
	release := "v1"
	if mode == badRelease {
		release = "v2-bad"
	}
	e := event{ID: id, TraceID: id, At: start.UTC(), Scenario: mode, Release: release, Target: target, Outcome: "ok", Spans: []span{}}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	dnsStart := time.Now()
	var addresses []net.IPAddr
	var err error
	var cacheValid bool
	if mode == staleAnswer && l.cache != nil && time.Now().Before(l.cache.expires) {
		addresses = []net.IPAddr{{IP: l.cache.ip}}
		cacheValid = true
	} else {
		addresses, err = l.resolver.LookupIPAddr(ctx, target)
	}
	dnsSpan := span{Name: "dns.resolve", DurationMS: time.Since(dnsStart).Milliseconds(), Outcome: "ok", Detail: target}
	if cacheValid {
		dnsSpan.Outcome = "cache_hit"
		dnsSpan.TTLRemainingMS = time.Until(l.cache.expires).Milliseconds()
	}
	if err != nil || len(addresses) == 0 {
		dnsSpan.Outcome = "error"
		dnsSpan.Detail = classifyDNS(err)
		e.Outcome = dnsSpan.Detail
		e.Spans = append(e.Spans, dnsSpan)
	} else {
		dnsSpan.Detail = addresses[0].IP.String()
		e.Spans = append(e.Spans, dnsSpan)
		if cacheValid {
			checkStart := time.Now()
			authoritative, checkErr := l.resolver.LookupIPAddr(ctx, target)
			check := span{Name: "dns.authoritative_check", DurationMS: time.Since(checkStart).Milliseconds(), Outcome: "error"}
			if checkErr == nil && len(authoritative) > 0 {
				check.Detail = authoritative[0].IP.String()
				check.Outcome = "match"
				if !authoritative[0].IP.Equal(addresses[0].IP) {
					check.Outcome = "mismatch"
				}
			} else {
				check.Detail = classifyDNS(checkErr)
			}
			e.Spans = append(e.Spans, check)
		}
		httpStart := time.Now()
		transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 400 * time.Millisecond}).DialContext(ctx, "tcp", net.JoinHostPort(addresses[0].IP.String(), strconv.Itoa(l.upstreamPort)))
		}}
		client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.lab.test/data", nil)
		resp, requestErr := client.Do(req)
		httpSpan := span{Name: "http.upstream", DurationMS: time.Since(httpStart).Milliseconds(), Outcome: "ok", Detail: "HTTP 200"}
		if requestErr != nil {
			httpSpan.Outcome = "error"
			httpSpan.Detail = "connection_failed"
			e.Outcome = "connection_failed"
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				httpSpan.Outcome = "error"
				httpSpan.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
				e.Outcome = "upstream_error"
			}
		}
		e.Spans = append(e.Spans, httpSpan)
	}
	e.TotalMS = time.Since(start).Milliseconds()
	if e.Outcome == "ok" && e.TotalMS >= 250 {
		e.Outcome = "slow"
	}
	e.Diagnosis = diagnose(e)
	l.mu.Lock()
	incidentOnset := e.Outcome != "ok" && !l.incidentActive
	l.incidentActive = e.Outcome != "ok"
	l.events = append(l.events, e)
	if len(l.events) > 100 {
		l.events = l.events[len(l.events)-100:]
	}
	l.counts[e.Outcome]++
	l.totalDurationMS += e.TotalMS
	l.dnsDurationMS += e.Spans[0].DurationMS
	l.mu.Unlock()
	if incidentOnset && l.incidents != nil {
		if _, err := l.incidents.CreateDraft(brief{Status: "draft_for_human_review", Summary: e.Diagnosis.Summary, Evidence: []string{e.ID}, Impact: syntheticImpact, Review: "Verify cause and scope before publishing."}, []event{e}); err != nil {
			log.Printf("persist incident draft for %s: %v", e.ID, err)
		}
	}
	line, _ := json.Marshal(e)
	log.Printf("%s", line)
	return e
}

func classifyDNS(err error) string {
	if err == nil {
		return "dns_no_answer"
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return "dns_timeout"
	}
	return "dns_error"
}
func isTimeout(err error) bool { var n net.Error; return errors.As(err, &n) && n.Timeout() }

func (l *lab) snapshot() snapshot {
	l.scenarioMu.RLock()
	defer l.scenarioMu.RUnlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := snapshot{Scenario: l.current, Events: append([]event{}, l.events...)}
	for i := len(s.Events) - 1; i >= 0; i-- {
		e := s.Events[i]
		if e.Outcome != "ok" {
			d := e.Diagnosis
			if d.Class == "" {
				d = diagnose(e)
			}
			s.Alert = &alert{EventID: e.ID, Reason: e.Outcome, At: e.At}
			s.Brief = &brief{Status: "draft_for_human_review", Summary: d.Summary, Evidence: append([]string(nil), d.Evidence...), Impact: "Synthetic probe affected; real-user impact is unknown and not measured.", Review: "Verify cause and scope before publishing."}
			break
		}
		// A healthy probe after a fault clears the active alert.
		break
	}
	return s
}

func (l *lab) routes() http.Handler {
	mux := http.NewServeMux()
	if l.incidents != nil {
		mountIncidentReview(mux, l.incidents)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(workbenchHTML))
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, l.snapshot()) })
	mux.HandleFunc("POST /api/probe", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, l.probe()) })
	mux.HandleFunc("POST /api/rollback", func(w http.ResponseWriter, _ *http.Request) {
		if err := l.setScenario(healthy); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, l.probe())
	})
	mux.HandleFunc("POST /api/scenario", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scenario scenario `json:"scenario"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || !validScenario(body.Scenario) {
			http.Error(w, "invalid scenario", http.StatusBadRequest)
			return
		}
		if err := l.setScenario(body.Scenario); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"scenario": string(body.Scenario)})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# TYPE lab_probes_total counter")
		for _, outcome := range []string{"ok", "slow", "dns_timeout", "dns_error", "dns_no_answer", "connection_failed", "upstream_error"} {
			_, _ = fmt.Fprintf(w, "lab_probes_total{outcome=%q} %d\n", outcome, l.counts[outcome])
		}
		_, _ = fmt.Fprintf(w, "# TYPE lab_probe_duration_seconds_sum counter\nlab_probe_duration_seconds_sum %g\n", float64(l.totalDurationMS)/1000)
		_, _ = fmt.Fprintf(w, "# TYPE lab_dns_duration_seconds_sum counter\nlab_dns_duration_seconds_sum %g\n", float64(l.dnsDurationMS)/1000)
	})
	return mux
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	switch os.Getenv("LAB_ROLE") {
	case "", "all":
		runWorkbench(false)
	case "workbench":
		runWorkbench(true)
	case "dns":
		runDNSProcess()
	case "upstream":
		log.Fatal(http.ListenAndServe(envOr("LAB_UPSTREAM_ADDR", ":8081"), upstreamHandler()))
	default:
		log.Fatal("invalid LAB_ROLE")
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func runWorkbench(remote bool) {
	var l *lab
	if remote {
		port, err := strconv.Atoi(envOr("LAB_UPSTREAM_PORT", "8081"))
		if err != nil || port < 1 || port > 65535 {
			log.Fatal("invalid LAB_UPSTREAM_PORT")
		}
		l = newRemoteLab(envOr("LAB_DNS_ADDR", "dns:5353"), envOr("LAB_DNS_ADMIN_URL", "http://dns:8091"), port)
	} else {
		var err error
		l, err = newLab()
		if err != nil {
			log.Fatal(err)
		}
	}
	defer l.close()
	store, err := newIncidentStore(envOr("LAB_INCIDENT_DIR", ".local/incidents"))
	if err != nil {
		log.Fatalf("incident store: %v", err)
	}
	l.incidents = store
	interval := 5 * time.Second
	if value := os.Getenv("LAB_PROBE_INTERVAL"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			log.Fatal("invalid LAB_PROBE_INTERVAL")
		}
		interval = parsed
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			l.probe()
		}
	}()
	addr := envOr("LAB_ADDR", ":8080")
	log.Printf("workbench listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, l.routes()))
}

func runDNSProcess() {
	var goodIP net.IP
	if raw := os.Getenv("LAB_GOOD_IP"); raw != "" {
		goodIP = net.ParseIP(raw).To4()
	} else {
		ips, err := net.LookupIP(envOr("LAB_UPSTREAM_HOST", "upstream"))
		if err != nil {
			log.Fatal(err)
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				goodIP = ip.To4()
				break
			}
		}
	}
	if goodIP == nil {
		log.Fatal("no IPv4 upstream address")
	}
	d, err := startDNSAt(envOr("LAB_DNS_LISTEN", ":5353"), goodIP, net.IPv4(127, 0, 0, 2))
	if err != nil {
		log.Fatal(err)
	}
	defer d.close()
	log.Printf("DNS listening on %s; control on %s; upstream IP %s", d.address(), envOr("LAB_DNS_ADMIN_ADDR", ":8091"), goodIP)
	log.Fatal(http.ListenAndServe(envOr("LAB_DNS_ADMIN_ADDR", ":8091"), dnsControlHandler(d)))
}

func dnsControlHandler(d *dnsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /mode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scenario scenario `json:"scenario"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || !validScenario(body.Scenario) {
			http.Error(w, "invalid mode", http.StatusBadRequest)
			return
		}
		d.mode.Store(body.Scenario)
		writeJSON(w, map[string]string{"scenario": string(body.Scenario)})
	})
	return mux
}
