# cloudnet V1

cloudnet V1 是一个面向单节点实验环境的轻量级 CNI 数据面。插件使用 Go、Linux Bridge、veth 和 network namespace，把容器接入宿主机上的 `cni-br0`，并提供本地 IPv4 地址分配、持久状态、`ADD`、`CHECK`、`DEL` 与 `VERSION`。

本项目不是一个只求 `ping` 成功的演示：实现会严格检查配置和运行时身份，持久化端点生命周期，保护并发分配，验证链接归属，并在失败时回滚已创建的资源。

## V1 范围

| 项目 | 固定值 |
| --- | --- |
| CNI network | `cloudnet-v1` |
| CNI type | `cloudnet` |
| CNI versions | `1.0.0`、`1.1.0` |
| Linux Bridge | `cni-br0` |
| 容器子网 | `10.77.0.0/24` |
| Bridge / gateway | `10.77.0.1/24` |
| 地址池 | `10.77.0.10` 到 `10.77.0.250` |
| MTU | `1500` |
| 地址族 | 仅 IPv4 |
| 二进制 | `/opt/cni/bin/cloudnet` |
| 配置 | `/etc/cni/net.d/10-cloudnet.conf` |
| 状态 | `/var/lib/cloudnet/networks/cloudnet-v1/` |
| 日志 | JSON，默认写 `stderr` |

V1 的目标是同一宿主机内的容器互通、容器到 Bridge gateway、正确的默认路由，以及完整、可恢复的 CNI 生命周期。

