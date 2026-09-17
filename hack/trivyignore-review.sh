#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# trivyignore-review — ask whether .trivyignore.yaml can shrink.
#
# Every entry in .trivyignore.yaml is a CVE inside an upstream binary
# (kubectl, cosign) that we download rather than build. They clear only
# when upstream ships a release rebuilt against the patched library, and
# nothing in this repo can make that happen. Left alone the entries sit
# until their expiry date fails a release pipeline — which is a rude way
# to be told "you could have removed three of these six weeks ago".
#
# So this runs weekly instead: fetch the newest upstream binaries, scan
# them WITHOUT the ignore file, and diff the result against the list.
#
# Trivy is the oracle, deliberately. The alternative is a hand-maintained
# table of "CVE-X needs golang.org/x/net >= 0.55.0", which is a second
# copy of the vulnerability database that goes stale silently. Asking the
# scanner what it finds today cannot go stale.
#
# Four things it reports:
#
#   CLEARABLE  listed, but the newest upstream release no longer trips it
#              -> delete the entry, bump the ARG in Dockerfile.tools
#   NEEDED     listed and still tripped -> leave alone
#   UNLISTED   found but not listed -> the gate WILL fail when the image
#              is next rebuilt; better to learn now than mid-release
#   EXPIRING   within EXPIRY_WARN_DAYS of expired_at -> review or extend
#
# Exits non-zero when any of CLEARABLE / UNLISTED / EXPIRING is non-empty,
# so a scheduled pipeline turns red while there is still time to act.
# A clean week is silent and green.
#
# Usage:  hack/trivyignore-review.sh [--repo-root DIR]

set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
EXPIRY_WARN_DAYS="${EXPIRY_WARN_DAYS:-30}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

IGNORE_FILE="$REPO_ROOT/.trivyignore.yaml"
DOCKERFILE="$REPO_ROOT/Dockerfile.tools"

[ -f "$IGNORE_FILE" ] || { echo "no $IGNORE_FILE — nothing to review"; exit 0; }

# Versions currently pinned, so the report can say whether a bump exists.
# sed, not `grep -oP`: this runs in the trivy image, which is Alpine, and
# BusyBox grep has no PCRE support. `grep -oP` works on a dev laptop and
# exits 2 in CI — the job would fail before it scanned anything.
cur_kubectl=$(sed -n 's/^ARG KUBECTL_VERSION=\([^ ]*\).*/\1/p' "$DOCKERFILE" | tail -1)
cur_cosign=$(sed -n 's/^ARG COSIGN_VERSION=\([^ ]*\).*/\1/p' "$DOCKERFILE" | tail -1)

echo "==> pinned:  kubectl $cur_kubectl   cosign $cur_cosign"

