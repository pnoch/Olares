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
- Applies a post-install workaround by disabling `hami-device-plugin` scheduling (to avoid GB10 NVML incompatibility crash loops).

## Environment variables

You can customize behavior:

```bash
sudo VERSION=1.12.4 \
  CLI_PATH=/tmp/olares-cli \
  REMOVE_DOCKER=true \
  DISABLE_HAMI_DEVICE_PLUGIN=true \
  bash tools/dgx/install_olares_dgx_spark.sh
```

- `VERSION`: Olares version to install.
- `CLI_PATH`: Path to executable `olares-cli`.
- `REMOVE_DOCKER`: `true` or `false`.
- `DISABLE_HAMI_DEVICE_PLUGIN`: `true` or `false`.

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
