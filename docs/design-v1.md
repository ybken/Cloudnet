# cloudnet V1 设计

## 1. 背景与目标

cloudnet V1 在单台 Ubuntu Server 24.04 节点上实现一个可安装、可验证、可恢复的 CNI 数据面。数据面由 Linux Bridge 和 veth 组成，本地 IPAM 为每个 endpoint 分配 IPv4 地址。设计重点不是扩大功能面，而是把 CNI 生命周期、内核状态、磁盘状态和失败语义做完整。

V1 的固定契约为：

- network `cloudnet-v1`，plugin type `cloudnet`；
- CNI `1.0.0` 与 `1.1.0`；
- Bridge `cni-br0`，地址 `10.77.0.1/24`，MTU 1500；
- endpoint 池 `10.77.0.10..10.77.0.250`；
- 单节点、仅 IPv4；
- 默认路由 `0.0.0.0/0 via 10.77.0.1`。

非目标包括 OVS、VXLAN、跨节点、Agent、控制面、Prometheus、IPv6、Kubernetes 安装、NetworkPolicy 和 NAT。Docker Engine 与 `docker` CLI 也不属于项目依赖。

## 2. 设计原则

1. **冲突时停止**：已有同名接口、Bridge 类型、MTU、地址或路由不符合契约时明确失败，不静默接管或改写。
2. **先留下证据**：ADD 在内核变更前持久化 `pending` endpoint，使进程崩溃后仍能判断应清理什么。
3. **删除需要所有权证明**：确定性名称只用于定位；veth 类型与完整 alias 才是删除许可。
4. **磁盘状态是一个事务单元**：单网络单 JSON 状态，endpoint 与 allocation 双向映射一起原子替换。
5. **共享资源与 endpoint 分离**：Bridge 属于 network，veth/IP 属于 endpoint；DEL 只处理后者。
6. **协议通道隔离**：stdout 只承载 CNI Result/Error，结构化日志只写 stderr。
7. **核心逻辑不调用命令行**：生产路径使用 CNI、ns、netlink、unix 和标准库；`ip`、`bridge`、`cnitool` 仅用于测试和诊断。

## 3. 组件边界

| 包 | 职责 |
| --- | --- |
| `cmd/cloudnet` | `skel.PluginMainFuncs` 入口、VERSION 和 CNI Error/Result 分发 |
| `internal/config` | 严格 JSON、固定 V1 配置、CNI runtime 参数和路径安全校验 |
| `internal/cni` | ADD/CHECK/DEL 编排、状态与网络事务、prevResult、Result |
| `internal/network` | Linux Bridge、veth、netns、地址、route、ownership 的 netlink 实现 |
| `internal/ipam` | IPv4 range、flock、state load/validate/atomic write |
| `internal/endpoint` | endpoint key、phase 和持久记录 schema |
| `internal/transaction` | 具名 LIFO compensating action stack |
| `internal/logging` | stderr JSON slog 及统一 invocation 字段 |
| `internal/errs` | 可识别错误种类及 context-preserving wrapping |

`internal/cni.Service` 依赖 `NetworkOps`，Linux 实现提供 `EnsureBridge`、`CheckBridge`、`CreateEndpoint`、`CheckEndpoint` 和 `DeleteEndpoint`。这个边界既便于非特权单元测试，也限定了未来替换数据面后端时不得泄漏到 IPAM/CNI 层的职责。

## 4. 数据路径与内核原理

```text
                  same-host L2 traffic
       +---------------------------------------+
       |                                       |
+------+-------+                         +-----+--------+
| netns A      |                         | netns B      |
| lo UP        |                         | lo UP        |
| eth0         |                         | eth0         |
| 10.77.0.10/24|                         | 10.77.0.11/24|
+------+-------+                         +-----+--------+
       | veth                                   | veth
       |                                        |
  cn<hash-a>                               cn<hash-b>
       |                                        |
+------+----------------------------------------+------+
| cni-br0 10.77.0.1/24, MTU 1500, UP                  |
+--------------------------+---------------------------+
                           |
                    host network stack
```

