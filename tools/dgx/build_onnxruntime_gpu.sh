#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="comfyuisharev2server-shared"
DEPLOYMENT="comfyuishare"
POD="$(sudo k3s kubectl -n "$NAMESPACE" get pod -l io.kompose.service=comfyuishare -o jsonpath='{.items[0].metadata.name}')"
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
./build.sh --config Release --build_shared_lib --parallel --use_cuda --cuda_home $CUDA_HOME --cudnn_home $CUDA_HOME --cmake_generator Ninja --use_openmp || true
WHEEL=$(realpath build/Linux/Arm64/Release/dist/*.whl)
python3 -m pip install --target=/app/backend/pipx --break-system-packages "${WHEEL}"
" 
log "Build complete. Uploaded wheel to /app/backend/pipx"
