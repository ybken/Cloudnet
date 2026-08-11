package config

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
)

// ---- 校验常量 ----
const (
	maxNetworkNameLength = 63   // 网络名最大长度
	maxContainerIDLength = 256  // 容器ID最大长度
	maxIfNameLength      = 15   // Linux 接口名最大长度（IFNAMSIZ-1）
	maxNetNSLength       = 4096 // netns 路径最大长度
)

// ---- 正则表达式 ----
var (
	// 网络名：首尾为字母或数字，中间可包含 . _ -，最长 63 字符
	// 这个限制确保网络名作为目录组件时安全，不能包含 .. 或 /
	networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
	// 容器ID：字母数字开头，可包含 . _ : -
	containerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	// 网络接口名：字母数字开头，可包含 . _ -
	ifNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// Validate 对整个 NetConf 进行校验。
//
// 校验分为两个层次：
//  1. CNI 协议字段：cniVersion、name、type 必须匹配
//  2. V1 固定配置：bridge、mtu、ipam 参数必须与代码常量一致
//
// 为什么不允许自定义值？
//
//	这是 cloudnet V1 的设计决策：固定配置简化了状态管理和故障排查。
//	如果 subnet 可以是任意的，那么重启后可能用不同的 subnet 配置去
//	解释已有的 cni-br0 和容器，产生不可预测的结果。
//
// 校验成功后，IPAM 的字符串字段会被解析为 netip 类型缓存到对应字段中。
func (c *NetConf) Validate() error {
	if c == nil {
		return invalidf("configuration is nil")
	}

	// ---- CNI 协议校验 ----
	if c.CNIVersion != "1.0.0" && c.CNIVersion != "1.1.0" {
		return invalidf("unsupported cniVersion %q (supported: 1.0.0, 1.1.0)", c.CNIVersion)
	}
	if err := ValidateNetworkName(c.Name); err != nil {
		return invalidf("network name: %v", err)
	}
	if c.Name != NetworkName {
		return invalidf("network name %q conflicts with fixed V1 name %q", c.Name, NetworkName)
	}
	if c.Type != PluginType {
		return invalidf("type is %q, want %q", c.Type, PluginType)
	}

	// ---- V1 固定配置校验 ----
	if c.Bridge != BridgeName {
		return invalidf("bridge is %q, want %q", c.Bridge, BridgeName)
	}
	if c.MTU != MTU {
		return invalidf("mtu is %d, want %d", c.MTU, MTU)
	}
	if err := c.IPAM.validate(); err != nil {
		return invalidf("ipam: %v", err)
	}

	// 日志级别规范化
	level, err := normalizeLogLevel(c.Log.Level)
	if err != nil {
		return invalidf("log: %v", err)
	}
	c.Log.Level = level
	return nil
}

// validate 对 IPAM 配置进行完整检查：
//   - 子网必须是合法的 IPv4 CIDR
//   - 子网必须被 mask（如不能传 "10.77.0.1/24" 而是 "10.77.0.0/24"）
//   - 子网前缀长度不能大于 30（至少要有 2 个主机位供 gateway + 第一个容器用）
//   - 网关、起点、终点都在子网内，且不在子网中作为 network/broadcast 的保留地址
//   - 范围禁止包含网关地址
//   - 所有值必须与 V1 固定常量匹配
func (c *IPAMConfig) validate() error {
	// 解析并校验子网
	prefix, err := netip.ParsePrefix(c.Subnet)
	if err != nil {
		return fmt.Errorf("subnet %q: %w", c.Subnet, err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("subnet %q is not IPv4", c.Subnet)
	}
	if prefix != prefix.Masked() {
		return fmt.Errorf("subnet %q has host bits set", c.Subnet)
	}
	// /31 和 /32 没有足够的地址空间（至少需要 gateway + 多个容器地址）
	if prefix.Bits() > 30 {
		return fmt.Errorf("subnet %q has no usable host range", c.Subnet)
	}

	// 解析各个地址字段
	gateway, err := parseIPv4("gateway", c.Gateway)
	if err != nil {
		return err
	}
	start, err := parseIPv4("rangeStart", c.RangeStart)
	if err != nil {
		return err
	}
	end, err := parseIPv4("rangeEnd", c.RangeEnd)
	if err != nil {
		return err
	}

	// 网关、起点、终点都必须在子网内
	for field, addr := range map[string]netip.Addr{"gateway": gateway, "rangeStart": start, "rangeEnd": end} {
		if !prefix.Contains(addr) {
			return fmt.Errorf("%s %s is outside subnet %s", field, addr, prefix)
		}
	}

	// 网关不能是 network address（如 10.77.0.0）或 broadcast address（如 10.77.0.255）
	network := prefix.Addr()
	broadcast := lastIPv4(prefix)
	if gateway == network || gateway == broadcast {
		return fmt.Errorf("gateway %s is a reserved network or broadcast address", gateway)
	}

	// rangeStart 必须在 rangeEnd 之前
	if start.Compare(end) > 0 {
		return fmt.Errorf("rangeStart %s is after rangeEnd %s", start, end)
	}

	// 起点和终点不能是 network/broadcast 地址
	for field, addr := range map[string]netip.Addr{"rangeStart": start, "rangeEnd": end} {
		if addr == network || addr == broadcast {
			return fmt.Errorf("%s %s is a reserved network or broadcast address", field, addr)
		}
	}

	// 分配范围不能包含网关地址
	// 例如：rangeStart=10.77.0.1, rangeEnd=10.77.0.250 会把 gateway 也当作可分配地址
	if start.Compare(gateway) <= 0 && end.Compare(gateway) >= 0 {
		return fmt.Errorf("allocation range %s-%s includes gateway %s", start, end, gateway)
	}

	// ---- 与 V1 固定常量对齐 ----
	if prefix.String() != Subnet {
		return fmt.Errorf("subnet is %q, want fixed V1 subnet %q", prefix, Subnet)
	}
	if gateway.String() != Gateway {
		return fmt.Errorf("gateway is %q, want fixed V1 gateway %q", gateway, Gateway)
	}
	if start.String() != RangeStart {
		return fmt.Errorf("rangeStart is %q, want fixed V1 rangeStart %q", start, RangeStart)
	}
	if end.String() != RangeEnd {
		return fmt.Errorf("rangeEnd is %q, want fixed V1 rangeEnd %q", end, RangeEnd)
	}

	// 缓存解析后的值，避免后续重复解析
	c.SubnetPrefix = prefix
	c.GatewayAddr = gateway
	c.RangeStartAddr = start
	c.RangeEndAddr = end
	return nil
}

// parseIPv4 解析一个 IPv4 地址字符串，如果不是有效的 IPv4 地址则返回错误。
func parseIPv4(field, value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s %q: %w", field, value, err)
	}
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("%s %q is not IPv4", field, value)
	}
	return addr, nil
}

