# Compliance control mappings

This document maps Keystone features to controls in the compliance frameworks
customers most often ask about: SOC 2, ISO/IEC 27001:2022, HIPAA, PCI DSS
v4.0.1, and GDPR. It is written for two audiences:

- **Procurement / vendor-risk reviewers** skimming for the load-bearing
  controls Keystone affects. See the summary table first; skip the per-
  framework sections unless something there matters to your audit.
- **Keystone customers** building their own compliance story. Keystone is a
  tool; your compliance posture is yours. We document what Keystone does and
  does not do so you know which side of the shared-responsibility line each
  control sits on.

## "Meets" vs "enables" — a critical distinction

Two very different claims are possible for any given control:

- **Keystone meets the control** — the control applies to the Keystone
  project itself (its development, supply chain, release process, or
  managed-SaaS operations if / when HexxLock Cloud ships). This is what
  HexxLock will claim about its own SaaS tier when that tier is shipped.
  Today it is partially claimed for the OSS release process (signed images,
  SBOM, DCO) and not claimed at all for a SaaS tier that does not yet
  exist.
- **Keystone enables the control** — the control applies to the customer's
  infrastructure, and Keystone provides features that make the control
  achievable. This is the product's actual value proposition for regulated
  customers: change-management, audit evidence, drift detection, and staged
  rollout are all *enabler* features.

Every row in every table below is tagged with one or both of `meets` /
`enables`. A row marked `enables` means "run Keystone and this control gets
easier to pass"; it does not mean "you are compliant because you bought
Keystone". That distinction matters — misreading it produces broken
compliance programmes.

Where a control is load-bearing but **not shipped yet**, the row is tagged
`[planned — Phase NN.NN]` using the phase numbers from `docs/roadmap.md` and
the internal roadmap. A `planned` row is honest about the gap; it is
not evidence of meeting anything today.

## Summary — top controls by framework

One-line mapping of each framework's highest-weight controls to the
Keystone feature that satisfies them. Use this before diving into the full
tables.

| Framework | Load-bearing control | Keystone feature | Role | Reference |
|---|---|---|---|---|
| SOC 2 | CC7.2 — monitoring of system components | DriftController + Prometheus metrics | Enables | `internal/drift/`, `docs/adrs/0014-slo-framework-multi-window-burn-rate.md` |
| SOC 2 | CC8.1 — change management | MigrationBundle + RolloutPolicy + ADR 0011 admission webhook | Enables | `internal/webhook/`, `docs/adrs/0011-admission-webhook-lints-not-just-reconciles.md` |
| SOC 2 | CC6.1 — logical access | Default ClusterRole least-privilege, cross-ns Secret rejection, DCO | Enables + Meets | `config/rbac/role.yaml`, `CONTRIBUTING.md` |
| ISO 27001:2022 | A.8.32 — change management | MigrationBundle immutability + content-hash anti-replay | Enables | `internal/migration/runner.go`, `CHANGELOG.md` v0.1.0 |
| ISO 27001:2022 | A.8.16 — monitoring activities | `keystone_drift_*` metric family + PrometheusRule | Enables | `<gitops-repo>/keystone/observability/` |
| ISO 27001:2022 | A.8.31 — separation of dev/test/prod | ClusterRegistration `tier` + RolloutPolicy `clusterTierSelector` | Enables | `docs/adrs/0006-tier-aware-rollout-policy.md` |
| HIPAA | §164.312(b) — audit controls | SQL runner records `applied_by` + `content_hash` + `duration_ms` | Enables | `internal/migration/runner.go`, `CHANGELOG.md` |
| HIPAA | §164.308(a)(1)(ii)(D) — information system activity review | DriftReport CR + Prometheus history | Enables | `internal/drift/`, `docs/observability.md` |
| PCI DSS v4.0.1 | Req 6.3 — secure development | DCO, ADR 0010 identifier validation, 31 SQLi unit tests | Meets (project) + Enables (customer) | `docs/adrs/0010-defense-in-depth-identifier-validation.md` |
| PCI DSS v4.0.1 | Req 10 — log and monitor all access | DriftController + MigrationExecution history | Enables | `internal/drift/`, `CHANGELOG.md` |
| PCI DSS v4.0.1 | Req 6.5.3 — separation of environments | Tier-aware rollouts | Enables | `docs/adrs/0006-tier-aware-rollout-policy.md` |
| GDPR | Art. 32 — security of processing | Identifier quoting, Secret-ref scoping, mTLS to PG | Enables (processor side) | `docs/adrs/0010-*`, `CHANGELOG.md` v0.1.0 |

