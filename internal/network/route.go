package network

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

func validateDefaultRoute(routes []netlink.Route, gateway netip.Addr, linkIndex int) error {
	defaults := make([]netlink.Route, 0, 1)
	for _, route := range routes {
		if isIPv4DefaultRoute(route) {
			defaults = append(defaults, route)
		}
	}
	if len(defaults) == 0 {
		return fmt.Errorf("default route is missing")
	}
	if len(defaults) > 1 {
		return fmt.Errorf("multiple IPv4 default routes found: %d", len(defaults))
	}
	route := defaults[0]
	actualGateway, err := addrFromIP(route.Gw)
	if err != nil || actualGateway != gateway || route.LinkIndex != linkIndex {
		return fmt.Errorf(
			"conflicting IPv4 default route: gateway=%v linkIndex=%d, want gateway=%s linkIndex=%d",
			route.Gw,
			route.LinkIndex,
			gateway,
			linkIndex,
		)
	}
	return nil
}

func isIPv4DefaultRoute(route netlink.Route) bool {
	if route.Dst == nil {
		return true
	}
	ones, bits := route.Dst.Mask.Size()
	return ones == 0 && bits == 32
}

func addDefaultRoute(link netlink.Link, gateway netip.Addr) error {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list existing IPv4 routes: %w", err)
	}
	for _, route := range routes {
		if isIPv4DefaultRoute(route) {
			return fmt.Errorf(
				"route configuration failed: existing default route gateway=%v linkIndex=%d",
				route.Gw,
				route.LinkIndex,
			)
		}
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        net.IP(append([]byte(nil), gateway.AsSlice()...)),
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("add default route via %s: %w", gateway, err)
	}
	return nil
}
