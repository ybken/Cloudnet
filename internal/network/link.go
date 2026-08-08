package network

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

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