以下内容明确不在 V1 范围内：OVS、VXLAN、跨节点网络、Agent、控制面、Prometheus、IPv6、NetworkPolicy、端口映射和 Kubernetes 安装。V1 也不创建 NAT/SNAT 规则；详见[当前限制](#当前限制)。

## 数据路径

```text
container / network namespace
+-------------------------------+
| lo (UP)                       |
| eth0 10.77.0.x/24 (UP)        |
| default via 10.77.0.1         |
+---------------+---------------+
                |
                | veth pair, MTU 1500
                |
       cn<13 hex> (UP, no IPv4)
                |
+---------------+---------------+
| cni-br0 10.77.0.1/24 (UP)     |
+---------------+---------------+
                |
       Linux host network stack
```

物理管理网卡和 Underlay 网卡不会加入 `cni-br0`，插件也不会修改它们。核心网络逻辑通过 `github.com/vishvananda/netlink`、CNI `ns` 包和 `golang.org/x/sys/unix` 操作内核对象，不会 `exec` `ip`、`bridge` 或 `ifconfig`。命令行工具只出现在审计、集成测试和人工排障步骤中。

## 代码结构

```text
cmd/cloudnet/             CNI 入口与 VERSION 分发
internal/cni/             ADD、CHECK、DEL 编排和 Result
internal/config/          严格 JSON 与 CNI 参数校验
internal/network/         Bridge、veth、netns、地址和路由
internal/ipam/            本地分配器、flock、原子状态文件
internal/endpoint/        端点身份和持久记录
internal/transaction/     LIFO 回滚栈
internal/logging/         stderr JSON slog
internal/errs/            分类错误与上下文包装
configs/                  可安装的 CNI 配置
hack/                     安装、集成、清理和环境采集脚本
docs/                     设计、测试报告和排障手册
```

## CNI 生命周期

### ADD

1. 严格解析 stdin：拒绝未知字段、重复 JSON key、额外 JSON 值和超过限制的输入。
2. 校验固定 V1 网络参数及 `CNI_CONTAINERID`、`CNI_NETNS`、`CNI_IFNAME`；ADD 要求绝对且规范的 netns 路径。
3. 在网络级 `flock` 下为 `(network, containerID, ifName)` 分配最低可用地址，并先持久化 `pending` 记录。
4. 创建或复用 `cni-br0`。已存在对象必须确实为独立 Linux Bridge，MTU 和唯一 IPv4 地址与配置一致，已有端口也只能是当前网络能以完整 alias 证明归属的 veth；物理、未归属或其他网络端口会触发冲突，插件不会 detach 或删除它们。
5. 由 endpoint tuple 的 SHA-256 确定性生成 host/临时 peer 名称。host 名以 `cn` 开头且总长 15 字节；两端写入精确 ownership alias。
6. 创建 veth，将 peer 移入目标 netns 并重命名为 `CNI_IFNAME`；两端 MTU 设为 1500。
7. 在 netns 内设置容器地址、唯一默认路由和 `lo`/容器接口 UP。
8. 在宿主机把无 IPv4 地址的 host veth 接入 `cni-br0` 并置 UP，然后复核 alias、master 和实际网络状态。
9. 将端点状态改为 `ready` 并原子持久化。
10. stdout 只打印标准 CNI Result，其中包含 Bridge、host veth、容器接口、容器 IP、gateway 和 IPv4 默认路由。

相同 endpoint 的重复 ADD 返回原地址。若同一 endpoint 通过另一个合法 netns 路径重试，也会保留地址、更新记录并对新 namespace 验证或重建。若 `ready` 端点仍完整，插件只验证后返回；若该项目拥有的端点已不一致，则在精确归属校验后重建。中途留下的 `pending` 记录会先清理该 endpoint 的已知资源，再继续恢复。

### CHECK

CHECK 不修复状态，而是逐项验证并返回具体 mismatch：

- allocation 存在、phase 为 `ready`，且网络、容器身份、netns、veth 名、子网、gateway、地址池、Bridge 和 MTU 全部匹配；
- `cni-br0` 是 UP 的 Linux Bridge，MTU 1500，并且 IPv4 地址恰好为 `10.77.0.1/24`；
- host veth 存在、类型为 veth、alias 精确匹配、UP、MTU 正确、master 为 `cni-br0`，且没有 IPv4 地址；
- netns 可打开；容器接口名称、类型、alias、UP、MTU 和唯一 IPv4 地址正确；
- `lo` 存在且 UP；恰好一条 IPv4 默认路由通过 `10.77.0.1` 和容器接口；
- stdin 带 `prevResult` 时，将其规范化为 CNI `types/100` 的 1.0/1.1 current result 表示，并对照接口、MTU、IP、gateway 与默认路由。

### DEL

DEL 是幂等的，并允许 `CNI_NETNS` 为空。它在同一网络锁下读取端点状态，优先删除 host veth；host 端不存在且 netns 仍存在时，才尝试删除容器端。任何现存链接都必须同时满足 `veth` 类型和精确 alias 才能删除。

netns 已被 runtime 删除时，host veth 仍可从持久状态清理。状态缺失时，插件从 `(network, containerID, ifName)` 推导 host veth 名，但仍需 alias 证明归属。链接已不存在视为成功；正常清理后释放地址并移除端点记录。DEL 不删除其他端点，也不删除共享 Bridge。

### VERSION

入口使用 CNI `skel`/`version` 包发布 `1.0.0` 和 `1.1.0`，由 CNI runtime 通过 `CNI_COMMAND=VERSION` 协商。不要把普通日志或额外 JSON 写到 stdout。

## IPAM 与状态

每个安全网络名对应一个目录：

```text
/var/lib/cloudnet/networks/cloudnet-v1/
|-- .lock
`-- state.json
```

`state.json` 是该网络的单一一致性单元，包含 schema version、固定网络配置、endpoint map 和 IP-to-endpoint allocation map。endpoint key 至少覆盖 network、container ID 和 ifname；记录包含 netns 信息、确定性 host veth、容器 IP、地址规划、Bridge、MTU、phase 及时间戳。netns 只作记录，DEL 不依赖它永远存在。

所有读取、分配、phase 迁移和释放都在 `.lock` 的进程级独占 `flock` 下进行。状态路径逐组件通过 fd-relative `openat` 和 `O_NOFOLLOW` 打开；目录、lock、state 的符号链接及非普通文件都会被拒绝。写入流程为同目录临时文件、`0600`、完整写入、文件 `fsync`、`renameat`、目录 `fsync`；网络目录和锁分别使用 `0700` 与 `0600`。损坏或不满足双向映射约束的状态会返回带路径的错误，原文件不会被空状态覆盖。

地址分配只遍历闭区间 `10.77.0.10..10.77.0.250`，跳过 gateway、network 和 broadcast，选择最低未占用地址。释放不存在的 endpoint 是成功 no-op。

## 回滚和共享 Bridge

ADD 先持久化 `pending`，再触碰 netns/veth。创建失败时，局部网络回滚会删除本次创建的 veth；外层 LIFO 回滚再次按确定性名称和完整 alias 清理 host 端，确认成功后才释放 IP。这个清理是 critical barrier：若删除失败，allocation 以 `pending` 状态隔离，不能被后续 endpoint 重分配。若 ready 状态持久化失败也遵循相同顺序。回滚错误通过 error wrapping/join 与原始错误一同保留，日志字段 `rollback` 会表明执行过补偿。

Bridge 是网络级共享资源，不是 endpoint 资源。每次 DEL 删除它会中断其他容器，也会让并发 ADD/DEL 发生生命周期竞争。因此 V1 的 DEL 和测试清理都会保留 `cni-br0`；`hack/cleanup-v1.sh` 只删除能精确证明归属的 endpoint 资源。

## 日志和错误

日志由 `log/slog` JSON handler 写到 stderr。命令结束记录 `timestamp`、`level`、`command`、`network`、缩短的 `containerID`、`ifName`、`netns`、`bridge`、`hostVeth`、`containerIP`、`duration`、`phase`、`error` 和 `rollback`。插件不记录完整 stdin、token 或无限长度的 `CNI_ARGS`。

错误保留分类和上下文，例如 `invalid config`、`bridge conflict`、`allocation exhausted`、`namespace open failed`、`veth create failed`、`route configuration failed`、`state persistence failed`、`check mismatch` 和 `rollback failed`。CNI 框架负责把失败编码为标准 CNI Error；普通日志不得混入 stdout。

## 构建和安装

环境要求：Ubuntu Server 24.04、Go 1.26.x、Linux network namespace 支持。运行集成验收还需要 containerd、nerdctl、cnitool、CNI reference plugins，以及 root 权限。

```bash
cd /home/cloudnet-template/src/cloudnet
make build
make test
make test-race
make vet
sudo make install
```

`make install` 只把 `build/cloudnet` 安装为 `/opt/cni/bin/cloudnet`，并把 `configs/10-cloudnet.conf` 安装为 `/etc/cni/net.d/10-cloudnet.conf`。Makefile 不隐式调用 sudo。也可以显式运行：

`sudo make install` 不编译 Go 代码，也不下载 module；它只安装已经由普通用户构建且通过新鲜度检查的产物。`sudo make integration` 在创建 namespace 前还会逐字节核对工作区 binary/config 与已安装文件，发现旧安装会直接拒绝执行。

```bash
sudo ./hack/install-v1.sh
```

安装可重复执行，不会安装 Docker Engine，也不会修改 VMware 管理网、Underlay、现有防火墙或物理网卡。

## cnitool 复现

本机 `cnitool` 帮助确认的调用形式为 `cnitool add|check|del <net> <netns>`。以下命令显式固定插件和配置目录；测试 namespace 使用 `cloudnet-test-` 前缀：

```bash
sudo ip netns add cloudnet-test-manual-a

sudo env \
  CNI_PATH=/opt/cni/bin \
  NETCONFPATH=/etc/cni/net.d \
  CNI_IFNAME=eth0 \
  cnitool add cloudnet-v1 /var/run/netns/cloudnet-test-manual-a

sudo env \
  CNI_PATH=/opt/cni/bin \
  NETCONFPATH=/etc/cni/net.d \
  CNI_IFNAME=eth0 \
  cnitool check cloudnet-v1 /var/run/netns/cloudnet-test-manual-a

sudo ip -n cloudnet-test-manual-a -br link
sudo ip -n cloudnet-test-manual-a -4 addr show dev eth0
sudo ip -n cloudnet-test-manual-a route show
sudo ip netns exec cloudnet-test-manual-a ping -c 3 10.77.0.1
sudo ip -d link show master cni-br0

sudo env \
  CNI_PATH=/opt/cni/bin \
  NETCONFPATH=/etc/cni/net.d \
  CNI_IFNAME=eth0 \
  cnitool del cloudnet-v1 /var/run/netns/cloudnet-test-manual-a
sudo ip netns del cloudnet-test-manual-a
```

如命令中途失败，使用 `sudo ./hack/cleanup-v1.sh`。该脚本只处理能证明属于本项目的测试 namespace、veth 和测试状态，不执行宽泛的 veth 删除，也不清空 nftables/iptables。

## nerdctl 复现

所有镜像必须经 DaoCloud。nerdctl 的 CNI flags 是全局参数，必须放在子命令之前；显式路径也避免 root 与普通用户的默认配置目录不同：

```bash
sudo nerdctl \
  --cni-path=/opt/cni/bin \
  --cni-netconfpath=/etc/cni/net.d \
  run --rm \
  --network cloudnet-v1 \
  docker.m.daocloud.io/library/busybox:1.37 \
  ip addr
```

双容器验收：

```bash
sudo nerdctl \
  --cni-path=/opt/cni/bin \
  --cni-netconfpath=/etc/cni/net.d \
  run -d --name cloudnet-v1-c1 \
  --network cloudnet-v1 \
  docker.m.daocloud.io/library/busybox:1.37 sleep 3600

sudo nerdctl \
  --cni-path=/opt/cni/bin \
  --cni-netconfpath=/etc/cni/net.d \
  run -d --name cloudnet-v1-c2 \
  --network cloudnet-v1 \
  docker.m.daocloud.io/library/busybox:1.37 sleep 3600

sudo nerdctl exec cloudnet-v1-c1 ip -4 addr show dev eth0
sudo nerdctl exec cloudnet-v1-c2 ip -4 addr show dev eth0
sudo nerdctl exec cloudnet-v1-c1 ip route
sudo nerdctl exec cloudnet-v1-c2 ip route

# 从上一条输出读取 c2 的地址，再从 c1 发起：
sudo nerdctl exec cloudnet-v1-c1 ping -c 3 <c2-ip>

sudo bridge fdb show br cni-br0
sudo ip -d link show master cni-br0
sudo sed -n '1,240p' /var/lib/cloudnet/networks/cloudnet-v1/state.json

sudo nerdctl \
  --cni-path=/opt/cni/bin \
  --cni-netconfpath=/etc/cni/net.d \
  rm -f cloudnet-v1-c1 cloudnet-v1-c2
```

最后一条命令只删除这两个命名测试容器，并让 runtime 触发 DEL。随后检查 state 中 endpoint/allocation 已释放；再次启动测试容器可观察最低可用地址是否被安全复用。

## 故障注入

优先运行带 trap 的特权测试，而不是手工破坏宿主机网络：

```bash
sudo make integration
sudo make clean-test
```

集成测试覆盖的故障包括容器接口 DOWN、默认路由缺失、host veth 脱离 Bridge、目标接口冲突、缺失 netns 和并发 endpoint。每次故障后都应验证具体 CHECK 错误，并检查没有残留 veth、错误占用 IP 或受影响的其他 endpoint。

失败时脚本会先把当前 phase、失败命令、有限长度的 CNI 输出、state 摘要和网络快照写到 stderr，再清理本次测试资源。临时目录默认仍会删除；需要保留完整文件调查时可显式运行 `sudo env CLOUDNET_KEEP_FAILURE_ARTIFACTS=1 make integration`。

手工注入只允许作用于 `cloudnet-test-` namespace 或能由 alias 精确证明归属的 veth。例如可把测试 namespace 的 `eth0` 置 DOWN 后运行 CHECK，再恢复 UP；可删除测试 namespace 的默认路由后运行 CHECK，再按 `via 10.77.0.1 dev eth0` 恢复。不要在有 endpoint 时替换 `cni-br0`，不要创建同名非 Bridge 对象覆盖现有接口。

详细步骤见[故障排查](docs/troubleshooting-v1.md)和[测试报告](docs/test-report-v1.md)。

## 测试

```bash
make test
make test-race
make vet
make verify
sudo make integration
```

`make verify` 是非特权验证；特权集成测试必须由 root 显式启动。测试脚本只创建明确的 cloudnet 测试资源，并通过 trap 清理。实际执行状态和输出摘要只记录在 [docs/test-report-v1.md](docs/test-report-v1.md)；未执行的 cnitool、nerdctl 或特权测试不会标记为通过。

## 当前限制

- V1 是单节点 IPv4 数据面，没有跨节点封装或路由分发。
- V1 不实现 NAT/SNAT/Masquerade，也不修改 nftables/iptables。容器已经有 `default via 10.77.0.1`，但访问外部网络仍要求宿主机另行启用 forwarding 和经过审查的 SNAT；这不是当前支持能力。
- Bridge 是持久共享资源，最后一个 endpoint DEL 后仍保留；运维清理需显式进行所有权和占用检查。
- 本地 state 是单节点事实来源，不支持多节点共享或控制面分配。
- 固定配置不接受自定义 subnet、Bridge 或 MTU；这避免 V1 状态与共享数据面发生漂移。
- 不提供 NetworkPolicy、DNS 管理、端口映射、带宽整形、IPv6 或多接口编排。

## V2 后端边界

V2 可以在不改 CNI 协议、配置校验、endpoint identity、IPAM、状态原子性、日志和事务语义的前提下替换 `internal/network` 后端。现有 CNI service 通过 `NetworkOps` 边界调用 Bridge/endpoint ensure、check 和 delete；OVS 后端应实现等价契约，并新增自己的端口 ownership 与事务验证。

VXLAN、跨节点 overlay、Agent 和控制面属于后续独立设计。它们不应以条件分支混入 V1 Linux Bridge 路径，也不应复用未经版本化的数据面状态。关键取舍见 [docs/design-v1.md](docs/design-v1.md)。
