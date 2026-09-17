# Keystone adopters

This file lists organisations running Keystone in production, staging, or
committed evaluation. It exists for two reasons:

1. **CNCF incubation signal.** The CNCF TOC's incubation application template
   requires "at least three publicly-documented adopters successfully using the
   project" (see the CNCF Incubation application template under
   `cncf/toc` on GitHub). Sandbox entry has no hard adopter bar; incubation
   does. This list is the evidence.
2. **Case-study material.** New evaluators want to see who else took the bet.
   A reviewable list beats a marketing page that nobody updates.

Entries are additive, not curated — if you meet the criteria below and email
us, you get added. We do not rank or feature adopters.

## Criteria for listing

- You run Keystone against a real PostgreSQL cluster (not a lab-only kick of
  the tyres).
- You can be contacted — a handle or a shared mailbox, not an anonymous note.
- You are willing to be cited in CNCF applications, conference talks, and
  case studies. Public is public.
- You describe your use case in one or two sentences so reviewers understand
  the shape of the deployment.

If you are running Keystone but cannot go public (regulated sector, NDA), email
`maintainers@hexxlock.com` anyway — we keep a private roster for our own
roadmap prioritisation and you will not appear in this file.

## Adopters

| Organisation | Status | Use case | Since | Contact | Notes |
|---|---|---|---|---|---|
| HexxLock Platform (Example Service) | Adopting, Q2 2026 | PostgreSQL schema management for the Example Service IAM service; first in-flight cutover from the legacy `db-migrator` / `db-provisioner` stack | 2026-04 | `platform@hexxlock.com` | Cutover planned this quarter; not yet fully migrated. |
| _[Pending — contact maintainers@hexxlock.com to be added]_ | — | — | — | — | — |
| _[Pending — contact maintainers@hexxlock.com to be added]_ | — | — | — | — | — |

**Legend for status**:

- _Evaluating_ — running Keystone against a non-production DB with committed
  intent to roll out.
- _Adopting_ — cutover in flight against production or staging; not yet the
  only migration tool in the path.
- _Production_ — Keystone is the canonical schema-management tool for at
  least one production PostgreSQL workload.

The HexxLock Platform entry is `Adopting` rather than `Production` on purpose.
Example Service has not yet retired its embedded `.Migrate()` calls (Phase 3.2 of
the roadmap); until it does, the honest label is Adopting. The entry will
advance to `Production` when Example Service's legacy paths are removed and the
`services/db-migrator` / `services/db-provisioner` deployments are scaled to
zero (Phase 4.2 + 8.1).

## How to be added

Email `maintainers@hexxlock.com` with:

```
Subject: [ADOPTERS] <Organisation> — Keystone adopter entry

Organisation: <public name>
Status:       <evaluating | adopting | production>
Use case:     <one or two sentences>
Since:        <YYYY-MM>
Contact:      <shared mailbox or handle>
Notes:        <optional — engine version, cluster count, shard count>
```

We will:

1. Reply to confirm receipt within five business days.
2. Open an MR adding your row, tagging you for a review +1 so you can approve
   the exact wording before it lands.
3. Merge once you approve. No wordsmithing without your sign-off.

### What gets verified

Per the CNCF TOC incubation application template, adopters should be
"publicly documented and verifiable". We take that to mean:

- Your entry points at something a TOC reviewer can corroborate — a public
  GitHub repo, a conference talk, a blog post, a support ticket, or simply an
  email reply from your listed contact confirming the deployment exists.
- Maintainers do not ask for proprietary details (cluster sizes, revenue,
  internal topology). We only ask enough to know the entry is real.
- The verification is lightweight and not notarised. If a reviewer asks, we
  forward their question to your listed contact and let you answer directly.

### Removal and opt-out policy

Email `maintainers@hexxlock.com` with subject `[ADOPTERS] remove <Organisation>`
and we will open an MR removing the row. No questions asked, no reason
required, no waiting period. The MR will merge on lazy consensus within seven
days (per `GOVERNANCE.md`); if you need a same-day removal, say so and we
will expedite.

If an entry becomes stale (contact bounces, repo disappears, the listed use
case demonstrably no longer exists) maintainers may move the row to a
`## Former adopters` section after emailing the listed contact twice over
30 days with no response. We do not delete history.

## Former adopters

None yet.

---

Last reviewed: 2026-04-16. Reviews are expected every minor release (see
`GOVERNANCE.md §Release cadence`).
