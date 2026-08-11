#!/usr/bin/env bash
# 清理具有破坏性，严格模式让未处理失败立即停止。
set -euo pipefail

# 目标限定为 V1 固定网络；绝不宽泛清空 veth、netns 或防火墙。
readonly NETWORK_NAME='cloudnet-v1'
readonly CNI_PATH_VALUE='/opt/cni/bin'
readonly NETCONF_PATH='/etc/cni/net.d'
readonly STATE_FILE='/var/lib/cloudnet/networks/cloudnet-v1/state.json'

if (( EUID != 0 )); then
  printf 'cleanup-v1.sh requires root; run: sudo make clean-test\n' >&2
  exit 1
fi

# 先完整检查依赖，避免清理到一半才失去验权能力。
for command_name in ip jq sha256sum sha512sum awk grep; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    printf 'required command is missing: %s\n' "${command_name}" >&2
    exit 1
  fi
done

# 只有集成测试严格前缀下的名称才进入候选集合。
is_test_namespace() {
  [[ $1 =~ ^cloudnet-test-[a-z0-9][a-z0-9_.-]*$ ]]
}

# 复刻 cnitool 以 netns 路径派生 containerID 的规则，
# 使脚本能从 namespace 反推出同一 endpoint identity。
cnitool_container_id() {
  local netns_path=$1
  local digest

  digest=$(printf '%s' "${netns_path}" | sha512sum | awk '{print $1}')
  printf 'cnitool-%s\n' "${digest:0:20}"
}

# 与 Go endpointDigest 一致：三个身份字段用 NUL 分隔后做 SHA-256。
endpoint_digest() {
  local container_id=$1
  local if_name=$2

  printf '%s\0%s\0%s' "${NETWORK_NAME}" "${container_id}" "${if_name}" \
    | sha256sum \
    | awk '{print $1}'
}

# 最后的删除屏障：推导名称、名称正则、veth 类型和完整 alias
# 四项全部通过后，才允许执行 ip link delete。
remove_owned_host_veth() {
  local host_veth=$1
  local container_id=$2
  local if_name=$3
  local digest expected_name expected_alias actual_alias

  digest=$(endpoint_digest "${container_id}" "${if_name}")
  expected_name="cn${digest:0:13}"
  expected_alias="cloudnet:v1:${NETWORK_NAME}:${digest}"

  if [[ ${host_veth} != "${expected_name}" ]]; then
    printf 'refusing veth cleanup: name %s does not match derived name %s\n' \
      "${host_veth}" "${expected_name}" >&2
    return 1
  fi
  if [[ ! ${host_veth} =~ ^cn[0-9a-f]{13}$ ]]; then
    printf 'refusing veth cleanup: unsafe interface name %s\n' "${host_veth}" >&2
    return 1
  fi
  # 链接缺失表示此前 DEL 已完成，幂等成功。
  if ! ip link show dev "${host_veth}" >/dev/null 2>&1; then
    return 0
  fi
  if ! ip -d link show dev "${host_veth}" | grep -qw veth; then
    printf 'refusing veth cleanup: %s is not a veth\n' "${host_veth}" >&2
    return 1
  fi
  actual_alias=$(<"/sys/class/net/${host_veth}/ifalias")
  if [[ ${actual_alias} != "${expected_alias}" ]]; then
    printf 'refusing veth cleanup: alias mismatch on %s\n' "${host_veth}" >&2
    return 1
  fi

  ip link delete dev "${host_veth}"
  printf 'deleted verified cloudnet test veth: %s\n' "${host_veth}"
}

