package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// EnsureBridge 创建缺失的共享 Bridge，或核验并补齐兼容的已有 Bridge。
// 它只补地址和 UP 状态，不覆盖冲突的类型、拓扑、MTU 或额外 IPv4。
//
// EnsureBridge creates the shared bridge when absent or verifies and completes
// a compatible existing bridge. It never changes a conflicting MTU or address.
func EnsureBridge(spec BridgeSpec) (created bool, err error) {
	if err := validateBridgeSpec(spec); err != nil {
		return false, fmt.Errorf("invalid bridge configuration: %w", err)
	}

	// 先查后建；EEXIST 仍重新读取，以正确处理并发创建者。
	link, found, err := linkByName(spec.Name)
	if err != nil {
		return false, err
	}
	if !found {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = spec.Name
		attrs.MTU = spec.MTU
		candidate := &netlink.Bridge{LinkAttrs: attrs}
		if err := netlink.LinkAdd(candidate); err != nil && !errors.Is(err, unix.EEXIST) {
			return false, fmt.Errorf("create bridge %q: %w", spec.Name, err)
		} else if err == nil {
			created = true
		}
		link, found, err = linkByName(spec.Name)
		if err != nil {
			return created, rollbackBridgeCreate(candidate, created, fmt.Errorf("reload bridge %q: %w", spec.Name, err))
		}
		if !found {
			return created, rollbackBridgeCreate(candidate, created, fmt.Errorf("bridge %q missing after create", spec.Name))
		}
	}

	// 同名对象不是 Linux Bridge 时立即停止，绝不尝试接管或替换。
	bridge, ok := link.(*netlink.Bridge)
	if !ok || link.Type() != "bridge" {
		return created, rollbackBridgeCreate(link, created, fmt.Errorf(
			"bridge conflict: link %q has type %q, want Linux bridge",
			spec.Name,
			link.Type(),
		))
	}
	if bridge.Attrs().MTU != spec.MTU {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf(
			"bridge conflict: %q MTU is %d, want %d",
			spec.Name,
			bridge.Attrs().MTU,
			spec.MTU,
		))
	}
	// 写地址或拉起设备前先检查端口，防止修改挂载了物理/未知端口的 Bridge。
	// 共享资源冲突必须保留现场供管理员处理。
	if err := checkBridgeTopology(bridge, spec.NetworkName); err != nil {
		return created, rollbackBridgeCreate(bridge, created, err)
	}

	expected := netip.PrefixFrom(spec.Gateway, spec.Subnet.Bits())
	addresses, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf("list bridge %q addresses: %w", spec.Name, err))
	}
	// absent 可补齐；present 无需操作；conflict 必须保留现场并失败。
	state, err := classifyIPv4Addresses(addresses, expected)
	if err != nil {
		return created, rollbackBridgeCreate(bridge, created, err)
	}
	if state == addressConflict {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf(
			"bridge conflict: %q IPv4 addresses do not exactly match %s",
			spec.Name,
			expected,
		))
	}
	if state == addressAbsent {
		address := &netlink.Addr{IPNet: ipNetFromPrefix(expected)}
		if err := netlink.AddrAdd(bridge, address); err != nil && !errors.Is(err, unix.EEXIST) {
			return created, rollbackBridgeCreate(bridge, created, fmt.Errorf(
				"configure bridge %q address %s: %w",
				spec.Name,
				expected,
				err,
			))
		}
	}
	if bridge.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(bridge); err != nil {
			return created, rollbackBridgeCreate(bridge, created, fmt.Errorf("set bridge %q up: %w", spec.Name, err))
		}
	}
	// setter 成功不等于最终状态正确，完成后再做一次只读全量复核。
	if err := CheckBridge(spec); err != nil {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf("verify bridge after ensure: %w", err))
	}
	return created, nil
}

