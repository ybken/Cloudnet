package cni

import (
	"net"
	"net/netip"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
)

type ResultData struct {
	CNIVersion   string
	NetNS        string
	BridgeName   string
	BridgeMAC    string
	HostName     string
	HostMAC      string
	IfName       string
	ContainerMAC string
	MTU          int
	Address      netip.Prefix
	Gateway      netip.Addr
}

func BuildResult(data ResultData) *current.Result {
	interfaces := []*current.Interface{
		{Name: data.BridgeName, Mac: data.BridgeMAC, Mtu: data.MTU},
		{Name: data.HostName, Mac: data.HostMAC, Mtu: data.MTU},
		{Name: data.IfName, Mac: data.ContainerMAC, Mtu: data.MTU, Sandbox: data.NetNS},
	}

	address := net.IPNet{
		IP:   net.IP(append([]byte(nil), data.Address.Addr().AsSlice()...)),
		Mask: net.CIDRMask(data.Address.Bits(), 32),
	}
	gateway := net.IP(append([]byte(nil), data.Gateway.AsSlice()...))
	defaultRoute := &cnitypes.Route{
		Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		GW:  gateway,
	}

	return &current.Result{
		CNIVersion: data.CNIVersion,
		Interfaces: interfaces,
		IPs: []*current.IPConfig{{
			Interface: current.Int(2),
			Address:   address,
			Gateway:   gateway,
		}},
		Routes: []*cnitypes.Route{defaultRoute},
	}
}

func PrintResult(data ResultData) error {
	return cnitypes.PrintResult(BuildResult(data), data.CNIVersion)
}
