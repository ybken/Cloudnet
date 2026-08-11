// Package endpoint 定义了 CNI endpoint 的持久化身份和生命周期状态。
//
// 什么是 endpoint？
//
//	endpoint = 一个容器的网络连接点。它的身份由三元组唯一确定：
//	(网络名, 容器ID, 接口名)，例如 ("cloudnet-v1", "abc123...", "eth0")。
//
// 为什么需要这个包？
//
//	这个包定义了 endpoint 的 Key（唯一标识）和 Record（完整持久化记录）。
//	它只做数据结构定义和校验，不涉及文件系统和网络操作。
//	这保证了 endpoint 的"身份"可以在 IPAM、网络、CNI 服务层之间安全传递。
//
// Record 包含什么？
//   - 三元组身份：NetworkName + ContainerID + IfName
//   - 网络信息：ContainerIP（分配的地址）、Subnet、Gateway、RangeStart、RangeEnd
//   - 数据面信息：HostVethName（宿主机侧 veth 名）、Bridge（网桥名）、MTU
//   - 生命周期：Phase（pending/ready）、CreatedAt、UpdatedAt
//   - 可选信息：NetNS（network namespace 路径，只作记录用，DEL 时不依赖）
//
// Phase 状态机：
//
//	(不存在) ──分配IP──▶ pending ──网络创建完成──▶ ready
//	                        │                        │
//	                        └──DEL/回滚──▶ (释放IP)   └──DEL──▶ (释放IP)
//
//	pending 的含义：IP 已分配并持久化，但网络资源（veth/路由/Bridge port）
//	可能尚未完整创建。如果进程在 pending 阶段崩溃，下次 ADD 或 DEL 可以
//	根据 pending 状态清理残留资源。
//
// 设计原则：
//   - 纯数据结构：无文件系统操作，无网络操作
//   - 严格校验：Validate() 对每个字段进行完整检查
//   - Key.ID() 使用 SHA-256 生成确定性的固定长度标识符
package endpoint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
)

// Phase 记录 endpoint 分配的生命周期阶段。
// 注意：Phase 是 json 序列化的字符串，所以使用 string 底层类型。
type Phase string

const (
	// PhasePending 表示 IP 地址已分配并写入 state.json，
	// 但内核网络资源（veth、路由、Bridge port）可能尚未完整创建。
	// 这给了 ADD 回滚和后续 DEL 足够的证据来清理中断操作留下的残留。
	PhasePending Phase = "pending"

	// PhaseReady 表示 endpoint 的所有网络资源已完整创建并验证通过。
	// 只有 PhaseReady 的 endpoint 能通过 CHECK 命令的验证。
	PhaseReady Phase = "ready"
)

// Key 是 endpoint 的最小 CNI 身份标识。
// 三个字段的组合唯一确定一个 endpoint：
//   - NetworkName：CNI 网络名，cloudnet V1 固定为 "cloudnet-v1"
//   - ContainerID：容器运行时分配的容器 ID（如 containerd 的 container ID）
//   - IfName：容器内的网络接口名（CNI_IFNAME，通常为 "eth0"）
type Key struct {
	NetworkName string
	ContainerID string
	IfName      string
}

// Validate 拒绝不完整或不安全的 endpoint 身份标识。
// 主要检查：字段非空、不含 NUL 字节、IfName 符合 Linux 接口命名规范。
// NetworkName 的路径安全性校验在网络名被当作目录组件时（IPAM store 层）再做更严格的检查。
func (k Key) Validate() error {
	if k.NetworkName == "" {
		return fmt.Errorf("endpoint network name is empty")
	}
	// NUL 字节是 NUL 分隔 tuple → SHA-256 digest 时的分隔符，
	// 身份字段本身不能包含 NUL，否则会破坏 tuple 边界
	if strings.IndexByte(k.NetworkName, 0) >= 0 {
		return fmt.Errorf("endpoint network name contains NUL")
	}
	if k.ContainerID == "" {
		return fmt.Errorf("endpoint container ID is empty")
	}
	if len(k.ContainerID) > 1024 {
		return fmt.Errorf("endpoint container ID is too long: %d bytes", len(k.ContainerID))
	}
	if strings.IndexByte(k.ContainerID, 0) >= 0 {
		return fmt.Errorf("endpoint container ID contains NUL")
	}
	if err := validateInterfaceName("container interface", k.IfName); err != nil {
		return err
	}
	return nil
}

