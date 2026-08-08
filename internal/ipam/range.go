package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

var (
	// ErrInvalidRange identifies a malformed or unsupported allocation range.
	ErrInvalidRange = errors.New("invalid IPv4 allocation range")
	// ErrExhausted means every usable address in the configured range is held.
	ErrExhausted = errors.New("IPv4 allocation range exhausted")
)

// Range is a validated IPv4 allocation interval within a subnet.
type Range struct {
	subnet    netip.Prefix
	gateway   netip.Addr
	start     netip.Addr
	end       netip.Addr
	network   netip.Addr
	broadcast netip.Addr
}

// NewRange validates and constructs an IPv4 allocation interval. Network,
// broadcast, and gateway addresses may lie inside start..end but are never
// returned by NextAvailable.
func NewRange(subnet netip.Prefix, gateway, start, end netip.Addr) (Range, error) {
	if !subnet.IsValid() || !subnet.Addr().Is4() {
		return Range{}, fmt.Errorf("%w: subnet %s is not IPv4", ErrInvalidRange, subnet)
	}
	if subnet != subnet.Masked() {
		return Range{}, fmt.Errorf("%w: subnet %s has host bits set", ErrInvalidRange, subnet)
	}
	if subnet.Bits() > 30 {
		return Range{}, fmt.Errorf("%w: subnet %s has no ordinary host range", ErrInvalidRange, subnet)
	}
	gateway = gateway.Unmap()
	start = start.Unmap()
	end = end.Unmap()
	for label, addr := range map[string]netip.Addr{
		"gateway": gateway,
		"start":   start,
		"end":     end,
	} {
		if !addr.IsValid() || !addr.Is4() || !subnet.Contains(addr) {
			return Range{}, fmt.Errorf("%w: %s address %s is outside %s", ErrInvalidRange, label, addr, subnet)
		}
	}
	if start.Compare(end) > 0 {
		return Range{}, fmt.Errorf("%w: start %s is after end %s", ErrInvalidRange, start, end)
	}

	network := subnet.Addr()
	broadcast := ipv4Broadcast(subnet)
	if gateway == network || gateway == broadcast {
		return Range{}, fmt.Errorf("%w: gateway %s is a reserved subnet address", ErrInvalidRange, gateway)
	}
	return Range{
		subnet:    subnet,
		gateway:   gateway,
		start:     start,
		end:       end,
		network:   network,
		broadcast: broadcast,
	}, nil
}

func (r Range) Subnet() netip.Prefix { return r.subnet }
func (r Range) Gateway() netip.Addr  { return r.gateway }
func (r Range) Start() netip.Addr    { return r.start }
func (r Range) End() netip.Addr      { return r.end }

// Validate reports whether r was constructed as a coherent IPv4 range.
func (r Range) Validate() error {
	validated, err := NewRange(r.subnet, r.gateway, r.start, r.end)
	if err != nil {
		return err
	}
	if validated.network != r.network || validated.broadcast != r.broadcast {
		return fmt.Errorf("%w: inconsistent derived bounds", ErrInvalidRange)
	}
	return nil
}

// Contains reports whether addr is inside the allocation interval and is not a
// network, broadcast, or gateway address.
func (r Range) Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.Is4() &&
		r.subnet.Contains(addr) &&
		addr.Compare(r.start) >= 0 &&
		addr.Compare(r.end) <= 0 &&
		addr != r.network && addr != r.broadcast && addr != r.gateway
}

// NextAvailable returns the lowest usable address absent from used.
func (r Range) NextAvailable(used map[netip.Addr]struct{}) (netip.Addr, error) {
	if err := r.Validate(); err != nil {
		return netip.Addr{}, err
	}
	for addr := r.start; ; addr = addr.Next() {
		if r.Contains(addr) {
			if _, occupied := used[addr]; !occupied {
				return addr, nil
			}
		}
		// netip.Addr.Next returns the invalid address after 255.255.255.255.
		// Testing the inclusive end before advancing avoids wrapping into an
		// endless invalid-address loop for a /0 range.
		if addr == r.end {
			break
		}
	}
	return netip.Addr{}, ErrExhausted
}

func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Addr().As4()
	network := binary.BigEndian.Uint32(bytes[:])
	bits := prefix.Bits()
	var mask uint32
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	broadcast := network | ^mask
	var result [4]byte
	binary.BigEndian.PutUint32(result[:], broadcast)
	return netip.AddrFrom4(result)
}
