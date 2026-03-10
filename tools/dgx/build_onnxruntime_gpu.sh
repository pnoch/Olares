#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="comfyuisharev2server-shared"
POD_LABEL="io.kompose.service=comfyui"

ORT_TAG="${ORT_TAG:-v1.24.3}"
BUILD_DIR="${BUILD_DIR:-/tmp/onnxruntime}"
CUDA_HOME_DEFAULT="${CUDA_HOME:-/usr/local/cuda}"
ORT_JOBS="${ORT_JOBS:-1}"
CUDA_ARCH="${CUDA_ARCH:-121}"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

die() {
  log "ERROR: $*"
  exit 1
}

find_pod() {
  local pod=""
  while [[ -z "$pod" ]]; do
    pod="$(sudo k3s kubectl -n "$NAMESPACE" get pod -l "$POD_LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -z "$pod" ]]; then
      log "waiting for ComfyUI pod..."
      sleep 3
    fi
  done
  printf '%s' "$pod"
}

POD="$(find_pod)"
log "Target pod: $POD"
log "ONNX Runtime tag: $ORT_TAG"
log "Build dir: $BUILD_DIR"
log "Requested parallel jobs: $ORT_JOBS"
log "CUDA arch: $CUDA_ARCH"

REMOTE_SCRIPT=$(cat <<'EOF_REMOTE'
set -euo pipefail

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

die() {
  log "ERROR: $*"
  exit 1
}

LOCK_DIR="/tmp/onnxruntime-build.lock"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  log "another ONNX Runtime build is already running in this pod; aborting"
  ps -ef | grep -E '[a]pt-get|[d]pkg|[o]nnxruntime|[b]uild.sh|[n]inja|[n]vcc|[c]icc' || true
  exit 1
fi
trap 'rmdir "$LOCK_DIR" >/dev/null 2>&1 || true' EXIT

show_dpkg_lock_holder() {
  local line=""
  line="$(ps -eo pid=,ppid=,etime=,comm=,args= 2>/dev/null | awk '$4=="apt-get" || $4=="dpkg" {print; exit}' || true)"
  if [[ -n "$line" ]]; then
    log "apt/dpkg active: $line"
  fi
}

apt_retry() {
  local tries=0
  local cmd="$1"
  until bash -lc "$cmd"; do
    tries=$((tries + 1))
    if [[ $tries -ge 30 ]]; then
      log "command failed after retries: $cmd"
      return 1
    fi
    show_dpkg_lock_holder
    log "apt is busy, retrying in 5s: $cmd"
    sleep 5
  done
}

resolve_cuda_home() {
  local p=""
  for p in "$CUDA_HOME" /usr/local/cuda-13.1 /usr/local/cuda-13.0 /usr/local/cuda; do
    if [[ -x "$p/bin/nvcc" ]]; then
      printf '%s' "$p"
      return 0
    fi
  done
  return 1
}

log "Installing build prerequisites"
apt_retry "apt-get update -qq"
apt_retry "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git python3-dev python3-pip build-essential cmake ninja-build curl libopenblas-dev libcurl4-openssl-dev pkg-config"

if [[ ! -x "$CUDA_HOME/bin/nvcc" ]]; then
  log "nvcc not found under $CUDA_HOME, attempting CUDA compiler package install"
  CUDNN_VER="$(dpkg-query -W -f='${Version}' libcudnn9-cuda-13 2>/dev/null || true)"
  if [[ -n "$CUDNN_VER" ]]; then
    CUDNN_DEPS="libcudnn9-headers-cuda-13=$CUDNN_VER libcudnn9-dev-cuda-13=$CUDNN_VER"
  else
    CUDNN_DEPS="libcudnn9-cuda-13 libcudnn9-headers-cuda-13 libcudnn9-dev-cuda-13"
  fi
  apt_retry "DEBIAN_FRONTEND=noninteractive apt-get install -y cuda-compiler-13-0 cuda-cudart-dev-13-0 cuda-libraries-dev-13-0 $CUDNN_DEPS"
fi

CUDA_HOME="$(resolve_cuda_home || true)"
[[ -n "$CUDA_HOME" ]] || die "nvcc not found after package install"

export CUDA_HOME
export PATH="$CUDA_HOME/bin:$PATH"
export CMAKE_BUILD_PARALLEL_LEVEL="$ORT_JOBS"
export MAX_JOBS="$ORT_JOBS"

case "$ORT_JOBS" in
  ''|*[!0-9]*)
    die "ORT_JOBS must be a positive integer"
    ;;
  0)
    die "ORT_JOBS must be >= 1"
    ;;
esac

log "Using CUDA_HOME=$CUDA_HOME"
log "Using CUDA_ARCH=$CUDA_ARCH"
log "Using ORT_JOBS=$ORT_JOBS"

log "GPU info:"
nvidia-smi || true

log "Memory info:"
cat /sys/fs/cgroup/memory.max 2>/dev/null || true
cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || true
free -h || true

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
cd "$BUILD_DIR"

log "Cloning onnxruntime $ORT_TAG"
git clone --depth 1 --branch "$ORT_TAG" https://github.com/microsoft/onnxruntime.git .

log "Installing wheel package"
python3 -m pip install --no-cache-dir --break-system-packages wheel

log "Starting ONNX Runtime build"
./build.sh \
  --config Release \
  --update \
  --build \
  --build_shared_lib \
  --build_wheel \
  --skip_tests \
  --allow_running_as_root \
  --parallel "$ORT_JOBS" \
  --use_cuda \
  --cuda_version 13.0 \
  --cuda_home "$CUDA_HOME" \
  --cudnn_home "$CUDA_HOME" \
  --cmake_generator Ninja \
  --cmake_extra_defines CMAKE_CUDA_ARCHITECTURES="$CUDA_ARCH"

WHEEL="$(find . -path '*Release/dist/*.whl' -name '*.whl' -print -quit)"
[[ -n "$WHEEL" ]] || die "no wheel found under build/*/Release/dist/*.whl"

WHEEL="$(realpath "$WHEEL")"
log "Built wheel: $WHEEL"

mkdir -p /app/backend/pipx

python3 -m pip install \
  --target=/app/backend/pipx \
  --break-system-packages \
  --upgrade \
  --no-deps \
  "$WHEEL"

python3 -m pip install \
  --target=/app/backend/pipx \
  --break-system-packages \
  --upgrade \
  --force-reinstall \
  "numpy==1.26.4" \
  "protobuf==4.25.8" \
  flatbuffers \
  packaging \
  sympy

DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  libgl1 \
  libglib2.0-0 \
  libsm6 \
  libxext6 \
  libxrender1

log "Wheel installed into /app/backend/pipx"
EOF_REMOTE
)

sudo k3s kubectl -n "$NAMESPACE" exec "$POD" -- env \
  ORT_TAG="$ORT_TAG" \
  BUILD_DIR="$BUILD_DIR" \
  CUDA_HOME="$CUDA_HOME_DEFAULT" \
  CUDA_ARCH="$CUDA_ARCH" \
  ORT_JOBS="$ORT_JOBS" \
  bash -lc "$REMOTE_SCRIPT"

log "Build complete"
