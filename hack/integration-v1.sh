#!/usr/bin/env bash
# -E 让函数继承 ERR trap；其余严格模式避免测试悄然漏检失败。
set -Eeuo pipefail

# 使用脚本位置而非调用目录定位构建产物与配置。
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly SCRIPT_DIR
PROJECT_ROOT=$(cd -- "${SCRIPT_DIR}/.." && pwd -P)
readonly PROJECT_ROOT
readonly NETWORK_NAME='cloudnet-v1'
readonly BRIDGE_NAME='cni-br0'
readonly GATEWAY='10.77.0.1'
readonly CNI_PATH_VALUE='/opt/cni/bin'
readonly NETCONF_PATH='/etc/cni/net.d'
readonly STATE_FILE='/var/lib/cloudnet/networks/cloudnet-v1/state.json'
# UTC 时间与 PID 组成运行 ID，避免并行/残留测试对象重名。
RUN_ID="$(date -u +%Y%m%d%H%M%S)-$$"
readonly RUN_ID
# stdout、stderr 与诊断证据隔离在本次运行的专属临时目录。
WORK_DIR=$(mktemp -d "/tmp/cloudnet-test-${RUN_ID}.XXXXXX")
readonly WORK_DIR
readonly CONCURRENCY="${CLOUDNET_TEST_CONCURRENCY:-6}"
readonly KEEP_FAILURE_ARTIFACTS="${CLOUDNET_KEEP_FAILURE_ARTIFACTS:-0}"

# ERR/EXIT trap 会输出当前阶段，直接指出失败属于哪项验收。
CURRENT_PHASE='startup'

# 记录本次创建的 namespace、后台进程和 veth，供 EXIT trap 精确清理。
declare -a namespaces=()
declare -a background_pids=()
declare -A namespace_ifnames=()
declare -A namespace_hosts=()

log() {
  printf '[integration-v1] %s\n' "$*"
}

fail() {
  printf '[integration-v1] FAIL: %s\n' "$*" >&2
  exit 1
}

# 在创建内核对象前完成外部命令依赖预检。
require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is missing: $1"
}

# 所有可能删除的 namespace 必须通过白名单语法。
is_test_namespace() {
  [[ $1 =~ ^cloudnet-test-[a-z0-9][a-z0-9_.-]*$ ]]
}

# 复刻 cnitool 从 netns 路径派生 containerID 的规则。
cnitool_container_id() {
  local netns_path=$1
  local digest

  digest=$(printf '%s' "${netns_path}" | sha512sum | awk '{print $1}')
  printf 'cnitool-%s\n' "${digest:0:20}"
}

# 与 Go 端相同：NUL 分隔身份 tuple 后计算 SHA-256。
endpoint_digest() {
  local container_id=$1
  local if_name=$2

  printf '%s\0%s\0%s' "${NETWORK_NAME}" "${container_id}" "${if_name}" \
    | sha256sum \
    | awk '{print $1}'
}

# 推导 cn + 13 hex 的确定性 host veth 名。
derived_host_veth() {
  local netns_path=$1
  local if_name=$2
  local container_id digest

  container_id=$(cnitool_container_id "${netns_path}")
  digest=$(endpoint_digest "${container_id}" "${if_name}")
  printf 'cn%s\n' "${digest:0:13}"
}

# alias 保留完整 digest，是清理和归属断言的授权依据。
derived_alias() {
  local netns_path=$1
  local if_name=$2
  local container_id digest

  container_id=$(cnitool_container_id "${netns_path}")
  digest=$(endpoint_digest "${container_id}" "${if_name}")
  printf 'cloudnet:v1:%s:%s\n' "${NETWORK_NAME}" "${digest}"
}

namespace_path() {
  printf '/run/netns/%s\n' "$1"
}

# 只接受测试前缀，并立即登记到 EXIT 清理集合。
create_namespace() {
  local namespace=$1

  is_test_namespace "${namespace}" || fail "unsafe test namespace name: ${namespace}"
  ip netns add "${namespace}"
  namespaces+=("${namespace}")
  namespace_ifnames["${namespace}"]='eth0'
}