- **network namespace** 隔离链接、地址和路由表；`ns.GetNS`/`NetNS.Do` 保证 netlink 调用发生在目标 namespace。
- **veth** 是成对的虚拟以太设备，一端收到的 frame 从另一端出现。删除任一端会删除 pair。
- **Linux Bridge** 在同一二层广播域内学习 MAC/FDB 并转发 frame。管理/Underlay 物理接口从不作为 slave；插件只把自己刚创建且校验过 alias 的 host veth 加入 Bridge。
- **地址和路由**：endpoint 与 gateway 同属 `/24`，邻居发现直接到 Bridge；容器间流量由 Bridge 交换。到 `10.77.0.1` 的流量进入 host stack。外部目的匹配默认路由，但 V1 不提供 host forwarding/SNAT。
- **MTU**：Bridge、host veth、container veth 都必须为 1500；CHECK 会检测漂移。

host veth 不配置 IPv4 地址。这样 host 的三层身份只落在共享 Bridge 上，避免每个端口产生多余的 connected route 或 ARP identity。

## 5. 配置与身份校验

JSON parser 先扫描 token 检测重复 object key，再用 `DisallowUnknownFields` 做 typed decode，并要求只有一个 JSON value。最大 stdin 为 1 MiB。V1 只接受固定网络名、type、Bridge、MTU 和地址规划；`log.level` 可为 debug/info/warn/error。

network name 使用 1 到 63 字节的保守 ASCII grammar：首尾为字母或数字，中间仅允许字母、数字、点、下划线和连字符。这从构造上拒绝绝对路径、`..` 和路径分隔符。状态目录还拒绝 symlink。

ADD/CHECK 要求：

- 非空且受长度/字符约束的 container ID；
- 1 到 15 字节的安全 ifname；
- 非空、绝对、clean、无 NUL 的 netns 路径。

DEL 仍要求 container ID 和 ifname，但允许 netns 为空；若提供 netns，则继续严格校验。

## 6. 确定性命名与所有权

endpoint tuple 为 `(networkName, containerID, ifName)`。命名模块以 NUL 分隔 tuple 后计算 SHA-256：

- host veth：`cn` + 13 个 hex 字符，恰好不超过 Linux 15 字节限制；
- 临时 peer：`cp` + 13 个 hex 字符，移动后重命名为 CNI ifname；
- alias：`cloudnet:v1:<network>:<full sha256 hex>`。

短名称便于在 state 丢失时推导，但 52-bit 截断 hash 理论上仍可能碰撞，因此创建时若名称已存在就失败。删除更不能仅凭短名称：链接必须是 `veth`，alias 必须与 endpoint 的完整 digest 精确相等。alias 同时写在 pair 两端，因此 host 端缺失但 netns 存在时仍可验证并删除 container 端。

## 7. ADD 时序

```text
CNI runtime      Service          IPAM/state       netlink/ns       stdout/stderr
    |               |                  |                |                 |
    | ADD + stdin   |                  |                |                 |
    |-------------->| parse/validate   |                |                 |
    |               | flock            |                |                 |
    |               |----------------->|                |                 |
    |               | allocate pending |                |                 |
    |               |----------------->| fsync+rename   |                 |
    |               | ensure bridge    |--------------->|                 |
    |               | create/move veth |--------------->|                 |
    |               | addr/route/up    |--------------->|                 |
    |               | verify endpoint  |--------------->|                 |
    |               | mark ready       |                |                 |
    |               |----------------->| fsync+rename   |                 |
    |               | unlock           |                |                 |
    | CNI Result    |----------------------------------------------->stdout|
    |               | completion JSON -------------------------------->stderr
```

详细步骤如下：

