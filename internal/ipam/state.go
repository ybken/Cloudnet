package ipam

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"time"

	"github.com/cloudnet/cloudnet/internal/endpoint"
	"golang.org/x/sys/unix"
)

// 文件与锁位于单网络目录。16 MiB 上限防止损坏文件造成无界读取；
// StateVersion 用于显式拒绝未知 schema。
const (
	// StateVersion is the on-disk state schema understood by this binary.
	StateVersion = 1
	stateFile    = "state.json"
	lockFile     = ".lock"
	maxStateSize = 16 << 20
)

var (
	ErrInvalidNetworkName = errors.New("invalid network name")
	ErrEndpointConflict   = errors.New("endpoint allocation conflicts with persisted state")
	ErrNetworkConflict    = errors.New("network configuration conflicts with persisted state")
	ErrLockedStoreClosed  = errors.New("locked store is no longer active")
)

// CorruptStateError reports a state file that could not be decoded or whose
// invariants failed. The store never rewrites such a file automatically.
type CorruptStateError struct {
	Path string
	Err  error
}

func (e *CorruptStateError) Error() string {
	return fmt.Sprintf("cloudnet state %s is corrupt: %v", e.Path, e.Err)
}

func (e *CorruptStateError) Unwrap() error { return e.Err }

// persistedState 是单网络一致性单元；Endpoints 与 Allocations 是必须同步
// 更新并互相验证的双向映射。
type persistedState struct {
	Version     int                        `json:"version"`
	NetworkName string                     `json:"networkName"`
	UpdatedAt   time.Time                  `json:"updatedAt"`
	Config      *persistedNetworkConfig    `json:"config"`
	Endpoints   map[string]endpoint.Record `json:"endpoints"`
	Allocations map[string]string          `json:"allocations"`
}

