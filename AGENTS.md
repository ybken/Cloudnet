# AGENTS.md

本文件适用于整个仓库。目标是让后续开发代理用最少上下文安全地继续工作。除非用户明确改变范围，以下项目契约和安全边界均为强约束。

## 1. 项目速览

cloudnet V1 是运行在单台 Ubuntu Server 24.04 节点上的轻量级 CNI 数据面。它由 Go 编写，通过 Linux Bridge、veth 和 network namespace 把容器接入宿主机，并提供本地 IPv4 IPAM、持久状态以及 CNI `ADD`、`CHECK`、`DEL`、`VERSION`。

```text
container netns
  lo UP
  eth0 10.77.0.x/24, default via 10.77.0.1
       |
       | veth pair, MTU 1500
       |
  cn<13 hex>, no host IPv4
       |
  cni-br0 10.77.0.1/24
       |
  Linux host stack
```

固定 V1 契约：

- module：`github.com/cloudnet/cloudnet`
- network/type：`cloudnet-v1` / `cloudnet`
- CNI versions：`1.0.0`、`1.1.0`
- Bridge/gateway：`cni-br0` / `10.77.0.1/24`
- endpoint subnet/pool：`10.77.0.0/24`，`10.77.0.10..10.77.0.250`
- MTU：1500；仅 IPv4
- binary：`/opt/cni/bin/cloudnet`
- config：`/etc/cni/net.d/10-cloudnet.conf`
- state：`/var/lib/cloudnet/networks/cloudnet-v1/`
- 日志：`log/slog` JSON，只写 stderr

生产网络逻辑使用 CNI/ns、`vishvananda/netlink`、`x/sys/unix` 和标准库；不得通过 `exec` 调用 `ip`、`bridge`、`ifconfig`。shell 工具只用于测试、安装和诊断。

## 2. 当前进展（2026-08-13）

已实现：

- 完整 ADD/CHECK/DEL/VERSION 和 CNI 1.0/1.1 Result；
- 严格配置/runtime 校验与 stdout/stderr 协议隔离；
- Bridge 创建、复用、拓扑冲突保护；
- 确定性 veth 名称与完整 ownership alias；
- netns 内地址、默认路由、接口和 loopback 配置；
- 本地 IPAM、网络级 flock、pending/ready 状态机；
- fd-relative `openat`、`O_NOFOLLOW`、fsync、atomic rename；
- 重复 ADD/DEL、缺失 netns、状态丢失推导和安全清理；
- LIFO rollback 和 critical cleanup barrier；
- 单元测试、race、vet、安装/清理/特权集成脚本和文档。

已知验证状态：

- `make verify` 已通过；
- 用户确认当前版本 `sudo make integration` 全部通过；脚本覆盖 ADD、重复 ADD、双 endpoint 通信、CHECK 漂移、DEL 幂等、缺失/空 netns、失败回滚及并发 ADD/DEL；
- commit `4fc57f4` 标记为 `v1 tested`，之后 `c09b94a` 主要补充注释和脚本保护；
- `docs/test-report-v1.md` 仍包含 integration 修复前的失败/PENDING 记录，属于待同步文档。更新时只写真实命令、日期和输出，不得擦除历史失败；
- nerdctl + DaoCloud 的最终单/双容器验收是否已执行没有当前证据，保持 PENDING，不得声称通过。

开始新任务时先运行 `git status --short`。工作树可能含用户修改，绝不覆盖、回滚或格式化无关变更。

## 3. V1 范围与非目标

V1 目标：单节点容器互通、容器到 gateway、地址分配、默认路由、完整 CNI 生命周期、并发、幂等、回滚和安全清理。

当前非目标：

- OVS、VXLAN、跨节点网络；
- Agent、控制面、Prometheus；
- IPv6、NetworkPolicy、Kubernetes 安装；
- Docker Engine 或 `docker` CLI；
- NAT/SNAT/Masquerade（V1 默认路由存在，但外网访问不是当前能力）。

不要借普通修复进入上述范围。V1.1 NAT 或 V2 OVS 只有在用户明确要求后才能开始。

## 4. 不可破坏的工程不变量