1. 解析配置并校验 runtime 参数，构造 allocation range、Bridge spec、确定性名称和 alias。
2. 获取 network flock。V1 在整个 ADD transaction 持锁，包括内核网络操作；这会串行化同一网络的 endpoint 变更，换来磁盘状态与补偿动作的简单一致性。
3. `Allocate` 返回已有 record 或最低可用 IP。新 record 先标记 `pending` 并显式 Commit。
4. `EnsureBridge`：不存在则创建；存在则确认类型。Bridge 自身不得被 enslave，已有 port 必须是具有当前 network 完整 ownership alias 的 veth；物理、未归属或其他 network port 均在任何地址/UP 写操作前触发 conflict。MTU 必须完全相同。IPv4 地址可以从 absent 补为 gateway，但任何不同或额外 IPv4 地址均为 conflict。最后设 UP 并复核。
5. 目标 netns 先确认 CNI ifname 不存在；host namespace 中 host/peer 临时名称也必须不存在。
6. 创建 veth、设置两端 MTU/alias、移动 peer、把 host 端接入 Bridge 并置 UP。
7. netns 内校验 alias，peer 重命名，配置唯一 IPv4，接口置 UP，添加唯一 IPv4 默认路由，并把 `lo` 置 UP。
8. 复核 host 类型/alias/MTU/UP/master/no-IPv4。
9. endpoint phase 改为 `ready` 并 Commit，清空 rollback stack，释放 flock。
10. 由 record 生成当前 CNI Result，再由 CNI library 按请求版本打印。

Bridge 创建过程内部失败时，只回滚本次新建的 Bridge。Bridge 已成功 ensure 后若 endpoint 后续失败，则保留 Bridge，因为它已成为有效的 network 级共享资源。

## 8. 重复 ADD 与崩溃恢复

| 观察到的状态 | 行为 |
| --- | --- |
| 无 record | 分配 IP，持久化 pending，创建网络，转 ready |
| ready 且实际 endpoint 完整 | 返回相同 IP，不重复创建 |
| ready 但 owned endpoint 不一致 | 精确 ownership 清理后，以同一 record/IP 重建 |
| pending | 清理该 endpoint 可能残留的 owned link，再继续创建 |
| 同 key、合法新 netns 路径 | 保留原 IP，更新 record，在新 namespace 验证或重建 |
| 同 key 但网络配置/host name 不同 | endpoint conflict，拒绝复用 |

关键 crash window：

- pending Commit 前退出：没有持久 allocation，也没有内核变更；
- pending Commit 后、veth 前退出：后续 ADD/DEL 能看到 pending 并释放；
- veth 创建后、ready 前退出：alias 和 pending record 共同提供恢复证据；
- ready Commit 后、Result 输出前退出：runtime 重试 ADD 会校验实际 endpoint 并返回相同 IP。

## 9. CHECK 设计

CHECK 只读地比较三类事实：配置/调用身份、持久状态、Linux 实际状态。

| 层次 | 检查项 |
| --- | --- |
| state | endpoint 存在、ready、完整 record 合法，identity/netns/host name/地址规划/Bridge/MTU 与本次调用一致 |
| Bridge | 对象存在且为独立 Linux Bridge、UP、MTU 1500、唯一 IPv4 为 gateway prefix；所有 port 都是当前 network owned veth |
| host veth | 存在、类型 veth、精确 alias、UP、MTU、master index、无 IPv4 |
| netns | 路径存在且可打开 |
| container | ifname 存在、veth、alias、UP、MTU、唯一预期 IPv4/prefix |
| loopback | `lo` 存在且 UP |
| route | 恰好一条 IPv4 default，gateway 和 link index 正确 |
| prevResult | 能按 CNI 版本解析；接口 identity/MTU、容器 IP、gateway、default route 与 record/调用一致 |

任何不一致均在 `check mismatch` 外层保留具体原因，例如 `bridge ... is down`、`host veth ... master index ...` 或 `default route is missing`。CHECK 不自动修复，因为静默修复会掩盖 runtime/state 漂移。

