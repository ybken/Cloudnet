package network

import (
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
)

type addressState uint8

const (
	addressAbsent addressState = iota
	addressPresent
	addressConflict
)

// classifyIPv4Addresses is strict by design: a cloudnet interface owns one
// IPv4 address, so any additional IPv4 address is a configuration conflict.
func classifyIPv4Addresses(addresses []netlink.Addr, expected netip.Prefix) (addressState, error) {
	seenExpected := false
	ipv4Count := 0
	for _, address := range addresses {
		prefix, err := prefixFromIPNet(address.IPNet)
		if err != nil {
			// AddrList(FAMILY_V4) should never return a non-IPv4 address.
			return addressConflict, fmt.Errorf("inspect IPv4 address %v: %w", address, err)
		}
		ipv4Count++
		if prefix == expected {
			seenExpected = true
		}
	}
	if ipv4Count == 0 {
		return addressAbsent, nil
	}
	if ipv4Count == 1 && seenExpected {
		return addressPresent, nil
	}
	return addressConflict, nil
}
