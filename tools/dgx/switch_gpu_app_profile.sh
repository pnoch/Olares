#!/usr/bin/env bash
set -euo pipefail

# GPU app profile switcher for single-GPU DGX Spark Olares nodes.
# - "ollama":   full GPU to Ollama, ComfyUI scaled down.
# - "comfyui":  full GPU to ComfyUI, Ollama scaled down.
# - "shared":   best-effort 50/50 split for both apps (depends on HAMi behavior).

PROFILE="${1:-}"

if [[ -z "${PROFILE}" ]]; then
  cat <<'EOF'
Usage:
  sudo bash tools/dgx/switch_gpu_app_profile.sh <profile>

Profiles:
  ollama   - full GPU to Ollama only
  comfyui  - full GPU to ComfyUI only
  shared   - best-effort 50/50 GPU memory split for both
EOF
  exit 1
fi

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: run as root (e.g. sudo bash tools/dgx/switch_gpu_app_profile.sh <profile>)"
  exit 1
fi

K3S="k3s kubectl"
OLLAMA_NS="ollamaserver-shared"
OLLAMA_DEPLOY="ollama"
COMFY_NS="comfyuisharev2server-shared"
COMFY_DEPLOY="comfyuishare"
KUBE_SYSTEM_NS="kube-system"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

patch_gpu_mem_percent() {
  local ns="$1"
  local deploy="$2"
  local container="$3"
  local percent="$4"

  ${K3S} -n "${ns}" patch deployment "${deploy}" --type='merge' -p "{
    \"spec\": {
      \"template\": {
        \"spec\": {
          \"containers\": [
            {
              \"name\": \"${container}\",
              \"resources\": {
                \"limits\": {
                  \"nvidia.com/gpu\": \"1\",
                  \"nvidia.com/gpumem-percentage\": \"${percent}\"
                },
                \"requests\": {
                  \"nvidia.com/gpu\": \"1\",
                  \"nvidia.com/gpumem-percentage\": \"${percent}\"
                }
              }
            }
          ]
        }
      }
    }
  }"
}

set_hami_mode_shared() {
  log "Switching HAMi node mode to time-slice with 2-way split..."
  ${K3S} patch configmap hami-device-plugin -n "${KUBE_SYSTEM_NS}" --type='merge' -p "{
    \"data\": {
      \"config.json\": \"{\\n  \\\"nodeconfig\\\": [\\n    {\\n      \\\"name\\\": \\\"$(hostname -s)\\\",\\n      \\\"operatingmode\\\": \\\"time-slice\\\",\\n      \\\"devicememoryscaling\\\": 1,\\n      \\\"devicesplitcount\\\": 2,\\n      \\\"migstrategy\\\": \\\"none\\\",\\n      \\\"filterdevices\\\": {\\n        \\\"uuid\\\": [],\\n        \\\"index\\\": []\\n      }\\n    }\\n  ]\\n}\\n\"
    }
  }"

  ${K3S} rollout restart daemonset/hami-device-plugin -n "${KUBE_SYSTEM_NS}"
  ${K3S} rollout restart deployment/hami-scheduler -n "${KUBE_SYSTEM_NS}"
  ${K3S} rollout status daemonset/hami-device-plugin -n "${KUBE_SYSTEM_NS}" --timeout=180s
  ${K3S} rollout status deployment/hami-scheduler -n "${KUBE_SYSTEM_NS}" --timeout=180s
}

clear_gpu_bindings() {
  # GPUBinding resources can hold stale app-level reservations.
  ${K3S} delete gpubinding.gpu.bytetrade.io -A --all --ignore-not-found
}

wait_and_show_status() {
  local ns="$1"
  local deploy="$2"
  local timeout="${3:-300s}"

  ${K3S} -n "${ns}" rollout status deployment/"${deploy}" --timeout="${timeout}" || true
  ${K3S} -n "${OLLAMA_NS}" get pods -o wide || true
  ${K3S} -n "${COMFY_NS}" get pods -o wide || true
  ${K3S} get gpubinding.gpu.bytetrade.io -A -o wide || true
}

case "${PROFILE}" in
  ollama)
    log "Applying profile: ollama (exclusive full GPU)"
    patch_gpu_mem_percent "${OLLAMA_NS}" "${OLLAMA_DEPLOY}" "ollama" "100"
    ${K3S} -n "${COMFY_NS}" scale deployment "${COMFY_DEPLOY}" --replicas=0
    clear_gpu_bindings
    ${K3S} -n "${OLLAMA_NS}" scale deployment "${OLLAMA_DEPLOY}" --replicas=1
    wait_and_show_status "${OLLAMA_NS}" "${OLLAMA_DEPLOY}"
    ;;
  comfyui)
    log "Applying profile: comfyui (exclusive full GPU)"
    patch_gpu_mem_percent "${COMFY_NS}" "${COMFY_DEPLOY}" "comfyui" "100"
    ${K3S} -n "${OLLAMA_NS}" scale deployment "${OLLAMA_DEPLOY}" --replicas=0
    clear_gpu_bindings
    ${K3S} -n "${COMFY_NS}" scale deployment "${COMFY_DEPLOY}" --replicas=1
    wait_and_show_status "${COMFY_NS}" "${COMFY_DEPLOY}"
    ;;
  shared)
    log "Applying profile: shared (best-effort 50/50)"
    set_hami_mode_shared
    patch_gpu_mem_percent "${OLLAMA_NS}" "${OLLAMA_DEPLOY}" "ollama" "50"
    patch_gpu_mem_percent "${COMFY_NS}" "${COMFY_DEPLOY}" "comfyui" "50"
    clear_gpu_bindings
    ${K3S} -n "${OLLAMA_NS}" scale deployment "${OLLAMA_DEPLOY}" --replicas=1
    ${K3S} -n "${COMFY_NS}" scale deployment "${COMFY_DEPLOY}" --replicas=1
    wait_and_show_status "${OLLAMA_NS}" "${OLLAMA_DEPLOY}"
    wait_and_show_status "${COMFY_NS}" "${COMFY_DEPLOY}"
    ;;
  *)
    echo "error: unsupported profile '${PROFILE}'. Use: ollama | comfyui | shared"
    exit 1
    ;;
esac

log "Done."
