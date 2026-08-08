package network

import (
	"net/netip"
	"strings"
	"testing"
)

func TestValidateBridgeSpec(t *testing.T) {
	valid := BridgeSpec{
		NetworkName: "cloudnet-v1",
		Name:        "cni-br0",
		Subnet:      netip.MustParsePrefix("10.77.0.0/24"),
		Gateway:     netip.MustParseAddr("10.77.0.1"),
		MTU:         1500,
	}

	tests := []struct {
		name    string
		mutate  func(*BridgeSpec)
		wantErr string
	}{
		{name: "empty network name", mutate: func(s *BridgeSpec) { s.NetworkName = "" }, wantErr: "network name"},
		{name: "unsafe network name", mutate: func(s *BridgeSpec) { s.NetworkName = "other:network" }, wantErr: "network name"},
		{name: "valid"},
		{name: "empty name", mutate: func(s *BridgeSpec) { s.Name = "" }, wantErr: "bridge name"},
		{name: "long name", mutate: func(s *BridgeSpec) { s.Name = strings.Repeat("b", 16) }, wantErr: "15"},
		{name: "ipv6 subnet", mutate: func(s *BridgeSpec) { s.Subnet = netip.MustParsePrefix("fd00::/64") }, wantErr: "IPv4"},
		{name: "unmasked subnet", mutate: func(s *BridgeSpec) { s.Subnet = netip.MustParsePrefix("10.77.0.9/24") }, wantErr: "masked"},
		{name: "gateway outside subnet", mutate: func(s *BridgeSpec) { s.Gateway = netip.MustParseAddr("10.78.0.1") }, wantErr: "gateway"},
		{name: "gateway is network", mutate: func(s *BridgeSpec) { s.Gateway = netip.MustParseAddr("10.77.0.0") }, wantErr: "network address"},
		{name: "gateway is broadcast", mutate: func(s *BridgeSpec) { s.Gateway = netip.MustParseAddr("10.77.0.255") }, wantErr: "broadcast"},
		{name: "bad mtu", mutate: func(s *BridgeSpec) { s.MTU = 0 }, wantErr: "MTU"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			err := validateBridgeSpec(spec)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateBridgeSpec() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateBridgeSpec() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateEndpointSpec(t *testing.T) {
	valid := EndpointSpec{
		BridgeName:   "cni-br0",
		NetNSPath:    "/run/netns/cloudnet-test-a",
		IfName:       "eth0",
		HostVethName: "cn0123456789ab",
		PeerVethName: "cp0123456789ab",
		Alias:        "cloudnet:v1:0123456789abcdef",
		Address:      netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:      netip.MustParseAddr("10.77.0.1"),
		MTU:          1500,
	}

	tests := []struct {
		name    string
		mutate  func(*EndpointSpec)
		wantErr string
	}{
		{name: "valid"},
		{name: "empty netns", mutate: func(s *EndpointSpec) { s.NetNSPath = "" }, wantErr: "netns"},
		{name: "invalid ifname", mutate: func(s *EndpointSpec) { s.IfName = "bad/name" }, wantErr: "interface name"},
		{name: "long host name", mutate: func(s *EndpointSpec) { s.HostVethName = strings.Repeat("v", 16) }, wantErr: "host veth"},
		{name: "same pair names", mutate: func(s *EndpointSpec) { s.PeerVethName = s.HostVethName }, wantErr: "distinct"},
		{name: "empty alias", mutate: func(s *EndpointSpec) { s.Alias = "" }, wantErr: "alias"},
		{name: "ipv6 address", mutate: func(s *EndpointSpec) { s.Address = netip.MustParsePrefix("fd00::10/64") }, wantErr: "IPv4"},
		{name: "network address", mutate: func(s *EndpointSpec) { s.Address = netip.MustParsePrefix("10.77.0.0/24") }, wantErr: "network address"},
		{name: "gateway outside prefix", mutate: func(s *EndpointSpec) { s.Gateway = netip.MustParseAddr("10.78.0.1") }, wantErr: "gateway"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			err := validateEndpointSpec(spec)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateEndpointSpec() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateEndpointSpec() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestPrefixRoundTrip(t *testing.T) {
	want := netip.MustParsePrefix("10.77.0.10/24")
	got, err := prefixFromIPNet(ipNetFromPrefix(want))
	if err != nil {
		t.Fatalf("prefixFromIPNet() error = %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %s, want %s", got, want)
	}
}
