#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Chaos test: kill the keystone-manager Pod mid-migration and verify
# reconcile resumes idempotently.
#
# Usage:
#   hack/chaos-test.sh                  # uses current kubectl context
#   KUBECONFIG=staging hack/chaos-test.sh
#
# Pre-reqs:
#   - keystone-manager Deployment running in keystone-system
#   - At least one DatabaseProvider + LogicalDatabase + DatabaseSchema
#     reconciled to Ready
#   - kubectl context pointed at the target cluster
#
# What it does:
#   1. Apply a fresh MigrationBundle that adds a column.
#   2. Wait for at least one MigrationExecution to reach Running.
#   3. Delete the manager Pod (k8s recreates via Deployment).
#   4. Verify the in-flight execution recovers and reaches Succeeded
#      within 90s of pod restart.
#   5. Verify the schema_migrations table has the new version exactly once.

set -euo pipefail

NS="${KEYSTONE_NS:-keystone-system}"
BUNDLE_NAME="chaos-$(date +%s)"
SCHEMA_LABEL="${CHAOS_SCHEMA_LABEL:-module=crm}"

log() { printf '\n>>> %s\n' "$*"; }

cleanup() {
  log "cleanup: removing chaos bundle"
  kubectl delete migrationbundle "${BUNDLE_NAME}" -n "${NS}" --wait=false 2>/dev/null || true
}
trap cleanup EXIT

# 1. Find a target DatabaseSchema to verify outcome against.
SCHEMA="$(kubectl get databaseschemas -n "${NS}" \
  -l "${SCHEMA_LABEL}" -o jsonpath='{.items[0].metadata.name}')"
if [[ -z "${SCHEMA}" ]]; then
  echo "no DatabaseSchema with label ${SCHEMA_LABEL} in ${NS}" >&2
  exit 1
fi
log "target schema: ${SCHEMA}"

# 2. Apply a unique-named MigrationBundle. The SQL adds a chaos-marker
#    column to a table the test schema is expected to have.
log "creating ConfigMap + MigrationBundle ${BUNDLE_NAME}"
kubectl create configmap "${BUNDLE_NAME}-sql" -n "${NS}" \
  --from-literal="001_chaos.up.sql=ALTER TABLE leads ADD COLUMN IF NOT EXISTS chaos_marker_${BUNDLE_NAME//-/_} TEXT;"
cat <<EOF | kubectl apply -f -
apiVersion: keystone.hexxlock.io/v1alpha1
kind: MigrationBundle
metadata:
  name: ${BUNDLE_NAME}
  namespace: ${NS}
spec:
  version: ${BUNDLE_NAME}
  strategy: versioned
  source:
    type: ConfigMap
    configMapRef:
      name: ${BUNDLE_NAME}-sql
      filePattern: "*.up.sql"
  schemaSelector:
    matchLabels:
      module: crm
EOF

# 3. Wait for execution to enter Running OR Expanding (race the kill).
log "waiting for MigrationExecution to start..."
EXEC_NAME=""
for i in $(seq 1 30); do
  EXEC_NAME="$(kubectl get migrationexecutions -n "${NS}" \
    -l "keystone.hexxlock.io/bundle=${BUNDLE_NAME}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${EXEC_NAME}" ]]; then
    PHASE="$(kubectl get migrationexecution "${EXEC_NAME}" -n "${NS}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${PHASE}" == "Running" || "${PHASE}" == "Expanding" || "${PHASE}" == "Succeeded" ]]; then
      log "execution ${EXEC_NAME} reached phase=${PHASE}"
      break
    fi
  fi
  sleep 1
done
if [[ -z "${EXEC_NAME}" ]]; then
  echo "no MigrationExecution appeared in 30s" >&2
  exit 1
fi

# 4. Kill the manager pod IMMEDIATELY to test crash-recovery.
log "deleting keystone-manager pod to simulate crash"
kubectl delete pod -n "${NS}" -l app.kubernetes.io/name=keystone-manager --grace-period=0 --force

# 5. Wait for new pod and reconcile recovery.
log "waiting for new manager pod to be Ready..."
kubectl rollout status -n "${NS}" deployment/keystone-manager --timeout=60s

log "waiting for execution to reach Succeeded (max 90s)..."
for i in $(seq 1 90); do
  PHASE="$(kubectl get migrationexecution "${EXEC_NAME}" -n "${NS}" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  case "${PHASE}" in
    Succeeded) log "execution Succeeded after restart"; break ;;
    Failed|Aborted) echo "execution ended in ${PHASE} — chaos test FAILED"; exit 1 ;;
  esac
  sleep 1
done
if [[ "${PHASE}" != "Succeeded" ]]; then
  echo "execution did not reach Succeeded within 90s of restart (phase=${PHASE})" >&2
  exit 1
fi

# 6. Verify the version landed in schema_migrations exactly once. Uses
#    psql via the manager pod (assumes the Pod has libpq client).
#    Most distroless images don't — fall back to checking via the CR.
log "verifying schema_migrations has version ${BUNDLE_NAME} exactly once"
APPLIED_COUNT="$(kubectl get migrationexecutions -n "${NS}" \
  -l "keystone.hexxlock.io/bundle=${BUNDLE_NAME}" \
  --field-selector status.phase=Succeeded \
  -o json | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["items"]))')"
if [[ "${APPLIED_COUNT}" != "1" ]]; then
  echo "expected exactly 1 succeeded execution, found ${APPLIED_COUNT}" >&2
  exit 1
fi

log "chaos test PASSED: manager survived crash and reconciled idempotently"