// ID 返回 endpoint 的稳定、固定长度存储键。
//
// 为什么用 SHA-256 而不是直接拼接？
//   - ContainerID 可能很长（64+ hex 字符），直接拼接会生成过长的 key
//   - SHA-256 产生 64 hex 字符的固定长度 key，便于索引
//   - 长度前缀编码确保 (net="a", cid="bc", if="d") 和
//     (net="ab", cid="c", if="d") 不会产生相同结果
//
// 编码方式：对每个组件先写 4 字节大端长度，再写内容，然后整体 SHA-256。
func (k Key) ID() string {
	h := sha256.New()
	var length [4]byte
	for _, part := range []string{k.NetworkName, k.ContainerID, k.IfName} {
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Record 是单个 endpoint 的完整持久化证据。
//
// 关键设计决策：
//   - NetNS 只是"信息性"字段：runtime 可能在 DEL 之前就已经删除了 namespace。
//     因此 DEL 不能依赖 NetNS 来定位资源；HostVethName 才是确定性的清理入口。
//   - 网络配置字段（Subnet/Gateway/RangeStart/RangeEnd/Bridge/MTU）被重复保存
//     在每个 endpoint record 中，这样做是为了在 state.json 中不依赖全局 config
//     也能独立校验每个 endpoint 的完整性。
type Record struct {
	// ---- 身份 ----
	NetworkName string `json:"networkName"`     // CNI 网络名，cloudnet V1 固定为 "cloudnet-v1"
	ContainerID string `json:"containerID"`     // 容器 ID
	IfName      string `json:"ifName"`          // 容器内接口名（如 eth0）
	NetNS       string `json:"netns,omitempty"` // network namespace 路径，仅作记录

	// ---- 数据面 ----
	HostVethName string `json:"hostVethName"` // 宿主机侧 veth 名（确定性生成，如 cn5665fb5b768ca）
	ContainerIP  string `json:"containerIP"`  // 分配给容器的 IPv4 地址（如 "10.77.0.10"）

	// ---- 网络配置（冗余保存，用于独立校验） ----
	Subnet     string `json:"subnet"`     // 子网 CIDR（如 "10.77.0.0/24"）
	Gateway    string `json:"gateway"`    // 网关地址（如 "10.77.0.1"）
	RangeStart string `json:"rangeStart"` // IP 池起始（如 "10.77.0.10"）
	RangeEnd   string `json:"rangeEnd"`   // IP 池结束（如 "10.77.0.250"）
	Bridge     string `json:"bridge"`     // Linux Bridge 名（固定 "cni-br0"）
	MTU        int    `json:"mtu"`        // MTU 值（固定 1500）

	// ---- 生命周期 ----
	Phase     Phase     `json:"phase"`     // pending 或 ready
	CreatedAt time.Time `json:"createdAt"` // 首次分配时间（UTC）
	UpdatedAt time.Time `json:"updatedAt"` // 最后修改时间（UTC）
}

// EndpointKey 从 Record 重建其 Key 三元组。
func (r Record) EndpointKey() Key {
	return Key{
		NetworkName: r.NetworkName,
		ContainerID: r.ContainerID,
		IfName:      r.IfName,
	}
}

// Validate 对从持久化 state.json 中读取的完整 Record 进行校验。
// 这不仅是字段级别的检查，还包括了业务规则的一致性检验。
func (r Record) Validate() error {
	if err := r.EndpointKey().Validate(); err != nil {
		return err
	}
	if err := validateInterfaceName("host veth", r.HostVethName); err != nil {
		return err
	}
	if err := validateInterfaceName("bridge", r.Bridge); err != nil {
		return err
	}
	if r.MTU <= 0 || r.MTU > 65535 {
		return fmt.Errorf("endpoint MTU %d is outside 1..65535", r.MTU)
	}

	// 解析并校验 subnet
	subnet, err := netip.ParsePrefix(r.Subnet)
	if err != nil || !subnet.IsValid() || !subnet.Addr().Is4() || subnet != subnet.Masked() {
		return fmt.Errorf("endpoint subnet %q is not a canonical IPv4 prefix", r.Subnet)
	}

	// 解析并校验 containerIP 在 subnet 内
	ip, err := netip.ParseAddr(r.ContainerIP)
	if err != nil || !ip.Is4() || !subnet.Contains(ip) {
		return fmt.Errorf("endpoint IP %q is not IPv4 inside %s", r.ContainerIP, subnet)
	}

	// 解析并校验 gateway 在 subnet 内
	gateway, err := netip.ParseAddr(r.Gateway)
	if err != nil || !gateway.Is4() || !subnet.Contains(gateway) {
		return fmt.Errorf("endpoint gateway %q is not IPv4 inside %s", r.Gateway, subnet)
	}

	// 校验分配范围
	rangeStart, err := netip.ParseAddr(r.RangeStart)
	if err != nil || !rangeStart.Is4() || !subnet.Contains(rangeStart) {
		return fmt.Errorf("endpoint range start %q is not IPv4 inside %s", r.RangeStart, subnet)
	}
	rangeEnd, err := netip.ParseAddr(r.RangeEnd)
	if err != nil || !rangeEnd.Is4() || !subnet.Contains(rangeEnd) || rangeStart.Compare(rangeEnd) > 0 {
		return fmt.Errorf("endpoint range end %q does not form a valid range in %s", r.RangeEnd, subnet)
	}

	// 校验 phase 只能是 pending 或 ready
	if r.Phase != PhasePending && r.Phase != PhaseReady {
		return fmt.Errorf("endpoint phase %q is invalid", r.Phase)
	}

	// 校验时间戳
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("endpoint createdAt is zero")
	}
	if r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("endpoint updatedAt is invalid")
	}
	return nil
}

// validateInterfaceName 校验 Linux 网络接口名是否安全。
// Linux 限制：最长 15 字节（IFNAMSIZ 包含结尾 NUL），
// 不能包含 /、:、NUL、空白字符，不能是 . 或 ..
func validateInterfaceName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", kind)
	}
	if len(name) > 15 {
		return fmt.Errorf("%s name %q exceeds 15 bytes", kind, name)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/:\x00") {
		return fmt.Errorf("%s name %q contains forbidden characters", kind, name)
	}
	for _, r := range name {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%s name %q contains whitespace", kind, name)
		}
	}
	return nil
}