// CheckBridge 只读验证 V1 Bridge 的完整契约，不做任何修复。
// CheckBridge verifies the complete V1 bridge contract without modifying it.
func CheckBridge(spec BridgeSpec) error {
	if err := validateBridgeSpec(spec); err != nil {
		return fmt.Errorf("invalid bridge configuration: %w", err)
	}
	bridge, err := requireBridge(spec.Name)
	if err != nil {
		return err
	}
	if bridge.Attrs().MTU != spec.MTU {
		return fmt.Errorf("check mismatch: bridge %q MTU is %d, want %d", spec.Name, bridge.Attrs().MTU, spec.MTU)
	}
	if err := checkBridgeTopology(bridge, spec.NetworkName); err != nil {
		return err
	}
	if bridge.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("check mismatch: bridge %q is down", spec.Name)
	}
	expected := netip.PrefixFrom(spec.Gateway, spec.Subnet.Bits())
	addresses, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("check bridge %q addresses: %w", spec.Name, err)
	}
	state, err := classifyIPv4Addresses(addresses, expected)
	if err != nil {
		return fmt.Errorf("check bridge %q addresses: %w", spec.Name, err)
	}
	if state != addressPresent {
		return fmt.Errorf("check mismatch: bridge %q IPv4 address is not exactly %s", spec.Name, expected)
	}
	return nil
}

// checkBridgeTopology 获取当前 namespace 的全部链接后验证 Bridge 端口集合。
func checkBridgeTopology(bridge *netlink.Bridge, networkName string) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("inspect bridge %q ports: %w", bridge.Attrs().Name, err)
	}
	return validateBridgeTopology(bridge, links, networkName)
}

// validateBridgeTopology 要求 Bridge 无 master，且每个 slave 都是带本网络
// 完整 alias 的 veth；物理口、其他网络或无 alias 端口均属冲突。
func validateBridgeTopology(bridge *netlink.Bridge, links []netlink.Link, networkName string) error {
	if bridge == nil || bridge.Attrs() == nil {
		return fmt.Errorf("bridge conflict: missing Linux bridge attributes")
	}
	bridgeAttrs := bridge.Attrs()
	if bridgeAttrs.Index <= 0 {
		return fmt.Errorf("bridge conflict: %q has invalid interface index %d", bridgeAttrs.Name, bridgeAttrs.Index)
	}
	if bridgeAttrs.MasterIndex != 0 {
		return fmt.Errorf(
			"bridge conflict: %q is itself attached to master index %d",
			bridgeAttrs.Name,
			bridgeAttrs.MasterIndex,
		)
	}

	for _, link := range links {
		if link == nil || link.Attrs() == nil {
			continue
		}
		attrs := link.Attrs()
		if attrs.MasterIndex != bridgeAttrs.Index {
			continue
		}
		if link.Type() != "veth" {
			return fmt.Errorf(
				"bridge conflict: port %q has type %q, want an owned cloudnet veth",
				attrs.Name,
				link.Type(),
			)
		}
		if !isNetworkVethAlias(attrs.Alias, networkName) {
			return fmt.Errorf(
				"bridge conflict: veth port %q ownership alias %q does not exactly match network %q",
				attrs.Name,
				attrs.Alias,
				networkName,
			)
		}
	}
	return nil
}

// isNetworkVethAlias 验证固定前缀、网络名和 64 位小写 hex digest，
// 而不是只做宽松的前缀匹配。
func isNetworkVethAlias(alias, networkName string) bool {
	digest, found := strings.CutPrefix(alias, HostVethAliasPrefix+networkName+":")
	if !found || len(digest) != 64 {
		return false
	}
	for index := range len(digest) {
		character := digest[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// rollbackBridgeCreate 只删除本次调用新建的 Bridge；复用的共享 Bridge 不删。
// 删除失败通过 errors.Join 与原始错误一并保留。
func rollbackBridgeCreate(link netlink.Link, created bool, original error) error {
	if !created || link == nil {
		return original
	}
	if err := netlink.LinkDel(link); err != nil {
		return errors.Join(original, fmt.Errorf("rollback bridge %q: %w", link.Attrs().Name, err))
	}
	return original
}
