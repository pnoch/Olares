#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-1.12.4}"
CLI_PATH="${CLI_PATH:-/tmp/olares-cli}"
REMOVE_DOCKER="${REMOVE_DOCKER:-true}"
DISABLE_HAMI_DEVICE_PLUGIN="${DISABLE_HAMI_DEVICE_PLUGIN:-true}"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    echo "error: run as root (e.g. sudo $0)"
    exit 1
  fi
}

require_cli() {
  if [[ ! -x "${CLI_PATH}" ]]; then
    echo "error: olares-cli not found or not executable at ${CLI_PATH}"
    echo "hint: place the built/downloaded cli binary there, or set CLI_PATH=/path/to/olares-cli"
    exit 1
  fi
}

prepare_system_packages() {
  log "Installing required system packages..."
  apt-get update
  apt-get install -y libudev-dev libpcap-dev build-essential pkg-config
}

remove_conflicting_runtimes() {
  if [[ "${REMOVE_DOCKER}" != "true" ]]; then
    log "Skipping docker/containerd package removal (REMOVE_DOCKER=${REMOVE_DOCKER})."
    return
  fi

  log "Removing conflicting docker/containerd packages if present..."
  apt-get remove -y \
    containerd \
    containerd.io \
    docker.io \
    docker-ce \
    docker-ce-cli \
    docker-ce-rootless-extras || true

  apt-get autoremove -y || true
}

run_install_flow() {
  log "Running precheck..."
  "${CLI_PATH}" precheck

  log "Downloading install wizard (${VERSION})..."
  "${CLI_PATH}" download wizard -v "${VERSION}"

  log "Downloading installation components (${VERSION})..."
  "${CLI_PATH}" download component -v "${VERSION}"

  log "Checking downloaded packages..."
  "${CLI_PATH}" download check -v "${VERSION}"

  cat <<'EOF'
------------------------------------------------------------------------------
Next step is interactive:
  - Olares installer will prompt for domain and Olares ID.
  - Use a unique Olares ID per node.
------------------------------------------------------------------------------
EOF

  log "Starting Olares install (${VERSION})..."
  "${CLI_PATH}" install -v "${VERSION}"
}

disable_hami_device_plugin_if_requested() {
  if [[ "${DISABLE_HAMI_DEVICE_PLUGIN}" != "true" ]]; then
    log "Skipping HAMi device-plugin disable (DISABLE_HAMI_DEVICE_PLUGIN=${DISABLE_HAMI_DEVICE_PLUGIN})."
    return
  fi

  log "Disabling HAMi device-plugin scheduling (workaround for GB10/NVML unsupported memory query)..."
  k3s kubectl patch daemonset hami-device-plugin -n kube-system --type=merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":{"hami-disabled":"true"}}}}}' || true
}

print_generated_account_info() {
  local install_log
  install_log="/root/.olares/versions/${VERSION}/logs/install.log"

  if [[ ! -f "${install_log}" ]]; then
    log "Install log not found at ${install_log}; skipping account/password display."
    return
  fi

  log "Generated account information from install log:"
  if command -v rg >/dev/null 2>&1; then
    rg -n -i "using Olares Local Name|using Olares ID|using password" "${install_log}" || true
  else
    grep -niE "using Olares Local Name|using Olares ID|using password" "${install_log}" || true
  fi
}

print_post_install_status() {
  local host_ip
  host_ip="$(hostname -I | awk '{print $1}')"

  log "Cluster node status:"
  k3s kubectl get nodes

  log "Non-running pods (if any):"
  k3s kubectl get pods -A | grep -E 'CrashLoopBackOff|Error|Pending|Init:' || true

  print_generated_account_info

  cat <<EOF
------------------------------------------------------------------------------
Done.

Wizard access (NodePort):
  http://${host_ip}:30180

If pod-to-service networking is broken after install:
  1) systemctl restart k3s
  2) if still broken, reboot the host
------------------------------------------------------------------------------
EOF
}

main() {
  require_root
  require_cli

  log "Starting DGX Spark Olares bootstrap"
  log "VERSION=${VERSION} CLI_PATH=${CLI_PATH}"
  log "REMOVE_DOCKER=${REMOVE_DOCKER} DISABLE_HAMI_DEVICE_PLUGIN=${DISABLE_HAMI_DEVICE_PLUGIN}"

  prepare_system_packages
  remove_conflicting_runtimes
  run_install_flow
  disable_hami_device_plugin_if_requested
  print_post_install_status
}

main "$@"