## SOC 2 — TSC 2017 + 2022 revision

The Trust Services Criteria are organised into **CC1–CC9** plus the
specific A / C / P / PI criteria. Only the Security category (CC1–CC9) is
universally applicable; A (Availability), C (Confidentiality),
PI (Processing Integrity), and P (Privacy) are added only if a customer
includes them in their scope. We map the Security criteria in full and add
callouts for A / PI / C where the mapping is obvious.

| Control | Relevant? | Keystone role | Implementation |
|---|---|---|---|
| CC1 — Control environment | Partial | Meets (project governance) | `GOVERNANCE.md`, `CODE_OF_CONDUCT.md`, `MAINTAINERS.md`, `CONTRIBUTING.md` |
| CC2 — Communication and information | Partial | Meets | `CHANGELOG.md`, public ADR log under `docs/adrs/`, `SECURITY.md` disclosure timeline |
| CC3 — Risk assessment | Partial | Meets (for the project itself) | Risk register is internal to HexxLock Platform today; published risk statements travel through ADRs and `docs/pentest-scope.md` |
| CC4 — Monitoring activities | Yes | Enables | `keystone_drift_*` metric family + ServiceMonitor + PrometheusRule (`<gitops-repo>/keystone/observability/`); SLOs per ADR 0014 |
| CC5 — Control activities | Yes | Enables + Meets | Enables: RolloutPolicy approval gates, admission webhook (ADR 0011). Meets: release-gate CI (`make verify`) per `CONTRIBUTING.md` |
| CC6.1 — Logical access — identification | Yes | Enables | Default least-privilege `ClusterRole` (`config/rbac/role.yaml`); cross-namespace Secret refs rejected at controller + Kyverno per `CHANGELOG.md` |
| CC6.2 — Logical access — authentication | Yes | Enables | mTLS to PostgreSQL providers via Linkerd injection (`CHANGELOG.md` v0.1.0 Security section); DatabaseProvider Secret-ref model |
| CC6.3 — Logical access — authorisation | Yes | Enables | RBAC-bounded CRD access; namespace-scoped ProductInstance; finalizer-controlled deletion |
| CC6.6 — Transmission | Yes | Enables | mTLS in-cluster; `sslmode=disable` rejected at DatabaseProvider API layer |
| CC6.7 — Credentials in transit | Yes | Enables | Credentials never logged; Secret-ref-only; DDL logs elide password fields |
| CC6.8 — Prevention of malicious software | Yes | Meets | Trivy fs + image scan in CI, fail on HIGH+CRITICAL (`SECURITY.md`); cosign-signed images; pinned tool versions |
| CC7.1 — Detection of vulnerabilities | Yes | Meets | `SECURITY.md` disclosure process; `make vuln-scan` gate |
| CC7.2 — Monitoring for anomalies | Yes | Enables | DriftController + DriftReport; `keystone_drift_detected_total` counter; multi-window burn-rate alerts (ADR 0014) |
| CC7.3 — Response to security events | Partial | Meets | `SECURITY.md §Disclosure timeline`; `docs/pentest-scope.md §Coordination` |
| CC7.4 — Post-incident analysis | Partial | Meets (project side) | Error-budget policy per ADR 0014; public advisory / CVE workflow in `SECURITY.md` |
| CC7.5 — Recovery | Partial | Enables | `docs/runbook.md` covers stuck reconcile, drift detected, pgroll Expanded wait, manager crashloop; chaos-test script `hack/chaos-test.sh` verifies 90-second idempotent recovery |
| CC8.1 — Change management | Yes | Enables | MigrationBundle content-hash anti-replay, RolloutPolicy staged rollout with approval gate, admission-webhook lints (ADR 0011), Conftest at MR time, Kyverno at admission |
| CC9.1 — Risk mitigation — vendor | N/A (product-level) | — | Not a Keystone concern; customer manages their own vendor posture including us |
| CC9.2 — Risk mitigation — incidents | Partial | Meets + Enables | Meets: `SECURITY.md`. Enables: DriftController gives customers the detection lever |

