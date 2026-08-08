package network

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	minimumIPv4MTU = 576
	maximumMTU     = 65535
	maximumAlias   = 255
)

var ownershipNetworkNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

// BridgeSpec is the complete V1 contract for the shared Linux bridge.
type BridgeSpec struct {
	NetworkName string
	Name        string
	Subnet      netip.Prefix
	Gateway     netip.Addr
	MTU         int
}

// EndpointSpec contains only kernel-facing endpoint data. Naming and
// persistence remain the caller's responsibility.
type EndpointSpec struct {
	BridgeName   string
	NetNSPath    string
	IfName       string
	HostVethName string
	PeerVethName string
	Alias        string
	Address      netip.Prefix
	Gateway      netip.Addr
	MTU          int
}

// DeleteSpec is deliberately smaller than EndpointSpec because CNI DEL may be
// called without a netns or after endpoint state has been lost.
type DeleteSpec struct {
	NetNSPath    string
	IfName       string
	HostVethName string
	Alias        string
}

func validateBridgeSpec(spec BridgeSpec) error {
	if !ownershipNetworkNamePattern.MatchString(spec.NetworkName) {
		return fmt.Errorf("network name %q is not safe for ownership matching", spec.NetworkName)
	}
	if err := validateInterfaceName("bridge name", spec.Name); err != nil {
		return err
	}
	if !spec.Subnet.IsValid() || !spec.Subnet.Addr().Is4() {
		return fmt.Errorf("bridge subnet must be a valid IPv4 prefix")
	}
	if spec.Subnet != spec.Subnet.Masked() {
		return fmt.Errorf("bridge subnet %s must be masked", spec.Subnet)
	}
	if !spec.Gateway.IsValid() || !spec.Gateway.Is4() {
		return fmt.Errorf("bridge gateway must be a valid IPv4 address")
	}
	if !spec.Subnet.Contains(spec.Gateway) {
		return fmt.Errorf("bridge gateway %s is outside subnet %s", spec.Gateway, spec.Subnet)
	}
	if spec.Gateway == spec.Subnet.Addr() {
		return fmt.Errorf("bridge gateway cannot be the subnet network address")
	}
	if isIPv4Broadcast(spec.Gateway, spec.Subnet) {
		return fmt.Errorf("bridge gateway cannot be the subnet broadcast address")
	}
	return validateMTU(spec.MTU)
}

func validateEndpointSpec(spec EndpointSpec) error {
	if err := validateInterfaceName("bridge name", spec.BridgeName); err != nil {
		return err
	}
	if spec.NetNSPath == "" {
		return fmt.Errorf("netns path is required")
	}
	if !filepath.IsAbs(spec.NetNSPath) || strings.ContainsRune(spec.NetNSPath, 0) {
		return fmt.Errorf("netns path must be an absolute path without NUL")
	}
	if err := validateInterfaceName("container interface name", spec.IfName); err != nil {
		return err
	}
	if err := validateInterfaceName("host veth name", spec.HostVethName); err != nil {
		return err
	}
	if err := validateInterfaceName("peer veth name", spec.PeerVethName); err != nil {
		return err
	}
	if spec.HostVethName == spec.PeerVethName ||
		spec.HostVethName == spec.IfName ||
		spec.PeerVethName == spec.IfName {
		return fmt.Errorf("host, peer, and container interface names must be distinct")
	}
	if spec.Alias == "" || len(spec.Alias) > maximumAlias || strings.ContainsRune(spec.Alias, 0) {
		return fmt.Errorf("veth alias must contain 1..%d bytes without NUL", maximumAlias)
	}
	if !spec.Address.IsValid() || !spec.Address.Addr().Is4() {
		return fmt.Errorf("endpoint address must be a valid IPv4 prefix")
	}
	if !spec.Gateway.IsValid() || !spec.Gateway.Is4() {
		return fmt.Errorf("endpoint gateway must be a valid IPv4 address")
	}
	network := spec.Address.Masked()
	if !network.Contains(spec.Gateway) {
		return fmt.Errorf("endpoint gateway %s is outside address prefix %s", spec.Gateway, spec.Address)
	}
	if spec.Address.Addr() == network.Addr() {
		return fmt.Errorf("endpoint address cannot be the subnet network address")
	}
	if isIPv4Broadcast(spec.Address.Addr(), network) {
		return fmt.Errorf("endpoint address cannot be the subnet broadcast address")
	}
	if spec.Address.Addr() == spec.Gateway {
		return fmt.Errorf("endpoint address cannot equal gateway")
	}
	return validateMTU(spec.MTU)
}

