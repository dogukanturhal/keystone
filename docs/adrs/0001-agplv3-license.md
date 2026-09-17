# 0001. Dual license: AGPL-3.0 operator + Apache-2.0 SDK

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Keystone is intended to ship as open source with a commercial cloud offering.
Every OSS infrastructure company that chose Apache 2.0 in the 2010s
(Elasticsearch, MongoDB, Redis, HashiCorp) has been cloud-strip-mined by AWS
and reactively changed to a restrictive source-available license years later
— always with significant community damage. The Grafana Labs 2021 move to
AGPL-3.0 is the data point that shows AGPL works without triggering the same
backlash, because network distribution of modifications is the *only* clause
it actually adds.

On the flip side, a pure AGPL-3.0 stance makes integration hard: third-party
operators (Crossplane providers, Terraform providers, Helm chart authors) that
link against Keystone's public types inherit AGPL, which is a non-starter for
commercial consumers.

## Decision

We dual-license:
- The operator, engine, and everything required to run Keystone in a cluster
  is **AGPL-3.0-or-later**. See `services/keystone/LICENSE`.
- The public SDK at `services/keystone/pkg/sdk/` and the CRD Go types
  imported by third parties are **Apache-2.0**. See
  `services/keystone/LICENSE-Apache-SDK`.

Contributors sign off under DCO (not CLA); the dual license is explicit in
`NOTICE` and in every source file's SPDX header.

## Consequences

Easier: AWS (or any hyperscaler) offering "Amazon Keystone" as a managed service
must open-source their modifications, making that business unprofitable.
Easier: operator plug-ins can import `pkg/sdk` freely.
Easier: "Is this OSI-approved?" — yes (both licenses are).

Harder: running patched internal forks requires publishing those patches if
Keystone is served over a network. Intentional; this is the whole reason for
AGPL.

Harder: shipping Keystone as a library inside a proprietary application is
forbidden by AGPL. We don't want this — and the SDK split is exactly what
enables the legitimate integration cases.

## Alternatives considered

- **Apache-2.0 everywhere**: easiest for adoption but strip-minable by AWS.
- **Business Source License (BSL)**: source-available but not OSI-approved;
  HashiCorp's 2023 switch caused lasting community fragmentation.
- **SSPL** (MongoDB's choice): blocked by OSI; AWS responded by forking.
- **GPL-3.0** (not Affero): missing the network-distribution trigger, so SaaS
  providers could embed modifications without publishing.
- **Elastic License 2.0**: not OSI-approved; smaller community.
