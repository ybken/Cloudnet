// 本文件验证内部 ResultData 到 CNI current Result 的接口索引、地址、网关、路由和版本兼容。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package cni

import (
	"net/netip"
	"testing"
)

func TestBuildResult(t *testing.T) {
	t.Parallel()

	data := ResultData{
		CNIVersion:   "1.1.0",
		NetNS:        "/run/netns/cloudnet-test-a",
		BridgeName:   "cni-br0",
		BridgeMAC:    "02:00:00:00:00:01",
		HostName:     "cn0123456789abc",
		HostMAC:      "02:00:00:00:00:02",
		IfName:       "eth0",
		ContainerMAC: "02:00:00:00:00:03",
		MTU:          1500,
		Address:      netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:      netip.MustParseAddr("10.77.0.1"),
	}

	result := BuildResult(data)
	if len(result.Interfaces) != 3 {
		t.Fatalf("interfaces = %d, want 3", len(result.Interfaces))
	}
	if result.Interfaces[2].Name != "eth0" || result.Interfaces[2].Sandbox != data.NetNS {
		t.Fatalf("container interface = %+v", result.Interfaces[2])
	}
	if len(result.IPs) != 1 || result.IPs[0].Interface == nil || *result.IPs[0].Interface != 2 {
		t.Fatalf("IP config = %+v, want interface index 2", result.IPs)
	}
	if got := result.IPs[0].Address.String(); got != data.Address.String() {
		t.Fatalf("address = %s, want %s", got, data.Address)
	}
	if got := result.IPs[0].Gateway.String(); got != data.Gateway.String() {
		t.Fatalf("gateway = %s, want %s", got, data.Gateway)
	}
	if len(result.Routes) != 1 || result.Routes[0].Dst.String() != "0.0.0.0/0" {
		t.Fatalf("routes = %+v, want IPv4 default route", result.Routes)
	}
}

func TestResultSupportsDeclaredVersions(t *testing.T) {
	t.Parallel()

	result := BuildResult(ResultData{
		CNIVersion: "1.1.0",
		BridgeName: "cni-br0",
		HostName:   "cn0123456789abc",
		IfName:     "eth0",
		NetNS:      "/run/netns/test",
		MTU:        1500,
		Address:    netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:    netip.MustParseAddr("10.77.0.1"),
	})

	for _, version := range []string{"1.0.0", "1.1.0"} {
		converted, err := result.GetAsVersion(version)
		if err != nil {
			t.Fatalf("GetAsVersion(%q): %v", version, err)
		}
		if got := converted.Version(); got != version {
			t.Fatalf("converted cniVersion = %q, want %q", got, version)
		}
	}
}
