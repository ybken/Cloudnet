#!/usr/bin/env bash
# 严格模式：命令失败、未定义变量或管道中间失败都会终止安装。
set -euo pipefail

# 从脚本真实位置推导项目根，使脚本可从任意当前目录执行。
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly SCRIPT_DIR
PROJECT_ROOT=$(cd -- "${SCRIPT_DIR}/.." && pwd -P)
readonly PROJECT_ROOT

# 目标位于系统目录；脚本不隐式 sudo，调用者必须明确授予 root。
if (( EUID != 0 )); then
  printf 'install-v1.sh requires root; run `make build` and then `sudo make install`.\n' >&2
  exit 1
fi

# 复用 Makefile 的新鲜度检查和安装路径，避免维护第二套复制逻辑。
make -C "${PROJECT_ROOT}" install

printf 'Installed %s and %s\n' \
  '/opt/cni/bin/cloudnet' \
  '/etc/cni/net.d/10-cloudnet.conf'
