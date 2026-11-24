package main

import (
	"fmt"
	"net"
)

// diagnosis is an evidence-derived hypothesis. Its class is never copied from
// the injected scenario label, which is private to the experiment controller.
type diagnosis struct {
	Class      string   `json:"class"`
	Confidence string   `json:"confidence"`
	Summary    string   `json:"summary"`
	Evidence   []string `json:"evidence"`
}

func diagnose(e event) diagnosis {
	d := diagnosis{Class: "unknown", Confidence: "low", Evidence: []string{e.ID}}
	var resolved, authority, upstream *span
	for i := range e.Spans {
		switch e.Spans[i].Name {
		case "dns.resolve":
			resolved = &e.Spans[i]
		case "dns.authoritative_check":
			authority = &e.Spans[i]
		case "http.upstream":
			upstream = &e.Spans[i]
		}
	}
	switch {
	case resolved != nil && resolved.Outcome == "cache_hit" && authority != nil && authority.Outcome == "mismatch":
		d.Class, d.Confidence = "stale_answer", "high"
		d.Summary = fmt.Sprintf("Probe %s used cached DNS address %s while the authoritative check returned %s; the cached answer may be stale.", e.ID, resolved.Detail, authority.Detail)
	case e.Target != "api.lab.test" && e.Release != "v1" && resolved != nil && resolved.Outcome == "error":
		d.Class, d.Confidence = "bad_release", "medium"
		d.Summary = fmt.Sprintf("Probe %s on release %s queried unexpected name %s and DNS resolution failed; check the release configuration.", e.ID, e.Release, e.Target)
	case resolved != nil && resolved.Outcome == "error" && resolved.Detail == "dns_timeout":
		d.Class, d.Confidence = "timeout", "high"
		d.Summary = fmt.Sprintf("Probe %s timed out during DNS resolution after %d ms.", e.ID, resolved.DurationMS)
	case resolved != nil && upstream != nil && upstream.Outcome == "error" && upstream.Detail == "connection_failed" && suspiciousLoopback(resolved.Detail):
		d.Class, d.Confidence = "wrong_answer", "medium"
		d.Summary = fmt.Sprintf("Probe %s resolved %s to loopback address %s, then could not connect to the upstream; the DNS answer is suspect.", e.ID, e.Target, resolved.Detail)
	case resolved != nil && resolved.DurationMS >= 250 && upstream != nil && upstream.Outcome == "ok":
		d.Class, d.Confidence = "slow", "high"
		d.Summary = fmt.Sprintf("Probe %s spent %d ms resolving DNS before the upstream responded successfully.", e.ID, resolved.DurationMS)
	case e.Outcome == "ok" && resolved != nil && resolved.Outcome == "ok" && upstream != nil && upstream.Outcome == "ok":
		d.Class, d.Confidence = "healthy", "high"
		d.Summary = fmt.Sprintf("Probe %s resolved DNS and received a successful upstream response.", e.ID)
	default:
		d.Summary = fmt.Sprintf("Probe %s observed %s; the available DNS and HTTP spans do not establish a specific cause.", e.ID, e.Outcome)
	}
	return d
}

func suspiciousLoopback(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.IsLoopback() && !ip.Equal(net.IPv4(127, 0, 0, 1))
}
