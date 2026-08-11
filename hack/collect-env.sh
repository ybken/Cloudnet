#!/usr/bin/env bash
# 采集应尽量继续，故不启用 -e；仍拒绝未定义变量并保留管道错误。
set -uo pipefail

# 为诊断报告添加稳定分隔标题，便于人工浏览。
section() {
  printf '\n===== %s =====\n' "$1"
}

# 检查命令是否安装并合并输出；单项失败不会中断后续采集。
run() {
  section "$*"
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'not installed: %s\n' "$1"
    return 0
  fi
  "$@" 2>&1 || printf '[command exited non-zero: %s]\n' "$*"
}

# 只列三层元数据，不读取状态文件正文或潜在秘密。
list_path() {
  local path=$1

  section "${path}"
  if [[ ! -e "${path}" ]]; then
    printf 'absent\n'
    return 0
  fi
  find "${path}" -maxdepth 3 -printf '%M %u:%g %s %TY-%Tm-%TdT%TH:%TM:%TS %p\n' 2>&1 \
    || printf '[unable to list %s]\n' "${path}"
}

# 第一组：工作区与工具版本，用于确认复现代码和运行环境。
section 'pwd'
pwd

section 'git status'
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git status --short --branch 2>&1 || true
else
  printf 'not inside a Git worktree\n'
fi

section 'AGENTS.md'
find /home/cloudnet-template -name AGENTS.md -type f -print 2>&1 \
  || printf '[unable to search for AGENTS.md]\n'

run go version
run containerd --version
run nerdctl --version

section 'cnitool'
if command -v cnitool >/dev/null 2>&1; then
  command -v cnitool
  cnitool --help 2>&1 || true
else
  printf 'not installed\n'
fi

# 第二组：网络拓扑快照；所有命令都是只读查询。
run ip -br addr
run ip route
run ip netns list
run ip -d link
run bridge link

# 第三组：CNI 安装与 cloudnet 状态路径的权限、大小和时间戳。
list_path /opt/cni/bin
list_path /etc/cni/net.d
list_path /var/lib/cloudnet

# 防火墙与 forwarding 会影响连通性，但脚本只采集、不修改。
run nft list ruleset
run iptables-save

section 'net.ipv4.ip_forward'
if [[ -r /proc/sys/net/ipv4/ip_forward ]]; then
  printf '%s\n' "$(</proc/sys/net/ipv4/ip_forward)"
else
  printf 'unreadable\n'
fi

# 固定实验环境的管理网/underlay 核对，用于证明 cloudnet 未接管物理口。
section 'interface carrying management address 192.168.80.135'
ip -o -4 addr show 2>&1 \
  | awk '$4 ~ /^192[.]168[.]80[.]135\// { print $2, $4 }' \
  || true

section 'interface carrying underlay address 192.168.232.11'
ip -o -4 addr show 2>&1 \
  | awk '$4 ~ /^192[.]168[.]232[.]11\// { print $2, $4 }' \
  || true

section 'default-route interface'
ip route show default 2>&1 || true
