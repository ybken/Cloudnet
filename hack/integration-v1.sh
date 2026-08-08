#!/usr/bin/env bash
set -Eeuo pipefail

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
RUN_ID="$(date -u +%Y%m%d%H%M%S)-$$"
readonly RUN_ID
WORK_DIR=$(mktemp -d "/tmp/cloudnet-test-${RUN_ID}.XXXXXX")
readonly WORK_DIR
readonly CONCURRENCY="${CLOUDNET_TEST_CONCURRENCY:-6}"
readonly KEEP_FAILURE_ARTIFACTS="${CLOUDNET_KEEP_FAILURE_ARTIFACTS:-0}"

CURRENT_PHASE='startup'

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

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is missing: $1"
}

is_test_namespace() {
  [[ $1 =~ ^cloudnet-test-[a-z0-9][a-z0-9_.-]*$ ]]
}

cnitool_container_id() {
  local netns_path=$1
  local digest

  digest=$(printf '%s' "${netns_path}" | sha512sum | awk '{print $1}')
  printf 'cnitool-%s\n' "${digest:0:20}"
}

endpoint_digest() {
  local container_id=$1
  local if_name=$2

  printf '%s\0%s\0%s' "${NETWORK_NAME}" "${container_id}" "${if_name}" \
    | sha256sum \
    | awk '{print $1}'
}

derived_host_veth() {
  local netns_path=$1
  local if_name=$2
  local container_id digest

  container_id=$(cnitool_container_id "${netns_path}")
  digest=$(endpoint_digest "${container_id}" "${if_name}")
  printf 'cn%s\n' "${digest:0:13}"
}

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

create_namespace() {
  local namespace=$1

  is_test_namespace "${namespace}" || fail "unsafe test namespace name: ${namespace}"
  ip netns add "${namespace}"
  namespaces+=("${namespace}")
  namespace_ifnames["${namespace}"]='eth0'
}

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

check_endpoint() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}

  CNI_PATH="${CNI_PATH_VALUE}" \
    NETCONFPATH="${NETCONF_PATH}" \
    CNI_IFNAME="${if_name}" \
    cnitool check "${NETWORK_NAME}" "$(namespace_path "${namespace}")"
}

delete_endpoint() {
  local namespace=$1
  local if_name=${namespace_ifnames["${namespace}"]:-eth0}

  CNI_PATH="${CNI_PATH_VALUE}" \
    NETCONFPATH="${NETCONF_PATH}" \
    CNI_IFNAME="${if_name}" \
    cnitool del "${NETWORK_NAME}" "$(namespace_path "${namespace}")" >/dev/null
}

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

on_error() {
  local status=$1
  local line=$2
  local command=$3

  printf '[integration-v1] ERROR: phase=%s exit=%d line=%s command=%q\n' \
    "${CURRENT_PHASE}" "${status}" "${line}" "${command}" >&2
}

run_checked() {
  local label=$1
  local output_file=$2
  shift 2

  if ! "$@" >"${output_file}" 2>&1; then
    print_artifact "${output_file}"
    fail "${label}"
  fi
}

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

cleanup() {
  local status=$?
  local namespace pid

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

trap 'on_error "$?" "$LINENO" "$BASH_COMMAND"' ERR
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

assert_state_ip_absent() {
  local ip_address=$1

  if [[ -e ${STATE_FILE} ]]; then
    jq -e --arg ip "${ip_address}" '(.allocations | has($ip)) | not' "${STATE_FILE}" >/dev/null \
      || fail "IP ${ip_address} remains allocated"
  fi
}

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
