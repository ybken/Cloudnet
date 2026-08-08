package network

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestClassifyIPv4Addresses(t *testing.T) {
	expected := netip.MustParsePrefix("10.77.0.1/24")
	addr := func(prefix string) netlink.Addr {
		return netlink.Addr{IPNet: ipNetFromPrefix(netip.MustParsePrefix(prefix))}
	}

	tests := []struct {
		name      string
		addresses []netlink.Addr
		want      addressState
	}{
		{name: "absent", want: addressAbsent},
		{name: "present", addresses: []netlink.Addr{addr("10.77.0.1/24")}, want: addressPresent},
		{name: "wrong gateway", addresses: []netlink.Addr{addr("10.77.0.2/24")}, want: addressConflict},
		{name: "wrong prefix", addresses: []netlink.Addr{addr("10.77.0.1/25")}, want: addressConflict},
		{name: "expected plus other", addresses: []netlink.Addr{addr("10.77.0.1/24"), addr("10.88.0.1/24")}, want: addressConflict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyIPv4Addresses(tc.addresses, expected)
			if err != nil {
				t.Fatalf("classifyIPv4Addresses() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("classifyIPv4Addresses() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateDefaultRoute(t *testing.T) {
	gw := net.ParseIP("10.77.0.1")
	exact := netlink.Route{LinkIndex: 7, Gw: gw, Dst: nil}
	wrongGateway := netlink.Route{LinkIndex: 7, Gw: net.ParseIP("10.77.0.2"), Dst: nil}
	nonDefault := netlink.Route{LinkIndex: 7, Gw: gw, Dst: ipNetFromPrefix(netip.MustParsePrefix("10.88.0.0/24"))}

	tests := []struct {
		name    string
		routes  []netlink.Route
		wantErr string
	}{
		{name: "exact", routes: []netlink.Route{exact}},
		{name: "non-default ignored", routes: []netlink.Route{nonDefault, exact}},
		{name: "missing", wantErr: "missing"},
		{name: "wrong gateway", routes: []netlink.Route{wrongGateway}, wantErr: "conflicting"},
		{name: "duplicate default", routes: []netlink.Route{exact, exact}, wantErr: "multiple"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDefaultRoute(tc.routes, netip.MustParseAddr("10.77.0.1"), 7)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateDefaultRoute() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateDefaultRoute() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
