# 0022. Structured JSON audit export for SIEM

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

Keystone's audit log lives in the cluster as `AuditEntry` CRs linked
by a SHA-256 hash chain (prev_hash → self_hash) and fronted by an
append-only webhook. That gives us:

- **Tamper-evident record** — any edit to a past entry breaks the
  chain; `audit.VerifyChain()` proves or disproves integrity.
- **Audit-grade retention** — entries are CRs, retained via etcd,
  bounded by `AuditLog.spec.retentionDays` (default 7 years).

Missing: **a SIEM stream**. Security teams want every mutation
event to land in Loki / Splunk / Elastic within seconds for
correlation, alerting, and long-term warm storage. Querying
`kubectl get auditentry --watch | jq` works for ad-hoc triage but
doesn't scale to fleet-level SIEM workflows — the 1 MiB etcd value
size cap and per-CR GET cost make it the wrong primary source for
hot queries.

Options considered:

- **CR-only (status quo)** — audit logs stay in etcd. Fails the SIEM
  requirement.
- **Stdout JSON + CR** — every committed entry emitted as one JSON
  line to stdout; Loki/Alloy/Promtail sidecar scrapes. CR stays
  authoritative.
- **Stdout JSON + CR + HTTP POST** — also push to the SIEM's HTTP
  ingest endpoint. Rejected for this phase — adds config surface and
  retry story without covering a use case the sidecar model misses.

Industry evidence: Liquibase Secure, Bytebase SaaS, and Atlas Cloud
all stream JSON to stdout as the primary SIEM interface. Loki
scraping is the de facto standard in our cluster already (Grafana
Alloy DaemonSet), so the sidecar/scraper side is "free."

## Decision

Ship **dual-sink audit** — CR plus stdout JSON:

### Exporter interface

New `audit.Exporter` interface (single method `Export(spec
AuditEntrySpec) error`) with two implementations:

- **`NoopExporter`** — zero-value default. Preserves pre-C1
  behaviour for envtest suites and any call site that doesn't opt
  into SIEM streaming.
- **`StdoutJSONExporter`** — emits one ECS-inspired newline-
  delimited JSON record per entry. Thread-safe via a mutex on the
  underlying `io.Writer`.

### ECS-inspired wire shape

```
{
  "@timestamp":           "2026-04-19T20:12:34.567Z",
  "event": {
    "kind":     "event",
    "dataset":  "keystone.audit",
    "category": ["database"],
    "action":   "reconcile",
    "outcome":  "success",
    "sequence": 42,
    "reason":   "…"
  },
  "user": {
    "name":   "system:serviceaccount:keystone-system:keystone-manager",
    "id":     "",
    "groups": ["system:serviceaccounts", "system:serviceaccounts:keystone-system"]
  },
  "keystone": {
    "verb": "reconcile",
    "resource": {
      "api_version": "keystone.hexxlock.io/v1alpha1",
      "kind":        "MigrationBundle",
      "namespace":   "keystone-system",
      "name":        "users-v1",
      "uid":         "…"
    },
    "chain": {
      "self_hash": "aabbccdd…",
      "prev_hash": "wxyz…"
    }
  }
}
```

Elastic Common Schema (ECS) names (`@timestamp`, `event.action`,
`user.name`, `event.outcome`) let the default Loki/Alloy/Elastic
mappings drop events into the right indices without per-field
transforms.

The `keystone.*` extension namespace carries operator-specific
fields (verb, resource ref, chain hashes) without colliding with
ECS reserved keys.

### Omitted from the stream

**`Before` and `After` JSON diffs are NOT emitted**. They live on
the CR (truncated to 32 KiB each) and are fetchable on demand via
`kubectl get auditentry/entry-000000000000000042 -o jsonpath=
'{.spec.before}'`. Keeping the SIEM stream lean cuts per-event cost
at scale — a 10-event-per-second fleet shipping full diffs would
push gigabytes per hour into warm SIEM storage for no active-query
benefit.

### Best-effort semantics

`Logger.exportEntry` calls the exporter AFTER the CR is created and
the AuditLog status bump is in flight. An export error is delivered
to `ExportErrorFn` (controller-runtime logger in production,
nil-silent in tests) but does NOT fail the `Append` call: the CR is
the authoritative ledger, and a dropped SIEM line is recoverable
(operators can reconcile from the CR list).

### Manager wiring

`cmd/manager/main.go` resolves `KEYSTONE_AUDIT_STDOUT` (default
"true") and injects `StdoutJSONExporter` into the logger. Envtest
runs set it to "false" to keep test output clean.

## Consequences

Easier: **SIEM onboarding is zero-config in production**. Default-on
means operators don't have to flip a flag to get coverage;
controller-runtime already streams to stdout, so Alloy picks the
JSON up alongside the manager's own log lines.

Easier: **audit correlation across services**. Every HexxLock
service already emits ECS-flavoured logs; keystone.audit joins the
same pipeline with the same field names.

Easier: **tamper-evidence + SIEM retention coexist**. The CR is the
integrity proof; SIEM is the search layer. When an alert fires at
3 AM, the SOC analyst queries Loki by `user.name` and `event.action`;
when the forensic follow-up needs the exact pre/post state, they
`kubectl get auditentry` against the CR.

Harder: **two formats to keep in sync**. A new AuditEntry field
must be added to both the CR and the ECS record. Mitigation: the
`toECSRecord` function is ~40 lines and unit-tested against every
field; a field addition that forgets the ECS mapping fails a
regression test.

Harder: **stdout contention**. Under extreme load the controller's
own structured logs and audit JSON share a single stdout file
descriptor. The exporter's mutex prevents interleaved bytes but
can't prevent log-line reorder if a framework logger flushes
between our `Write` calls. Noted as a caveat; Alloy handles this
correctly as long as each line is valid JSON.

Soft: **`Before`/`After` are stream-invisible**. A SOC query that
wants to know "what changed when this bundle version bumped" has
to fetch the CR. Documented in CONTRIBUTING; worst-case cost is one
extra kubectl call per investigated event.

## Alternatives considered

- **File output instead of stdout**. Requires a writable volume,
  log rotation, and a separate scraping config. Stdout is the
  Kubernetes-native path: every cluster operator knows how to read
  it.
- **HTTP POST to SIEM directly**. Couples the operator to a
  specific SIEM and requires retry / buffer logic. Rejected for
  this phase; can land as a second Exporter implementation later
  without changing the wire shape.
- **Protobuf / Avro payload**. ECS JSON is the lingua franca; the
  SIEM pipelines consume JSON natively. Binary formats require
  schema registry infrastructure Keystone doesn't want to own.
- **Emit only on failures (outcome=error)**. Rejected — successful
  changes are just as important to the audit narrative
  (authorisation for later "who approved this?" queries).
- **Full CR JSON (Before + After included)**. Rejected on cost
  grounds per above. Operators that want full-fidelity SIEM can
  build a secondary controller that POSTs the CR body.
