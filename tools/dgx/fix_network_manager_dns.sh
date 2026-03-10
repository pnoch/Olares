#!/usr/bin/env bash
set -euo pipefail

DNS_SERVERS="${DNS_SERVERS:-8.8.8.8 1.1.1.1}"
CONNECTION_NAME="${CONNECTION_NAME:-}"
RESTART_COREDNS="${RESTART_COREDNS:-true}"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

die() {
  log "ERROR: $*"
  exit 1
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    die "run as root (e.g. sudo $0)"
  fi
}

require_nmcli() {
  command -v nmcli >/dev/null 2>&1 || die "nmcli not found"
}

auto_detect_connection() {
  local line name device
  while IFS=: read -r name device; do
    [[ -z "${name}" ]] && continue
    [[ "${device}" == "lo" ]] && continue
    [[ "${device}" == tailscale* ]] && continue
    echo "${name}"
    return 0
  done < <(nmcli -t -f NAME,DEVICE connection show --active)
  return 1
}

main() {
  require_root
  require_nmcli

  if [[ -z "${CONNECTION_NAME}" ]]; then
    CONNECTION_NAME="$(auto_detect_connection || true)"
    [[ -n "${CONNECTION_NAME}" ]] || die "unable to auto-detect active NetworkManager connection; set CONNECTION_NAME=..."
  fi

  log "Using connection: ${CONNECTION_NAME}"
  log "Setting DNS servers: ${DNS_SERVERS}"

  nmcli connection modify "${CONNECTION_NAME}" \
    ipv4.ignore-auto-dns yes \
    ipv4.dns "${DNS_SERVERS}" \
    ipv6.ignore-auto-dns yes

  log "Reconnecting ${CONNECTION_NAME}..."
  nmcli connection down "${CONNECTION_NAME}" || true
  nmcli connection up "${CONNECTION_NAME}"

  log "Verifying host DNS..."
  getent hosts github.com >/dev/null 2>&1 || die "DNS resolution still failing for github.com"
  getent hosts huggingface.co >/dev/null 2>&1 || die "DNS resolution still failing for huggingface.co"
  getent hosts pypi.joinolares.cn >/dev/null 2>&1 || die "DNS resolution still failing for pypi.joinolares.cn"
  log "Host DNS resolution OK"

  if [[ "${RESTART_COREDNS}" == "true" ]] && command -v k3s >/dev/null 2>&1; then
    log "Restarting CoreDNS..."
    k3s kubectl rollout restart deployment/coredns -n kube-system
    k3s kubectl rollout status deployment/coredns -n kube-system --timeout=180s
    log "CoreDNS rollout complete"
  fi

  cat <<'EOF'
------------------------------------------------------------------------------
Done.

If ComfyUI Network Manager still shows old state:
  1) Open ComfyUI -> Network Manager
  2) Click "Save & Check"
  3) Hard refresh browser (Ctrl+Shift+R)
------------------------------------------------------------------------------
EOF
}

main "$@"
