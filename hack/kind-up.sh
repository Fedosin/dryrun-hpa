#!/usr/bin/env bash
# Create a kind cluster with metrics-server. Uses the podman provider when
# podman is available.
set -euo pipefail

KIND_CLUSTER=${KIND_CLUSTER:-dryrun-hpa}
METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION:-latest}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-}

if [[ -z "${KIND_EXPERIMENTAL_PROVIDER:-}" ]] && command -v podman >/dev/null 2>&1; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  args=(create cluster --name "${KIND_CLUSTER}" --wait 2m)
  [[ -n "${KIND_NODE_IMAGE}" ]] && args+=(--image "${KIND_NODE_IMAGE}")
  kind "${args[@]}"
fi
kubectl config use-context "kind-${KIND_CLUSTER}"

METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION} "$(dirname "${BASH_SOURCE[0]}")/install-metrics-server.sh"
echo "Cluster kind-${KIND_CLUSTER} is ready."