**Availability (A) callouts:**
- A1.2 — environmental protections — N/A, platform control, not product-level.
- A1.3 — recovery testing — Enables via `hack/chaos-test.sh`; verified at release time.

**Processing Integrity (PI) callouts:**
- PI1.1 — system processing meets objectives — Enables via MigrationExecution phase state machine + transactional SQL apply per `CHANGELOG.md`.
- PI1.5 — output completeness — Enables via content-hash on every applied migration.

**Confidentiality (C) callouts:**
- C1.1 — confidential information is identified — Partial. Keystone does not classify data; it handles DDL, not DML.
- C1.2 — confidential information is protected — Enables via mTLS + Secret-ref scoping.

## ISO/IEC 27001:2022 Annex A

ISO 27001:2022 restructured Annex A into four themes (Organisational, People,
Physical, Technological). We map only Technological (`A.8.x`) and the
Organisational controls that Keystone materially affects. People and
Physical controls are out of scope for a software product.

| Control | Relevant? | Keystone role | Implementation |
|---|---|---|---|
| A.5.22 — monitoring, review, and change management of supplier services | Partial | Meets | Supplier here = HexxLock; governance, release cadence, and security policy are public in this repo |
| A.5.37 — documented operating procedures | Yes | Meets | `docs/runbook.md`, `docs/observability.md`, `docs/adrs/*`, `SECURITY.md` |
| A.8.2 — privileged access rights | Yes | Enables | Default `ClusterRole` least-privilege; admin DB creds via Secret-ref only; ADR 0004 justifies no `kube-rbac-proxy` |
| A.8.5 — secure authentication | Yes | Enables | mTLS to PG; no `sslmode=disable`; Linkerd-injected transport |
| A.8.9 — configuration management | Yes | Enables | All state is a CR; GitOps-enforced via ArgoCD; ADR 0007 |
| A.8.12 — data leakage prevention | Partial | Enables | Credentials never logged; Secret refs namespace-scoped; cross-ns refs rejected at Kyverno |
| A.8.15 — logging | Yes | Enables | MigrationExecution status history; `schema_migrations` table records `content_hash`, `duration_ms`, `applied_by`; structured logs via `zap` |
| A.8.16 — monitoring activities | Yes | Enables | `keystone_drift_*` + `controller_runtime_*` metrics; multi-window burn-rate alerts (ADR 0014); DriftReport CR |
| A.8.23 — web filtering | N/A | — | Not applicable to an in-cluster operator |
| A.8.24 — cryptography | Yes | Meets + Enables | Meets: cosign signature on every release image (`SECURITY.md`). Enables: TLS to PG; PKCS8 cert handling per the Linkerd PKCS#8 learning |
| A.8.25 — secure development life cycle | Yes | Meets | `CONTRIBUTING.md` DCO, `make verify` gate, 31-test SQL-injection suite per ADR 0010, admission-webhook contract test per ADR 0011 |
| A.8.26 — application security requirements | Yes | Meets | ADR 0010 identifier validation, ADR 0011 admission, ADR 0014 SLO; `docs/pentest-scope.md` third-party validation |
| A.8.27 — secure system architecture | Yes | Meets | ADR series 0001–0014; defence-in-depth; CRD-first boundary |
| A.8.28 — secure coding | Yes | Meets | `golangci-lint` pinned version, `make lint` gate, code review required on `internal/postgres/`, `internal/migration/pgroll/` |
| A.8.29 — security testing | Yes | Meets | envtest integration `[planned — Phase 2.1]`; SQLi unit tests shipped in v0.1.0 |
| A.8.30 — outsourced development | N/A | — | Keystone is not outsourced |
| A.8.31 — separation of development, test, and production environments | Yes | Enables | ClusterRegistration `tier` + RolloutPolicy `clusterTierSelector` (`canary`/`free`/`paid`/`internal`) per ADR 0006 |
| A.8.32 — change management | Yes | Enables | MigrationBundle immutability + content-hash; RolloutPolicy approval gates; admission webhook (ADR 0011); Conftest + Kyverno policy chain |
| A.8.33 — test information | Partial | Enables | Tenants can use ProductInstance to spin up isolated tenant DBs for test data; responsibility for the test data itself is the customer's |
| A.8.34 — protection of systems during audit testing | Partial | Enables | `pentest/<engagement-id>` immutable-branch policy in `docs/pentest-scope.md` |

