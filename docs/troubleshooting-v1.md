# cloudnet V1 故障排查

## 安全规则

排障前先确认资源确实属于 cloudnet。只读命令可以广泛查看，但删除动作必须同时满足项目命名和 ownership alias 证明。

任何场景都不要执行：

```text
iptables -F
nft flush ruleset
删除所有 veth
删除所有 netns
git reset --hard
把管理/Underlay/VMware 网卡加入或移出 cni-br0
```

推荐先采集证据：

```bash
cd /home/cloudnet-template/src/cloudnet
./hack/collect-env.sh

sudo ip -d link show cni-br0
sudo ip -d link show master cni-br0
sudo bridge link show
sudo ip netns list
sudo sed -n '1,260p' /var/lib/cloudnet/networks/cloudnet-v1/state.json
```

`collect-env.sh` 只读，不应改变网络。命令报错时保存 stdout、stderr、命令名、container ID、ifname、netns 和时间点；不要只保留最后一句错误。

## 1. stdout 被日志污染

### 现象

- runtime 报 JSON decode error、`invalid character` 或无法解析 CNI Result；
- stdout 中在 Result/Error 前后出现普通文本或 JSON log；
- 插件从终端直接运行时，看起来 Result 和日志混在一起。

### 诊断

shell 终端会同时显示 stdout/stderr，不能据此判断协议污染。用测试 namespace 或不会进入内核阶段的非法配置，把两条流分别重定向，再分别验证：

```bash
/opt/cni/bin/cloudnet < /tmp/cloudnet-invalid.conf \
  > /tmp/cloudnet.stdout \
  2> /tmp/cloudnet.stderr

sed -n '1,80p' /tmp/cloudnet.stdout
sed -n '1,80p' /tmp/cloudnet.stderr
```

直接调用时仍需完整 CNI 环境变量；更稳妥的复现方式是运行仓库中的 stdout 隔离测试或 integration script。预期：stdout 只有一个标准 CNI Result/Error JSON，stderr 每行都是独立 JSON log。空 stdin 会被 CNI skel 在 handler 前拒绝，因此 stderr 可能为空，这是正常行为。

### 处理

- 检查新增代码是否使用 `fmt.Print*`、默认输出到 stdout 的 logger，或把命令调试文本写到了 stdout；
- 业务日志统一经 `internal/logging` 写 `os.Stderr`；
- Result 使用 CNI `types.PrintResult`，Error 交给 `skel.PluginMainFuncs`；
- 修复后运行 `go test ./...` 和 stdout 隔离用例，再做一次 cnitool ADD/CHECK/DEL。

## 2. IPAM 状态损坏

### 现象

stderr/CNI Error 包含 `state persistence failed`、`cloudnet state ... is corrupt`、JSON decode error、unsupported version 或 endpoint/allocation mismatch。后续 ADD/CHECK/DEL 会继续失败；插件不会用空 state 覆盖文件。

### 诊断

```bash
sudo stat /var/lib/cloudnet/networks/cloudnet-v1/state.json
sudo sed -n '1,320p' /var/lib/cloudnet/networks/cloudnet-v1/state.json
sudo jq . /var/lib/cloudnet/networks/cloudnet-v1/state.json
sudo ls -la /var/lib/cloudnet/networks/cloudnet-v1
sudo ip -d link show master cni-br0
```

先复制证据到项目外的调查目录：

```bash
sudo cp --preserve=all \
  /var/lib/cloudnet/networks/cloudnet-v1/state.json \
  /tmp/cloudnet-state.json.corrupt
```

逐项对照：`version`、`networkName`、固定 config、endpoint key、`containerIP`、`hostVethName`、phase，以及 endpoints/allocations 的双向映射。

### 处理