// lastIPv4 计算给定前缀的广播地址。
// 例如：对 10.77.0.0/24 返回 10.77.0.255。
//
// 计算方式：将网络地址的 host bits 全部置 1。
//
//	network = 10.77.0.0   = 0x0A4D0000
//	mask = /24 → host_bits = 8, host_mask = (1<<8)-1 = 0xFF
//	broadcast = network | host_mask = 0x0A4D00FF = 10.77.0.255
func lastIPv4(prefix netip.Prefix) netip.Addr {
	octets := prefix.Addr().As4()
	value := binary.BigEndian.Uint32(octets[:])
	hostBits := 32 - prefix.Bits()
	value |= uint32(1<<hostBits) - 1
	var last [4]byte
	binary.BigEndian.PutUint32(last[:], value)
	return netip.AddrFrom4(last)
}

// ValidateNetworkName 将网络名限制在一个小范围的、可移植的 ASCII 字符集内，
// 并从根本上拒绝路径穿越。因为网络名会被用作状态目录的路径组件。
//
// 规则：
//   - 1 到 63 字符
//   - 首尾为字母或数字
//   - 中间只能包含字母、数字、点、下划线、连字符
//   - 不允许 . 或 ..（防止路径穿越）
func ValidateNetworkName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(name) > maxNetworkNameLength {
		return fmt.Errorf("length %d exceeds %d", len(name), maxNetworkNameLength)
	}
	if name == "." || name == ".." || !networkNamePattern.MatchString(name) {
		return fmt.Errorf("%q contains unsafe characters", name)
	}
	return nil
}

// IsSafeNetworkName 是 ValidateNetworkName 的布尔版本。
func IsSafeNetworkName(name string) bool {
	return ValidateNetworkName(name) == nil
}

// ValidateIfName 校验容器网络接口名是否安全。
// Linux 限制：最长 15 字节，只能是字母、数字、点、下划线、连字符。
func ValidateIfName(ifName string) error {
	if ifName == "" {
		return fmt.Errorf("ifname must not be empty")
	}
	if len(ifName) > maxIfNameLength {
		return fmt.Errorf("ifname length %d exceeds Linux limit %d", len(ifName), maxIfNameLength)
	}
	if !ifNamePattern.MatchString(ifName) {
		return fmt.Errorf("ifname %q contains unsafe characters", ifName)
	}
	return nil
}

// ValidateRuntime 校验 CNI 运行时环境变量。
//
// CNI runtime 通过环境变量向插件传递容器信息：
//   - CNI_CONTAINERID：容器 ID
//   - CNI_NETNS：容器 network namespace 路径
//   - CNI_IFNAME：容器内的接口名
//
// 参数 requireNetNS：
//   - ADD/CHECK 时为 true：netns 必须非空、绝对路径、规范化
//   - DEL 时为 false：netns 可以为空（runtime 可能已经删除了 namespace）
func ValidateRuntime(containerID, netns, ifName string, requireNetNS bool) error {
	// 容器 ID 校验
	if containerID == "" {
		return invalidf("containerID must not be empty")
	}
	if len(containerID) > maxContainerIDLength || !containerIDPattern.MatchString(containerID) {
		return invalidf("containerID is malformed or exceeds %d bytes", maxContainerIDLength)
	}

	// 接口名校验
	if err := ValidateIfName(ifName); err != nil {
		return invalidf("%v", err)
	}

	// netns 路径校验
	if netns == "" {
		if requireNetNS {
			return invalidf("netns must not be empty")
		}
		return nil
	}
	if len(netns) > maxNetNSLength || strings.ContainsRune(netns, '\x00') {
		return invalidf("netns is malformed or exceeds %d bytes", maxNetNSLength)
	}
	// 必须是绝对路径
	if !filepath.IsAbs(netns) {
		return invalidf("netns path %q is not absolute", netns)
	}
	// 必须是 clean（不能包含 /../ 或 /./ 等）
	if filepath.Clean(netns) != netns {
		return invalidf("netns path %q is not clean", netns)
	}
	return nil
}