## HIPAA Security Rule — §164.308 + §164.312

**Important framing**: Keystone does NOT store Protected Health Information
(PHI). Keystone is a schema-management tool — it handles DDL, not DML. It
sees table *structures*, not *rows*. That said, §164.312(b) Audit Controls
explicitly applies to "activity in systems that contain or use ePHI", and
schema changes against a PHI-bearing database *are* such activity. So the
Audit Controls obligations travel even though the data-plane obligations do
not.

| Rule | Relevant? | Keystone role | Implementation |
|---|---|---|---|
| §164.308(a)(1)(i) — Security management process | Partial | Enables | DriftController + MigrationExecution give the "regular review of information system activity" evidence |
| §164.308(a)(1)(ii)(D) — Information system activity review | Yes | Enables | DriftReport CR + `keystone_drift_*` metrics + MigrationExecution status history provide the reviewable record |
| §164.308(a)(3) — Workforce security | N/A | — | Policy concern, not a product concern |
| §164.308(a)(4) — Information access management | Yes | Enables | Default least-privilege `ClusterRole`; tenant isolation via ProductInstance |
| §164.308(a)(5)(ii)(C) — Log-in monitoring | N/A (for Keystone itself) | — | Applies to the ePHI-bearing DB, not to Keystone. Keystone's own auth is k8s RBAC |
| §164.308(a)(6) — Security incident procedures | Yes | Meets | `SECURITY.md §Disclosure timeline` |
| §164.308(a)(7) — Contingency plan | Partial | Enables | `hack/chaos-test.sh` verifies manager crash-recovery; `docs/runbook.md` covers recovery paths |
| §164.308(a)(8) — Evaluation | Yes | Meets | `docs/pentest-scope.md` — scheduled third-party pentest per major release |
| §164.312(a) — Access control | Yes | Enables | RBAC-bound CRD access; namespace-scoped ProductInstance |
| §164.312(a)(2)(iv) — Encryption and decryption | N/A (at-rest) | — | Encryption-at-rest is the upstream PG + storage operator concern |
| §164.312(b) — Audit controls | Yes | Enables | `schema_migrations` table records `content_hash`, `duration_ms`, `applied_by`, `applied_at` per execution; MigrationExecution.status.conditions audit-trail |
| §164.312(c)(1) — Integrity | Yes | Enables | Content-hash anti-replay; DriftController hashes = mismatch detection |
| §164.312(d) — Person or entity authentication | Yes (for Keystone → PG) | Enables | mTLS via Linkerd; admin-credentials Secret model |
| §164.312(e)(1) — Transmission security | Yes | Enables | mTLS to PG; `sslmode=disable` rejected |

