#!/usr/bin/env bash
# Undo test/perf/setup.sh: remove the load, kwok and fake-metrics-server, and
# reinstall metrics-server so real workloads (and the e2e tests) get metrics
# again. The controller stays deployed with config/perf.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
LOADGEN=${LOADGEN:-${ROOT}/bin/loadgen}
RUN=${RUN:-perf}

if [[ -x "${LOADGEN}" ]]; then
  "${LOADGEN}" delete --run "${RUN}"
fi
kubectl delete nodes -l type=kwok --ignore-not-found
kubectl delete -k "${ROOT}/test/perf/fake-metrics-server" --ignore-not-found
kwok_url="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"
kubectl delete -f "${kwok_url}/stage-fast.yaml" --ignore-not-found
kubectl delete -f "${kwok_url}/kwok.yaml" --ignore-not-found
"${ROOT}/hack/install-metrics-server.sh"
