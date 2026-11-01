#!/usr/bin/env bash
# Load a locally built image into a kind cluster (works with podman and docker).
set -euo pipefail

KIND_CLUSTER=${1:?usage: kind-load.sh CLUSTER IMAGE}
IMG=${2:?usage: kind-load.sh CLUSTER IMAGE}

if [[ -z "${KIND_EXPERIMENTAL_PROVIDER:-}" ]] && command -v podman >/dev/null 2>&1; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

if [[ "${KIND_EXPERIMENTAL_PROVIDER:-docker}" == podman ]]; then
  archive=$(mktemp --suffix=.tar)
  trap 'rm -f "${archive}"' EXIT
  podman save --format docker-archive -o "${archive}" "${IMG}"
  kind load image-archive "${archive}" --name "${KIND_CLUSTER}"
else
  kind load docker-image "${IMG}" --name "${KIND_CLUSTER}"
fi
