# cloudnet V1 测试报告

## 报告状态

- 报告日期：2026-08-07
- 节点：`cloudnet-node-a`
- 项目路径：`/home/cloudnet-template/src/cloudnet`
- 当前结论：非特权单元、race、vet、build、VERSION 和脚本静态/权限保护验证已实际通过；需要 sudo 的安装、netns 集成、cnitool 和 nerdctl 功能验收尚未执行。
- 阻塞原因：VM 上的 sudo 要求交互式密码，当前执行环境无法提供。`sudo make install` 在认证阶段退出，未修改系统目录。
- 额外探测：`unshare --user --map-root-user --net true` 因写入 `/proc/self/uid_map` 返回 `Operation not permitted`，因此也不能用隔离 user namespace 替代真实 root 验收。

本报告只把实际运行过的命令标为通过。测试源码存在不等于特权场景已经验证。

## 环境审计

以下为本机实际只读审计结果：

| 项目 | 实际值 |
| --- | --- |
| OS | Ubuntu Server 24.04.4 LTS |
| Kernel | `7.0.0-28` |
| Go | `go1.26.5` |
| containerd | `2.2.1`，service active |
| nerdctl | `2.3.5` |
| cnitool | `/usr/local/bin/cnitool`，CNI `1.3.0` |
| CNI reference plugins | `1.9.1` |
| 管理接口 | `mgmt0`，`192.168.80.135/24` |
| 默认路由 | via `192.168.80.2`，dev `mgmt0` |
| Underlay 接口 | `underlay0`，`192.168.232.11/24` |
| network namespaces | 无 |
| `cni-br0` / bridge ports | 无 |
| `/opt/cni/bin/cloudnet` | 不存在，尚未安装 |
| `/etc/cni/net.d` | 仅 `.cni-concurrency.lock`，尚无 cloudnet config |
| `/var/lib/cloudnet` | `root:root`、`0750`；内部因 sudo 密码不可读，不能判断旧状态 |
| `/var/log/cloudnet` | `root:root`、`0750`；内部因 sudo 密码不可读 |
| `net.ipv4.ip_forward` | `1` |
| UFW | active，默认 FORWARD DROP |
| 实时 nftables/iptables | 需要 sudo，未读取；没有把未知规则写成“为空” |

实际运行 `./hack/collect-env.sh` exit 0，正确识别上述工具、接口、路由、空 netns/Bridge 和 `ip_forward=1`。对受限的 nftables、iptables 和 state 内容，脚本明确打印 permission denied 并继续，没有把审计失败伪装成空状态。

## 非特权验证

### 汇总

| 命令 | 结果 | 实际摘要 |
| --- | --- | --- |
| `go test ./...` | PASS | 所有 Go package 通过 |
| `go test -race ./...` | PASS | 所有 Go package 在 race detector 下通过 |
| `go vet ./...` | PASS | exit 0，无输出 |
| `go build -trimpath -o /tmp/cloudnet ./cmd/cloudnet` | PASS | 生成临时 binary 成功 |
| `make verify` | PASS | 依次完成 test、test-race、vet、build，exit 0 |
| VERSION 临时 binary | PASS | 正确声明 CNI 1.0.0/1.1.0 |
| stdout/stderr 隔离 | PASS | 非特权非法配置路径实测，见下文 |
| `go test ./internal/network` | PASS | network helper 单元测试通过 |
| `go test -race ./internal/network` | PASS | network helper race 测试通过 |
| `go vet ./internal/network` | PASS | exit 0 |

VERSION 实际命令及 stdout：

```bash
CNI_COMMAND=VERSION /tmp/cloudnet
```

```json
{"cniVersion":"1.1.0","supportedVersions":["1.0.0","1.1.0"]}
```

### 实际单元覆盖

全仓 `go test ./...` 和 `go test -race ./...` 实际执行了下列覆盖：

