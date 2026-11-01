#!/usr/bin/env bash
# Copy generated artifacts into the Helm chart so it can't drift from config/:
# CRDs, the controller's RBAC rules, and the Grafana dashboards.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHART=${ROOT}/charts/dryrun-hpa

rm -rf "${CHART}/crds" "${CHART}/files"
mkdir -p "${CHART}/crds" "${CHART}/files/dashboards"
cp "${ROOT}"/config/crd/bases/*.yaml "${CHART}/crds/"
cp "${ROOT}/config/rbac/role.yaml" "${CHART}/files/controller-role.yaml"
cp "${ROOT}"/config/prometheus/dashboards/*.json "${CHART}/files/dashboards/"
