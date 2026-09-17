# Keystone Governance

This document describes how decisions get made in the Keystone project.
It is intentionally lightweight while the project is small — expect
revisions as the community grows.

## Roles

### Contributors

Anyone who submits an issue, a merge request, a review, or a
substantive discussion comment. No formal onboarding. Contributors are
bound by the DCO (`CONTRIBUTING.md`) and the Code of Conduct
(`CODE_OF_CONDUCT.md`).

### Maintainers

Maintainers own the project's direction, review and merge MRs, release
versions, and enforce community standards. They are expected to spend
meaningful time on the project each week.

**Becoming a maintainer** requires:

1. Three months of substantive contribution (code, reviews, or
   documentation — measured by impact, not commit count)
2. Two existing maintainers supporting the nomination
3. No objection from other maintainers within 7 days of the nomination
   thread

Stepping down is by email to `maintainers@hexxlock.com` — no drama, no
reputation cost. Emeritus maintainers keep commit credit; their +1 on
votes stops counting.

**Current maintainers** (as of 2026-04-16):

| Name | Email | Role |
|---|---|---|
| Doğukan Turhal (`dogukanturhal`) | dogukanturhal@hexxlock.com | Project Lead |
| HexxLock Platform Team | maintainers@hexxlock.com | Core maintainers |

A live list is mirrored in `MAINTAINERS` at the repo root.

### Project Lead

One maintainer holds the lead role — tiebreaker on contentious
technical decisions, public representation, and responsibility for
security incident coordination. The lead is elected annually by a
simple majority of maintainers.

## Decision-making

**Default: lazy consensus.** An MR or proposal that sits for 7 days
without objection from a maintainer is considered approved. Most
day-to-day work happens here.

**Contentious changes** — anything that touches the CRD API surface,
the license, governance, or marked `[vote]` by a maintainer — go to a
vote. Votes are:

- Open for 7 calendar days (14 for API-breaking or governance changes)
- Decided by simple majority of maintainer +1 votes (ties go to the
  Project Lead)
- Recorded in an ADR under `docs/adrs/`

**Architecture decisions** follow the process documented in
`docs/adrs/README.md`. Every load-bearing decision gets an ADR;
reviewers push back on "just trust me" reasoning.

## Release cadence

- **Minor releases** target every 6 weeks. Scope is cut two weeks
  before; only bug fixes land in the stabilisation window.
- **Patch releases** ship ad-hoc for security fixes and severe
  regressions. Target: within 7 days of a fix landing on the default
  branch.
- **Major releases** (breaking the CRD API) are rare, require a vote,
  and ship with a full migration guide + at least one minor cycle of
  deprecation warnings.

Every release carries SBOM + CVE scan + cosign signature (see
`SECURITY.md`).

## Scope and sponsorship

HexxLock sponsors the core maintainer time today. The project stays
**vendor-neutral in direction**: no feature is merged solely because
HexxLock wants it; no feature is blocked solely because HexxLock
doesn't.

If the project is donated to a foundation (CNCF is the near-term
target), governance migrates to whatever that foundation requires.
HexxLock-specific decisions (commercial tiers, trademark policy) will
remain outside the OSS project's governance.

## Commercial work

A paid cloud tier is on the roadmap. It is built by HexxLock in a
separate, non-open repository. The boundary is strict:

- OSS must remain fully functional without the paid tier. The paid
  tier cannot hide features behind a paywall; it can only add features
  around the OSS core (hosted UI, managed CI, SaaS drift monitoring).
- Paid-tier code never lands in this repository.
- Paid-tier customer support does not entitle customers to
  prioritisation over OSS contributors in the issue tracker.

## Amending this document

Changes to `GOVERNANCE.md` require a vote (see Decision-making). The
resulting ADR must describe the motivation and what changed. This
document is not frozen — it is expected to evolve — but every change
is auditable.
