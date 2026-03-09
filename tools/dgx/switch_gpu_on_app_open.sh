#!/usr/bin/env bash
set -euo pipefail

# Event-driven GPU profile switcher for DGX Spark single-GPU nodes.
# Intended to be called by an "app opened" hook.
#
# Usage:
#   sudo bash tools/dgx/switch_gpu_on_app_open.sh ollama
#   sudo bash tools/dgx/switch_gpu_on_app_open.sh comfyui
#
# Behavior:
# - Maps app -> GPU profile (ollama/comfyui).
# - Uses a lock file to prevent concurrent switches.
# - Enforces cooldown to avoid fast flip-flop.

APP_NAME="${1:-}"
FORCE="${FORCE:-false}"
COOLDOWN_SECONDS="${COOLDOWN_SECONDS:-180}"
STATE_DIR="${STATE_DIR:-/var/lib/olares-gpu-switch}"
LOCK_FILE="${STATE_DIR}/switch.lock"
LAST_PROFILE_FILE="${STATE_DIR}/last_profile"
LAST_TS_FILE="${STATE_DIR}/last_switch_ts"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWITCH_SCRIPT="${SCRIPT_DIR}/switch_gpu_app_profile.sh"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

if [[ -z "${APP_NAME}" ]]; then
  echo "usage: $0 <ollama|comfyui>"
  exit 1
fi

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: run as root (or with sudo)"
  exit 1
fi

if [[ ! -x "${SWITCH_SCRIPT}" ]]; then
  echo "error: switch script not found/executable: ${SWITCH_SCRIPT}"
  exit 1
fi

case "${APP_NAME}" in
  ollama) TARGET_PROFILE="ollama" ;;
  comfyui|comfyuishare|comfyuisharev2) TARGET_PROFILE="comfyui" ;;
  *)
    echo "error: unsupported app '${APP_NAME}', expected ollama|comfyui"
    exit 1
    ;;
esac

mkdir -p "${STATE_DIR}"

# shellcheck disable=SC2129
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
  log "another switch is in progress, skipping"
  exit 0
fi

NOW="$(date +%s)"
LAST_PROFILE=""
LAST_TS="0"

if [[ -f "${LAST_PROFILE_FILE}" ]]; then
  LAST_PROFILE="$(cat "${LAST_PROFILE_FILE}" || true)"
fi

if [[ -f "${LAST_TS_FILE}" ]]; then
  LAST_TS="$(cat "${LAST_TS_FILE}" || echo 0)"
fi

if [[ "${TARGET_PROFILE}" == "${LAST_PROFILE}" && "${FORCE}" != "true" ]]; then
  log "profile already '${TARGET_PROFILE}', no switch needed"
  exit 0
fi

DELTA=$((NOW - LAST_TS))
if (( DELTA < COOLDOWN_SECONDS )) && [[ "${FORCE}" != "true" ]]; then
  log "cooldown active (${DELTA}s < ${COOLDOWN_SECONDS}s), skipping switch to '${TARGET_PROFILE}'"
  exit 0
fi

log "switching GPU profile to '${TARGET_PROFILE}' (app open: ${APP_NAME})"
bash "${SWITCH_SCRIPT}" "${TARGET_PROFILE}"

echo "${TARGET_PROFILE}" > "${LAST_PROFILE_FILE}"
echo "${NOW}" > "${LAST_TS_FILE}"
log "switch completed"