func validateDeleteSpec(spec DeleteSpec) error {
	if err := validateInterfaceName("host veth name", spec.HostVethName); err != nil {
		return err
	}
	if err := validateInterfaceName("container interface name", spec.IfName); err != nil {
		return err
	}
	if spec.Alias == "" || len(spec.Alias) > maximumAlias || strings.ContainsRune(spec.Alias, 0) {
		return fmt.Errorf("veth alias must contain 1..%d bytes without NUL", maximumAlias)
	}
	if spec.NetNSPath != "" &&
		(!filepath.IsAbs(spec.NetNSPath) || strings.ContainsRune(spec.NetNSPath, 0)) {
		return fmt.Errorf("netns path must be absolute when present")
	}
	return nil
}

func validateInterfaceName(label, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(name) > MaxInterfaceNameLength {
		return fmt.Errorf("%s %q exceeds Linux's %d-byte limit", label, name, MaxInterfaceNameLength)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/:\x00") {
		return fmt.Errorf("%s %q is not a safe Linux interface name", label, name)
	}
	return nil
}

func validateMTU(mtu int) error {
	if mtu < minimumIPv4MTU || mtu > maximumMTU {
		return fmt.Errorf("MTU %d is outside supported range %d..%d", mtu, minimumIPv4MTU, maximumMTU)
	}
	return nil
}

func isIPv4Broadcast(addr netip.Addr, subnet netip.Prefix) bool {
	if !addr.Is4() || !subnet.IsValid() || !subnet.Addr().Is4() {
		return false
	}
	bits := subnet.Bits()
	if bits < 0 || bits > 32 {
		return false
	}
	a := addr.As4()
	n := subnet.Masked().Addr().As4()
	address := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	network := uint32(n[0])<<24 | uint32(n[1])<<16 | uint32(n[2])<<8 | uint32(n[3])
	var hostMask uint32
	if bits == 0 {
		hostMask = ^uint32(0)
	} else {
		hostMask = ^uint32(0) >> bits
	}
	return address == network|hostMask
}

func ipNetFromPrefix(prefix netip.Prefix) *net.IPNet {
	addr := prefix.Addr().Unmap()
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	return &net.IPNet{
		IP:   net.IP(append([]byte(nil), addr.AsSlice()...)),
		Mask: net.CIDRMask(prefix.Bits(), bits),
	}
}

func prefixFromIPNet(network *net.IPNet) (netip.Prefix, error) {
	if network == nil {
		return netip.Prefix{}, fmt.Errorf("nil IP network")
	}
	ones, bits := network.Mask.Size()
	if ones < 0 || bits != 32 {
		return netip.Prefix{}, fmt.Errorf("IP network %v is not IPv4 CIDR", network)
	}
	addr, ok := netip.AddrFromSlice(network.IP)
	if !ok || !addr.Unmap().Is4() {
		return netip.Prefix{}, fmt.Errorf("IP network %v has invalid IPv4 address", network)
	}
	return netip.PrefixFrom(addr.Unmap(), ones), nil
}

func addrFromIP(ip net.IP) (netip.Addr, error) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid IP %v", ip)
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("IP %v is not IPv4", ip)
	}
	return addr, nil
}
