package network

import (
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestValidateBridgeTopology(t *testing.T) {
	t.Parallel()

	const (
		networkName = "cloudnet-v1"
		bridgeIndex = 42
	)
	ownedAlias := HostVethAlias(networkName, "container-a", "eth0")

	tests := []struct {
		name             string
		bridgeMaster     int
		links            func(*netlink.Bridge) []netlink.Link
		wantErrSubstring string
	}{
		{
			name:  "standalone empty bridge",
			links: func(bridge *netlink.Bridge) []netlink.Link { return []netlink.Link{bridge} },
		},
		{
			name:             "bridge enslaved to another master",
			bridgeMaster:     7,
			links:            func(bridge *netlink.Bridge) []netlink.Link { return []netlink.Link{bridge} },
			wantErrSubstring: "master index",
		},
		{
			name: "physical or dummy port",
			links: func(bridge *netlink.Bridge) []netlink.Link {
				attrs := testLinkAttrs("underlay0", 50, bridgeIndex, "")
				return []netlink.Link{bridge, &netlink.Dummy{LinkAttrs: attrs}}
			},
			wantErrSubstring: "type \"dummy\"",
		},
		{
			name: "unowned veth port",
			links: func(bridge *netlink.Bridge) []netlink.Link {
				attrs := testLinkAttrs("cn0000000000000", 51, bridgeIndex, "")
				return []netlink.Link{bridge, &netlink.Veth{LinkAttrs: attrs}}
			},
			wantErrSubstring: "ownership alias",
		},
		{
			name: "wrong network veth port",
			links: func(bridge *netlink.Bridge) []netlink.Link {
				attrs := testLinkAttrs("cn0000000000001", 52, bridgeIndex, HostVethAlias("other-network", "container-a", "eth0"))
				return []netlink.Link{bridge, &netlink.Veth{LinkAttrs: attrs}}
			},
			wantErrSubstring: "ownership alias",
		},
		{
			name: "uppercase digest is not exact",
			links: func(bridge *netlink.Bridge) []netlink.Link {
				prefix := HostVethAliasPrefix + networkName + ":"
				alias := prefix + strings.ToUpper(strings.TrimPrefix(ownedAlias, prefix))
				attrs := testLinkAttrs("cn0000000000002", 53, bridgeIndex, alias)
				return []netlink.Link{bridge, &netlink.Veth{LinkAttrs: attrs}}
			},
			wantErrSubstring: "ownership alias",
		},
		{
			name: "owned cloudnet veth port",
			links: func(bridge *netlink.Bridge) []netlink.Link {
				owned := testLinkAttrs("cn0000000000003", 54, bridgeIndex, ownedAlias)
				unattached := testLinkAttrs("mgmt0", 55, 0, "")
				return []netlink.Link{
					bridge,
					&netlink.Veth{LinkAttrs: owned},
					&netlink.Dummy{LinkAttrs: unattached},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bridgeAttrs := testLinkAttrs("cni-br0", bridgeIndex, test.bridgeMaster, "")
			bridge := &netlink.Bridge{LinkAttrs: bridgeAttrs}
			err := validateBridgeTopology(bridge, test.links(bridge), networkName)
			if test.wantErrSubstring == "" {
				if err != nil {
					t.Fatalf("validateBridgeTopology() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErrSubstring) {
				t.Fatalf("validateBridgeTopology() error = %v, want substring %q", err, test.wantErrSubstring)
			}
			if !strings.Contains(err.Error(), "bridge conflict") {
				t.Fatalf("validateBridgeTopology() error = %v, want bridge conflict classification", err)
			}
		})
	}
}

func testLinkAttrs(name string, index, masterIndex int, alias string) netlink.LinkAttrs {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	attrs.Index = index
	attrs.MasterIndex = masterIndex
	attrs.Alias = alias
	return attrs
}