# 先证明记录来自 cnitool 测试 namespace，再优先调用正常 CNI DEL；
# 无论 cnitool 是否可用，host veth 都必须独立验权。
delete_endpoint() {
  local namespace=$1
  local netns_path=$2
  local container_id=$3
  local if_name=$4
  local host_veth=$5
  local expected_container_id

  if ! is_test_namespace "${namespace}"; then
    printf 'refusing cleanup for unsafe namespace name: %s\n' "${namespace}" >&2
    return 1
  fi
  if [[ ${netns_path} != "/run/netns/${namespace}" && ${netns_path} != "/var/run/netns/${namespace}" ]]; then
    printf 'refusing cleanup for unexpected netns path: %s\n' "${netns_path}" >&2
    return 1
  fi
  expected_container_id=$(cnitool_container_id "${netns_path}")
  if [[ ${container_id} != "${expected_container_id}" ]]; then
    printf 'refusing cleanup: endpoint %s was not created by cnitool for %s\n' \
      "${container_id}" "${netns_path}" >&2
    return 1
  fi

  if command -v cnitool >/dev/null 2>&1 \
    && [[ -x /opt/cni/bin/cloudnet ]] \
    && [[ -f /etc/cni/net.d/10-cloudnet.conf ]]; then
    CNI_PATH="${CNI_PATH_VALUE}" \
      NETCONFPATH="${NETCONF_PATH}" \
      CNI_IFNAME="${if_name}" \
      cnitool del "${NETWORK_NAME}" "${netns_path}" >/dev/null
  else
    printf 'cnitool/plugin/config unavailable; only verified link cleanup is possible for %s\n' \
      "${namespace}" >&2
  fi

  remove_owned_host_veth "${host_veth}" "${container_id}" "${if_name}"
}

# 避免持久 state 扫描和 live namespace 扫描重复处理同一对象。
declare -A handled_namespaces=()

# 持久记录可处理“netns 已先删除”的场景。损坏 state 刻意原样保留，
# 不用猜测式清理覆盖故障证据。
if [[ -e ${STATE_FILE} ]]; then
  if ! jq -e '.version == 1 and (.endpoints | type == "object")' "${STATE_FILE}" >/dev/null; then
    printf 'refusing cleanup because cloudnet state is corrupt: %s\n' "${STATE_FILE}" >&2
    exit 1
  fi

  # jq 先筛出测试路径，循环内仍对身份、路径和所有权做二次核验。
  while IFS=$'\t' read -r namespace netns_path container_id if_name host_veth; do
    [[ -n ${namespace} ]] || continue
    if ! is_test_namespace "${namespace}"; then
      continue
    fi
    delete_endpoint "${namespace}" "${netns_path}" "${container_id}" "${if_name}" "${host_veth}"
    handled_namespaces["${namespace}"]=1
  done < <(
    jq -r '
      .endpoints[]
      | select(.netns | type == "string")
      | select(.netns | test("^/(var/)?run/netns/cloudnet-test-[a-z0-9][a-z0-9_.-]*$"))
      | [(.netns | split("/") | last), .netns, .containerID, .ifName, .hostVethName]
      | @tsv
    ' "${STATE_FILE}"
  )
fi

# ADD 在 state 持久化前失败时可能只剩 live namespace，因此再扫描一次。
while read -r namespace _; do
  [[ -n ${namespace} ]] || continue
  if ! is_test_namespace "${namespace}"; then
    continue
  fi

  netns_path="/run/netns/${namespace}"
  container_id=$(cnitool_container_id "${netns_path}")
  digest=$(endpoint_digest "${container_id}" 'eth0')
  host_veth="cn${digest:0:13}"

  # 无 state 记录时按 cnitool 规则推导身份并尝试幂等 DEL。
  if [[ -z ${handled_namespaces["${namespace}"]+present} ]]; then
    if command -v cnitool >/dev/null 2>&1 \
      && [[ -x /opt/cni/bin/cloudnet ]] \
      && [[ -f /etc/cni/net.d/10-cloudnet.conf ]]; then
      CNI_PATH="${CNI_PATH_VALUE}" \
        NETCONFPATH="${NETCONF_PATH}" \
        CNI_IFNAME='eth0' \
        cnitool del "${NETWORK_NAME}" "${netns_path}" >/dev/null || true
    fi
    remove_owned_host_veth "${host_veth}" "${container_id}" 'eth0'
  fi

  # endpoint 清理完成后才删 namespace；共享 cni-br0 始终保留。
  ip netns delete "${namespace}"
  printf 'deleted cloudnet test namespace: %s\n' "${namespace}"
done < <(ip netns list)

printf 'cloudnet V1 test cleanup complete; shared bridge cni-br0 was retained.\n'