// persistedNetworkConfig remains after the last endpoint is released. The
// shared bridge therefore cannot silently acquire a different IPAM identity on
// a later ADD while this network state still exists.
type persistedNetworkConfig struct {
	Subnet     string `json:"subnet"`
	Gateway    string `json:"gateway"`
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`
	Bridge     string `json:"bridge"`
	MTU        int    `json:"mtu"`
}

// newPersistedState 仅用于文件不存在的初始网络，不替代损坏文件。
func newPersistedState(networkName string) persistedState {
	return persistedState{
		Version:     StateVersion,
		NetworkName: networkName,
		Endpoints:   make(map[string]endpoint.Record),
		Allocations: make(map[string]string),
	}
}

// loadState 限制大小、严格解码并验证全部不变量。仅 ENOENT 创建空状态；
// 损坏文件原样保留并返回 CorruptStateError。
func loadState(dir *os.File, path, networkName string) (persistedState, error) {
	file, err := openRegularFileAt(dir, stateFile, path, unix.O_RDONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		return newPersistedState(networkName), nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read state %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return persistedState{}, fmt.Errorf("stat state %s: %w", path, err)
	}
	if info.Size() > maxStateSize {
		return persistedState{}, &CorruptStateError{Path: path, Err: fmt.Errorf("file exceeds %d bytes", maxStateSize)}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStateSize+1))
	if err != nil {
		return persistedState{}, fmt.Errorf("read state %s: %w", path, err)
	}
	if len(raw) > maxStateSize {
		return persistedState{}, &CorruptStateError{Path: path, Err: fmt.Errorf("file exceeds %d bytes", maxStateSize)}
	}

	var state persistedState
	if err := decodeStateJSON(raw, &state); err != nil {
		return persistedState{}, &CorruptStateError{Path: path, Err: fmt.Errorf("decode JSON: %w", err)}
	}
	if err := validatePersistedState(state, networkName); err != nil {
		return persistedState{}, &CorruptStateError{Path: path, Err: err}
	}
	return state, nil
}

// decodeStateJSON 拒绝重复 key、未知字段和尾随第二个 JSON 值，
// 避免解析器的“最后值获胜”掩盖损坏。
func decodeStateJSON(raw []byte, state *persistedState) error {
	if err := checkUniqueStateJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(state); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// checkUniqueStateJSON 对完整 token 流递归检查每一层 object。
func checkUniqueStateJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := checkUniqueStateValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// checkUniqueStateValue 消费一个 JSON 值，并在每个 object 作用域维护 key 集；
// array 元素继续递归，所以嵌套对象同样不能重复 key。
func checkUniqueStateValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := checkUniqueStateValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("expected object end")
		}
	case '[':
		for decoder.More() {
			if err := checkUniqueStateValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("expected array end")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

// validatePersistedState 验证 schema、网络配置、每条 Record 和双向映射；
// 读盘与写盘共同使用这套核心不变量。
func validatePersistedState(state persistedState, networkName string) error {
	if state.Version != StateVersion {
		return fmt.Errorf("state version %d is unsupported (want %d)", state.Version, StateVersion)
	}
	if state.NetworkName != networkName {
		return fmt.Errorf("state network %q does not match directory network %q", state.NetworkName, networkName)
	}
	if state.Endpoints == nil || state.Allocations == nil {
		return fmt.Errorf("state endpoint or allocation map is null")
	}
	if state.Config == nil {
		return fmt.Errorf("state network config is null")
	}
	allocationRange, err := state.Config.allocationRange()
	if err != nil {
		return fmt.Errorf("state network config: %w", err)
	}
	if len(state.Endpoints) != len(state.Allocations) {
		return fmt.Errorf("state has %d endpoints but %d allocations", len(state.Endpoints), len(state.Allocations))
	}

	for id, record := range state.Endpoints {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("endpoint %s: %w", id, err)
		}
		if record.NetworkName != networkName {
			return fmt.Errorf("endpoint %s belongs to network %q", id, record.NetworkName)
		}
		if !state.Config.matchesRecord(record) {
			return fmt.Errorf("endpoint %s network configuration differs from persisted network config", id)
		}
		allocatedIP := netip.MustParseAddr(record.ContainerIP)
		if !allocationRange.Contains(allocatedIP) {
			return fmt.Errorf("endpoint %s IP %s is not usable in persisted allocation range", id, allocatedIP)
		}
		if want := record.EndpointKey().ID(); id != want {
			return fmt.Errorf("endpoint map key %s does not match record key %s", id, want)
		}
		if allocatedTo, ok := state.Allocations[record.ContainerIP]; !ok || allocatedTo != id {
			return fmt.Errorf("endpoint %s IP %s has no matching allocation", id, record.ContainerIP)
		}
	}
	for rawIP, id := range state.Allocations {
		ip, err := netip.ParseAddr(rawIP)
		if err != nil || !ip.Is4() || ip.String() != rawIP {
			return fmt.Errorf("allocation key %q is not a canonical IPv4 address", rawIP)
		}
		record, ok := state.Endpoints[id]
		if !ok {
			return fmt.Errorf("allocation %s references missing endpoint %s", rawIP, id)
		}
		if record.ContainerIP != rawIP {
			return fmt.Errorf("allocation %s disagrees with endpoint %s IP %s", rawIP, id, record.ContainerIP)
		}
	}
	return nil
}

// allocationRange 将磁盘字符串恢复为经验证的 Range，并核验 Bridge/MTU。
func (c persistedNetworkConfig) allocationRange() (Range, error) {
	subnet, err := netip.ParsePrefix(c.Subnet)
	if err != nil {
		return Range{}, fmt.Errorf("parse subnet %q: %w", c.Subnet, err)
	}
	gateway, err := netip.ParseAddr(c.Gateway)
	if err != nil {
		return Range{}, fmt.Errorf("parse gateway %q: %w", c.Gateway, err)
	}
	start, err := netip.ParseAddr(c.RangeStart)
	if err != nil {
		return Range{}, fmt.Errorf("parse range start %q: %w", c.RangeStart, err)
	}
	end, err := netip.ParseAddr(c.RangeEnd)
	if err != nil {
		return Range{}, fmt.Errorf("parse range end %q: %w", c.RangeEnd, err)
	}
	if c.Bridge == "" || len(c.Bridge) > 15 || c.MTU <= 0 || c.MTU > 65535 {
		return Range{}, fmt.Errorf("bridge %q or MTU %d is invalid", c.Bridge, c.MTU)
	}
	return NewRange(subnet, gateway, start, end)
}

// matchesRecord 确认 endpoint 没有偏离网络级固定配置。
func (c persistedNetworkConfig) matchesRecord(record endpoint.Record) bool {
	return c.Subnet == record.Subnet &&
		c.Gateway == record.Gateway &&
		c.RangeStart == record.RangeStart &&
		c.RangeEnd == record.RangeEnd &&
		c.Bridge == record.Bridge &&
		c.MTU == record.MTU
}

var renameStateFileAt = unix.Renameat

// writeStateAtomic 执行“同目录临时文件 -> file fsync -> renameat -> dir fsync”。
// rename 让读者只见旧版或完整新版，目录 fsync 保证重启后目录项持久。
func writeStateAtomic(dir *os.File, path string, state persistedState) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	raw = append(raw, '\n')

	if err := requireRegularOrAbsentAt(dir, stateFile, path); err != nil {
		return err
	}
	temporary, temporaryName, err := createStateTemporary(dir, path)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		_ = temporary.Close()
		if !renamed {
			_ = unix.Unlinkat(int(dir.Fd()), temporaryName, 0)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod state temporary file %s: %w", temporary.Name(), err)
	}
	if _, err := temporary.Write(raw); err != nil {
		return fmt.Errorf("write state temporary file %s: %w", temporary.Name(), err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("fsync state temporary file %s: %w", temporary.Name(), err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state temporary file %s: %w", temporary.Name(), err)
	}
	if err := renameStateFileAt(int(dir.Fd()), temporaryName, int(dir.Fd()), stateFile); err != nil {
		return fmt.Errorf("atomically replace state %s: %w", path, err)
	}
	renamed = true

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync state directory for %s: %w", path, err)
	}
	return nil
}

// createStateTemporary 使用随机名与 O_EXCL，避免并发碰撞和跟随已有条目；
// 100 次是极端随机碰撞或目录污染时的有界重试。
func createStateTemporary(dir *os.File, statePath string) (*os.File, string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("generate state temporary name: %w", err)
		}
		name := ".state.json.tmp-" + hex.EncodeToString(random[:])
		file, err := openRegularFileAt(
			dir,
			name,
			statePath+" temporary file",
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL,
			0o600,
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create state temporary file: %w", err)
		}
		return file, name, nil
	}
	return nil, "", errors.New("create state temporary file: exhausted unique names")
}

// sortedRecords 消除 Go map 随机迭代顺序，按 containerID/ifName 稳定输出。
func sortedRecords(records map[string]endpoint.Record) []endpoint.Record {
	result := make([]endpoint.Record, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].EndpointKey(), result[j].EndpointKey()
		if left.ContainerID != right.ContainerID {
			return left.ContainerID < right.ContainerID
		}
		return left.IfName < right.IfName
	})
	return result
}
