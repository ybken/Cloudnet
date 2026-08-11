// Package cni 编排 CNI 生命周期：它把协议输入、持久 IPAM、Linux 网络操作、
// 回滚和日志串成一个事务。具体 netlink 操作留在 internal/network 中。
package cni

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/cloudnet/cloudnet/internal/config"
	"github.com/cloudnet/cloudnet/internal/endpoint"
	"github.com/cloudnet/cloudnet/internal/errs"
	"github.com/cloudnet/cloudnet/internal/ipam"
	"github.com/cloudnet/cloudnet/internal/logging"
	"github.com/cloudnet/cloudnet/internal/network"
	"github.com/cloudnet/cloudnet/internal/transaction"
	"github.com/containernetworking/cni/pkg/skel"
)

// NetworkOps 是编排层依赖的最小数据面接口。单元测试用 fake 实现验证事务顺序，
// 生产环境则由 linuxNetworkOps 转发到 network 包。
type NetworkOps interface {
	EnsureBridge(network.BridgeSpec) (bool, error)
	CheckBridge(network.BridgeSpec) error
	CreateEndpoint(network.EndpointSpec) error
	CheckEndpoint(network.EndpointSpec) error
	DeleteEndpoint(network.DeleteSpec) error
}

// linuxNetworkOps 不保存状态，只是把接口调用适配到 Linux 实现。
type linuxNetworkOps struct{}

func (linuxNetworkOps) EnsureBridge(spec network.BridgeSpec) (bool, error) {
	return network.EnsureBridge(spec)
}

func (linuxNetworkOps) CheckBridge(spec network.BridgeSpec) error {
	return network.CheckBridge(spec)
}

func (linuxNetworkOps) CreateEndpoint(spec network.EndpointSpec) error {
	return network.CreateEndpoint(spec)
}

func (linuxNetworkOps) CheckEndpoint(spec network.EndpointSpec) error {
	return network.CheckEndpoint(spec)
}

func (linuxNetworkOps) DeleteEndpoint(spec network.DeleteSpec) error {
	return network.DeleteEndpoint(spec)
}

// Service 聚合一次 CNI 命令所需的依赖。字段公开是为了让测试注入临时 Store、
// fake NetworkOps 和日志缓冲区，不需要真实 root/netns 权限。
type Service struct {
	Store     *ipam.Store
	Network   NetworkOps
	LogWriter io.Writer
}

// NewDefaultService 构造生产服务：状态落在 /var/lib/cloudnet，网络操作针对
// 当前 Linux 主机，结构化日志写 stderr。
func NewDefaultService() (*Service, error) {
	store, err := ipam.NewStore(ipam.DefaultStateRoot)
	if err != nil {
		return nil, err
	}
	return &Service{Store: store, Network: linuxNetworkOps{}, LogWriter: os.Stderr}, nil
}

