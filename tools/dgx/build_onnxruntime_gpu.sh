#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="comfyuisharev2server-shared"
DEPLOYMENT="comfyuishare"
POD=""
while [[ -z "$POD" ]]; do
  POD="$(sudo k3s kubectl -n "$NAMESPACE" get pod -l io.kompose.service=comfyui -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -z "$POD" ]]; then
    printf '[%s] waiting for ComfyUI pod…\\n' "$(date '+%Y-%m-%d %H:%M:%S')"
    sleep 3
  fi
done
ORT_TAG="v2.16.1"
BUILD_DIR="/tmp/onnxruntime"
CUDA_HOME="/usr/local/cuda"
EXTRA_OPTS="--cuda_version=13.0 --cudnn_version=9.8 --use_cuda --use_openmp --parallel"

log(){ printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

log "Building ONNX Runtime GPU inside pod $POD"

sudo k3s kubectl -n "$NAMESPACE" exec "$POD" -- bash -lc "
set -euo pipefail
apt-get update >/dev/null
apt-get install -y git python3-dev python3-pip build-essential cmake ninja-build curl libopenblas-dev libcurl4-openssl-dev pkg-config >/dev/null
rm -rf $BUILD_DIR
mkdir -p $BUILD_DIR
cd $BUILD_DIR
git clone --depth 1 --branch $ORT_TAG https://github.com/microsoft/onnxruntime.git .
pip install --no-cache-dir --break-system-packages wheel
./build.sh --config Release --build_shared_lib --parallel --use_cuda --cuda_home $CUDA_HOME --cudnn_home $CUDA_HOME --cmake_generator Ninja --use_openmp
cd build/Linux/Arm64/Release/dist
shopt -s nullglob
files=(*.whl)
if [[ ${#files[@]} -eq 0 ]]; then
  log "no wheel found: build/Linux/Arm64/Release/dist/*.whl"
  exit 1
fi
WHEEL=$(realpath "${files[0]}")
python3 -m pip install --target=/app/backend/pipx --break-system-packages "${WHEEL}"
" 
log "Build complete. Uploaded wheel to /app/backend/pipx"
