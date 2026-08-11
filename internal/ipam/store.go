package ipam

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/cloudnet/cloudnet/internal/endpoint"
	"golang.org/x/sys/unix"
)

// DefaultStateRoot 是生产状态根；测试通过 NewStore 注入临时目录。
const DefaultStateRoot = "/var/lib/cloudnet"

var networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

// Store 持有所有网络状态目录的根路径；自身不缓存可变状态。
type Store struct {
	root string
}

// LockedStore 只在 WithLock callback 期间有效。变更先发生在内存副本中，
// Commit 或 callback 正常结束时才原子写盘；active 防止对象逃逸后继续使用。
type LockedStore struct {
	networkName string
	statePath   string
	stateDir    *os.File
	state       persistedState
	dirty       bool
	active      bool
}

// AllocationRequest 只携带调用方负责的身份/网络元数据。ContainerIP、range、
// phase 和时间戳由 Allocate 填写，避免调用方伪造 Store 管理字段。
type AllocationRequest struct {
	Endpoint endpoint.Record
	Range    Range
}

// NewStore 将 root 转成清理后的绝对路径；真正的防 symlink 打开延迟到 WithLock，
// 因而构造 Store 本身不会产生文件系统副作用。
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("state root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve state root %q: %w", root, err)
	}
	return &Store{root: filepath.Clean(absolute)}, nil
}

// ValidateNetworkName ensures a network name can be used as exactly one safe
// path component. The conservative ASCII grammar also keeps operational paths
// and logs predictable.
func ValidateNetworkName(name string) error {
	if len(name) > 63 || !networkNamePattern.MatchString(name) {
		return fmt.Errorf("%w %q: use 1..63 ASCII letters, digits, dot, underscore, or hyphen; start and end alphanumeric", ErrInvalidNetworkName, name)
	}
	return nil
}

// networkDir 先保证网络名只能成为一个安全路径组件；安全打开并不依赖
// 拼接后的字符串，而由 securefs 的 fd-relative openat 完成。
func (s *Store) networkDir(networkName string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("state store is nil")
	}
	if err := ValidateNetworkName(networkName); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "networks", networkName), nil
}

// StatePath 返回用于日志和诊断的 state.json 路径。
func (s *Store) StatePath(networkName string) (string, error) {
	dir, err := s.networkDir(networkName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFile), nil
}

// LockPath 返回永久 .lock 文件的展示路径。
func (s *Store) LockPath(networkName string) (string, error) {
	dir, err := s.networkDir(networkName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, lockFile), nil
}

