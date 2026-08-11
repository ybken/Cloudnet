package cni

import (
	"net"
	"net/netip"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
)

// ResultData 是编排层与 CNI wire format 之间的中间模型。
// 使用 netip 保存地址可避免在核心逻辑中混用可变的 net.IP slice；
// BuildResult 才把它转换成 CNI types/100 所需的 net.IP/net.IPNet。
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

// BuildResult 构造 CNI 1.0/1.1 的 current Result。
// Interfaces 的顺序是协议的一部分：下面 IPConfig.Interface 固定引用索引 2，
// 即容器命名空间中的接口，而不是 Bridge 或 host veth。
func BuildResult(data ResultData) *current.Result {
	interfaces := []*current.Interface{
		{Name: data.BridgeName, Mac: data.BridgeMAC, Mtu: data.MTU},
		{Name: data.HostName, Mac: data.HostMAC, Mtu: data.MTU},
		{Name: data.IfName, Mac: data.ContainerMAC, Mtu: data.MTU, Sandbox: data.NetNS},
	}

	// append 到新 slice，避免 net.IP 与 netip.Addr 的底层数据产生意外共享。
	address := net.IPNet{
		IP:   net.IP(append([]byte(nil), data.Address.Addr().AsSlice()...)),
		Mask: net.CIDRMask(data.Address.Bits(), 32),
	}
	gateway := net.IP(append([]byte(nil), data.Gateway.AsSlice()...))
	// 明确构造 IPv4 0.0.0.0/0，避免不同消费者对空 Dst 有不同解释。
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

// PrintResult 让 CNI 库按调用者请求的版本编码 Result，而不是手写 JSON。
func PrintResult(data ResultData) error {
	return cnitypes.PrintResult(BuildResult(data), data.CNIVersion)
}