- config：合法 V1、CNI common fields、1.0.0/1.1.0、默认 log level、未知/重复 key、额外 JSON、固定 subnet/gateway/range/MTU、IPv4-only、runtime 参数、network name 路径安全；
- range：第一个/最后一个地址、gateway 排除、network/broadcast 排除、无效范围、耗尽、IPv4 最大地址终止；
- store：pending/ready 持久化、重复 allocation、同 key 新 netns 保持 IP 并更新记录、冲突 allocation、最后一次 release 后保留 network config、重复 release、地址复用、地址耗尽；
- concurrency：50 个 goroutine 经真实 filesystem flock 并发分配，50 个 IP 唯一且全部落盘；另验证整个 callback 在 network lock 下串行；
- durability：callback error 的 commit 语义、显式 commit、损坏 state 保留且 callback 不执行、重复/未知/尾随 JSON 拒绝、state 大小限制、rename failure 保留旧 state、临时文件清理、目录/lock/state 权限，以及 root/networks/network/lock/state 符号链接拒绝；
- endpoint/network identity：endpoint key 稳定且无 tuple 歧义、host/peer 名稳定且不超过 15 字节、各 key component 隔离、完整 alias 精确 ownership；
- network pure logic：Bridge/endpoint spec、Bridge standalone 与 owned-port topology、物理/未归属/wrong-network port 冲突、IPv4 address classification、default-route mismatch、prefix conversion；这些不是内核 netlink 集成测试；
- CNI service：重复 ADD 保持 IP 且不重复创建、同 key 新 netns 保持 IP 并重建、ADD failure 释放 pending allocation、critical cleanup 失败时隔离 allocation、ready reconcile 持久 pending、缺失 netns/重复 DEL、逐字段 CHECK mismatch、日志诊断 schema；
- Result/prevResult：标准接口/IP/gateway/default route、声明版本、缺失 prevResult、目标容器接口 IPv4 恰好一条及具体 mismatch；
- transaction/error：LIFO rollback、普通补偿失败后继续、critical 补偿失败后停止依赖动作、Commit disarm、error kind/context/cause wrapping；
- logging：JSON handler、level filtering、固定字段、container ID 截断。

### stdout/stderr 隔离实测

使用固定 V1 config、仅把 MTU 改为 1400 的非法输入调用临时 binary ADD。该路径在 config 阶段失败，不需要 root 或内核网络变更。

- process exit：`1`；
- stdout：只有一个 CNI Error JSON，`code: 999`，message 明确包含 `invalid config` 和 MTU mismatch；
- stderr：只有一行 slog JSON；
- stderr 字段：`timestamp`、`level`、`command`、`network`、`containerID`、`ifName`、`netns`、`bridge`、`hostVeth`、`containerIP`、`duration`、`phase`、`error`、`rollback`。

另以空 stdin 调用：skel 在 handler 之前返回 stdout CNI Error `code: 6`，stderr 为空。该行为符合 CNI skel 的前置校验路径，也证明应用日志没有写入 stdout。

## 脚本与 Makefile 验证

### 实际通过

```bash
bash -n \
  hack/collect-env.sh \
  hack/install-v1.sh \
  hack/integration-v1.sh \
  hack/cleanup-v1.sh
```

结果：exit 0，无输出。

```bash
make -n build test test-race vet install integration clean-test verify
make -n integration clean-test
```

结果：exit 0。dry-run 展开确认：

- `install` 只有创建两个目标目录、安装 `build/cloudnet` 和安装 `configs/10-cloudnet.conf`；
- Makefile 中没有隐式 sudo；
- `integration` 先 build，再调用测试脚本；
- `clean-test` 只调用项目 cleanup。

```bash
make verify
```

结果：exit 0；全仓 test、race、vet、build 均成功。

权限保护负向测试：

| 命令 | 实际结果 |
| --- | --- |
| 非 root `make install` | exit 2，明确提示使用 `sudo make install`，未安装 |
| 非 root `./hack/integration-v1.sh` | exit 1；trap 删除临时工作目录，未创建网络资源 |
| 非 root `./hack/cleanup-v1.sh` | exit 1，未操作网络 |
| `sudo make install` | sudo 认证失败 exit 1；make/install recipe 未执行 |

`shellcheck` 与 `shfmt` 未安装，因此没有执行，也没有在本报告中标为通过。

## cnitool 状态

本机实际运行 `cnitool` 和 `cnitool --help`，两者显示：

```text
cnitool add    <net> <netns>
cnitool check  <net> <netns>
cnitool del    <net> <netns>
cnitool gc     <net> <netns>
cnitool status <net> <netns>
```

这确认了 README 使用的参数形式。以下功能命令未执行：

- `cnitool add cloudnet-v1 <netns>`：PENDING；
- `cnitool check cloudnet-v1 <netns>`：PENDING；
- `cnitool del cloudnet-v1 <netns>`：PENDING；
- 重复 ADD、重复 DEL、netns 先删除：PENDING。

原因：`/opt/cni/bin/cloudnet` 和 `/etc/cni/net.d/10-cloudnet.conf` 尚未安装，安装需要 sudo 密码。

## nerdctl 状态

本机实际运行 `nerdctl --help` 与 `nerdctl run --help`，确认：

- global flag `--cni-path`，本机显示默认 `/opt/cni/bin`；
- global flag `--cni-netconfpath`，普通用户默认在 home 下，因此文档必须显式指定 `/etc/cni/net.d`；
- `run` 支持 `--network` / `--net`。

没有实际拉取或运行镜像，没有创建 `cloudnet-v1-c1`/`cloudnet-v1-c2`。因此下列项目全部为 PENDING：

