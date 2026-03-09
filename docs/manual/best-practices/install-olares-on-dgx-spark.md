# Install Olares On DGX Spark

This guide provides a repeatable way to install Olares on NVIDIA DGX Spark nodes.

## Prerequisites

- Ubuntu 24.04 on DGX Spark.
- `olares-cli` binary available on target node.
- Root shell (`sudo -i`) or command-level `sudo`.

## One-command bootstrap script

Use the script added in this repo:

```bash
sudo bash tools/dgx/install_olares_dgx_spark.sh
```

Default behavior:

- Installs required system packages.
- Removes docker/containerd packages that conflict with Olares-managed containerd.
- Runs:
  - `precheck`
  - `download wizard`
  - `download component`
  - `download check`
  - `install`
- Applies a post-install HAMi fix for GB10 unified-memory GPUs:
  - Sets `preConfiguredDeviceMemory` in `hami-scheduler-device` (default `131072` MB).
  - Restarts `hami-device-plugin`.
  - Optionally swaps HAMi image if `HAMI_IMAGE` is set.

## Environment variables

You can customize behavior:

```bash
sudo VERSION=1.12.4 \
  CLI_PATH=/tmp/olares-cli \
  REMOVE_DOCKER=true \
  ENABLE_HAMI_GB10_FIX=true \
  HAMI_PRECONFIGURED_DEVICE_MEMORY_MB=131072 \
  HAMI_IMAGE=beclab/hami:v2.6.11 \
  bash tools/dgx/install_olares_dgx_spark.sh
```

- `VERSION`: Olares version to install.
- `CLI_PATH`: Path to executable `olares-cli`.
- `REMOVE_DOCKER`: `true` or `false`.
- `ENABLE_HAMI_GB10_FIX`: `true` or `false`.
- `HAMI_PRECONFIGURED_DEVICE_MEMORY_MB`: fallback GPU memory for unified-memory GPUs (GB10 default `131072`).
- `HAMI_IMAGE`: HAMi image used by device-plugin and monitor (default `beclab/hami:v2.6.11`).

## Notes for GB10

- PR `Project-HAMi/HAMi#1637` is required to avoid `nvml get memory error ret=Not Supported` panic.
- This guide defaults to `beclab/hami:v2.6.11`, which is intended to avoid the GB10 crash path.

## Interactive step

`olares-cli install` prompts for:

- Domain (default `olares.com`).
- Olares ID.

Use a unique Olares ID on each DGX Spark node.

## Access after install

Wizard entrypoint:

```text
http://<node-ip>:30180
```

## Troubleshooting

If pods fail to resolve service names or cannot reach service IPs:

1. Restart k3s:
   ```bash
   sudo systemctl restart k3s
   ```
2. If still broken, reboot host:
   ```bash
   sudo reboot
   ```

## GPU app profiles (single-GPU nodes)

For DGX Spark nodes with one GPU, use this helper to switch app GPU profiles:

```bash
sudo bash tools/dgx/switch_gpu_app_profile.sh ollama
sudo bash tools/dgx/switch_gpu_app_profile.sh comfyui
sudo bash tools/dgx/switch_gpu_app_profile.sh shared
sudo bash tools/dgx/switch_gpu_app_profile.sh best-effort
sudo bash tools/dgx/switch_gpu_app_profile.sh auto
```

Profiles:

- `ollama`: full GPU to Ollama (`100%`), ComfyUI scaled down.
- `comfyui`: full GPU to ComfyUI (`100%`), Ollama scaled down.
- `shared`: balanced `50/50` split (safer default when both run).
- `best-effort`: sets both to `100%` (more throughput for single active app, but higher contention risk if both are busy).
- `auto`: infers mode from deployment replicas:
  - both replicas > 0 -> `best-effort`
  - only `ollama` replicas > 0 -> `ollama`
  - only `comfyuishare` replicas > 0 -> `comfyui`
  - both replicas = 0 -> no-op

The script also clears stale `GPUBinding` resources to avoid stuck `Pending` pods after profile switches.

## Switch on app open (recommended)

If your launcher can execute a command on app-open, use:

```bash
sudo bash tools/dgx/switch_gpu_on_app_open.sh ollama
sudo bash tools/dgx/switch_gpu_on_app_open.sh comfyui
```

This wrapper:

- maps app-open event to profile (`ollama` or `comfyui`);
- adds a lock to avoid concurrent switches;
- adds cooldown (`COOLDOWN_SECONDS`, default 180) to avoid rapid flip-flop.

Optional parameters:

```bash
sudo COOLDOWN_SECONDS=300 bash tools/dgx/switch_gpu_on_app_open.sh ollama
sudo FORCE=true bash tools/dgx/switch_gpu_on_app_open.sh comfyui
```

State files are stored in:

```text
/var/lib/olares-gpu-switch
```

## Open WebUI self-managed updates

If Market updates lag behind upstream, you can update Open WebUI directly:

```bash
sudo bash tools/dgx/upgrade_openwebui.sh
```

Optional:

```bash
sudo bash tools/dgx/upgrade_openwebui.sh --tag v0.8.10
sudo bash tools/dgx/upgrade_openwebui.sh --image docker.io/beclab/open-webui-open-webui:v0.8.10
```

The script:

- auto-discovers your Open WebUI deployment by Olares labels;
- resolves latest stable `beclab/open-webui-open-webui` tag when no tag is given;
- updates deployment image and `USER_AGENT`;
- waits for rollout and auto-rolls back on failure.