// Add 完成“先记录 pending，再创建内核对象，最后标记 ready”的完整事务。
//
// retErr 供 defer 中的 completion 日志读取；phase 则记录失败发生在哪个阶段。
// 整个事务持有网络级 flock，使同一网络的地址分配和内核变更串行。
func (s *Service) Add(args *skel.CmdArgs) (result ResultData, retErr error) {
	started := time.Now()
	phase := "parse"
	rolledBack := false
	fields := invocationFields("ADD", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, rolledBack)
	}()

	if err := s.validate(); err != nil {
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "initialize ADD service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)

	phase = "validate"
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, true); err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}
	// 用带不变量的 Range 值封装四个地址，IPAM 不再接收松散参数。
	allocationRange, err := ipam.NewRange(
		conf.IPAM.SubnetPrefix,
		conf.IPAM.GatewayAddr,
		conf.IPAM.RangeStartAddr,
		conf.IPAM.RangeEndAddr,
	)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "build allocation range", err)
	}

	// endpoint 身份不包含 netns：runtime 重试时路径可能变化，但同一
	// (network, containerID, ifName) 必须复用原 IP 与确定性 veth 名。
	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	bridgeSpec := bridgeSpecFromConfig(conf)
	alias := network.HostVethAlias(conf.Name, args.ContainerID, args.IfName)
	hostName := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	peerName := network.PeerVethName(conf.Name, args.ContainerID, args.IfName)
	// ADD 回滚刻意只从 host 端清理：host veth 不存在时其 peer 也不可能存在；
	// 贸然进入一个已被复用的 netns，反而可能删除恰好同名的无关接口。
	// 因此此处故意不填写 NetNSPath。
	rollbackDeleteSpec := network.DeleteSpec{
		IfName:       args.IfName,
		HostVethName: hostName,
		Alias:        alias,
	}
	var record endpoint.Record

	phase = "transaction"
	// 锁覆盖磁盘与内核操作，避免地址释放与残留 veth 清理之间出现竞争窗口。
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		request := ipam.AllocationRequest{
			Range: allocationRange,
			Endpoint: endpoint.Record{
				NetworkName:  conf.Name,
				ContainerID:  args.ContainerID,
				IfName:       args.IfName,
				NetNS:        args.Netns,
				HostVethName: hostName,
				Subnet:       conf.IPAM.SubnetPrefix.String(),
				Gateway:      conf.IPAM.GatewayAddr.String(),
				Bridge:       conf.Bridge,
				MTU:          conf.MTU,
			},
		}
		allocated, created, err := locked.Allocate(request)
		if err != nil {
			if errors.Is(err, ipam.ErrExhausted) {
				return errs.Wrap(errs.KindAllocationExhausted, "allocate endpoint IP", err)
			}
			return errs.Wrap(errs.KindStatePersistence, "allocate endpoint IP", err)
		}
		record = allocated
		fields.ContainerIP = allocated.ContainerIP

		// 回滚动作按 LIFO 执行。release 必须排在 delete veth 后面，确保残留
		// 链接不会与重新分配同一 IP 的新 endpoint 冲突。
		var rollback transaction.Stack
		registerRelease := func() {
			rollback.Defer("release endpoint allocation", func() error {
				if _, _, err := locked.Release(key); err != nil {
					return err
				}
				return locked.Commit()
			})
		}
		fail := func(cause error) error {
			rolledBack = true
			return rollback.Rollback(cause)
		}

		// 新 endpoint 必须在触碰网络前单独 Commit pending，留下崩溃恢复证据。
		// 已有 pending 说明上次 ADD 可能中断，同样需要启用释放补偿。
		if created {
			phase = "persist-pending"
			if err := locked.Commit(); err != nil {
				return errs.Wrap(errs.KindStatePersistence, "persist pending endpoint", err)
			}
			registerRelease()
		} else if allocated.Phase == endpoint.PhasePending {
			registerRelease()
		}

		// 持久化 pending 可能是进程在 veth 设置任意阶段退出后的残留。
		// 必须先确认 owned link 已清掉，后续回滚才有资格释放它的地址。
		// 这也使下一次 ADD 能从一个明确的起点恢复。
		if !created && allocated.Phase == endpoint.PhasePending {
			phase = "recover-pending"
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				rolledBack = true
				return errs.Wrap(errs.KindEndpointCleanup, "clean interrupted endpoint", err)
			}
		}

		phase = "ensure-bridge"
		if _, err := s.Network.EnsureBridge(bridgeSpec); err != nil {
			return fail(errs.Wrap(errs.KindBridgeConflict, "ensure shared bridge", err))
		}

		endpointSpec, err := endpointSpecFromRecord(allocated, args.Netns, peerName, alias)
		if err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "read allocated endpoint", err))
		}
		// 重复 ADD 的快速路径：实际 endpoint 完整时直接复用。若发生漂移，
		// 先持久化 pending，再只删除能以 alias 证明归属的旧 endpoint 后重建。
		if !created && allocated.Phase == endpoint.PhaseReady {
			phase = "idempotency-check"
			if err := s.Network.CheckEndpoint(endpointSpec); err == nil {
				rollback.Commit()
				return nil
			}
			phase = "reconcile-pending"
			pending, err := locked.MarkPending(key)
			if err != nil {
				return errs.Wrap(errs.KindStatePersistence, "mark endpoint pending for reconciliation", err)
			}
			record = pending
			if err := locked.Commit(); err != nil {
				return errs.Wrap(errs.KindStatePersistence, "persist pending endpoint for reconciliation", err)
			}
			phase = "reconcile-delete"
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				rolledBack = true
				return errs.Wrap(errs.KindEndpointCleanup, "remove inconsistent owned endpoint", err)
			}
			registerRelease()
		}

		phase = "create-veth"
		// CreateEndpoint 可能在 veth 已创建后失败，所以删除补偿必须在调用前布防。
		// 这是 critical barrier：删除失败时停止释放 IP，让 allocation 保持隔离，
		// 避免残留链接与随后复用此 IP 的 endpoint 冲突。
		rollback.DeferCritical("delete owned veth", func() error {
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				return errs.Wrap(errs.KindEndpointCleanup, "rollback owned endpoint", err)
			}
			return nil
		})
		if err := s.Network.CreateEndpoint(endpointSpec); err != nil {
			return fail(classifySetupError("create endpoint", err))
		}

		// 只有内核态完整通过复核，endpoint 才能从 pending 迁移为 ready。
		phase = "persist-ready"
		ready, err := locked.MarkReady(key)
		if err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "mark endpoint ready", err))
		}
		record = ready
		if err := locked.Commit(); err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "persist ready endpoint", err))
		}
		rollback.Commit()
		return nil
	})
	if err != nil {
		var classified *errs.Error
		if errors.As(err, &classified) {
			return ResultData{}, err
		}
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "run ADD transaction", err)
	}

	phase = "result"
	result, err = resultFromRecord(conf.CNIVersion, record, args.Netns)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "build CNI result", err)
	}
	return result, nil
}

