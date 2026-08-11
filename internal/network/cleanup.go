package network

import (
	"errors"
	"fmt"
	"os"

	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// DeleteEndpoint 对链接或 netns 缺失保持幂等；任何现存链接都必须通过
// veth 类型与精确 alias 双重验权后才会删除。
//
// DeleteEndpoint is idempotent for missing links and a missing netns. A link
// that exists is deleted only after its type and alias prove exact ownership.
func DeleteEndpoint(spec DeleteSpec) error {
	if err := validateDeleteSpec(spec); err != nil {
		return fmt.Errorf("invalid endpoint delete configuration: %w", err)
	}
	// 优先删 host 端：删除任一端会清掉整个 pair，且不依赖 netns 路径存在。
	// 这也是 runtime 已先删除 namespace 时最可靠的恢复路径。
	host, found, err := linkByName(spec.HostVethName)
	if err != nil {
		return err
	}
	if found {
		if err := VerifyVethOwnership(host, spec.Alias); err != nil {
			return err
		}
		if err := netlink.LinkDel(host); err != nil {
			return fmt.Errorf("delete owned host veth %q: %w", spec.HostVethName, err)
		}
		return nil
	}
	// host 不存在且未提供 netns 时，能做的安全清理已经完成。
	if spec.NetNSPath == "" {
		return nil
	}
	missing, err := namespaceMissing(spec.NetNSPath)
	if err != nil {
		return err
	}
	if missing {
		return nil
	}
	// stat 与 GetNS 间可能发生删除；这个竞态对 DEL 仍是成功状态。
	netNS, err := openNetNS(spec.NetNSPath)
	if err != nil {
		// The namespace may disappear between stat and GetNS during DEL.
		if _, statErr := os.Stat(spec.NetNSPath); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer netNS.Close()
	// 仅在 host 缺失时回退到容器端，并继续要求完整 alias；
	// 同名 eth0 本身绝不是删除许可。
	return netNS.Do(func(_ cnins.NetNS) error {
		containerLink, found, err := linkByName(spec.IfName)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := VerifyVethOwnership(containerLink, spec.Alias); err != nil {
			return err
		}
		if err := netlink.LinkDel(containerLink); err != nil {
			return fmt.Errorf("delete owned container veth %q: %w", spec.IfName, err)
		}
		return nil
	})
}
