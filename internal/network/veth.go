package network

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// CreateEndpoint 创建带所有权 alias 的 veth pair，把 peer 移入容器 netns，
// 配置两端并将 host 端接入 Bridge。
//
// CreateEndpoint creates, moves, configures, and attaches one owned veth pair.
// Any failure after LinkAdd removes that exact newly-created pair.
func CreateEndpoint(spec EndpointSpec) error {
	if err := validateEndpointSpec(spec); err != nil {
		return fmt.Errorf("invalid endpoint configuration: %w", err)
	}
	// 先验证前置资源与名称冲突，尽量让失败发生在 LinkAdd 之前。
	bridge, err := requireBridge(spec.BridgeName)
	if err != nil {
		return err
	}
	netNS, err := openNetNS(spec.NetNSPath)
	if err != nil {
		return err
	}
	defer netNS.Close()
	if err := preflightContainerName(netNS, spec.IfName); err != nil {
		return fmt.Errorf("preflight target netns: %w", err)
	}
	for _, name := range []string{spec.HostVethName, spec.PeerVethName} {
		if _, found, err := linkByName(name); err != nil {
			return err
		} else if found {
			return fmt.Errorf("veth create failed: host namespace link %q already exists", name)
		}
	}

	// LinkAdd 是本函数开始产生内核副作用的边界。
	attrs := netlink.NewLinkAttrs()
	attrs.Name = spec.HostVethName
	attrs.MTU = spec.MTU
	pair := &netlink.Veth{LinkAttrs: attrs, PeerName: spec.PeerVethName}
	if err := netlink.LinkAdd(pair); err != nil {
		return fmt.Errorf("veth create failed for %q/%q: %w", spec.HostVethName, spec.PeerVethName, err)
	}
	// fail 统一清理本次 pair；删除任一端会让内核同步删除另一端。
	// LinkDel 报错但随后确认链接已消失时，仍视为清理完成。
	var createdHost netlink.Link
	fail := func(original error) error {
		target := createdHost
		if target == nil {
			var lookupErr error
			var targetFound bool
			target, targetFound, lookupErr = linkByName(spec.HostVethName)
			if lookupErr != nil {
				return errors.Join(original, fmt.Errorf("rollback look up veth %q: %w", spec.HostVethName, lookupErr))
			}
			if !targetFound {
				return original
			}
		}
		if err := netlink.LinkDel(target); err != nil {
			_, stillPresent, lookupErr := linkByName(spec.HostVethName)
			if lookupErr != nil || stillPresent {
				return errors.Join(original, fmt.Errorf("rollback veth %q: %w", spec.HostVethName, err))
			}
		}
		return original
	}

	host, found, err := linkByName(spec.HostVethName)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("host veth %q missing after create", spec.HostVethName)
		}
		return fail(err)
	}
	createdHost = host
	peer, found, err := linkByName(spec.PeerVethName)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("peer veth %q missing after create", spec.PeerVethName)
		}
		return fail(err)
	}
	// 两端显式设置 MTU 和 alias；不能假设创建参数或 namespace 移动会同步值。
	// 相同 alias 使任一端都能独立证明 endpoint 所有权。
	if err := netlink.LinkSetMTU(host, spec.MTU); err != nil {
		return fail(fmt.Errorf("set host veth %q MTU: %w", spec.HostVethName, err))
	}
	if err := netlink.LinkSetMTU(peer, spec.MTU); err != nil {
		return fail(fmt.Errorf("set peer veth %q MTU: %w", spec.PeerVethName, err))
	}
	if err := netlink.LinkSetAlias(host, spec.Alias); err != nil {
		return fail(fmt.Errorf("set host veth %q ownership alias: %w", spec.HostVethName, err))
	}
	if err := netlink.LinkSetAlias(peer, spec.Alias); err != nil {
		return fail(fmt.Errorf("set peer veth %q ownership alias: %w", spec.PeerVethName, err))
	}
	// netlink setter 不更新传入的 LinkAttrs 快照；按名称重新读取后，
	// 才能确认内核确实保存 alias，陈旧快照不能用于破坏性操作。
	//
	// Netlink setters do not update the LinkAttrs snapshot passed to them.
	// Reload both ends so ownership checks observe what the kernel committed.
	host, err = reloadOwnedVeth(spec.HostVethName, spec.Alias)
	if err != nil {
		return fail(err)
	}
	createdHost = host
	peer, err = reloadOwnedVeth(spec.PeerVethName, spec.Alias)
	if err != nil {
		return fail(err)
	}
	// 移动后 peer 从当前 namespace 消失，只能通过 netNS.Do 再访问。
	if err := netlink.LinkSetNsFd(peer, int(netNS.Fd())); err != nil {
		return fail(fmt.Errorf("move peer %q to netns %q: %w", spec.PeerVethName, spec.NetNSPath, err))
	}
	// 移动 peer 会改变 pair 状态，所以挂 Bridge 前再次刷新 host 快照。
	//
	// Moving the peer changes link state. Refresh the host again instead of
	// relying on the pre-move snapshot for the destructive work that follows.
	host, err = reloadOwnedVeth(spec.HostVethName, spec.Alias)
	if err != nil {
		return fail(err)
	}
	createdHost = host
	if err := netlink.LinkSetMaster(host, bridge); err != nil {
		return fail(fmt.Errorf("attach owned host veth %q to bridge %q: %w", spec.HostVethName, spec.BridgeName, err))
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return fail(fmt.Errorf("set host veth %q up: %w", spec.HostVethName, err))
	}
	if err := configureContainer(netNS, spec); err != nil {
		return fail(fmt.Errorf("configure container endpoint: %w", err))
	}
	if err := checkHostEndpoint(spec, bridge); err != nil {
		return fail(fmt.Errorf("verify host endpoint after create: %w", err))
	}
	return nil
}

// linkLookupFunc 让刷新/验权逻辑可在无特权测试中注入 fake lookup。
type linkLookupFunc func(string) (netlink.Link, bool, error)

// reloadOwnedVeth 从内核重新读取并验证指定 veth 的所有权。
func reloadOwnedVeth(name, expectedAlias string) (netlink.Link, error) {
	return reloadOwnedVethWith(name, expectedAlias, linkByName)
}

func reloadOwnedVethWith(
	name string,
	expectedAlias string,
	lookup linkLookupFunc,
) (netlink.Link, error) {
	link, found, err := lookup(name)
	if err != nil {
		return nil, fmt.Errorf("reload veth %q after kernel update: %w", name, err)
	}
	if !found {
		return nil, fmt.Errorf("veth %q is missing after kernel update", name)
	}
	if err := VerifyVethOwnership(link, expectedAlias); err != nil {
		return nil, err
	}
	return link, nil
}