1. stdout 只允许 CNI Result 或 CNI Error；普通/JSON 日志只写 stderr。
2. 删除接口前必须同时验证类型为 veth 且完整 alias 精确匹配；确定性短名称只用于定位，不能单独作为删除许可。
3. 管理网卡、Underlay 和任何非项目端口绝不能加入、移出或被 `cni-br0` 修改。
4. `cni-br0` 是共享 network resource；endpoint DEL 和常规测试清理不得删除它。
5. IP 只有在 owned endpoint 已删除或确认不存在后才能释放。cleanup 失败时保留 pending allocation，避免重复地址。
6. 同一 `(network, containerID, ifName)` 重复 ADD 返回原 IP；重复 DEL 和资源已不存在视为成功。
7. 状态损坏时返回错误并保留证据；不得用空状态覆盖。写入必须继续保持 lock + fsync + atomic rename 语义。
8. netlink setter 不会自动刷新已有 `Link` 快照。依赖更新后属性进行校验或破坏性操作时，必须重新从内核读取对象。
9. CHECK 只验证，不修复；错误必须指出具体 mismatch。
10. 不记录完整 stdin、token、密钥或无限长度的 `CNI_ARGS`。

## 5. 主机和操作安全

已知实验节点：`cloudnet-node-a`，管理地址 `192.168.80.135`，Underlay `192.168.232.11`。历史审计中接口名为 `mgmt0`、`underlay0`，但任何写操作前仍应只读确认当前实际接口，不能只凭旧记录假定。

禁止：

- `iptables -F`、`nft flush ruleset` 或覆盖现有防火墙；
- 删除所有 veth、模糊批量删除 namespace；
- 修改 VMware 网卡、管理网、Underlay 或把物理接口 enslave 到 Bridge；
- 修改非项目系统目录；
- `git reset --hard`、覆盖未提交代码；
- 安装 Docker、Kubernetes、OVS（除非后续范围明确批准）。

系统写入仅限：

- `/opt/cni/bin/cloudnet`
- `/etc/cni/net.d/10-cloudnet.conf`
- `/var/lib/cloudnet`
- `/var/log/cloudnet`
- 能以名称和 alias 证明属于项目的 `cni-br0` / cloudnet veth / `cloudnet-test-*` namespace

所有 OCI/Docker Hub 镜像必须使用 DaoCloud，例如 `docker.m.daocloud.io/library/busybox:1.37`。只用 `nerdctl`，不用 `docker`。

## 6. 代码地图与阅读顺序

| 路径 | 职责 |
| --- | --- |
| `cmd/cloudnet/main.go` | CNI skel 入口和版本声明 |
| `internal/cni/commands.go` | skel 参数到 Service、Result 输出 |
| `internal/cni/service.go` | ADD/CHECK/DEL 主编排和事务 phase |
| `internal/cni/result.go` | 标准 CNI Result |
| `internal/cni/prev_result.go` | CHECK prevResult 解析与对照 |
| `internal/config/` | 严格 JSON、固定配置和 runtime 校验 |
| `internal/network/bridge.go` | Bridge ensure/check/拓扑保护 |
| `internal/network/veth.go` | veth 创建、移动、配置及局部回滚 |
| `internal/network/check.go` | host/container/link/address/route CHECK |
| `internal/network/cleanup.go` | ownership-aware endpoint 删除 |
| `internal/network/names.go` | 确定性名称和 alias |
| `internal/ipam/range.go` | IPv4 分配范围算法 |
| `internal/ipam/store.go` | flock 下 allocate/get/phase/release |
| `internal/ipam/securefs.go` | 安全路径和原子状态写入 |
| `internal/endpoint/state.go` | endpoint key/record/phase schema |
| `internal/transaction/rollback.go` | 普通与 critical LIFO 回滚 |
| `internal/logging/`、`internal/errs/` | JSON 日志与分类错误 |
| `hack/integration-v1.sh` | 特权端到端验收，失败时采证并精确清理 |
| `hack/cleanup-v1.sh` | 只清理可证明归属的测试资源 |

理解调用链时按以下顺序读，避免一开始陷入底层细节：