不要写入 `{}`、不要删除损坏文件后直接重跑 ADD、不要手工把某个 IP 从 map 中删掉而不处理对应 endpoint。先核对每个持久 endpoint 与带 `cloudnet:v1:` alias 的实际 veth，再制定一次性恢复方案。测试环境可在保留副本后运行 `sudo make clean-test`；它只处理严格的测试资源。非测试状态需要人工逐 endpoint 对账，本项目不自动“猜测修复”损坏 state。

## 3. netns 不存在

### 现象

- ADD/CHECK 返回 `namespace open failed`；
- runtime 先删除 namespace，再调用 DEL；
- state 的 `netns` 路径已失效，但 host veth 仍存在。

### 诊断

```bash
sudo ip netns list
sudo test -e /var/run/netns/<cloudnet-test-name>
sudo ip -d link show master cni-br0
sudo sed -n '1,260p' /var/lib/cloudnet/networks/cloudnet-v1/state.json
```

ADD/CHECK 必须能打开 netns，路径缺失应失败。DEL 不同：它允许 `CNI_NETNS` 为空或路径已消失，并用 state 中的 host veth 名清理；state 缺失时也能由 endpoint tuple 推导名称。

### 处理

- 对 runtime teardown，让 runtime 正常重试 DEL；不要为了 DEL 重新创建一个同路径但身份不同的 namespace；
- cnitool 故障注入应使用 integration script，它会保留原 endpoint identity 并传入原路径；
- host veth 删除前仍必须通过 veth type + exact alias；
- DEL 成功后确认 endpoint/allocation 消失、host veth 消失，而 `cni-br0` 保留。

## 4. veth 残留

### 现象

DEL 后 `ip link` 仍显示 `cn...`，或 ADD 报确定性 host/peer 名已存在。

### 诊断

```bash
sudo ip -d link show master cni-br0
sudo ip -d -o link show type veth
sudo bridge link show
sudo sed -n '1,300p' /var/lib/cloudnet/networks/cloudnet-v1/state.json
```

真正的 cloudnet host veth 应满足：

- 名称为 endpoint tuple 推导的 `cn<13 hex>`；
- type 为 `veth`；
- alias 精确为 `cloudnet:v1:cloudnet-v1:<full digest>`；
- 正常 endpoint 的 master 为 `cni-br0`。

名称前缀本身不是删除依据。一个叫 `cn...` 但没有精确 alias 的接口必须保留并报告 ownership conflict。

### 处理

```bash
cd /home/cloudnet-template/src/cloudnet
sudo make clean-test
```

cleanup 会把严格的测试 namespace 与 state 对账，并在删除前复核确定性名称、类型和完整 alias。若它因 ownership mismatch 拒绝删除，保留现场并人工调查，不要用 `ip link show type veth` 的结果批量删除。

## 5. Bridge 配置冲突

### 现象

ADD/CHECK 返回 `bridge conflict` 或具体 MTU/address/type mismatch。

### 诊断

```bash
sudo ip -d link show cni-br0
sudo ip -4 addr show dev cni-br0
sudo bridge link show master cni-br0
```

预期对象必须：

- type 为 Linux `bridge`；
- MTU 为 1500；
- UP；
- IPv4 地址恰好为 `10.77.0.1/24`。

已有同名 dummy/veth/物理接口、MTU 不是 1500、存在不同/额外 IPv4 地址都会触发拒绝。Bridge 没有 IPv4 时 ADD 可以补上 gateway；CHECK 永远只检查，不修改。

### 处理

先判断对象是谁创建、是否有非 cloudnet 用户、是否承载 endpoint。插件不会替换冲突对象。只有在确认是隔离实验中的项目测试对象、没有其他 endpoint 且已经保留证据后，才由明确的项目清理流程处理。不要直接删除同名未知接口，更不要修改物理/VMware 网卡的 master。

空但正确的 `cni-br0` 是预期状态：endpoint DEL 故意不删除共享 Bridge。

## 6. 默认路由缺失或冲突

### 现象

