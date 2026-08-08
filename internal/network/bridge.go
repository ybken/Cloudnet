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

// EnsureBridge creates the shared bridge when absent or verifies and completes
// a compatible existing bridge. It never changes a conflicting MTU or address.
func EnsureBridge(spec BridgeSpec) (created bool, err error) {
	if err := validateBridgeSpec(spec); err != nil {
		return false, fmt.Errorf("invalid bridge configuration: %w", err)
	}

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
	if err := checkBridgeTopology(bridge, spec.NetworkName); err != nil {
		return created, rollbackBridgeCreate(bridge, created, err)
	}

	expected := netip.PrefixFrom(spec.Gateway, spec.Subnet.Bits())
	addresses, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf("list bridge %q addresses: %w", spec.Name, err))
	}
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
	if err := CheckBridge(spec); err != nil {
		return created, rollbackBridgeCreate(bridge, created, fmt.Errorf("verify bridge after ensure: %w", err))
	}
	return created, nil
}

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

func checkBridgeTopology(bridge *netlink.Bridge, networkName string) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("inspect bridge %q ports: %w", bridge.Attrs().Name, err)
	}
	return validateBridgeTopology(bridge, links, networkName)
}

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

func rollbackBridgeCreate(link netlink.Link, created bool, original error) error {
	if !created || link == nil {
		return original
	}
	if err := netlink.LinkDel(link); err != nil {
		return errors.Join(original, fmt.Errorf("rollback bridge %q: %w", link.Attrs().Name, err))
	}
	return original
}