`main.go -> commands.go -> config -> service.go phases -> endpoint/IPAM -> network -> result/rollback`

设计细节优先查 `docs/design-v1.md`；用户操作查 `README.md`；排障查 `docs/troubleshooting-v1.md`；真实执行证据查 `docs/test-report-v1.md`。

## 7. 生命周期摘要

ADD：parse/validate -> flock -> allocate + persist pending -> ensure Bridge -> create/move/configure veth -> verify -> persist ready -> print Result。失败时逆序清理；critical veth cleanup 失败会阻止 IP release。

CHECK：锁内核对 ready record -> Bridge -> host veth -> netns/container link/IP/lo/default route -> optional prevResult。只报告具体差异。

DEL：锁内读取 state；优先按 host veth + exact alias 删除，必要时进入仍存在的 netns 清理 peer；state 缺失可推导名称但仍需 alias；之后释放 IP/record；永不删除共享 Bridge。

## 8. 开发工作流

实现前：

1. 说明假设和成功标准；不清楚或存在破坏性歧义时先问。
2. 先读相关实现与测试，优先沿用现有 API 和模式。
3. 对 bug 先写能复现的最小测试，再修实现。
4. 只改任务需要的文件；不顺手重构、改注释或格式化无关代码。

常用验证：

```bash
go test ./...
go test -race ./...
go vet ./...
make verify
bash -n hack/*.sh
git diff --check
git status --short
```

窄改动先运行对应 package 测试；涉及共享状态、事务、CNI contract 或 network API 时必须再运行全仓 test/race/vet。

安装/特权验证：

```bash
make build
sudo make install
sudo make integration
```

不要直接运行 `sudo make build`。Makefile 会检查 build 新鲜度以及已安装 binary/config 与仓库是否一致。特权 integration 会创建明确的 `cloudnet-test-*` 资源并通过 trap 清理；失败时可用：

```bash
sudo env CLOUDNET_KEEP_FAILURE_ARTIFACTS=1 make integration
```

不得声称未实际运行的测试通过。系统测试输出要同步到 `docs/test-report-v1.md`，包括失败和残留风险。

## 9. 后续规划

规划表示推荐顺序，不代表自动授权实现。

### P0：收口 V1 证据和发布

1. 用最新代码重新记录 `make verify` 与完整 `sudo make integration` 输出。
2. 完成 cnitool 独立 ADD/CHECK/DEL、重复调用和缺失 netns 复现。
3. 使用 DaoCloud BusyBox 完成 nerdctl 单/双容器、互 ping、默认路由、自动 DEL 和 IP 复用验收。
4. 更新 `docs/test-report-v1.md`，使其与最新成功事实一致，同时保留历史失败与修复说明。
5. 复核 `git status` 和文档后再建议 `v1.0.0` tag；未经用户要求不 commit、不 tag、不 push。

### P1：V1 维护性

- 只针对发现的真实缺陷补测试、错误上下文和诊断；
- 保持固定 V1 contract，避免提前泛化配置或后端；
- 对状态 schema 或 ownership 规则的任何变更都需要兼容/迁移设计和故障测试。

### P2：可选 V1.1

只有用户明确要求后才考虑 `ipMasq`。必须使用独立、可识别、幂等且不覆盖现有规则的 nftables 生命周期，并提供独立测试。核心 V1 不依赖 NAT。

### P3：V2

OVS/VXLAN、跨节点、Agent 和控制面属于独立设计。优先保留 `NetworkOps`、CNI contract、IPAM、状态原子性、ownership、幂等和回滚语义，用新 backend 替换 `internal/network`，不要在 V1 Bridge 路径散布条件分支。

## 10. 提交前检查

- 每一处改动都能直接追溯到当前任务；
- 没有协议日志进入 stdout；
- 没有仅凭接口名删除资源；
- 没有释放仍可能被残留接口使用的 IP；
- 没有修改管理/Underlay/防火墙/无关目录；
- test、race、vet 与相关特权验证按风险实际执行；
- 文档只记录真实结果；
- `git status` 中没有意外文件；
- 未越界进入 V2 或其他非目标。
