package config

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxNetworkNameLength = 63
	maxContainerIDLength = 256
	maxIfNameLength      = 15
	maxNetNSLength       = 4096
)

var (
	networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
	containerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	ifNamePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

func (c *NetConf) Validate() error {
	if c == nil {
		return invalidf("configuration is nil")
	}
	if c.CNIVersion != "1.0.0" && c.CNIVersion != "1.1.0" {
		return invalidf("unsupported cniVersion %q (supported: 1.0.0, 1.1.0)", c.CNIVersion)
	}
	if err := ValidateNetworkName(c.Name); err != nil {
		return invalidf("network name: %v", err)
	}
	if c.Name != NetworkName {
		return invalidf("network name %q conflicts with fixed V1 name %q", c.Name, NetworkName)
	}
	if c.Type != PluginType {
		return invalidf("type is %q, want %q", c.Type, PluginType)
	}
	if c.Bridge != BridgeName {
		return invalidf("bridge is %q, want %q", c.Bridge, BridgeName)
	}
	if c.MTU != MTU {
		return invalidf("mtu is %d, want %d", c.MTU, MTU)
	}
	if err := c.IPAM.validate(); err != nil {
		return invalidf("ipam: %v", err)
	}
	level, err := normalizeLogLevel(c.Log.Level)
	if err != nil {
		return invalidf("log: %v", err)
	}
	c.Log.Level = level
	return nil
}

func (c *IPAMConfig) validate() error {
	prefix, err := netip.ParsePrefix(c.Subnet)
	if err != nil {
		return fmt.Errorf("subnet %q: %w", c.Subnet, err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("subnet %q is not IPv4", c.Subnet)
	}
	if prefix != prefix.Masked() {
		return fmt.Errorf("subnet %q has host bits set", c.Subnet)
	}
	if prefix.Bits() > 30 {
		return fmt.Errorf("subnet %q has no usable host range", c.Subnet)
	}

	gateway, err := parseIPv4("gateway", c.Gateway)
	if err != nil {
		return err
	}
	start, err := parseIPv4("rangeStart", c.RangeStart)
	if err != nil {
		return err
	}
	end, err := parseIPv4("rangeEnd", c.RangeEnd)
	if err != nil {
		return err
	}
	for field, addr := range map[string]netip.Addr{"gateway": gateway, "rangeStart": start, "rangeEnd": end} {
		if !prefix.Contains(addr) {
			return fmt.Errorf("%s %s is outside subnet %s", field, addr, prefix)
		}
	}

	network := prefix.Addr()
	broadcast := lastIPv4(prefix)
	if gateway == network || gateway == broadcast {
		return fmt.Errorf("gateway %s is a reserved network or broadcast address", gateway)
	}
	if start.Compare(end) > 0 {
		return fmt.Errorf("rangeStart %s is after rangeEnd %s", start, end)
	}
	for field, addr := range map[string]netip.Addr{"rangeStart": start, "rangeEnd": end} {
		if addr == network || addr == broadcast {
			return fmt.Errorf("%s %s is a reserved network or broadcast address", field, addr)
		}
	}
	if start.Compare(gateway) <= 0 && end.Compare(gateway) >= 0 {
		return fmt.Errorf("allocation range %s-%s includes gateway %s", start, end, gateway)
	}

	if prefix.String() != Subnet {
		return fmt.Errorf("subnet is %q, want fixed V1 subnet %q", prefix, Subnet)
	}
	if gateway.String() != Gateway {
		return fmt.Errorf("gateway is %q, want fixed V1 gateway %q", gateway, Gateway)
	}
	if start.String() != RangeStart {
		return fmt.Errorf("rangeStart is %q, want fixed V1 rangeStart %q", start, RangeStart)
	}
	if end.String() != RangeEnd {
		return fmt.Errorf("rangeEnd is %q, want fixed V1 rangeEnd %q", end, RangeEnd)
	}

	c.SubnetPrefix = prefix
	c.GatewayAddr = gateway
	c.RangeStartAddr = start
	c.RangeEndAddr = end
	return nil
}

func parseIPv4(field, value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s %q: %w", field, value, err)
	}
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("%s %q is not IPv4", field, value)
	}
	return addr, nil
}

func lastIPv4(prefix netip.Prefix) netip.Addr {
	octets := prefix.Addr().As4()
	value := binary.BigEndian.Uint32(octets[:])
	hostBits := 32 - prefix.Bits()
	value |= uint32(1<<hostBits) - 1
	var last [4]byte
	binary.BigEndian.PutUint32(last[:], value)
	return netip.AddrFrom4(last)
}

// ValidateNetworkName constrains state-directory components to a small,
// portable alphabet and rejects path traversal by construction.
func ValidateNetworkName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(name) > maxNetworkNameLength {
		return fmt.Errorf("length %d exceeds %d", len(name), maxNetworkNameLength)
	}
	if name == "." || name == ".." || !networkNamePattern.MatchString(name) {
		return fmt.Errorf("%q contains unsafe characters", name)
	}
	return nil
}

func IsSafeNetworkName(name string) bool {
	return ValidateNetworkName(name) == nil
}

func ValidateIfName(ifName string) error {
	if ifName == "" {
		return fmt.Errorf("ifname must not be empty")
	}
	if len(ifName) > maxIfNameLength {
		return fmt.Errorf("ifname length %d exceeds Linux limit %d", len(ifName), maxIfNameLength)
	}
	if !ifNamePattern.MatchString(ifName) {
		return fmt.Errorf("ifname %q contains unsafe characters", ifName)
	}
	return nil
}

// ValidateRuntime checks CNI_CONTAINERID, CNI_NETNS, and CNI_IFNAME. DEL is
// represented by requireNetNS=false, allowing an empty netns after runtime
// teardown while still validating it when supplied.
func ValidateRuntime(containerID, netns, ifName string, requireNetNS bool) error {
	if containerID == "" {
		return invalidf("containerID must not be empty")
	}
	if len(containerID) > maxContainerIDLength || !containerIDPattern.MatchString(containerID) {
		return invalidf("containerID is malformed or exceeds %d bytes", maxContainerIDLength)
	}
	if err := ValidateIfName(ifName); err != nil {
		return invalidf("%v", err)
	}
	if netns == "" {
		if requireNetNS {
			return invalidf("netns must not be empty")
		}
		return nil
	}
	if len(netns) > maxNetNSLength || strings.ContainsRune(netns, '\x00') {
		return invalidf("netns is malformed or exceeds %d bytes", maxNetNSLength)
	}
	if !filepath.IsAbs(netns) {
		return invalidf("netns path %q is not absolute", netns)
	}
	if filepath.Clean(netns) != netns {
		return invalidf("netns path %q is not clean", netns)
	}
	return nil
}