# stdout Result 与 stderr JSON 日志分文件保存，以验证协议通道隔离。
add_endpoint() {
  local namespace=$1
  local output_file=$2
  local error_file=$3
  local if_name=${4:-eth0}
  local netns_path rc=0

  netns_path=$(namespace_path "${namespace}")
  namespace_ifnames["${namespace}"]="${if_name}"
  CNI_PATH="${CNI_PATH_VALUE}" \
    NETCONFPATH="${NETCONF_PATH}" \
    CNI_IFNAME="${if_name}" \
    cnitool add "${NETWORK_NAME}" "${netns_path}" >"${output_file}" 2>"${error_file}" || rc=$?
  return "${rc}"
}

# CHECK 成功不输出 Result；调用方按场景捕获诊断。
check_endpoint() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}

  CNI_PATH="${CNI_PATH_VALUE}" \
    NETCONFPATH="${NETCONF_PATH}" \
    CNI_IFNAME="${if_name}" \
    cnitool check "${NETWORK_NAME}" "$(namespace_path "${namespace}")"
}

# 正常清理走真实 CNI DEL，覆盖与 runtime 相同的调用路径。
delete_endpoint() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}

  CNI_PATH="${CNI_PATH_VALUE}" \
    NETCONFPATH="${NETCONF_PATH}" \
    CNI_IFNAME="${if_name}" \
    cnitool del "${NETWORK_NAME}" "$(namespace_path "${namespace}")" >/dev/null
}

# trap 兜底也要求名称、类型和完整 alias，不宽泛删除 veth。
remove_owned_host_veth() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}
  local netns_path host_veth expected_alias actual_alias

  netns_path=$(namespace_path "${namespace}")
  host_veth=${namespace_hosts["${namespace}"]:-$(derived_host_veth "${netns_path}" "${if_name}")}
  expected_alias=$(derived_alias "${netns_path}" "${if_name}")
  [[ ${host_veth} =~ ^cn[0-9a-f]{13}$ ]] || return 0
  ip link show dev "${host_veth}" >/dev/null 2>&1 || return 0
  ip -d link show dev "${host_veth}" | grep -qw veth || return 0
  actual_alias=$(<"/sys/class/net/${host_veth}/ifalias")
  [[ ${actual_alias} == "${expected_alias}" ]] || return 0
  ip link delete dev "${host_veth}" || true
}

print_artifact() {
  local path=$1

  [[ -f ${path} ]] || return 0
  printf '[integration-v1] --- %s ---\n' "${path}" >&2
  sed -n '1,160p' "${path}" >&2
}

