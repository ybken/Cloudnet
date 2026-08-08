#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly SCRIPT_DIR
PROJECT_ROOT=$(cd -- "${SCRIPT_DIR}/.." && pwd -P)
readonly PROJECT_ROOT

if (( EUID != 0 )); then
  printf 'install-v1.sh requires root; run `make build` and then `sudo make install`.\n' >&2
  exit 1
fi

make -C "${PROJECT_ROOT}" install

printf 'Installed %s and %s\n' \
  '/opt/cni/bin/cloudnet' \
  '/etc/cni/net.d/10-cloudnet.conf'
