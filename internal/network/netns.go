package network

import (
	"errors"
	"fmt"
	"net"
	"os"

	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// openNetNS 先 stat 提供明确路径错误，再由 CNI ns 包打开 namespace fd。
// 返回值由调用方 Close，避免长期持有 netns 引用。
func openNetNS(path string) (cnins.NetNS, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("namespace open failed for %q: %w", path, err)
	}
	netNS, err := cnins.GetNS(path)
	if err != nil {
		return nil, fmt.Errorf("namespace open failed for %q: %w", path, err)
	}
	return netNS, nil
}

// namespaceMissing 只把 ENOENT 视为正常缺失；权限/I/O 错误不能伪装成
// 幂等成功，否则 DEL 可能释放仍在使用的 allocation。
func namespaceMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, fmt.Errorf("inspect netns %q: %w", path, err)
}

// preflightContainerName 在创建 veth 前进入目标 namespace，确认 CNI_IFNAME
// 未被占用，避免后续 rename 碰撞现有接口。
func preflightContainerName(netNS cnins.NetNS, ifName string) error {
	return netNS.Do(func(_ cnins.NetNS) error {
		_, found, err := linkByName(ifName)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("target interface conflict: %q already exists in netns", ifName)
		}
		return nil
	})
}

// configureContainer 在目标 namespace 内完成验权、改名、MTU、IPv4、UP、
// 默认路由和 loopback 设置。NetNS.Do 会切换 namespace 并在返回时恢复。
func configureContainer(netNS cnins.NetNS, spec EndpointSpec) error {
	return netNS.Do(func(_ cnins.NetNS) error {
		// peer 已被移动，必须在目标 netns 内重新按临时名称查找。
		peer, found, err := linkByName(spec.PeerVethName)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("moved peer %q is missing in target netns", spec.PeerVethName)
		}
		if err := VerifyVethOwnership(peer, spec.Alias); err != nil {
			return err
		}
		if _, found, err := linkByName(spec.IfName); err != nil {
			return err
		} else if found {
			return fmt.Errorf("target interface conflict: %q already exists in netns", spec.IfName)
		}
		// 验权完成后才把临时 cp... 名称改为 runtime 请求的 eth0 等名称。
		if err := netlink.LinkSetName(peer, spec.IfName); err != nil {
			return fmt.Errorf("rename peer %q to %q: %w", spec.PeerVethName, spec.IfName, err)
		}
		containerLink, found, err := linkByName(spec.IfName)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("container interface %q missing after rename", spec.IfName)
		}
		if err := netlink.LinkSetMTU(containerLink, spec.MTU); err != nil {
			return fmt.Errorf("set container interface %q MTU: %w", spec.IfName, err)
		}
		// 只允许唯一预期 IPv4；额外地址不会被静默删除。
		addresses, err := netlink.AddrList(containerLink, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("list container interface %q addresses: %w", spec.IfName, err)
		}
		state, err := classifyIPv4Addresses(addresses, spec.Address)
		if err != nil {
			return err
		}
		if state == addressConflict {
			return fmt.Errorf("container interface %q has conflicting IPv4 addresses", spec.IfName)
		}
		if state == addressAbsent {
			if err := netlink.AddrAdd(containerLink, &netlink.Addr{IPNet: ipNetFromPrefix(spec.Address)}); err != nil {
				return fmt.Errorf("configure container address %s on %q: %w", spec.Address, spec.IfName, err)
			}
		}
		if err := netlink.LinkSetUp(containerLink); err != nil {
			return fmt.Errorf("set container interface %q up: %w", spec.IfName, err)
		}
		// V1 要求唯一 default route；已有任意默认路由都按冲突处理。
		if err := addDefaultRoute(containerLink, spec.Gateway); err != nil {
			return err
		}
		loopback, found, err := linkByName("lo")
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("loopback interface is missing in target netns")
		}
		if loopback.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(loopback); err != nil {
				return fmt.Errorf("set loopback up: %w", err)
			}
		}
		return nil
	})
}
