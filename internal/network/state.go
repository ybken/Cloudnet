package network

import (
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// addressState 区分“可以补齐的缺失”和“必须停止的冲突”。
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
			// FAMILY_V4 理论上不应返回非 IPv4；若发生则按冲突处理，绝不忽略。
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