// Check 只读对照配置、持久状态、Linux 实际状态及可选 prevResult。
// 它不修复漂移，否则 runtime 无法区分健康状态与被静默重建的状态。
func (s *Service) Check(args *skel.CmdArgs) (retErr error) {
	started := time.Now()
	phase := "parse"
	fields := invocationFields("CHECK", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, false)
	}()

	if err := s.validate(); err != nil {
		return errs.Wrap(errs.KindCheckMismatch, "initialize CHECK service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, true); err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}

	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	phase = "check-state"
	// 与 ADD/DEL 使用同一把锁，让 CHECK 观察到稳定的事务快照。
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		record, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("allocation state for endpoint %s is missing", key.ID())
		}
		fields.ContainerIP = record.ContainerIP
		if record.Phase != endpoint.PhaseReady {
			return allocationStateFieldMismatch("phase", record.Phase, endpoint.PhaseReady)
		}
		if err := recordMatchesInvocation(record, conf, args); err != nil {
			return err
		}

		// 按共享资源、endpoint、prevResult 的顺序逐层收窄故障位置。
		phase = "check-bridge"
		if err := s.Network.CheckBridge(bridgeSpecFromConfig(conf)); err != nil {
			return err
		}
		endpointSpec, err := endpointSpecFromRecord(
			record,
			args.Netns,
			network.PeerVethName(conf.Name, args.ContainerID, args.IfName),
			network.HostVethAlias(conf.Name, args.ContainerID, args.IfName),
		)
		if err != nil {
			return err
		}
		phase = "check-endpoint"
		if err := s.Network.CheckEndpoint(endpointSpec); err != nil {
			return err
		}
		observed, err := resultFromRecord(conf.CNIVersion, record, args.Netns)
		if err != nil {
			return err
		}
		phase = "check-prev-result"
		return ValidatePrevResult(conf.RawPrevResult, conf.CNIVersion, observed)
	})
	if err != nil {
		return errs.Wrap(errs.KindCheckMismatch, "verify endpoint", err)
	}
	return nil
}