- DaoCloud BusyBox 单容器 ADD/DEL；
- 双容器地址、默认路由和互 ping；
- Bridge FDB/host veth/state 对照；
- 删除容器触发自动 DEL；
- 删除后 IP 复用。

没有访问 docker.io，也没有使用 Docker Engine 或 `docker` 命令。

## 特权集成状态

`hack/integration-v1.sh` 已实现并通过 bash parse/dry-run/非 root guard，但尚未以 root 执行。默认并发数为 6，可通过 `CLOUDNET_TEST_CONCURRENCY=2..16` 调整。资源使用 `cloudnet-test-*` namespace、`/tmp/cloudnet-test-*` 工作目录和精确 ownership cleanup；Bridge 按设计保留。

以下矩阵不得解读为失败；它表示尚无实际特权证据：

| 验收项 | 状态 | 尚需取得的证据 |
| --- | --- | --- |
| 安装幂等 | PENDING | 两次 `sudo make install`、mode/hash/config 对照 |
| 基础 ADD | PENDING | Bridge/address/UP、eth0/lo/default route、ping gateway |
| 两 endpoint | PENDING | IP 不重复、双向 ping、host ping |
| 正常 CHECK | PENDING | cnitool/integration exit 0 |
| 接口 DOWN CHECK | PENDING | 注入后具体失败、恢复后成功 |
| route 缺失 CHECK | PENDING | 注入后具体失败、恢复后成功 |
| host veth 脱离 Bridge | PENDING | CHECK master mismatch |
| 正常/重复 DEL | PENDING | veth/state/IP 清理，Bridge 与另一个 endpoint 保留 |
| netns 先消失 | PENDING | host veth 与 allocation 清理 |
| `CNI_NETNS=''` DEL | PENDING | containerID 存在时幂等清理 |
| ADD 失败回滚 | PENDING | 目标 ifname 冲突后无 veth/IP 残留 |
| Bridge 类型冲突 | PENDING | 拒绝同名非 Bridge 且不覆盖对象 |
| 特权并发 | PENDING | 2..16 endpoint 无重复，全部 DEL 后无残留 |
| stdout 无污染 | PARTIAL | 非特权错误路径已过；成功 ADD Result 待 root 实测 |
| nerdctl 单/双容器 | PENDING | DaoCloud image、通信、自动 DEL、IP 复用 |

集成脚本在调用前还会用 `cmp` 确认已安装 `/opt/cni/bin/cloudnet` 与当前 `build/cloudnet` 相同，避免测试旧 binary。

最终只读复核仍只看到 `lo`、`mgmt0` 和 `underlay0`，`ip netns list` 为空，`cni-br0` 不存在；非特权验证没有在宿主机遗留网络对象。

## 完成标准对照

| V1 完成标准 | 当前状态 |
| --- | --- |
| `go test ./...` | PASS |
| `go test -race ./...` | PASS |
| `go vet ./...` | PASS |
| build / VERSION | PASS |
| CNI ADD/CHECK/DEL | PENDING ROOT |
| 重复 ADD/DEL | PENDING ROOT |
| 两 namespace 通信 | PENDING ROOT |
| 两 nerdctl 容器通信 | PENDING ROOT |
| 默认路由实际状态 | PENDING ROOT |
| netns 先删除后 DEL | PENDING ROOT |
| 故障后无 veth/IPAM 残留 | PENDING ROOT |
| 并发 IP 无重复 | PASS UNIT；PENDING ROOT INTEGRATION |
| stdout 无日志污染 | PASS ERROR PATH；PENDING SUCCESS ADD PATH |
| JSON 诊断字段 | PASS UNIT/ERROR PATH |
| 安装脚本可重复运行 | PASS DRY-RUN/GUARD；PENDING ROOT INSTALL |
| cleanup 不误删无关网络 | PASS CODE/GUARD REVIEW；PENDING ROOT EXECUTION |
| 文档 | PRESENT；需在特权验收后更新本报告 |
| OVS/VXLAN/Agent/Prometheus 未进入范围 | PASS CODE REVIEW |

## root 验收后的更新要求

获得 sudo 后，应按顺序执行并把真实时间、exit code、关键输出和清理后状态追加到本报告：

```bash
cd /home/cloudnet-template/src/cloudnet
sudo make install
sudo make install
sudo make integration
```

然后执行 README 中的 cnitool 与 nerdctl 复现步骤。任何失败都要记录为失败并保留 stderr/state/link 证据；不能只在修复后覆盖成“通过”。验收结束只删除明确的 cloudnet 测试 namespace 和两个命名 nerdctl 容器，保留共享 `cni-br0`，不修改防火墙或 VMware 网卡。
