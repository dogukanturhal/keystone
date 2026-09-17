# Keystone maintainers

This file is the authoritative list of people with maintainer privileges on
the Keystone project. It pairs with [`GOVERNANCE.md`](GOVERNANCE.md), which
documents the role itself — responsibilities, nomination process, stepping
down, decision-making weight. This file is the roster; `GOVERNANCE.md` is
the rulebook.

`CODEOWNERS` in the repo root mirrors this list and is the mechanism CI uses
to require a maintainer review on MRs touching load-bearing paths
(`api/**`, `internal/controller/**`, `internal/postgres/**`,
`internal/webhook/**`, `config/crd/**`, `docs/adrs/**`). The two files must
stay in sync; a pre-commit hook under `hack/git-hooks/` (Phase 9.3 — planned)
will enforce this mechanically. Until then, sync is on the honour system
enforced by MR review.

CNCF graduation requires maintainer diversity across at least two
organisations (see CNCF TOC graduation criteria). Today Keystone has a
single-org maintainership under HexxLock. The `[OPEN]` row below is the
graduation-critical slot; recruiting a second-organisation maintainer is a
standing project goal, not a nice-to-have.

## Active maintainers

| GitHub handle | Name | Organisation | Focus area | Email | GPG fingerprint | Since |
|---|---|---|---|---|---|---|
| [`dogukanturhal`](https://github.com/dogukanturhal) | Doğukan Turhal | HexxLock | Project Lead — overall direction, CRD API, security review, release engineering | `dogukanturhal@hexxlock.com` | `[TBD — to be published at https://hexxlock.com/.well-known/keys/dogukanturhal.asc before v0.2.0]` | 2026-04 |
| `[OPEN — see GOVERNANCE.md §Maintainers to apply]` | — | _second organisation — not HexxLock_ | Any of: engine (`internal/postgres`, `internal/migration`), controller runtime, webhook chain, docs | — | — | — |

The `[OPEN]` slot is **explicitly reserved for a contributor from an
organisation other than HexxLock**. A second HexxLock maintainer would not
satisfy the CNCF diversity bar and is not what this slot is for. Path to
apply is documented in `GOVERNANCE.md §Maintainers` — three months of
substantive contribution, two existing-maintainer nominations, seven-day
no-objection window.

## Emeritus maintainers

| GitHub handle | Name | Organisation | Role | Active | Emeritus |
|---|---|---|---|---|---|

None yet. The project is too young to have anyone step down. Emeritus
maintainers retain commit credit and a named entry; their +1 on votes stops
counting from the date they move to this table.

## How to become a maintainer

See [`GOVERNANCE.md §Maintainers`](GOVERNANCE.md#maintainers). The short
version:

1. Submit substantive contributions for roughly three months. Substantive
   means code that changes behaviour, reviews that catch real issues, or
   documentation that unblocks other contributors. Drive-by typo fixes are
   welcome but do not count towards maintainership.
2. An existing maintainer nominates you by email to
   `maintainers@hexxlock.com` with a concrete list of contributions.
3. A second existing maintainer seconds the nomination.
4. A seven-day no-objection window opens on the `maintainers@hexxlock.com`
   thread. Any existing maintainer may block with a reasoned objection.
5. On successful vote, an MR lands adding your row to this file and your
   entry to `CODEOWNERS`, plus your public key to
   `https://hexxlock.com/.well-known/keys/`.

The project deliberately keeps the bar low relative to the commitment —
maintainership is work, not honour. Stepping back is a normal outcome, not
a failure (see `GOVERNANCE.md` on the no-drama exit policy).

## Contact

- Project-wide maintainer mailbox: `maintainers@hexxlock.com`
- Security reports: `security@hexxlock.com` (see [`SECURITY.md`](SECURITY.md))
- Code of conduct concerns: `conduct@hexxlock.com` (see
  [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md))

Individual maintainer emails are listed above for attribution and PGP
verification only — please use the shared mailboxes for anything project-
related so no single person becomes a bottleneck.

---

<!-- begin:maintainers-json -->
```json
{
  "schema_version": "1.0",
  "project": "keystone",
  "last_reviewed": "2026-04-16",
  "active": [
    {
      "github": "dogukanturhal",
      "name": "Doğukan Turhal",
      "organisation": "HexxLock",
      "focus": "Project Lead — overall direction, CRD API, security review, release engineering",
      "email": "dogukanturhal@hexxlock.com",
      "gpg_fingerprint": null,
      "since": "2026-04"
    },
    {
      "github": null,
      "name": null,
      "organisation": null,
      "focus": "OPEN — reserved for a non-HexxLock contributor; see GOVERNANCE.md §Maintainers",
      "email": null,
      "gpg_fingerprint": null,
      "since": null,
      "status": "open"
    }
  ],
  "emeritus": []
}
```
<!-- end:maintainers-json -->

The JSON block between the markers is machine-readable and authoritative for
any automation (release notes, CNCF annual review submissions, bot
assignment). Update both the table and the JSON in the same commit; CI will
fail the MR if they diverge (check implemented in `hack/check-maintainers.sh`
— Phase 9.3 planned).
