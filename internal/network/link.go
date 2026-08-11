package network

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// linkByName 把 netlink 的未找到异常规范化为 (nil, false, nil)，
// 使调用方能区分幂等缺失和真正的内核查询失败。
func linkByName(name string) (netlink.Link, bool, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		return link, true, nil
	}
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("look up link %q: %w", name, err)
}

// requireBridge 同时检查 Go 具体类型与内核 type 字符串，避免把同名普通链接
// 当作 Bridge 继续执行 master 或地址操作。
func requireBridge(name string) (*netlink.Bridge, error) {
	link, found, err := linkByName(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("bridge %q is missing", name)
	}
	bridge, ok := link.(*netlink.Bridge)
	if !ok || link.Type() != "bridge" {
		return nil, fmt.Errorf("bridge conflict: link %q has type %q, want Linux bridge", name, link.Type())
	}
	return bridge, nil
}