// Del 幂等删除 endpoint 与 allocation，但保留共享 Bridge。runtime 可能先删除
// netns 再调用 DEL，因此该命令允许空 netns，并优先从 host veth 清理 pair。
func (s *Service) Del(args *skel.CmdArgs) (retErr error) {
	started := time.Now()
	phase := "parse"
	fields := invocationFields("DEL", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, false)
	}()

	if err := s.validate(); err != nil {
		return errs.Wrap(errs.KindStatePersistence, "initialize DEL service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, false); err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}

	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	expectedHost := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	alias := network.HostVethAlias(conf.Name, args.ContainerID, args.IfName)
	phase = "delete-endpoint"
	// 只有内核对象已删除（或确认不存在）后才释放 allocation。
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		record, found, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		// state 丢失时仍可用确定性名称定位；state 存在时先防御性核对，
		// 避免被篡改的记录把清理目标指向别的链接。
		hostName := expectedHost
		if found {
			fields.ContainerIP = record.ContainerIP
			if record.HostVethName != expectedHost {
				return fmt.Errorf(
					"persisted host veth %q does not match deterministic name %q",
					record.HostVethName,
					expectedHost,
				)
			}
			hostName = record.HostVethName
		}
		if err := s.Network.DeleteEndpoint(network.DeleteSpec{
			NetNSPath:    args.Netns,
			IfName:       args.IfName,
			HostVethName: hostName,
			Alias:        alias,
		}); err != nil {
			return errs.Wrap(errs.KindEndpointCleanup, "delete owned endpoint", err)
		}
		if found {
			phase = "release-allocation"
			if _, _, err := locked.Release(key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var classified *errs.Error
		if errors.As(err, &classified) {
			return err
		}
		return errs.Wrap(errs.KindStatePersistence, "delete endpoint transaction", err)
	}
	return nil
}

// validate 检查服务依赖，并为省略的 NetworkOps 安装生产默认值。
func (s *Service) validate() error {
	if s == nil {
		return fmt.Errorf("service is nil")
	}
	if s.Store == nil {
		return fmt.Errorf("state store is nil")
	}
	if s.Network == nil {
		s.Network = linuxNetworkOps{}
	}
	return nil
}

// newLogger 保证日志总有输出目标；非法级别理论上已被配置校验拒绝，fallback
// 仍使测试或直接调用 Service 时不会因日志初始化失败而丢失主流程。
func (s *Service) newLogger(level string) *slog.Logger {
	output := s.LogWriter
	if output == nil {
		output = os.Stderr
	}
	logger, err := logging.New(level, output)
	if err != nil {
		logger, _ = logging.New("info", output)
	}
	return logger
}

// bridgeSpecFromConfig 把配置层模型收窄为 network 包需要的 Bridge 契约。
func bridgeSpecFromConfig(conf *config.NetConf) network.BridgeSpec {
	return network.BridgeSpec{
		NetworkName: conf.Name,
		Name:        conf.Bridge,
		Subnet:      conf.IPAM.SubnetPrefix,
		Gateway:     conf.IPAM.GatewayAddr,
		MTU:         conf.MTU,
	}
}

// endpointSpecFromRecord 解析持久化地址，并与本次 netns 组合成内核操作参数。
// 主动解析而非 MustParse，可把磁盘损坏作为可诊断错误返回。
func endpointSpecFromRecord(record endpoint.Record, netns, peerName, alias string) (network.EndpointSpec, error) {
	ip, err := netip.ParseAddr(record.ContainerIP)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted container IP %q: %w", record.ContainerIP, err)
	}
	subnet, err := netip.ParsePrefix(record.Subnet)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted subnet %q: %w", record.Subnet, err)
	}
	gateway, err := netip.ParseAddr(record.Gateway)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted gateway %q: %w", record.Gateway, err)
	}
	return network.EndpointSpec{
		BridgeName:   record.Bridge,
		NetNSPath:    netns,
		IfName:       record.IfName,
		HostVethName: record.HostVethName,
		PeerVethName: peerName,
		Alias:        alias,
		Address:      netip.PrefixFrom(ip, subnet.Bits()),
		Gateway:      gateway,
		MTU:          record.MTU,
	}, nil
}

// resultFromRecord 用持久记录生成协议结果；MAC 在 Service 层没有额外查询，
// 因而留空，Result 中仍完整包含接口身份、地址、网关与路由。
func resultFromRecord(cniVersion string, record endpoint.Record, netns string) (ResultData, error) {
	spec, err := endpointSpecFromRecord(
		record,
		netns,
		network.PeerVethName(record.NetworkName, record.ContainerID, record.IfName),
		network.HostVethAlias(record.NetworkName, record.ContainerID, record.IfName),
	)
	if err != nil {
		return ResultData{}, err
	}
	return ResultData{
		CNIVersion: cniVersion,
		NetNS:      netns,
		BridgeName: record.Bridge,
		HostName:   record.HostVethName,
		IfName:     record.IfName,
		MTU:        record.MTU,
		Address:    spec.Address,
		Gateway:    spec.Gateway,
	}, nil
}