- CHECK 返回 `default route is missing`、`multiple IPv4 default routes` 或 `conflicting IPv4 default route`；
- 容器能看到 eth0 地址但到 gateway/外部目的的路径不正确；
- ADD 返回 `route configuration failed`。

### 诊断

```bash
sudo ip -n <cloudnet-test-name> -4 route show
sudo ip -n <cloudnet-test-name> -4 addr show dev eth0
sudo ip -n <cloudnet-test-name> link show dev eth0
```

预期只有一条 IPv4 default：

```text
default via 10.77.0.1 dev eth0
```

ADD 不接管预先存在的 default route，即使它看似相同；干净 CNI netns 中已有 default 表示调用顺序或其他插件发生冲突。

### 处理

在 fault-injection 测试 namespace 中可显式恢复：

```bash
sudo ip -n <cloudnet-test-name> route add default via 10.77.0.1 dev eth0
```

生产式 runtime 场景优先执行该 endpoint 的 DEL 后重新 ADD，而不是长期手工修补。若有多个网络插件，检查 config list 顺序和 prevResult，确认谁负责 default route。

## 7. CHECK 失败

### 现象

runtime 只报告 CNI CHECK 失败，但 endpoint 可能仍部分可通信。

### 诊断顺序

1. 从 stderr JSON 读取 `phase` 和完整 wrapped `error`；
2. 检查 state 是否存在且 phase 为 `ready`；
3. 检查 Bridge type/UP/address/MTU；
4. 检查 host veth type/alias/UP/master/MTU/no-IPv4；
5. 检查 netns、container eth0、lo、地址和默认路由；
6. 若配置含 prevResult，检查 interface sandbox、MTU、IP、gateway 和 default route。

```bash
sudo ip -d link show cni-br0
sudo ip -d link show master cni-br0
sudo ip -n <cloudnet-test-name> -d link show
sudo ip -n <cloudnet-test-name> -4 addr show
sudo ip -n <cloudnet-test-name> -4 route show
```

### 处理

CHECK 设计为检测器，不会自动修复。先依据具体 mismatch 查明是 fault injection、外部工具漂移、runtime 顺序还是 state 问题。对确认属于 cloudnet 的单个 endpoint，可 DEL 后 ADD 重建。不要把 `check failed` 当作理由清空 Bridge、全部 veth 或防火墙。

## 8. nerdctl 找不到 CNI 配置

### 现象

nerdctl 报 network `cloudnet-v1` 不存在、找不到 config 或找不到 plugin binary。普通用户 `nerdctl --help` 显示的默认 netconf path 可能是用户 home，而安装发生在 `/etc/cni/net.d`。

### 诊断

```bash
sudo ls -l /opt/cni/bin/cloudnet
sudo ls -l /etc/cni/net.d/10-cloudnet.conf
sudo /opt/cni/bin/cloudnet
nerdctl --help
```

直接运行 plugin 没有 CNI 环境时返回错误并不代表 VERSION/安装失败。重点确认 executable mode、config JSON 的 `name`/`type`，以及显式路径。

### 处理

把 nerdctl 全局 flags 放在子命令前：

```bash
sudo nerdctl \
  --cni-path=/opt/cni/bin \
  --cni-netconfpath=/etc/cni/net.d \
  run --rm --network cloudnet-v1 \
  docker.m.daocloud.io/library/busybox:1.37 true
```

不要依赖 root/普通用户不同的默认值。重新安装时运行 `make build` 后显式 `sudo make install`；Makefile 自身不会调用 sudo。

## 9. 镜像拉取失败

### 现象

nerdctl 在 CNI ADD 前失败，报 DNS、TLS、registry、timeout 或 manifest 错误；state 中通常没有新 endpoint。

### 诊断

先在 host network 单独验证 DaoCloud 镜像：

```bash
sudo nerdctl run --rm --net host \
  docker.m.daocloud.io/library/busybox:1.37 true
```

或：