HIPAA Business Associate Agreement (BAA) territory: if HexxLock ships a
managed-cloud Keystone tier (Phase 15+) that runs against customer ePHI-
bearing databases, HexxLock becomes a Business Associate and a BAA applies.
The OSS tier has no BAA obligation — the customer runs the operator in
their own cluster.

## PCI DSS v4.0.1

PCI DSS 4.0.1 is the active standard (v3.2.1 retired 2024-03-31). Listed
below are the requirements Keystone affects materially. Req 1, 2, 3, 4, 5,
9, 11, 12 mostly sit outside Keystone's attack surface.

| Requirement | Relevant? | Keystone role | Implementation |
|---|---|---|---|
| Req 6.2 — bespoke software developed securely | Yes | Meets | DCO, ADR 0010 identifier validation, `make verify` gate, 31 SQLi tests per `docs/adrs/0010-*` |
| Req 6.3 — security vulnerabilities identified | Yes | Meets | `make vuln-scan` (Trivy) in CI, HIGH/CRITICAL fail the build; `SECURITY.md §Supply-chain assurances` |
| Req 6.5.1 — injection flaws | Yes | Meets | Defence-in-depth identifier validation per ADR 0010; 31 unit tests in `internal/postgres/quote_test.go` |
| Req 6.5.3 — separation of production and non-production environments | Yes | Enables | ClusterRegistration `tier` + RolloutPolicy `clusterTierSelector` per ADR 0006 |
| Req 7.1 — access is limited | Yes | Enables | Default least-privilege `ClusterRole`; namespace-scoped CRDs |
| Req 7.2.5 — access-control system restricts privileges | Yes | Enables | RBAC bindings minimal; ADR 0004 rejects `kube-rbac-proxy` in favour of Linkerd mTLS |
| Req 8.2 — strong authentication | Yes | Enables | mTLS to PG; cross-namespace Secret refs rejected |
| Req 8.2.8 — idle-session timeout | N/A (for Keystone) | — | Not a session-handling component |
| Req 8.6.1 — credentials used in interactive sessions | Partial | Enables | `keystonectl` respects `KUBECONFIG`; no embedded credentials |
| Req 10.2 — audit logs reconstruct events | Yes | Enables | MigrationExecution + DriftReport + `schema_migrations` table provide per-change reconstruction |
| Req 10.2.1 — automated audit logs for access to system components | Yes | Enables | Controller-runtime logs + structured zap + `controller_runtime_*` metrics |
| Req 10.3 — audit logs are protected | Partial | Enables | Log export is the customer's log pipeline; Keystone ships logs as stdout/stderr per 12-factor |
| Req 10.4.1 — time-synchronised logs | Partial | Enables | `applied_at` column is UTC; customer syncs cluster time |
| Req 10.7 — timely detection, alerting, and addressing of failures | Yes | Enables | Multi-window burn-rate alerts per ADR 0014; `keystone_drift_staleness_seconds` gauge |
| **Req 10.7 — 4.0 broadening to change-detection** | Yes | Enables | The DriftController is the change-detection lever; DriftReport CRs record detected drift for PCI audit |
| Req 11.3.1 — external pentests | Yes | Meets | `docs/pentest-scope.md` — at least once per major release |
| Req 11.5 — change-detection mechanism | Yes | Enables | DriftController hourly fingerprint + DriftReport CR is exactly this |

**PCI DSS 4.0 callout:** v4.0 broadened Req 10 to explicitly require change
detection. The DriftController (`internal/drift/`) is the Keystone feature
that satisfies this — hourly fingerprint of every Ready DatabaseSchema,
diff against per-schema baseline, DriftReport CR + metric on mismatch. This
is a load-bearing feature for PCI customers specifically.

## GDPR — Article 32 and processor obligations

Keystone's GDPR posture depends on whether HexxLock is a processor or a
sub-processor for a given deployment:

- **OSS operator, customer-run**: no HexxLock involvement → no GDPR
  relationship with HexxLock. The customer is the controller / processor
  vis-à-vis their own users.
