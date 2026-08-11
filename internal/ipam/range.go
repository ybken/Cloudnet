// Package ipam 是 cloudnet 的本地 IPv4 地址管理器（IP Address Management）。
//
// 为什么需要 IPAM？
//
//	每个容器的 eth0 都需要一个不冲突的 IPv4 地址。cloudnet 没有外部 DHCP 服务器，
//	也没有 etcd/consul 等分布式存储，所以需要一个本地的、基于文件的 IPAM。
//	它把 "哪些地址已经分配了" 记录在磁盘上的 state.json 中。
//
// 本文件定义了 Range——IP 地址池的范围和分配逻辑。
package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

var (
	// ErrInvalidRange 表示地址池配置不合法（如 start 在 end 之后）
	ErrInvalidRange = errors.New("invalid IPv4 allocation range")
	// ErrExhausted 表示地址池中所有可用地址都已被分配
	ErrExhausted = errors.New("IPv4 allocation range exhausted")
)

// Range 是一个验证过的 IPv4 地址分配区间。
//
// 关键概念：
//   - subnet：容器所在的 CIDR 子网（如 10.77.0.0/24）
//   - gateway：子网的网关地址（如 10.77.0.1），这个地址不会被分配给容器
//   - start/end：实际可分配的地址范围（如 10.77.0.10 到 10.77.0.250）
//   - network：子网的网络地址（如 10.77.0.0），不能分配给容器
//   - broadcast：子网的广播地址（如 10.77.0.255），不能分配给容器
//
// 分配规则（NextAvailable）：
//
//	从 start 开始往 end 扫描，跳过 network、broadcast、gateway 和已占用的地址，
//	返回第一个空闲地址。这保证了地址复用——释放低地址后，新容器会优先拿到低地址。
type Range struct {
	subnet    netip.Prefix // 子网 CIDR
	gateway   netip.Addr   // 网关地址（不可分配）
	start     netip.Addr   // 分配范围起始（含）
	end       netip.Addr   // 分配范围结束（含）
	network   netip.Addr   // 子网网络地址（不可分配，如 10.77.0.0）
	broadcast netip.Addr   // 子网广播地址（不可分配，如 10.77.0.255）
}

// NewRange 验证并构造一个 IPv4 地址分配区间。
//
// 参数：
//   - subnet：子网 CIDR，如 10.77.0.0/24
//   - gateway：网关地址，必须在子网内但不能在起点和终点之间
//   - start：分配范围起点（含），如 10.77.0.10
//   - end：分配范围终点（含），如 10.77.0.250
//
// 检查内容：
//   - 子网必须有效且为 IPv4
//   - 子网必须 masked（不能有 host bits）
//   - /31 和 /32 前缀不支持（没有足够的主机地址空间）
//   - gateway、start、end 必须在子网内
//   - start <= end
//   - gateway 不能是网络地址或广播地址
func NewRange(subnet netip.Prefix, gateway, start, end netip.Addr) (Range, error) {
	if !subnet.IsValid() || !subnet.Addr().Is4() {
		return Range{}, fmt.Errorf("%w: subnet %s is not IPv4", ErrInvalidRange, subnet)
	}
	if subnet != subnet.Masked() {
		return Range{}, fmt.Errorf("%w: subnet %s has host bits set", ErrInvalidRange, subnet)
	}
	if subnet.Bits() > 30 {
		return Range{}, fmt.Errorf("%w: subnet %s has no ordinary host range", ErrInvalidRange, subnet)
	}
	gateway = gateway.Unmap()
	start = start.Unmap()
	end = end.Unmap()
	// 校验所有地址都在子网内
	for label, addr := range map[string]netip.Addr{
		"gateway": gateway,
		"start":   start,
		"end":     end,
	} {
		if !addr.IsValid() || !addr.Is4() || !subnet.Contains(addr) {
			return Range{}, fmt.Errorf("%w: %s address %s is outside %s", ErrInvalidRange, label, addr, subnet)
		}
	}
	// 起点必须在终点之前或相等
	if start.Compare(end) > 0 {
		return Range{}, fmt.Errorf("%w: start %s is after end %s", ErrInvalidRange, start, end)
	}

	// 网关地址不能是 network 或 broadcast
	network := subnet.Addr()
	broadcast := ipv4Broadcast(subnet)
	if gateway == network || gateway == broadcast {
		return Range{}, fmt.Errorf("%w: gateway %s is a reserved subnet address", ErrInvalidRange, gateway)
	}
	return Range{
		subnet:    subnet,
		gateway:   gateway,
		start:     start,
		end:       end,
		network:   network,
		broadcast: broadcast,
	}, nil
}

// ---- 访问器 ----
func (r Range) Subnet() netip.Prefix { return r.subnet }
func (r Range) Gateway() netip.Addr  { return r.gateway }
func (r Range) Start() netip.Addr    { return r.start }
func (r Range) End() netip.Addr      { return r.end }

// Validate 通过重新构造一个 Range 来校验当前 Range 的一致性。
func (r Range) Validate() error {
	validated, err := NewRange(r.subnet, r.gateway, r.start, r.end)
	if err != nil {
		return err
	}
	if validated.network != r.network || validated.broadcast != r.broadcast {
		return fmt.Errorf("%w: inconsistent derived bounds", ErrInvalidRange)
	}
	return nil
}

// Contains 判断一个地址是否在可分配范围内。
// 注意：network、broadcast、gateway 三个地址会返回 false，
// 即使它们在 start..end 的数值范围内。
//
// 使用场景：
//   - 校验从 state.json 中读取的 containerIP 是否合法
//   - 在重新 ADD 时确认旧地址仍然可用
func (r Range) Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.Is4() &&
		r.subnet.Contains(addr) &&
		addr.Compare(r.start) >= 0 &&
		addr.Compare(r.end) <= 0 &&
		addr != r.network && addr != r.broadcast && addr != r.gateway
}

// NextAvailable 返回 used 集合中不存在的第一个可用地址。
//
// 扫描策略：从 start 向 end 逐地址扫描（递增），排除不可分配的地址，
// 返回第一个不在 used 中的地址。这是"最低可用地址"策略。
//
// 为什么从低到高？
//
//	当容器被删除后，其地址被释放。新容器创建时优先使用最近释放的低地址，
//	而不是不断往高处分配。这样可以延迟地址池耗尽的时机。
//
// 参数 used 是当前已分配的地址集合（key 是地址，value 忽略）。
func (r Range) NextAvailable(used map[netip.Addr]struct{}) (netip.Addr, error) {
	if err := r.Validate(); err != nil {
		return netip.Addr{}, err
	}
	for addr := r.start; ; addr = addr.Next() {
		// Contains 内部已过滤 network/broadcast/gateway
		if r.Contains(addr) {
			if _, occupied := used[addr]; !occupied {
				return addr, nil
			}
		}
		// 到达终点后退出。不能无条件 addr.Next() 直到无效地址，
		// 因为对 /0 子网而言 addr.Next() 会环绕到无效值。
		if addr == r.end {
			break
		}
	}
	return netip.Addr{}, ErrExhausted
}

// ipv4Broadcast 计算给定子网的广播地址。
// 实现原理：(network | ^mask) —— 将 host bits 全部置 1。
func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Addr().As4()
	network := binary.BigEndian.Uint32(bytes[:])
	bits := prefix.Bits()
	var mask uint32
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	broadcast := network | ^mask
	var result [4]byte
	binary.BigEndian.PutUint32(result[:], broadcast)
	return netip.AddrFrom4(result)
}