## 10. DEL 时序与缺失 netns

DEL 解析同一固定配置，但 netns 可为空。它获取 network flock，按 endpoint key 查 state：

1. state 存在时，确认 persisted host veth 等于确定性推导值；使用本次 CNI_NETNS（可能为空），不盲信 record 中可能过期的 netns。
2. state 缺失时仍推导 host veth 和完整 alias，以处理 state 已清但 link 尚在的重试窗口。
3. host link 存在：验证 veth + exact alias 后删除。删除 pair 会同步清理 container 端。
4. host link 不存在且 netns 非空/存在：进入 netns 查找 CNI ifname；不存在是成功，存在则验证 ownership 后删除。
5. netns 已消失、链接也不存在：成功 no-op。
6. 只有内核 endpoint 清理成功后才释放 allocation/record；WithLock 在返回前原子 Commit。
7. 不删除 Bridge，不触碰其他 endpoint。

若进程在链接删除后、state Commit 前崩溃，重试 DEL 会观察到链接缺失，再幂等释放原 record。若 ownership 不匹配，DEL 失败并保留 state，避免释放 IP 后把未知同名链接留成不可追踪对象。

## 11. IPAM 数据模型

目录布局：

```text
/var/lib/cloudnet/networks/<safe-network-name>/
|-- .lock       # permanent flock target, mode 0600
`-- state.json  # versioned state, mode 0600
```

`state.json` 逻辑 schema：

```json
{
  "version": 1,
  "networkName": "cloudnet-v1",
  "updatedAt": "<UTC timestamp>",
  "config": {
    "subnet": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "rangeStart": "10.77.0.10",
    "rangeEnd": "10.77.0.250",
    "bridge": "cni-br0",
    "mtu": 1500
  },
  "endpoints": {
    "<sha256 endpoint key>": {
      "networkName": "cloudnet-v1",
      "containerID": "<runtime ID>",
      "ifName": "eth0",
      "netns": "<informational path>",
      "hostVethName": "cn<13 hex>",
      "containerIP": "10.77.0.10",
      "phase": "ready"
    }
  },
  "allocations": {
    "10.77.0.10": "<sha256 endpoint key>"
  }
}
```

实际 endpoint record 还持久化 subnet、gateway、range、Bridge、MTU、createdAt 和 updatedAt。load 时验证：schema version、network name、config、record、地址可用性、map 数量，以及 endpoint-to-IP 与 IP-to-endpoint 的双向一致性。

分配器从 rangeStart 到 rangeEnd 逐地址扫描，排除 network/broadcast/gateway 和已用地址，返回最低可用值。相同 key 返回同一 record；range exhausted 返回独立错误。Release 同时从两个 map 删除，重复 Release 为 no-op。即使最后一个 endpoint 被释放，network config 仍留在 state 中，防止复用同一 Bridge 时悄然改变网络身份。

## 12. 并发与持久性

状态根目录到 network 目录的每个路径组件都通过已验证 directory fd 和 `openat(O_NOFOLLOW|O_CLOEXEC)` 打开或创建；`.lock` 与 `state.json` 还必须是单硬链接普通文件。`.lock` 使用 `Flock(LOCK_EX)`，EINTR 时重试。所有 CNI 进程在同一 network directory 上竞争同一 inode，因此 50 个并发 ADD 也只能在持锁视图上选择地址，不会读取同一旧快照。

写入算法：

1. 在 state 同目录创建 `.state.json.tmp-*`；
2. chmod 0600；
3. encode、完整写入、file fsync、close；
4. atomic rename 覆盖 `state.json`；
5. directory fsync，持久化目录项更新；
6. 任一步失败清理尚未 rename 的临时文件并返回错误。

读取或 invariant 校验失败时返回 `CorruptStateError{Path, Err}`。callback 不会拿到空 state，因而不会覆盖调查证据。

## 13. 回滚模型

外层 transaction stack 注册具名补偿动作，并严格按 LIFO 执行：

```text
forward:   persist pending -> create veth -> persist ready
rollback:                    delete veth => release allocation
```

其中 `=>` 是依赖屏障而不是普通顺序：外层预先注册 host-only 的 critical cleanup，只有确定性 host veth 不存在或在完整 alias 校验后删除成功，才允许释放 allocation。删除失败时停止更早的补偿并保留 `pending` allocation，避免残留接口的地址被重新分配。host-only 避免进入可能已经复用的 netns 误碰同名无关接口。

网络层 `CreateEndpoint` 还负责自己的细粒度补偿：一旦 `LinkAdd` 成功，后续 alias、move、master、address、route 或 verification 失败都会定位本次 host veth 并删除 pair。只有链接已经消失才把 delete error 视为完成；否则通过 `errors.Join` 保留 rollback failure。

transaction stack 的 `RollbackError` 同时 unwrap 原始 cause 和各补偿 failure。这样不会发生“回滚错误覆盖最初故障”的诊断丢失。日志的 phase 指出失败阶段，rollback boolean 指示是否执行过外层补偿。

## 14. Bridge 生命周期决策

`cni-br0` 是 network-level shared resource。endpoint DEL 不删除它，原因有四点：

1. 其他 endpoint 可能仍是 Bridge slave；
2. 并发 ADD 可能刚完成 ensure、尚未创建 veth；
3. 删除 gateway 会立即中断仍存 endpoint 的 host/gateway 连通；
4. 把“最后一个 endpoint”判断与 kernel/state 做成跨进程无竞态事务，复杂度不适合 V1。

保留空 Bridge 的代价是需要显式运维清理，但这是可见且可控的。安装、DEL、integration cleanup 均不会把物理网卡加入或移出 Bridge，也不会修改管理/Underlay。

## 15. stdout、日志与错误协议

- ADD 成功：stdout 为 CNI Result；
- CHECK/DEL 成功：stdout 无普通文本；
- 命令失败：CNI skel 输出标准 CNI Error JSON；
- VERSION：version package 输出支持版本；
- 所有应用日志：stderr JSON。

logger 将顶层 time key 改名为 `timestamp`，container ID 截断到 12 字节。每次操作结束写一条统一 completion record。配置解析前使用 info logger；配置有效后按 `log.level` 重建 logger。完整 stdin 和 CNI_ARGS 不进入日志。

## 16. 安全边界

- 不调用 `iptables -F` 或 `nft flush ruleset`，V1 根本不创建防火墙/NAT 规则；
- 不枚举后宽泛删除 veth；只删除确定性 endpoint 且 alias 精确匹配的链接；
- 不删除非 `cloudnet-test-*` namespace；
- 不修改 VMware 网卡、管理网或 Underlay，不把物理接口加入 `cni-br0`；
- 不自动删除共享 Bridge；
- 不信任 network name 作为未经校验的路径；
- 不跟随 state hierarchy、lock 或 state file 的符号链接；
- 不用损坏 state 继续运行；
- 安装目标仅为项目 binary 与单个配置文件。

## 17. V2 OVS 替换边界

V2 若采用 OVS，应新增等价的 `NetworkOps` backend，而不是在 Linux Bridge 函数内部散布 OVS 分支。以下契约应保持：

- CNI stdin/runtime validation 和 stdout/stderr 规则；
- endpoint tuple、确定性 ownership identity 和安全 DEL；
- IPAM range、flock、atomic state、pending/ready phase；
- ADD idempotency、CHECK specificity、DEL/missing-netns 语义；
- transaction compensation 和 error wrapping。

需要替换的部分是 Bridge/port 的 ensure、check、create、delete 与数据面 ownership 证明。OVS port UUID/external IDs 应承担 Linux veth alias 对应的证明职责，并使用版本化的新 backend metadata。VXLAN、跨节点隧道、Agent 与控制面是再上一层的独立工作，不属于一次“换 backend”即可偷偷带入的范围。
