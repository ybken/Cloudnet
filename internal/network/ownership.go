package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

func verifyOwnedLink(attrs *netlink.LinkAttrs, expectedAlias string) error {
	if attrs == nil {
		return fmt.Errorf("veth ownership check: missing link attributes")
	}
	if expectedAlias == "" || attrs.Alias != expectedAlias {
		return fmt.Errorf(
			"veth ownership mismatch for %q: alias %q does not exactly match %q",
			attrs.Name,
			attrs.Alias,
			expectedAlias,
		)
	}
	return nil
}

// VerifyVethOwnership rejects non-veth links and requires an exact alias.
func VerifyVethOwnership(link netlink.Link, expectedAlias string) error {
	if link == nil {
		return fmt.Errorf("veth ownership check: link is nil")
	}
	if link.Type() != "veth" {
		return fmt.Errorf("veth ownership mismatch for %q: type is %q", link.Attrs().Name, link.Type())
	}
	return verifyOwnedLink(link.Attrs(), expectedAlias)
}