// recordMatchesInvocation 防止 CHECK 用当前配置解释另一份网络身份或地址规划。
// 每个字段单独报错，便于从日志直接定位漂移来源。
func recordMatchesInvocation(record endpoint.Record, conf *config.NetConf, args *skel.CmdArgs) error {
	expectedHost := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if record.NetworkName != conf.Name {
		return allocationStateFieldMismatch("networkName", record.NetworkName, conf.Name)
	}
	if record.ContainerID != args.ContainerID {
		return allocationStateFieldMismatch("containerID", record.ContainerID, args.ContainerID)
	}
	if record.IfName != args.IfName {
		return allocationStateFieldMismatch("ifName", record.IfName, args.IfName)
	}
	if record.NetNS != args.Netns {
		return allocationStateFieldMismatch("netns", record.NetNS, args.Netns)
	}
	if record.HostVethName != expectedHost {
		return allocationStateFieldMismatch("hostVethName", record.HostVethName, expectedHost)
	}
	if expected := conf.IPAM.SubnetPrefix.String(); record.Subnet != expected {
		return allocationStateFieldMismatch("subnet", record.Subnet, expected)
	}
	if expected := conf.IPAM.GatewayAddr.String(); record.Gateway != expected {
		return allocationStateFieldMismatch("gateway", record.Gateway, expected)
	}
	if expected := conf.IPAM.RangeStartAddr.String(); record.RangeStart != expected {
		return allocationStateFieldMismatch("rangeStart", record.RangeStart, expected)
	}
	if expected := conf.IPAM.RangeEndAddr.String(); record.RangeEnd != expected {
		return allocationStateFieldMismatch("rangeEnd", record.RangeEnd, expected)
	}
	if record.Bridge != conf.Bridge {
		return allocationStateFieldMismatch("bridge", record.Bridge, conf.Bridge)
	}
	if record.MTU != conf.MTU {
		return allocationStateFieldMismatch("mtu", record.MTU, conf.MTU)
	}
	return nil
}

// allocationStateFieldMismatch 统一状态字段不匹配的诊断格式。
func allocationStateFieldMismatch(field string, actual, expected interface{}) error {
	return fmt.Errorf(
		"allocation state field %q mismatch: actual=%q expected=%q",
		field,
		fmt.Sprint(actual),
		fmt.Sprint(expected),
	)
}

// classifySetupError 把 network 包的详细错误归入稳定类别，同时保留原错误链。
// 文本匹配仅针对本项目受控的底层错误消息。
func classifySetupError(operation string, err error) error {
	kind := errs.KindVethCreate
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "namespace open failed") {
		kind = errs.KindNamespaceOpen
	} else if strings.Contains(message, "route configuration failed") ||
		strings.Contains(message, "default route") {
		kind = errs.KindRouteConfiguration
	}
	return errs.Wrap(kind, operation, err)
}

// invocationFields 在配置尚未解析时也能采集安全的 runtime 日志字段。
func invocationFields(command string, args *skel.CmdArgs) logging.InvocationFields {
	fields := logging.InvocationFields{Command: command}
	if args != nil {
		fields.ContainerID = args.ContainerID
		fields.IfName = args.IfName
		fields.NetNS = args.Netns
	}
	return fields
}

// logCompletion 是三个命令的统一收尾日志：成功用 info，失败用 error，
// 并带上耗时、最后 phase 与是否进入过回滚。
func logCompletion(
	logger *slog.Logger,
	fields logging.InvocationFields,
	duration time.Duration,
	phase string,
	err error,
	rollback bool,
) {
	logger = logging.WithInvocation(logger, fields)
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
	}
	logger.LogAttrs(
		context.Background(),
		level,
		"CNI command completed",
		logging.OperationAttrs(duration, phase, err, rollback)...,
	)
}