# 清理前收集有限日志、测试 state 摘要和网络快照，便于复盘。
dump_failure_evidence() {
  local path

  printf '[integration-v1] failure evidence: phase=%s workDir=%s\n' \
    "${CURRENT_PHASE}" "${WORK_DIR}" >&2
  for path in \
    "${WORK_DIR}"/*.err \
    "${WORK_DIR}"/*.log \
    "${WORK_DIR}"/*.out \
    "${WORK_DIR}"/*.json; do
    [[ -e ${path} ]] || continue
    print_artifact "${path}"
  done

  if [[ -e ${STATE_FILE} ]]; then
    printf '[integration-v1] --- cloudnet test state summary ---\n' >&2
    jq '{
      version,
      networkName,
      endpointCount: (.endpoints | length),
      allocationCount: (.allocations | length),
      testEndpoints: [
        .endpoints[]
        | select((.netns // "") | test("^/(var/)?run/netns/cloudnet-test-"))
      ]
    }' "${STATE_FILE}" >&2 || true
  fi

  printf '[integration-v1] --- network snapshot before cleanup ---\n' >&2
  ip -br link >&2 || true
  ip -br -4 addr >&2 || true
  ip netns list >&2 || true
  bridge link >&2 || true
  ip -d link show master "${BRIDGE_NAME}" >&2 || true
}

# ERR trap 记录退出码、行号和命令；资源统一由 EXIT trap 清理。
on_error() {
  local status=$1
  local line=$2
  local command=$3

  printf '[integration-v1] ERROR: phase=%s exit=%d line=%s command=%q\n' \
    "${CURRENT_PHASE}" "${status}" "${line}" "${command}" >&2
}

# 保存外部命令输出，失败时先打印 artifact 再终止。
run_checked() {
  local label=$1
  local output_file=$2
  shift 2

  if ! "$@" >"${output_file}" 2>&1; then
    print_artifact "${output_file}"
    fail "${label}"
  fi
}

# ADD 包装会同时展示 stdout/stderr，区分协议错误和诊断日志。
add_endpoint_or_fail() {
  local namespace=$1
  local output_file=$2
  local error_file=$3
  local label=$4
  local if_name=${5:-eth0}

  if ! add_endpoint "${namespace}" "${output_file}" "${error_file}" "${if_name}"; then
    print_artifact "${error_file}"
    print_artifact "${output_file}"
    fail "${label}; CNI artifacts are included in the failure evidence"
  fi
}

# 保存原状态码，等待后台作业，采证后逐项清理，并以原状态码退出。
# 此函数永远保留共享 Bridge。
cleanup() {
  local status=$?
  local namespace pid

  # 防清理失败递归触发 trap；set +e 让其尽力处理全部对象。
  trap - ERR EXIT INT TERM
  set +e
  for pid in "${background_pids[@]}"; do
    wait "${pid}" >/dev/null 2>&1
  done
  if (( status != 0 )); then
    dump_failure_evidence
  fi
  for namespace in "${namespaces[@]}"; do
    is_test_namespace "${namespace}" || continue
    delete_endpoint "${namespace}" >/dev/null 2>&1
    remove_owned_host_veth "${namespace}"
    if ip netns list | awk '{print $1}' | grep -Fxq "${namespace}"; then
      ip netns delete "${namespace}"
    fi
  done
  # 失败且显式设置开关时才保留临时证据。
  if (( status == 0 )) || [[ ${KEEP_FAILURE_ARTIFACTS} != 1 ]]; then
    rm -rf -- "${WORK_DIR}"
  else
    printf '[integration-v1] retained failure artifacts: %s\n' "${WORK_DIR}" >&2
  fi
  set -e
  if (( status != 0 )); then
    printf '[integration-v1] cleanup ran after failure (exit %d); cni-br0 was retained.\n' "${status}" >&2
  fi
  exit "${status}"
}

# ERR 负责定位，EXIT 负责收尾；INT/TERM 转换为惯用退出码。
trap 'on_error "$?" "$LINENO" "$BASH_COMMAND"' ERR
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# stdout 必须恰好一个 CNI 1.1.0 Result，且包含 IPv4 default。
assert_result_json() {
  local path=$1

  jq -s -e '
    length == 1
    and .[0].cniVersion == "1.1.0"
    and (. [0].interfaces | type == "array")
    and (. [0].ips | type == "array" and length == 1)
    and (. [0].routes | any(.dst == "0.0.0.0/0" and .gw == "10.77.0.1"))
  ' "${path}" >/dev/null || fail "stdout is not one valid CNI 1.1.0 result: ${path}"
}

# 将 stderr 当 JSON Lines 解析，核对统一结构字段。
assert_json_logs() {
  local path=$1

  [[ -s ${path} ]] || fail "expected structured stderr log output: ${path}"
  jq -s -e '
    length >= 1
    and all(.[].timestamp; type == "string")
    and all(.[].level; type == "string")
    and all(.[].command; type == "string")
    and all(.[].network; . == "cloudnet-v1")
    and all(.[].ifName; type == "string")
    and all(.[].phase; type == "string")
    and all(.[]; has("duration") and has("error") and has("rollback"))
  ' "${path}" >/dev/null || fail "stderr contains malformed or incomplete JSON logs: ${path}"
}

# 从单一 Result 提取后续连通性/归属断言所需值。
result_ip() {
  jq -er '.[0].ips[0].address | split("/")[0]' < <(jq -s '.' "$1")
}

result_host_veth() {
  jq -er '
    [.[0].interfaces[] | select(.name | test("^cn[0-9a-f]{13}$"))]
    | if length == 1 then .[0].name else error("expected exactly one host veth") end
  ' \
    < <(jq -s '.' "$1")
}

# 从 sysfs/ip JSON 同时核验类型、UP、MTU 和 gateway。
assert_bridge() {
  [[ -d /sys/class/net/${BRIDGE_NAME}/bridge ]] || fail "${BRIDGE_NAME} is not a Linux bridge"
  ip -j link show dev "${BRIDGE_NAME}" \
    | jq -e '.[0].flags | index("UP") != null' >/dev/null \
    || fail "${BRIDGE_NAME} is not UP"
  ip -j link show dev "${BRIDGE_NAME}" \
    | jq -e '.[0].mtu == 1500' >/dev/null \
    || fail "${BRIDGE_NAME} MTU is not 1500"
  ip -j -4 addr show dev "${BRIDGE_NAME}" \
    | jq -e 'any(.[0].addr_info[]; .local == "10.77.0.1" and .prefixlen == 24)' >/dev/null \
    || fail "${BRIDGE_NAME} does not have 10.77.0.1/24"
}

# 跨 netns/host 验证 lo、eth0、地址、路由、master、alias 及 host 无 IPv4。
assert_endpoint() {
  local namespace=$1
  local ip_address=$2
  local host_veth=$3
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}
  local expected_alias

  ip -n "${namespace}" -j link show dev lo \
    | jq -e '.[0].flags | index("UP") != null' >/dev/null \
    || fail "lo is down in ${namespace}"
  ip -n "${namespace}" -j link show dev "${if_name}" \
    | jq -e --arg ifname "${if_name}" '.[0].ifname == $ifname and .[0].mtu == 1500 and (.[0].flags | index("UP") != null)' >/dev/null \
    || fail "${namespace}/${if_name} is missing, down, or has the wrong MTU"
  ip -n "${namespace}" -j -4 addr show dev "${if_name}" \
    | jq -e --arg ip "${ip_address}" 'any(.[0].addr_info[]; .local == $ip and .prefixlen == 24)' >/dev/null \
    || fail "${namespace}/${if_name} does not have ${ip_address}/24"
  ip -n "${namespace}" -j route show default \
    | jq -e --arg ifname "${if_name}" 'any(.[]; .gateway == "10.77.0.1" and .dev == $ifname)' >/dev/null \
    || fail "${namespace} has no default route via 10.77.0.1"

  [[ ${host_veth} =~ ^cn[0-9a-f]{13}$ ]] || fail "unsafe host veth name: ${host_veth}"
  ip -d link show dev "${host_veth}" | grep -qw veth \
    || fail "${host_veth} is not a veth"
  ip -j link show dev "${host_veth}" \
    | jq -e --arg bridge "${BRIDGE_NAME}" '.[0].master == $bridge and (.[0].flags | index("UP") != null)' >/dev/null \
    || fail "${host_veth} is down or is not attached to ${BRIDGE_NAME}"
  expected_alias=$(derived_alias "$(namespace_path "${namespace}")" "${if_name}")
  [[ $(<"/sys/class/net/${host_veth}/ifalias") == "${expected_alias}" ]] \
    || fail "${host_veth} ownership alias does not match"
  ip -j addr show dev "${host_veth}" \
  | jq -e '[.[].addr_info[]? | select(.family == "inet")] | length == 0' >/dev/null \
  || fail "${host_veth} unexpectedly has an IPv4 address"
}

# 释放后的 IP 必须从 allocations 反向映射消失。
assert_state_ip_absent() {
  local ip_address=$1

  if [[ -e ${STATE_FILE} ]]; then
    jq -e --arg ip "${ip_address}" '(.allocations | has($ip)) | not' "${STATE_FILE}" >/dev/null \
      || fail "IP ${ip_address} remains allocated"
  fi
}

# 使用完整 endpoint identity 搜索，不依赖 map key 实现细节。
assert_endpoint_state_absent() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}
  local container_id

  [[ -e ${STATE_FILE} ]] || return 0
  container_id=$(cnitool_container_id "$(namespace_path "${namespace}")")
  jq -e \
    --arg network "${NETWORK_NAME}" \
    --arg container_id "${container_id}" \
    --arg if_name "${if_name}" '
      (.endpoints | type == "object")
      and ([
        .endpoints[]
        | select(
          .networkName == $network
          and .containerID == $container_id
          and .ifName == $if_name
        )
      ] | length == 0)
    ' "${STATE_FILE}" >/dev/null \
    || fail "endpoint state remains for ${namespace}/${if_name}"
}

# 故障注入后 CHECK 必须失败、stdout 为空且 stderr 有可操作诊断。
expect_check_failure() {
  local namespace=$1
  local label=$2
  local output_file="${WORK_DIR}/check-${namespace}-${label}.out"
  local error_file="${WORK_DIR}/check-${namespace}-${label}.err"

  if check_endpoint "${namespace}" >"${output_file}" 2>"${error_file}"; then
    fail "CHECK unexpectedly succeeded after ${label}"
  fi
  [[ ! -s ${output_file} ]] || fail "failed CHECK polluted stdout after ${label}"
  grep -qiE 'check mismatch|mismatch|down|route|master|bridge' "${error_file}" \
    || fail "CHECK failure after ${label} was not diagnostic"
}

# 主流程先完成权限、命令、安装新鲜度与配置预检，再产生副作用。
if (( EUID != 0 )); then
  fail 'integration-v1.sh requires root; run sudo make integration'
fi

for command_name in ip bridge ping cnitool jq sha256sum sha512sum awk grep sed sort uniq mktemp wc cmp; do
  require_command "${command_name}"
done

[[ -x ${PROJECT_ROOT}/build/cloudnet ]] || fail 'build/cloudnet is missing; run make build'
[[ -x /opt/cni/bin/cloudnet ]] || fail '/opt/cni/bin/cloudnet is not installed'
[[ -r /etc/cni/net.d/10-cloudnet.conf ]] || fail '/etc/cni/net.d/10-cloudnet.conf is not installed'
cmp -s "${PROJECT_ROOT}/build/cloudnet" /opt/cni/bin/cloudnet \
  || fail 'installed plugin differs from build/cloudnet; run sudo make install'
jq -e '
  .cniVersion == "1.1.0"
  and .name == "cloudnet-v1"
  and .type == "cloudnet"
  and .bridge == "cni-br0"
  and .mtu == 1500
  and .ipam.subnet == "10.77.0.0/24"
  and .ipam.gateway == "10.77.0.1"
  and .ipam.rangeStart == "10.77.0.10"
  and .ipam.rangeEnd == "10.77.0.250"
' /etc/cni/net.d/10-cloudnet.conf >/dev/null || fail 'installed CNI config does not match V1'

[[ ${CONCURRENCY} =~ ^[0-9]+$ ]] || fail 'CLOUDNET_TEST_CONCURRENCY must be an integer'
(( CONCURRENCY >= 2 && CONCURRENCY <= 16 )) \
  || fail 'CLOUDNET_TEST_CONCURRENCY must be between 2 and 16'

# A/B：ADD、重复 ADD、双 endpoint 连通，以及 Result/日志格式。
CURRENT_PHASE='A/B ADD and connectivity'
log 'A/B: ADD, idempotent ADD, bridge checks, and two-endpoint connectivity'
ns_a="cloudnet-test-a-${RUN_ID}"
ns_b="cloudnet-test-b-${RUN_ID}"
create_namespace "${ns_a}"
create_namespace "${ns_b}"

add_endpoint_or_fail \
  "${ns_a}" \
  "${WORK_DIR}/a-add.json" \
  "${WORK_DIR}/a-add.log" \
  'first endpoint ADD failed'
assert_result_json "${WORK_DIR}/a-add.json"
assert_json_logs "${WORK_DIR}/a-add.log"
ip_a=$(result_ip "${WORK_DIR}/a-add.json")
host_a=$(result_host_veth "${WORK_DIR}/a-add.json")
namespace_hosts["${ns_a}"]="${host_a}"

add_endpoint_or_fail \
  "${ns_a}" \
  "${WORK_DIR}/a-add-again.json" \
  "${WORK_DIR}/a-add-again.log" \
  'idempotent endpoint ADD failed'
assert_result_json "${WORK_DIR}/a-add-again.json"
[[ $(result_ip "${WORK_DIR}/a-add-again.json") == "${ip_a}" ]] \
  || fail 'repeated ADD returned a different address'

add_endpoint_or_fail \
  "${ns_b}" \
  "${WORK_DIR}/b-add.json" \
  "${WORK_DIR}/b-add.log" \
  'second endpoint ADD failed'
assert_result_json "${WORK_DIR}/b-add.json"
ip_b=$(result_ip "${WORK_DIR}/b-add.json")
host_b=$(result_host_veth "${WORK_DIR}/b-add.json")
namespace_hosts["${ns_b}"]="${host_b}"
[[ ${ip_a} != "${ip_b}" ]] || fail 'two endpoints received the same address'

assert_bridge
assert_endpoint "${ns_a}" "${ip_a}" "${host_a}"
assert_endpoint "${ns_b}" "${ip_b}" "${host_b}"
run_checked \
  "${ns_a} could not ping gateway ${GATEWAY}" \
  "${WORK_DIR}/ping-a-gateway.out" \
  ip netns exec "${ns_a}" ping -c 2 -W 2 "${GATEWAY}"
run_checked \
  "${ns_a} could not ping ${ns_b} at ${ip_b}" \
  "${WORK_DIR}/ping-a-b.out" \
  ip netns exec "${ns_a}" ping -c 2 -W 2 "${ip_b}"
run_checked \
  "${ns_b} could not ping ${ns_a} at ${ip_a}" \
  "${WORK_DIR}/ping-b-a.out" \
  ip netns exec "${ns_b}" ping -c 2 -W 2 "${ip_a}"
run_checked \
  "host could not ping ${ns_a} at ${ip_a}" \
  "${WORK_DIR}/ping-host-a.out" \
  ping -c 2 -W 2 "${ip_a}"
run_checked \
  "host could not ping ${ns_b} at ${ip_b}" \
  "${WORK_DIR}/ping-host-b.out" \
  ping -c 2 -W 2 "${ip_b}"

# C：注入 link、route、master 漂移，确认 CHECK 只报告而不修复。
CURRENT_PHASE='C CHECK drift detection'
log 'C: CHECK success and diagnosed topology drift'

# Baseline: healthy endpoint must pass CHECK.
check_endpoint "${ns_a}" >/dev/null

# Drift 1: container interface down.
ip -n "${ns_a}" link set dev eth0 down
expect_check_failure "${ns_a}" 'container-link-down'

# Restore the complete expected state.
ip -n "${ns_a}" link set dev eth0 up
ip -n "${ns_a}" route replace default via "${GATEWAY}" dev eth0
check_endpoint "${ns_a}" >/dev/null

# Drift 2: default route missing.
ip -n "${ns_a}" route delete default
expect_check_failure "${ns_a}" 'default-route-missing'

# Restore default route.
ip -n "${ns_a}" route replace default via "${GATEWAY}" dev eth0
check_endpoint "${ns_a}" >/dev/null

# Drift 3: host veth detached from bridge.
ip link set dev "${host_a}" nomaster
expect_check_failure "${ns_a}" 'host-veth-detached'

# Restore host veth.
ip link set dev "${host_a}" master "${BRIDGE_NAME}"
ip link set dev "${host_a}" up
check_endpoint "${ns_a}" >/dev/null

# D：DEL 幂等、共享资源不受影响，以及最低地址可复用。
CURRENT_PHASE='D DEL idempotency'
log 'D: normal and repeated DEL while another endpoint remains healthy'
delete_endpoint "${ns_a}"
ip link show dev "${host_a}" >/dev/null 2>&1 && fail "${host_a} remains after DEL"
assert_state_ip_absent "${ip_a}"
assert_endpoint_state_absent "${ns_a}"
delete_endpoint "${ns_a}"
assert_endpoint_state_absent "${ns_a}"
[[ -d /sys/class/net/${BRIDGE_NAME}/bridge ]] || fail 'shared bridge was deleted by DEL'
check_endpoint "${ns_b}" >/dev/null
run_checked \
  "remaining endpoint ${ns_b} could not ping gateway after peer DEL" \
  "${WORK_DIR}/ping-b-after-a-del.out" \
  ip netns exec "${ns_b}" ping -c 2 -W 2 "${GATEWAY}"

CURRENT_PHASE='D address reuse'
log 'D: released lowest address is reusable'
ns_reuse="cloudnet-test-reuse-${RUN_ID}"
create_namespace "${ns_reuse}"
add_endpoint_or_fail \
  "${ns_reuse}" \
  "${WORK_DIR}/reuse-add.json" \
  "${WORK_DIR}/reuse-add.log" \
  'address-reuse endpoint ADD failed'
ip_reuse=$(result_ip "${WORK_DIR}/reuse-add.json")
host_reuse=$(result_host_veth "${WORK_DIR}/reuse-add.json")
namespace_hosts["${ns_reuse}"]="${host_reuse}"
[[ ${ip_reuse} == "${ip_a}" ]] || fail "released address ${ip_a} was not reused; got ${ip_reuse}"
delete_endpoint "${ns_reuse}"
assert_state_ip_absent "${ip_reuse}"
assert_endpoint_state_absent "${ns_reuse}"

# E：覆盖 netns 先删除和空 CNI_NETNS 两种合法 DEL 场景。
CURRENT_PHASE='E missing netns DEL'
log 'E: namespace removal before DEL'
ns_gone="cloudnet-test-gone-${RUN_ID}"
create_namespace "${ns_gone}"
add_endpoint_or_fail \
  "${ns_gone}" \
  "${WORK_DIR}/gone-add.json" \
  "${WORK_DIR}/gone-add.log" \
  'missing-netns scenario ADD failed'
ip_gone=$(result_ip "${WORK_DIR}/gone-add.json")
host_gone=$(result_host_veth "${WORK_DIR}/gone-add.json")
namespace_hosts["${ns_gone}"]="${host_gone}"
ip netns delete "${ns_gone}"
delete_endpoint "${ns_gone}"
ip link show dev "${host_gone}" >/dev/null 2>&1 && fail "${host_gone} remains after missing-netns DEL"
assert_state_ip_absent "${ip_gone}"
assert_endpoint_state_absent "${ns_gone}"

CURRENT_PHASE='E empty CNI_NETNS DEL'
log 'E: DEL accepts an empty CNI_NETNS and remains idempotent without state'
ns_empty="cloudnet-test-empty-${RUN_ID}"
create_namespace "${ns_empty}"
add_endpoint_or_fail \
  "${ns_empty}" \
  "${WORK_DIR}/empty-add.json" \
  "${WORK_DIR}/empty-add.log" \
  'empty-CNI_NETNS scenario ADD failed'
ip_empty=$(result_ip "${WORK_DIR}/empty-add.json")
host_empty=$(result_host_veth "${WORK_DIR}/empty-add.json")
namespace_hosts["${ns_empty}"]="${host_empty}"
container_empty=$(cnitool_container_id "$(namespace_path "${ns_empty}")")
CNI_COMMAND='DEL' \
  CNI_CONTAINERID="${container_empty}" \
  CNI_NETNS='' \
  CNI_IFNAME='eth0' \
  CNI_PATH="${CNI_PATH_VALUE}" \
  /opt/cni/bin/cloudnet \
    </etc/cni/net.d/10-cloudnet.conf \
    >"${WORK_DIR}/empty-del.out" \
    2>"${WORK_DIR}/empty-del.log"
[[ ! -s ${WORK_DIR}/empty-del.out ]] || fail 'successful empty-netns DEL polluted stdout'
assert_json_logs "${WORK_DIR}/empty-del.log"
ip link show dev "${host_empty}" >/dev/null 2>&1 \
	&& fail "${host_empty} remains after empty-netns DEL"
assert_state_ip_absent "${ip_empty}"
assert_endpoint_state_absent "${ns_empty}"
delete_endpoint "${ns_empty}"

# F：预置 dummy eth0 制造冲突，验证 IP/veth 回滚且未知接口不受损。
CURRENT_PHASE='F ADD rollback injection'
log 'F: mid-ADD interface conflict rolls back allocation and veth'
ns_fail="cloudnet-test-fail-${RUN_ID}"
create_namespace "${ns_fail}"
ip -n "${ns_fail}" link add eth0 type dummy
ip -n "${ns_fail}" link set dev eth0 up
fail_host=$(derived_host_veth "$(namespace_path "${ns_fail}")" 'eth0')
before_allocations=$(jq -r '.allocations | length' "${STATE_FILE}")
if add_endpoint "${ns_fail}" "${WORK_DIR}/fail-add.out" "${WORK_DIR}/fail-add.err"; then
  fail 'ADD unexpectedly succeeded with a conflicting eth0'
fi
[[ ! -s ${WORK_DIR}/fail-add.out ]] || fail 'failed ADD polluted stdout'
after_allocations=$(jq -r '.allocations | length' "${STATE_FILE}")
[[ ${after_allocations} == "${before_allocations}" ]] \
  || fail 'failed ADD leaked an IP allocation'
ip link show dev "${fail_host}" >/dev/null 2>&1 && fail 'failed ADD leaked a host veth'
ip -n "${ns_fail}" -d link show dev eth0 | grep -qw dummy \
  || fail 'failed ADD damaged the pre-existing dummy interface'

# G：并发 ADD 验证地址唯一；并发 DEL 验证状态和链接全部释放。
CURRENT_PHASE='G concurrent ADD and DEL'
log "G: ${CONCURRENCY} concurrent ADD operations receive unique addresses"
declare -a concurrent_namespaces=()
declare -a add_pids=()
for (( index = 0; index < CONCURRENCY; index++ )); do
  namespace="cloudnet-test-con-${index}-${RUN_ID}"
  create_namespace "${namespace}"
  concurrent_namespaces+=("${namespace}")
  add_endpoint \
    "${namespace}" \
    "${WORK_DIR}/con-${index}.json" \
    "${WORK_DIR}/con-${index}.log" &
  add_pids+=("$!")
  background_pids+=("$!")
done

for pid in "${add_pids[@]}"; do
  wait "${pid}" || fail "concurrent ADD process ${pid} failed"
done

# 汇总并发 Result 后用 sort -u 检查地址唯一性。
: >"${WORK_DIR}/concurrent-ips"
for (( index = 0; index < CONCURRENCY; index++ )); do
  namespace=${concurrent_namespaces["${index}"]}
  assert_result_json "${WORK_DIR}/con-${index}.json"
  ip_address=$(result_ip "${WORK_DIR}/con-${index}.json")
  host_veth=$(result_host_veth "${WORK_DIR}/con-${index}.json")
  namespace_hosts["${namespace}"]="${host_veth}"
  printf '%s\n' "${ip_address}" >>"${WORK_DIR}/concurrent-ips"
  assert_endpoint "${namespace}" "${ip_address}" "${host_veth}"
done

unique_count=$(sort -u "${WORK_DIR}/concurrent-ips" | wc -l)
[[ ${unique_count} -eq ${CONCURRENCY} ]] \
  || fail "concurrent ADD returned only ${unique_count} unique addresses"

declare -a del_pids=()
for namespace in "${concurrent_namespaces[@]}"; do
  delete_endpoint "${namespace}" &
  del_pids+=("$!")
  background_pids+=("$!")
done
for pid in "${del_pids[@]}"; do
  wait "${pid}" || fail "concurrent DEL process ${pid} failed"
done

for namespace in "${concurrent_namespaces[@]}"; do
  host_veth=${namespace_hosts["${namespace}"]}
  ip link show dev "${host_veth}" >/dev/null 2>&1 \
    && fail "${host_veth} remains after concurrent DEL"
  assert_endpoint_state_absent "${namespace}"
done
while read -r ip_address; do
  assert_state_ip_absent "${ip_address}"
done <"${WORK_DIR}/concurrent-ips"

delete_endpoint "${ns_b}"
assert_state_ip_absent "${ip_b}"
assert_endpoint_state_absent "${ns_b}"
assert_bridge

CURRENT_PHASE='complete'
log 'PASS: ADD/CHECK/DEL, rollback, missing-netns cleanup, and concurrency all succeeded'
