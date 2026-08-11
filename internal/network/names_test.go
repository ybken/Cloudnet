// 本文件验证确定性 veth 名称的长度/稳定性、tuple 边界隔离和完整 alias 所有权。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package network

import (
	"strings"
	"testing"
)

func TestHostVethNameIsStableAndLinuxSafe(t *testing.T) {
	t.Parallel()

	got := HostVethName("cloudnet-v1", strings.Repeat("a", 64), "eth0")
	if got != HostVethName("cloudnet-v1", strings.Repeat("a", 64), "eth0") {
		t.Fatal("HostVethName() is not deterministic")
	}
	if len(got) > MaxInterfaceNameLength {
		t.Fatalf("HostVethName() length = %d, want <= %d", len(got), MaxInterfaceNameLength)
	}
	if !strings.HasPrefix(got, HostVethPrefix) {
		t.Fatalf("HostVethName() = %q, want prefix %q", got, HostVethPrefix)
	}
}

func TestHostVethIdentitySeparatesEveryKeyPart(t *testing.T) {
	t.Parallel()

	base := HostVethName("cloudnet-v1", "container-a", "eth0")
	others := []string{
		HostVethName("cloudnet-v2", "container-a", "eth0"),
		HostVethName("cloudnet-v1", "container-b", "eth0"),
		HostVethName("cloudnet-v1", "container-a", "net1"),
	}
	for _, got := range others {
		if got == base {
			t.Errorf("different endpoint produced the same name %q", got)
		}
	}
}

func TestPeerVethNameIsStableDistinctAndLinuxSafe(t *testing.T) {
	t.Parallel()

	host := HostVethName("cloudnet-v1", "container-a", "eth0")
	peer := PeerVethName("cloudnet-v1", "container-a", "eth0")
	if peer != PeerVethName("cloudnet-v1", "container-a", "eth0") {
		t.Fatal("PeerVethName() is not deterministic")
	}
	if host == peer {
		t.Fatalf("host and peer names are both %q", host)
	}
	if len(peer) > MaxInterfaceNameLength || !strings.HasPrefix(peer, PeerVethPrefix) {
		t.Fatalf("PeerVethName() = %q, want prefix %q and length <= %d", peer, PeerVethPrefix, MaxInterfaceNameLength)
	}
}

func TestHostVethAliasIsExactOwnershipProof(t *testing.T) {
	t.Parallel()

	const networkName = "cloudnet-v1"
	const containerID = "container-a"
	const ifName = "eth0"

	alias := HostVethAlias(networkName, containerID, ifName)
	if !strings.HasPrefix(alias, HostVethAliasPrefix+networkName+":") {
		t.Fatalf("HostVethAlias() = %q, want explicit cloudnet prefix and network", alias)
	}
	if !OwnsHostVeth(alias, networkName, containerID, ifName) {
		t.Fatal("OwnsHostVeth() rejected exact alias")
	}
	for _, candidate := range []string{
		"",
		alias + "-suffix",
		strings.TrimSuffix(alias, alias[len(alias)-1:]),
		HostVethAlias(networkName, "container-b", ifName),
	} {
		if OwnsHostVeth(candidate, networkName, containerID, ifName) {
			t.Errorf("OwnsHostVeth(%q) accepted non-exact alias", candidate)
		}
	}
}