- **HexxLock Cloud (when it ships — Phase 15+)** against customer databases
  that contain PII: HexxLock becomes a processor (or sub-processor) and
  GDPR Art. 28 and Art. 32 apply. A Data Processing Agreement (DPA) is
  required before onboarding.

GDPR-relevant technical measures under Art. 32 (security of processing):

| Art. 32 requirement | Keystone role | Implementation |
|---|---|---|
| (1)(a) pseudonymisation and encryption | Enables | mTLS to PG (Linkerd); Secret-ref model (no embedded creds) |
| (1)(b) ongoing confidentiality, integrity, availability, and resilience | Enables | DriftController integrity-check; RolloutPolicy availability via staged rollout; chaos-test recovery |
| (1)(c) ability to restore availability and access in a timely manner | Partial | Upstream PG backup / CNPG recovery is out of scope; Keystone resumes idempotently on restart |
| (1)(d) regular testing | Meets | `docs/pentest-scope.md` + CI `make verify` |
| (2) assessment of risk | Meets (project-side) | ADR series + `docs/pentest-scope.md` |
| Art. 28 processor contract | N/A today | DPA applies only if HexxLock Cloud is in the picture; DPA lives as a legal document (not markdown) — customers requesting one email `maintainers@hexxlock.com` |

**Data Processing Agreement**: the DPA template is a legal document held
outside this repo (it is not a markdown file). When HexxLock Cloud ships,
the DPA reference will appear in the Cloud onboarding documentation and a
link will be added here. For now, OSS users have no DPA relationship with
HexxLock.

## Audit-artefact trail

Where evidence for the above lives. This is the list a SOC 2 / ISO 27001
auditor should expect to receive from HexxLock when evaluating the
managed-tier product, and a customer should expect to assemble themselves
for the OSS tier.

| Artefact | Location | Format | Retention |
|---|---|---|---|
| SBOM | `bin/keystone-sbom.spdx.json`; OCI referrer on every release image | SPDX JSON | Per release, indefinitely via image tags |
| Signed images | `ghcr.io/dogukanturhal/keystone-manager:<tag>` and `github.com/hexxlock/keystone` once mirrored | cosign-keyless, OIDC from GitLab CI | Per release, indefinitely |
| Vulnerability scan reports | GitLab CI artefacts per release; `make vuln-scan` regenerates | Trivy JSON | Per release |
| Pentest report | Held by `security@hexxlock.com`; attestation letter customer-shareable, full report under NDA | PDF + JSON / SARIF | Per engagement (~yearly) |
| Audit logs (operator) | Kubernetes cluster log pipeline (customer-owned) | stdout/stderr JSON via zap | Customer-controlled |
| Audit logs (data plane) | `schema_migrations` table rows + DriftReport CRs | SQL + YAML | As long as the DB exists |
| ADR log — decisions | `docs/adrs/` in git | Markdown | Git history |
| Change history | Git commit log, DCO-signed | Git | Indefinitely |
| SLO evidence | Prometheus + Grafana `grafana.example.com/d/keystone-slo` per ADR 0014 | TSDB | Customer-controlled retention |
| Admission rejections | Kubernetes audit log + Kyverno policy reports | JSON | Customer-controlled |

For SOC 2 CC7.2 and ISO 27001 A.8.16 specifically, the evidence package
combines: the `keystone_drift_*` metric family recorded in Prometheus, the
multi-window burn-rate alert rules from ADR 0014, the admission-webhook
rejection record (ADR 0011), and the DriftReport CRs generated by the
DriftController. That bundle is what a reviewer asking "how do you know you
would detect a schema change?" gets handed.

## Revision history

| Date | Change | Author |
|---|---|---|
| 2026-04-16 | Initial draft. Mapped SOC 2 CC1–CC9 + relevant A/PI/C callouts, ISO 27001:2022 Annex A technological controls, HIPAA §164.308/.312, PCI DSS v4.0.1 Req 6/7/8/10/11, GDPR Art. 32. | Project Lead |
