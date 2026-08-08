package network

import (
	"fmt"
	"net"

	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// CheckEndpoint verifies both ends of a veth pair and the default route.
// Call CheckBridge separately to verify the shared bridge address and MTU.
func CheckEndpoint(spec EndpointSpec) error {
	if err := validateEndpointSpec(spec); err != nil {
		return fmt.Errorf("invalid endpoint configuration: %w", err)
	}
	bridge, err := requireBridge(spec.BridgeName)
	if err != nil {
		return err
	}
	if err := checkHostEndpoint(spec, bridge); err != nil {
		return err
	}
	netNS, err := openNetNS(spec.NetNSPath)
	if err != nil {
		return err
	}
	defer netNS.Close()
	if err := netNS.Do(func(_ cnins.NetNS) error {
		containerLink, found, err := linkByName(spec.IfName)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("check mismatch: container interface %q is missing", spec.IfName)
		}
		if err := VerifyVethOwnership(containerLink, spec.Alias); err != nil {
			return fmt.Errorf("check mismatch: %w", err)
		}
		if containerLink.Attrs().MTU != spec.MTU {
			return fmt.Errorf(
				"check mismatch: container interface %q MTU is %d, want %d",
				spec.IfName,
				containerLink.Attrs().MTU,
				spec.MTU,
			)
		}
		if containerLink.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("check mismatch: container interface %q is down", spec.IfName)
		}
		addresses, err := netlink.AddrList(containerLink, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("check container interface %q addresses: %w", spec.IfName, err)
		}
		state, err := classifyIPv4Addresses(addresses, spec.Address)
		if err != nil {
			return err
		}
		if state != addressPresent {
			return fmt.Errorf(
				"check mismatch: container interface %q IPv4 address is not exactly %s",
				spec.IfName,
				spec.Address,
			)
		}
		loopback, found, err := linkByName("lo")
		if err != nil {
			return err
		}
		if !found || loopback.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("check mismatch: loopback interface is missing or down")
		}
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("check container routes: %w", err)
		}
		if err := validateDefaultRoute(routes, spec.Gateway, containerLink.Attrs().Index); err != nil {
			return fmt.Errorf("check mismatch: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func checkHostEndpoint(spec EndpointSpec, bridge *netlink.Bridge) error {
	host, found, err := linkByName(spec.HostVethName)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("check mismatch: host veth %q is missing", spec.HostVethName)
	}
	if err := VerifyVethOwnership(host, spec.Alias); err != nil {
		return fmt.Errorf("check mismatch: %w", err)
	}
	if host.Attrs().MTU != spec.MTU {
		return fmt.Errorf("check mismatch: host veth %q MTU is %d, want %d", spec.HostVethName, host.Attrs().MTU, spec.MTU)
	}
	if host.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("check mismatch: host veth %q is down", spec.HostVethName)
	}
	if host.Attrs().MasterIndex != bridge.Attrs().Index {
		return fmt.Errorf(
			"check mismatch: host veth %q master index is %d, want bridge %q index %d",
			spec.HostVethName,
			host.Attrs().MasterIndex,
			spec.BridgeName,
			bridge.Attrs().Index,
		)
	}
	addresses, err := netlink.AddrList(host, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("check host veth %q addresses: %w", spec.HostVethName, err)
	}
	if len(addresses) != 0 {
		return fmt.Errorf("check mismatch: host veth %q must not have an IPv4 address", spec.HostVethName)
	}
	return nil
}
