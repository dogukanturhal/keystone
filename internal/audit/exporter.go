// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// Exporter is the SIEM sink for audit entries. Implementations
// consume an AuditEntrySpec after it has been persisted to the
// cluster (CR created, chain advanced) and emit it to a downstream
// aggregator — stdout for Loki / Alloy / Promtail scraping, or a
// structured file target, or (future) an HTTP POST.
//
// Errors returned by Export are logged but do NOT fail the Append
// call: the CR is the authoritative record, and a dropped SIEM
// line is recoverable (operators can reconcile from the CR list).
// Making Export best-effort keeps admission-adjacent controllers
// responsive when the SIEM ingestion pipe is briefly unavailable.
type Exporter interface {
	Export(spec keystonev1alpha1.AuditEntrySpec) error
}

// NoopExporter is the zero-config default. The audit logger uses
// it when no exporter is wired explicitly (e.g. envtest suites),
// which keeps test output clean and preserves pre-C1 behaviour.
type NoopExporter struct{}

// Export discards the event. Always returns nil.
func (NoopExporter) Export(keystonev1alpha1.AuditEntrySpec) error { return nil }

// StdoutJSONExporter writes one newline-delimited JSON object per
// audit entry to os.Stdout. The schema is ECS-inspired so common
// SIEM normalisers (Elastic, Loki with Alloy, Splunk) can grok it
// without per-field remapping:
//
//	{
//	  "@timestamp": "2026-04-19T20:12:34.567Z",
//	  "event": {
//	    "kind": "event",
//	    "dataset": "keystone.audit",
//	    "category": ["database"],
//	    "action": "reconcile",
//	    "outcome": "success",
//	    "sequence": 42,
//	    "reason": "…"
//	  },
//	  "user": {
//	    "name": "system:serviceaccount:keystone-system:keystone-manager",
//	    "id": "",
//	    "groups": ["system:serviceaccounts", "system:serviceaccounts:keystone-system"]
//	  },
//	  "keystone": {
//	    "verb": "reconcile",
//	    "resource": {
//	      "api_version": "keystone.hexxlock.io/v1alpha1",
//	      "kind": "MigrationBundle",
//	      "namespace": "keystone-system",
//	      "name": "users-v1",
//	      "uid": "…"
//	    },
//	    "chain": {
//	      "self_hash": "abcd…",
//	      "prev_hash": "wxyz…"
//	    }
//	  }
//	}
//
// Size-sensitive fields (Before/After) are NOT emitted — they are
// persisted on the CR and can be retrieved on demand. SIEM storage
// is typically hot / expensive; keeping the stream lean preserves
// per-event cost at scale.
type StdoutJSONExporter struct {
	// out is where JSON lines are written. Overridable for tests;
	// defaults to os.Stdout when zero-value.
	out io.Writer
	// mu serialises Write calls so multiple goroutines don't
	// interleave bytes on the same os.Stdout file descriptor.
	mu sync.Mutex
}

// NewStdoutJSONExporter returns an Exporter that writes to os.Stdout.
// Dedicated constructor so callers that want a file target can use
// NewWriterExporter(w) instead.
func NewStdoutJSONExporter() *StdoutJSONExporter {
	return &StdoutJSONExporter{out: os.Stdout}
}

// NewWriterExporter lets callers point the stream at an arbitrary
// io.Writer (a log.Writer, a gzip pipe, an in-memory buffer for
// tests). Behaviour is otherwise identical to the stdout variant.
func NewWriterExporter(w io.Writer) *StdoutJSONExporter {
	if w == nil {
		w = os.Stdout
	}
	return &StdoutJSONExporter{out: w}
}

// Export marshals spec into the ECS-inspired shape and writes one
// newline-terminated JSON line. The mutex serialises concurrent
// Append calls so audit lines don't interleave on the FD.
func (e *StdoutJSONExporter) Export(spec keystonev1alpha1.AuditEntrySpec) error {
	record := toECSRecord(spec)
	buf, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal audit json: %w", err)
	}
	buf = append(buf, '\n')

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.out.Write(buf); err != nil {
		return fmt.Errorf("write audit json: %w", err)
	}
	return nil
}

// -- ECS shape ---------------------------------------------------------

// ecsRecord is the flat on-wire shape. Fields are grouped into
// nested objects matching the ECS convention so SIEM mappers pick
// them up without ad-hoc transforms.
type ecsRecord struct {
	Timestamp string         `json:"@timestamp"`
	Event     ecsEvent       `json:"event"`
	User      ecsUser        `json:"user"`
	Keystone  ecsKeystoneExt `json:"keystone"`
}

type ecsEvent struct {
	Kind     string   `json:"kind"`     // always "event"
	Dataset  string   `json:"dataset"`  // always "keystone.audit"
	Category []string `json:"category"` // always ["database"]
	Action   string   `json:"action"`   // mirrors keystone.verb
	Outcome  string   `json:"outcome"`  // success | error | warn
	Sequence int64    `json:"sequence"`
	Reason   string   `json:"reason,omitempty"`
}

type ecsUser struct {
	Name   string   `json:"name"`
	ID     string   `json:"id,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

type ecsKeystoneExt struct {
	Verb     string          `json:"verb"`
	Resource ecsResourceRef  `json:"resource"`
	Chain    ecsChainSummary `json:"chain"`
}

type ecsResourceRef struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}

type ecsChainSummary struct {
	SelfHash string `json:"self_hash"`
	PrevHash string `json:"prev_hash,omitempty"`
}

// toECSRecord maps an AuditEntrySpec onto the ECS-flavoured wire shape.
func toECSRecord(s keystonev1alpha1.AuditEntrySpec) ecsRecord {
	ts := s.Timestamp.UTC().Format(time.RFC3339Nano)
	return ecsRecord{
		Timestamp: ts,
		Event: ecsEvent{
			Kind:     "event",
			Dataset:  "keystone.audit",
			Category: []string{"database"},
			Action:   s.Verb,
			Outcome:  s.Outcome,
			Sequence: s.Sequence,
			Reason:   s.Reason,
		},
		User: ecsUser{
			Name:   s.Actor.Username,
			ID:     s.Actor.UID,
			Groups: s.Actor.Groups,
		},
		Keystone: ecsKeystoneExt{
			Verb: s.Verb,
			Resource: ecsResourceRef{
				APIVersion: s.ResourceRef.APIVersion,
				Kind:       s.ResourceRef.Kind,
				Namespace:  s.ResourceRef.Namespace,
				Name:       s.ResourceRef.Name,
				UID:        s.ResourceRef.UID,
			},
			Chain: ecsChainSummary{
				SelfHash: s.SelfHash,
				PrevHash: s.PrevHash,
			},
		},
	}
}