latest_kubectl=$(curl -sSL --max-time 60 https://dl.k8s.io/release/stable.txt)
# Written to a file rather than piped: `grep -m1` exits on the first match
# and closes the pipe, curl dies of SIGPIPE with exit 23, and pipefail
# turns a successful lookup into a failed script.
curl -sSL --max-time 60 -o "$WORK/cosign-release.json" \
  https://api.github.com/repos/sigstore/cosign/releases/latest
latest_cosign=$(grep -m1 '"tag_name"' "$WORK/cosign-release.json" | cut -d'"' -f4)

echo "==> latest:  kubectl $latest_kubectl   cosign $latest_cosign"

# Scan the NEWEST upstream binaries, not the pinned ones: the question is
# "would upgrading clear anything", which the pinned versions cannot answer.
mkdir -p "$WORK/bin"
curl -sSL --max-time 300 -o "$WORK/bin/kubectl" \
  "https://dl.k8s.io/release/${latest_kubectl}/bin/linux/amd64/kubectl"
curl -sSL --max-time 300 -o "$WORK/bin/cosign" \
  "https://github.com/sigstore/cosign/releases/download/${latest_cosign}/cosign-linux-amd64"
chmod +x "$WORK/bin/"*

# `rootfs`, not `fs`. Both accept a directory, but only rootfs runs the
# analyzers that identify a stray Go binary — `fs` reported ZERO findings
# for the identical binaries that vuln-scan-tools-image finds eight in,
# which would have made this job cheerfully advise deleting every entry
# in the list. Verified against the pinned trivy version, not latest:
# the report is only useful if its oracle is the same scanner as the gate.
#
# No --ignorefile here on purpose: we want the raw truth to diff against.
# Thresholds mirror vuln-scan-tools-image so the two agree on what counts.
trivy rootfs --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed \
  --format json --output "$WORK/scan.json" "$WORK/bin" >/dev/null 2>&1

# tee'd so the CI job can keep the report as an artifact: a scheduled job
# that goes red is only useful if the reasoning survives log expiry.
# pipefail carries python's exit status through the pipe.
REPORT="${REPORT:-$REPO_ROOT/trivyignore-review.txt}"
python3 - "$IGNORE_FILE" "$WORK/scan.json" "$EXPIRY_WARN_DAYS" \
  "$cur_kubectl" "$latest_kubectl" "$cur_cosign" "$latest_cosign" <<'PY' | tee "$REPORT"
import json, sys, datetime, re

ignore_file, scan_file, warn_days = sys.argv[1], sys.argv[2], int(sys.argv[3])
cur_k, new_k, cur_c, new_c = sys.argv[4:8]

# Deliberately not PyYAML: the trivy image has no pip, and the file's
# shape is fixed and simple. Only `- id:` and `expired_at:` are needed.
listed = {}
cur_id = None
for line in open(ignore_file):
    m = re.match(r'\s*-\s+id:\s*(\S+)', line)
    if m:
        cur_id = m.group(1).strip('"\'')
        listed[cur_id] = None
        continue
    m = re.match(r'\s*expired_at:\s*(\S+)', line)
    if m and cur_id:
        listed[cur_id] = m.group(1).strip('"\'')

found = set()
installed = {}          # PkgName -> InstalledVersion, harvested from findings
scan = json.load(open(scan_file))
for res in scan.get('Results') or []:
    for v in res.get('Vulnerabilities') or []:
        found.add(v['VulnerabilityID'])
        if v.get('PkgName') and v.get('InstalledVersion'):
            installed.setdefault(v['PkgName'], v['InstalledVersion'])

# ---------------------------------------------------------------------------
# Second opinion before recommending a deletion.
#
# "Trivy is the oracle" (see header) holds for *finding* things — a scanner
# that reports a CVE is evidence the CVE is reachable. It does NOT hold for
# the absence of one: Trivy's DB can simply lag, and then `listed - found`
# quietly recommends deleting a still-valid exception.
#
# That is not hypothetical. On 2026-08-04 this script classified
# CVE-2026-33814 as CLEARABLE in CI while the identical script, in the same
# aquasec/trivy:0.58.2 image, against the same binaries, classified it STILL
# NEEDED minutes apart — same scanner version, different DB snapshot.
# GO-2026-4918 meanwhile declares golang.org/x/net affected from 0 up to
# 0.53.0, and kubectl v1.36.3 demonstrably vendors 0.49.0.
#
# Acting on that verdict deletes a real exception, and vuln-scan-tools-image
# (allow_failure:false, runs on default branch and tags) then goes red on the
# first scan that DOES see the CVE — which is precisely the seven-week blind
# gate this file exists to prevent.
#
# So a candidate is only truly CLEARABLE if the authoritative advisory agrees.
# vuln.go.dev / OSV is not a hand-maintained second copy — it is upstream's
# own database, queried live, so it cannot go stale the way a table in this
# repo would. Anything we cannot positively confirm is reported separately and
# explicitly NOT recommended for deletion.
# ---------------------------------------------------------------------------
def _osv(path, payload=None):
    import urllib.request
    req = urllib.request.Request(
        "https://api.osv.dev/v1/" + path,
        data=json.dumps(payload).encode() if payload is not None else None,
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.load(r)

def still_affected(cve):
    """(verdict, why). True = the advisory still covers a version we ship.

    Query by PACKAGE+VERSION rather than reading ranges out of the CVE record
    and comparing them here. Two reasons:
      * OSV does the range arithmetic, so this script never owns a version
        comparator that can be subtly wrong about pre-releases or +incompat.
      * The CVE-numbered record is often a thin NVD conversion carrying only a
        GIT commit range and no package at all — CVE-2026-33814 is exactly
        that. The ecosystem range lives in the alias (GO-2026-4918). Querying
        by package returns whichever record actually models Go, so aliases are
        matched instead of missed.
    """
    aliases = {cve}
    try:
        rec = _osv("vulns/%s" % cve)
        if rec.get('withdrawn'):
            return False, "withdrawn upstream %s" % rec['withdrawn']
        aliases |= set(rec.get('aliases') or [])
        aliases.add(rec.get('id'))
    except Exception:
        pass        # thin/absent CVE record is fine; the package query decides

    if not installed:
        return True, "no package inventory in scan — cannot confirm"
    for pkg, ver in sorted(installed.items()):
        try:
            res = _osv("query", {"package": {"name": pkg, "ecosystem": "Go"},
                                 "version": str(ver).lstrip('vV')})
        except Exception as e:
            return True, "OSV lookup failed (%s) — assuming still needed" % type(e).__name__
        for v in res.get('vulns') or []:
            ids = {v.get('id')} | set(v.get('aliases') or [])
            if ids & aliases:
                return True, "%s %s still in range per %s" % (pkg, ver, v.get('id'))
    return False, "no advisory covers any version we ship"

clearable, unconfirmed = [], []
for cve in sorted(set(listed) - found):
    affected, why = still_affected(cve)
    (unconfirmed if affected else clearable).append("%s  (%s)" % (cve, why))

needed    = sorted(set(listed) & found)
unlisted  = sorted(found - set(listed))

today = datetime.date.today()
expiring = []
for cve, exp in listed.items():
    if not exp:
        continue
    try:
        d = datetime.date.fromisoformat(exp)
    except ValueError:
        continue
    left = (d - today).days
    if left <= warn_days:
        expiring.append((cve, exp, left))
expiring.sort(key=lambda x: x[2])

def show(title, rows):
    print("\n== %s (%d)" % (title, len(rows)))
    for r in rows:
        print("   %s" % r)

print("\n================ .trivyignore.yaml review ================")
print("kubectl  pinned %s  latest %s%s" % (cur_k, new_k, "   <-- bump available" if cur_k != new_k else ""))
print("cosign   pinned %s  latest %s%s" % (cur_c, new_c, "   <-- bump available" if cur_c != new_c else ""))

show("CLEARABLE — Trivy no longer trips these AND the upstream advisory agrees; delete the entry and bump the ARG", clearable)
show("STILL NEEDED — upstream has not rebuilt yet; leave alone", needed)
show("UNCONFIRMED — Trivy stopped reporting these but the upstream advisory still covers what we ship. DO NOT DELETE: the DB is lagging, not fixed", unconfirmed)
show("UNLISTED — will FAIL vuln-scan-tools-image on the next image rebuild", unlisted)
show("EXPIRING within %d days — review or extend expired_at" % warn_days,
     ["%s  expires %s (%d days)" % (c, e, d) for c, e, d in expiring])

# `unconfirmed` deliberately does NOT make the job red. There is nothing an
# operator can do about Trivy's DB lagging behind vuln.go.dev, and a job that
# is permanently red for a non-actionable reason is the same blind gate as
# allow_failure:true — which is what this whole file exists to undo.
action = bool(clearable or unlisted or expiring)
print("\n---------------------------------------------------------")
if action:
    print("ACTION REQUIRED — see above. Procedure: .trivyignore.yaml header.")
else:
    print("No action: the list is minimal and nothing expires soon.")
sys.exit(1 if action else 0)
PY
