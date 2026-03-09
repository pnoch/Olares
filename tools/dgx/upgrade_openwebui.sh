#!/usr/bin/env bash
set -euo pipefail

# Self-managed Open WebUI upgrader for Olares clusters.
# - Finds the Open WebUI deployment by Olares labels.
# - Resolves the latest beclab tag (or uses a user-provided tag/image).
# - Applies upgrade and waits for rollout.
# - Rolls back automatically if rollout fails.
#
# Usage examples:
#   sudo bash tools/dgx/upgrade_openwebui.sh
#   sudo bash tools/dgx/upgrade_openwebui.sh --tag v0.8.10
#   sudo bash tools/dgx/upgrade_openwebui.sh --image docker.io/beclab/open-webui-open-webui:v0.8.10
#   sudo TIMEOUT=900s bash tools/dgx/upgrade_openwebui.sh

K3S="k3s kubectl"
DEFAULT_REPO="docker.io/beclab/open-webui-open-webui"
TIMEOUT="${TIMEOUT:-420s}"

TARGET_TAG=""
TARGET_IMAGE=""
NAMESPACE=""
DEPLOYMENT=""

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

usage() {
  cat <<'EOF'
Usage:
  sudo bash tools/dgx/upgrade_openwebui.sh [--tag <vX.Y.Z>] [--image <full-image>] [--namespace <ns>] [--deployment <name>]

Options:
  --tag         Tag in beclab/open-webui-open-webui (for example: v0.8.10)
  --image       Full image reference (for example: docker.io/beclab/open-webui-open-webui:v0.8.10)
  --namespace   Override namespace discovery
  --deployment  Override deployment name (default: openwebui)

Defaults:
  - Without --tag/--image, resolves latest stable tag from Docker Hub.
  - Auto-discovers deployment via label: applications.app.bytetrade.io/raw-app-name=openwebui
EOF
}

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: run as root (sudo)"
  exit 1
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      TARGET_TAG="${2:-}"
      shift 2
      ;;
    --image)
      TARGET_IMAGE="${2:-}"
      shift 2
      ;;
    --namespace)
      NAMESPACE="${2:-}"
      shift 2
      ;;
    --deployment)
      DEPLOYMENT="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument '$1'"
      usage
      exit 1
      ;;
  esac
done

if [[ -n "${TARGET_TAG}" && -n "${TARGET_IMAGE}" ]]; then
  echo "error: use either --tag or --image, not both"
  exit 1
fi

if [[ -z "${NAMESPACE}" || -z "${DEPLOYMENT}" ]]; then
  discovered="$(${K3S} get deploy -A -l applications.app.bytetrade.io/raw-app-name=openwebui -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}')"
  if [[ -z "${discovered}" ]]; then
    echo "error: no Open WebUI deployment found by label"
    exit 1
  fi

  first="$(printf '%s\n' "${discovered}" | head -n1)"
  if [[ -z "${NAMESPACE}" ]]; then
    NAMESPACE="${first%%/*}"
  fi
  if [[ -z "${DEPLOYMENT}" ]]; then
    DEPLOYMENT="${first##*/}"
  fi
fi

resolve_latest_tag() {
  curl -fsSL 'https://hub.docker.com/v2/repositories/beclab/open-webui-open-webui/tags?page_size=100' \
    | jq -r '.results[].name' \
    | rg '^v[0-9]+\.[0-9]+\.[0-9]+$' \
    | sort -V \
    | tail -n1
}

if [[ -z "${TARGET_IMAGE}" ]]; then
  if [[ -z "${TARGET_TAG}" ]]; then
    log "Resolving latest stable tag from Docker Hub..."
    TARGET_TAG="$(resolve_latest_tag)"
  fi
  if [[ -z "${TARGET_TAG}" ]]; then
    echo "error: failed to resolve target tag"
    exit 1
  fi
  TARGET_IMAGE="${DEFAULT_REPO}:${TARGET_TAG}"
fi

current_image="$(${K3S} -n "${NAMESPACE}" get deploy "${DEPLOYMENT}" -o jsonpath='{.spec.template.spec.containers[0].image}')"
if [[ "${current_image}" == "${TARGET_IMAGE}" ]]; then
  log "Open WebUI already on target image: ${TARGET_IMAGE}"
  exit 0
fi

log "Upgrading ${NAMESPACE}/${DEPLOYMENT}"
log "Current image: ${current_image}"
log "Target image:  ${TARGET_IMAGE}"

${K3S} -n "${NAMESPACE}" set image "deployment/${DEPLOYMENT}" "openwebui=${TARGET_IMAGE}"

# Keep USER_AGENT aligned with image tag for easier diagnostics.
if [[ "${TARGET_IMAGE}" == *:* ]]; then
  resolved_tag="${TARGET_IMAGE##*:}"
  if [[ "${resolved_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    ${K3S} -n "${NAMESPACE}" set env "deployment/${DEPLOYMENT}" "USER_AGENT=OpenWebUI/${resolved_tag#v}" >/dev/null
  fi
fi

if ! ${K3S} -n "${NAMESPACE}" rollout status "deployment/${DEPLOYMENT}" --timeout="${TIMEOUT}"; then
  log "Rollout failed, performing rollback..."
  ${K3S} -n "${NAMESPACE}" rollout undo "deployment/${DEPLOYMENT}"
  ${K3S} -n "${NAMESPACE}" rollout status "deployment/${DEPLOYMENT}" --timeout="${TIMEOUT}" || true
  echo "error: upgrade failed and rollback executed"
  exit 1
fi

new_image="$(${K3S} -n "${NAMESPACE}" get deploy "${DEPLOYMENT}" -o jsonpath='{.spec.template.spec.containers[0].image}')"
ready="$(${K3S} -n "${NAMESPACE}" get deploy "${DEPLOYMENT}" -o jsonpath='{.status.readyReplicas}')"
replicas="$(${K3S} -n "${NAMESPACE}" get deploy "${DEPLOYMENT}" -o jsonpath='{.status.replicas}')"

log "Upgrade complete: ${new_image} (${ready}/${replicas} ready)"

