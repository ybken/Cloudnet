#!/usr/bin/env bash
set -uo pipefail

section() {
  printf '\n===== %s =====\n' "$1"
}

run() {
  section "$*"
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'not installed: %s\n' "$1"
    return 0
  fi
  "$@" 2>&1 || printf '[command exited non-zero: %s]\n' "$*"
}

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

run ip -br addr
run ip route
run ip netns list
run ip -d link
run bridge link

list_path /opt/cni/bin
list_path /etc/cni/net.d
list_path /var/lib/cloudnet

run nft list ruleset
run iptables-save

section 'net.ipv4.ip_forward'
if [[ -r /proc/sys/net/ipv4/ip_forward ]]; then
  printf '%s\n' "$(</proc/sys/net/ipv4/ip_forward)"
else
  printf 'unreadable\n'
fi

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