```bash
sudo nerdctl pull docker.m.daocloud.io/library/busybox:1.37
```

确认命令中完整 registry 是 `docker.m.daocloud.io`，不要省略 registry 让 nerdctl 回落到 docker.io。检查 host DNS、时间、CA、containerd 状态及 DaoCloud 可达性。镜像 pull 发生在宿主机侧，不能用 cloudnet V1 缺少 NAT 来解释 pull 本身失败。

### 处理

修复 host 到 DaoCloud 的解析/连接后重试。不要安装 Docker Engine，也不要改用 `docker` 命令。测试报告应把“镜像未拉取”和“CNI ADD 失败”分开记录。

## 10. MTU 问题

### 现象

CHECK 报 Bridge/host/container MTU mismatch，小包 ping 成功但大包或某些协议失败，或 ADD 因已有 Bridge MTU 冲突而停止。

### 诊断

```bash
sudo ip -d link show cni-br0
sudo ip -d link show master cni-br0
sudo ip -n <cloudnet-test-name> -d link show dev eth0
```

三处都必须是 1500。V1 config 也必须是 1500，不能通过改 JSON 绕开 fixed contract。

测试 namespace 可用不分片 ping 验证 payload 上限，命令参数需结合 IPv4/ICMP header 计算：

```bash
sudo ip netns exec <cloudnet-test-name> ping -c 3 -M do -s 1472 10.77.0.1
```

### 处理

- 新 endpoint 两端会由 ADD 设为 1500；
- 已有 Bridge MTU 不同视为外部冲突，插件不会静默修改；
- 查明谁创建/修改 Bridge 后，在没有其他用户且符合项目边界时统一恢复；
- 不要为了修 V1 MTU 修改管理或 Underlay 网卡。

## 11. 权限或 state directory 不可写

### 现象

ADD/DEL 报 create directory、open lock、chmod、temporary file、fsync 或 rename 失败。

### 诊断

```bash
sudo namei -l /var/lib/cloudnet/networks/cloudnet-v1
sudo ls -ld /var/lib/cloudnet /var/lib/cloudnet/networks/cloudnet-v1
sudo ls -la /var/lib/cloudnet/networks/cloudnet-v1
```

network directory 预期 `0700`，`.lock`/`state.json` 预期 `0600`，CNI 通常以 root 运行。目录必须是真目录，不能是 symlink。

### 处理

确认路径确属本项目后修正 owner/mode；不要把 state 改成 world-writable，也不要把目录替换为 symlink。若 ready Commit 失败，ADD 会尝试删除本次 owned veth 并释放 allocation；仍需检查 stderr 的 rollback failure 和实际残留。

## 12. 地址耗尽

### 现象

ADD 返回 `allocation exhausted`。池的闭区间是 `10.77.0.10..10.77.0.250`，最多 241 个地址。

### 诊断

```bash
sudo jq '.endpoints | length, .allocations | length' \
  /var/lib/cloudnet/networks/cloudnet-v1/state.json
sudo ip -d link show master cni-br0
```

对照 runtime 中仍存在的容器与每条 endpoint。不要只因为数量大就删除 state。

### 处理

让 runtime 对已结束容器执行 DEL。测试资源使用 `sudo make clean-test`。V1 地址规划固定，不支持临时扩大 range；需要不同规划时应建立新的、版本化网络设计。

## 13. 容器不能访问外网

容器能 ping `10.77.0.1`、同 Bridge endpoint 能互通、default route 也正确，但不能访问外网，通常是 V1 的已知范围限制，而非 ADD 错误。

V1 不设置 `net.ipv4.ip_forward`，也不创建 nftables/iptables masquerade。外网访问需要宿主机另行配置 forwarding、return path 和经过审查的 SNAT。不要为了测试执行 `iptables -F`、`nft flush ruleset`，也不要把物理接口加入 `cni-br0`。V1 主线验收只要求 endpoint、gateway 和同节点通信。
