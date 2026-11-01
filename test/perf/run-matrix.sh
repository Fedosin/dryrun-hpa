#!/usr/bin/env bash
# Measure the controller's CPU and memory over a matrix of object counts and
# DryrunHPA parameters. Run test/perf/setup.sh on the cluster first.
#
# For every point of COUNTS x REPLICAS x METRICS x BEHAVIOR it:
#   1. deletes the previous load and restarts the controller (clean heap),
#   2. creates COUNT Deployments with DryrunHPAs on kwok nodes,
#   3. waits until every DryrunHPA has its VirtualWorkload, then WARMUP,
#   4. samples the controller's /metrics for DURATION,
# and finally writes a Markdown report with per-object cost fits.
#
#   COUNTS="0 100 500 1000" METRICS="1 3" test/perf/run-matrix.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
COUNTS=${COUNTS:-0 50 100 250 500}
REPLICAS=${REPLICAS:-3}
METRICS=${METRICS:-1}
BEHAVIOR=${BEHAVIOR:-false}
WARMUP=${WARMUP:-2m}
DURATION=${DURATION:-5m}
INTERVAL=${INTERVAL:-15s}
PROFILES=${PROFILES:-false}
RUN=${RUN:-perf}
CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE:-dryrun-hpa-system}
READY_TIMEOUT=${READY_TIMEOUT:-900}
RESULTS=${RESULTS:-${ROOT}/test/perf/results/$(date -u +%Y%m%dT%H%M%SZ)}

mkdir -p "${RESULTS}"
LOADGEN=${LOADGEN:-${ROOT}/bin/loadgen}
if [[ ! -x "${LOADGEN}" || -n "$(find "${ROOT}/cmd/loadgen" -newer "${LOADGEN}" -name '*.go' 2>/dev/null)" ]]; then
  (cd "${ROOT}" && CGO_ENABLED=0 go build -o "${LOADGEN}" ./cmd/loadgen)
fi

log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }

restart_controller() {
  kubectl -n "${CONTROLLER_NAMESPACE}" rollout restart deployment dryrun-hpa-controller >/dev/null
  kubectl -n "${CONTROLLER_NAMESPACE}" rollout status deployment dryrun-hpa-controller --timeout=5m >/dev/null
}

wait_virtual_workloads() {
  local want=$1 deadline=$((SECONDS + READY_TIMEOUT)) have
  while :; do
    have=$(kubectl get virtualworkloads.dryrun-hpa.io -A --no-headers 2>/dev/null | wc -l)
    ((have >= want)) && return 0
    if ((SECONDS > deadline)); then
      log "timed out: ${have}/${want} VirtualWorkloads"
      return 1
    fi
    log "waiting for VirtualWorkloads: ${have}/${want}"
    sleep 10
  done
}

cleanup() { "${LOADGEN}" delete --run "${RUN}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

{
  echo "context=$(kubectl config current-context)"
  echo "controller=$(kubectl -n "${CONTROLLER_NAMESPACE}" get deployment dryrun-hpa-controller -o jsonpath='{.spec.template.spec.containers[0].image}')"
  echo "counts=${COUNTS} replicas=${REPLICAS} metrics=${METRICS} behavior=${BEHAVIOR}"
  echo "warmup=${WARMUP} duration=${DURATION} interval=${INTERVAL}"
} >"${RESULTS}/params.txt"

for behavior in ${BEHAVIOR}; do
  for metrics in ${METRICS}; do
    for replicas in ${REPLICAS}; do
      for count in ${COUNTS}; do
        point="count=${count},replicas=${replicas},metrics=${metrics},behavior=${behavior}"
        log "point ${point}"
        "${LOADGEN}" delete --run "${RUN}"
        restart_controller
        "${LOADGEN}" apply --run "${RUN}" --count "${count}" --replicas "${replicas}" \
          --metrics "${metrics}" --behavior="${behavior}"
        wait_virtual_workloads "${count}"
        log "warming up for ${WARMUP}"
        sleep "${WARMUP}"
        profile_args=()
        [[ "${PROFILES}" == true ]] && profile_args=(--profile-dir "${RESULTS}/profiles")
        log "sampling for ${DURATION}"
        "${LOADGEN}" sample --namespace "${CONTROLLER_NAMESPACE}" --duration "${DURATION}" \
          --interval "${INTERVAL}" --label "${point}" -o "${RESULTS}/samples.jsonl" "${profile_args[@]}"
      done
    done
  done
done

"${LOADGEN}" report "${RESULTS}/samples.jsonl" >"${RESULTS}/report.md"
log "report: ${RESULTS}/report.md"
cat "${RESULTS}/report.md"
