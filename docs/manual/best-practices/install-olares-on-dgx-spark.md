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
  HAMI_IMAGE=projecthami/hami:<tag-with-pr-1637> \
  bash tools/dgx/install_olares_dgx_spark.sh
```

- `VERSION`: Olares version to install.
- `CLI_PATH`: Path to executable `olares-cli`.
- `REMOVE_DOCKER`: `true` or `false`.
- `ENABLE_HAMI_GB10_FIX`: `true` or `false`.
- `HAMI_PRECONFIGURED_DEVICE_MEMORY_MB`: fallback GPU memory for unified-memory GPUs (GB10 default `131072`).
- `HAMI_IMAGE`: optional full image reference for HAMi daemonset. Leave empty to keep current image.

## Notes for GB10

- PR `Project-HAMi/HAMi#1637` is required to avoid `nvml get memory error ret=Not Supported` panic.
- If your bundled HAMi image does not include that PR, set `HAMI_IMAGE` to a fixed build that includes it.

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