// WithLock 在 callback 全程持有进程级独占 flock。callback 成功会自动提交 dirty
// 状态；失败则丢弃尚未显式 Commit 的内存变更。ADD 的显式 Commit 可先落 pending，
// 这是刻意跨越 callback 失败边界的崩溃恢复点。
func (s *Store) WithLock(networkName string, callback func(*LockedStore) error) error {
	if callback == nil {
		return fmt.Errorf("state lock callback is nil")
	}
	dir, err := s.networkDir(networkName)
	if err != nil {
		return err
	}
	dirHandle, err := openNetworkStateDir(s.root, networkName)
	if err != nil {
		return err
	}
	defer dirHandle.Close()

	lockPath := filepath.Join(dir, lockFile)
	lock, err := openRegularFileAt(dirHandle, lockFile, lockPath, unix.O_CREAT|unix.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open permanent network lock %s: %w", lockPath, err)
	}
	defer lock.Close()
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod permanent network lock %s: %w", lockPath, err)
	}
	if err := flockExclusive(lock); err != nil {
		return fmt.Errorf("lock network state %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	statePath := filepath.Join(dir, stateFile)
	state, err := loadState(dirHandle, statePath, networkName)
	if err != nil {
		return err
	}
	locked := &LockedStore{
		networkName: networkName,
		statePath:   statePath,
		stateDir:    dirHandle,
		state:       state,
		active:      true,
	}
	defer func() { locked.active = false }()

	if err := callback(locked); err != nil {
		return err
	}
	if err := locked.Commit(); err != nil {
		return fmt.Errorf("commit network %q state: %w", networkName, err)
	}
	return nil
}

// flockExclusive 遇到信号中断 EINTR 就重试，其他错误原样返回。
func flockExclusive(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// ensureActive 阻止 callback 结束后继续访问已失去锁保护的状态副本。
func (s *LockedStore) ensureActive() error {
	if s == nil || !s.active {
		return ErrLockedStoreClosed
	}
	return nil
}

// Commit 在继续持锁时持久写入变更。ADD 借此在 namespace/veth 工作前先记录
// pending；dirty=false 时是 no-op，便于 WithLock 无条件收尾调用。
func (s *LockedStore) Commit() error {
	if err := s.ensureActive(); err != nil {
		return err
	}
	if !s.dirty {
		return nil
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := validatePersistedState(s.state, s.networkName); err != nil {
		return fmt.Errorf("refuse to persist invalid state: %w", err)
	}
	if err := writeStateAtomic(s.stateDir, s.statePath, s.state); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Allocate 为新 endpoint 选择最低可用 IP 并创建 pending；同一 key 重试会
// 验证不可变字段并返回原记录。第二个返回值表示这次是否新建。
func (s *LockedStore) Allocate(request AllocationRequest) (endpoint.Record, bool, error) {
	if err := s.ensureActive(); err != nil {
		return endpoint.Record{}, false, err
	}
	if err := validateRequest(request, s.networkName); err != nil {
		return endpoint.Record{}, false, err
	}

	key := request.Endpoint.EndpointKey()
	id := key.ID()
	networkConfig := networkConfigFromRequest(request)
	if s.state.Config != nil && *s.state.Config != networkConfig {
		return endpoint.Record{}, false, fmt.Errorf("%w for network %q", ErrNetworkConflict, s.networkName)
	}
	if existing, ok := s.state.Endpoints[id]; ok {
		if err := matchesRequest(existing, request); err != nil {
			return endpoint.Record{}, false, err
		}
		if existing.NetNS != request.Endpoint.NetNS {
			existing.NetNS = request.Endpoint.NetNS
			existing.UpdatedAt = time.Now().UTC()
			s.state.Endpoints[id] = existing
			s.dirty = true
		}
		return existing, false, nil
	}

	used := make(map[netip.Addr]struct{}, len(s.state.Allocations))
	for raw := range s.state.Allocations {
		// State invariants were checked on load and after every mutation.
		used[netip.MustParseAddr(raw)] = struct{}{}
	}
	addr, err := request.Range.NextAvailable(used)
	if err != nil {
		return endpoint.Record{}, false, fmt.Errorf("allocate endpoint %s: %w", id, err)
	}

	now := time.Now().UTC()
	record := request.Endpoint
	record.ContainerIP = addr.String()
	record.Subnet = request.Range.Subnet().String()
	record.Gateway = request.Range.Gateway().String()
	record.RangeStart = request.Range.Start().String()
	record.RangeEnd = request.Range.End().String()
	record.Phase = endpoint.PhasePending
	record.CreatedAt = now
	record.UpdatedAt = now
	if err := record.Validate(); err != nil {
		return endpoint.Record{}, false, fmt.Errorf("build pending endpoint: %w", err)
	}
	s.state.Endpoints[id] = record
	s.state.Allocations[record.ContainerIP] = id
	if s.state.Config == nil {
		s.state.Config = &networkConfig
	}
	s.dirty = true
	return record, true, nil
}

// networkConfigFromRequest 提取所有 endpoint 共享的网络级不变量。
func networkConfigFromRequest(request AllocationRequest) persistedNetworkConfig {
	return persistedNetworkConfig{
		Subnet:     request.Range.Subnet().String(),
		Gateway:    request.Range.Gateway().String(),
		RangeStart: request.Range.Start().String(),
		RangeEnd:   request.Range.End().String(),
		Bridge:     request.Endpoint.Bridge,
		MTU:        request.Endpoint.MTU,
	}
}

// validateRequest 划清调用方与 Store 的字段所有权，并核对持锁网络身份。
func validateRequest(request AllocationRequest, networkName string) error {
	key := request.Endpoint.EndpointKey()
	if err := key.Validate(); err != nil {
		return fmt.Errorf("invalid allocation endpoint: %w", err)
	}
	if key.NetworkName != networkName {
		return fmt.Errorf("%w: endpoint network %q does not match locked network %q", ErrEndpointConflict, key.NetworkName, networkName)
	}
	if err := ValidateNetworkName(key.NetworkName); err != nil {
		return err
	}
	if err := request.Range.Validate(); err != nil {
		return err
	}
	if request.Endpoint.ContainerIP != "" || request.Endpoint.Phase != "" ||
		!request.Endpoint.CreatedAt.IsZero() || !request.Endpoint.UpdatedAt.IsZero() {
		return fmt.Errorf("allocation request contains store-managed endpoint fields")
	}
	if request.Endpoint.Subnet != "" && request.Endpoint.Subnet != request.Range.Subnet().String() {
		return fmt.Errorf("%w: endpoint subnet %q differs from range %s", ErrEndpointConflict, request.Endpoint.Subnet, request.Range.Subnet())
	}
	if request.Endpoint.Gateway != "" && request.Endpoint.Gateway != request.Range.Gateway().String() {
		return fmt.Errorf("%w: endpoint gateway %q differs from range %s", ErrEndpointConflict, request.Endpoint.Gateway, request.Range.Gateway())
	}
	if request.Endpoint.HostVethName == "" || request.Endpoint.Bridge == "" || request.Endpoint.MTU <= 0 {
		return fmt.Errorf("allocation endpoint is missing host veth, bridge, or MTU metadata")
	}
	return nil
}

// matchesRequest 是幂等复用的守门条件；netns/时间戳可变，其余身份与
// 网络字段必须精确一致，原 IP 也必须仍在 range 中。
func matchesRequest(existing endpoint.Record, request AllocationRequest) error {
	want := request.Endpoint
	want.Subnet = request.Range.Subnet().String()
	want.Gateway = request.Range.Gateway().String()
	want.RangeStart = request.Range.Start().String()
	want.RangeEnd = request.Range.End().String()
	if existing.NetworkName != want.NetworkName ||
		existing.ContainerID != want.ContainerID ||
		existing.IfName != want.IfName ||
		existing.HostVethName != want.HostVethName ||
		existing.Subnet != want.Subnet ||
		existing.Gateway != want.Gateway ||
		existing.RangeStart != want.RangeStart ||
		existing.RangeEnd != want.RangeEnd ||
		existing.Bridge != want.Bridge ||
		existing.MTU != want.MTU ||
		!request.Range.Contains(netip.MustParseAddr(existing.ContainerIP)) {
		return fmt.Errorf("%w for endpoint %s", ErrEndpointConflict, existing.EndpointKey().ID())
	}
	return nil
}

// GetEndpoint 在锁内按稳定 SHA-256 key 查询 endpoint。
func (s *LockedStore) GetEndpoint(key endpoint.Key) (endpoint.Record, bool, error) {
	if err := s.ensureActive(); err != nil {
		return endpoint.Record{}, false, err
	}
	if err := key.Validate(); err != nil {
		return endpoint.Record{}, false, err
	}
	if key.NetworkName != s.networkName {
		return endpoint.Record{}, false, fmt.Errorf("endpoint network %q does not match locked network %q", key.NetworkName, s.networkName)
	}
	record, ok := s.state.Endpoints[key.ID()]
	return record, ok, nil
}

// ListEndpoints 返回确定性排序副本，便于诊断和测试稳定比较。
func (s *LockedStore) ListEndpoints() ([]endpoint.Record, error) {
	if err := s.ensureActive(); err != nil {
		return nil, err
	}
	return sortedRecords(s.state.Endpoints), nil
}

// MarkPending 表示已有 endpoint 即将被协调。破坏性网络操作前持久化这一迁移，
// 进程中断后下一次 ADD/DEL 才知道需要恢复；重复标记是 no-op。
func (s *LockedStore) MarkPending(key endpoint.Key) (endpoint.Record, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, err
	}
	if !ok {
		return endpoint.Record{}, fmt.Errorf("endpoint %s does not exist", key.ID())
	}
	if record.Phase == endpoint.PhasePending {
		return record, nil
	}
	record.Phase = endpoint.PhasePending
	record.UpdatedAt = time.Now().UTC()
	s.state.Endpoints[key.ID()] = record
	s.dirty = true
	return record, nil
}

// MarkReady 只在网络创建和复核成功后调用；重复标记是 no-op。
func (s *LockedStore) MarkReady(key endpoint.Key) (endpoint.Record, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, err
	}
	if !ok {
		return endpoint.Record{}, fmt.Errorf("endpoint %s does not exist", key.ID())
	}
	if record.Phase == endpoint.PhaseReady {
		return record, nil
	}
	record.Phase = endpoint.PhaseReady
	record.UpdatedAt = time.Now().UTC()
	s.state.Endpoints[key.ID()] = record
	s.dirty = true
	return record, nil
}

// Release 在同一变更中删除 endpoint 和反向 allocation。缺失是成功 no-op；
// 双向映射不一致则报告损坏，不“修复”并覆盖证据。
func (s *LockedStore) Release(key endpoint.Key) (endpoint.Record, bool, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, false, err
	}
	if !ok {
		return endpoint.Record{}, false, nil
	}
	id := key.ID()
	allocatedTo, ok := s.state.Allocations[record.ContainerIP]
	if !ok || allocatedTo != id {
		return endpoint.Record{}, false, &CorruptStateError{
			Path: s.statePath,
			Err:  fmt.Errorf("endpoint %s allocation %s is inconsistent", id, record.ContainerIP),
		}
	}
	delete(s.state.Allocations, record.ContainerIP)
	delete(s.state.Endpoints, id)
	s.dirty = true
	return record, true, nil
}
