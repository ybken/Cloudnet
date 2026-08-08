// Package endpoint defines the durable identity and lifecycle state of a CNI
// endpoint. It deliberately contains no filesystem or network operations.
package endpoint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
)

// Phase records whether an allocation is merely reserved or its network setup
// has completed. A pending record gives ADD rollback and later DEL enough
// evidence to clean up after an interrupted operation.
type Phase string

const (
	PhasePending Phase = "pending"
	PhaseReady   Phase = "ready"
)

// Key is the minimum CNI identity for an endpoint.
type Key struct {
	NetworkName string
	ContainerID string
	IfName      string
}

// Validate rejects incomplete or unsafe endpoint identities. Network names get
// their stricter path-safety validation in the state store package.
func (k Key) Validate() error {
	if k.NetworkName == "" {
		return fmt.Errorf("endpoint network name is empty")
	}
	if strings.IndexByte(k.NetworkName, 0) >= 0 {
		return fmt.Errorf("endpoint network name contains NUL")
	}
	if k.ContainerID == "" {
		return fmt.Errorf("endpoint container ID is empty")
	}
	if len(k.ContainerID) > 1024 {
		return fmt.Errorf("endpoint container ID is too long: %d bytes", len(k.ContainerID))
	}
	if strings.IndexByte(k.ContainerID, 0) >= 0 {
		return fmt.Errorf("endpoint container ID contains NUL")
	}
	if err := validateInterfaceName("container interface", k.IfName); err != nil {
		return err
	}
	return nil
}

// ID is a stable, fixed-length storage key. Length-prefixing each component
// makes the encoded identity unambiguous before hashing.
func (k Key) ID() string {
	h := sha256.New()
	var length [4]byte
	for _, part := range []string{k.NetworkName, k.ContainerID, k.IfName} {
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Record is the complete durable evidence for an endpoint and its allocation.
// NetNS is informational: DEL must be able to use HostVethName when it no longer
// exists.
type Record struct {
	NetworkName  string    `json:"networkName"`
	ContainerID  string    `json:"containerID"`
	IfName       string    `json:"ifName"`
	NetNS        string    `json:"netns,omitempty"`
	HostVethName string    `json:"hostVethName"`
	ContainerIP  string    `json:"containerIP"`
	Subnet       string    `json:"subnet"`
	Gateway      string    `json:"gateway"`
	RangeStart   string    `json:"rangeStart"`
	RangeEnd     string    `json:"rangeEnd"`
	Bridge       string    `json:"bridge"`
	MTU          int       `json:"mtu"`
	Phase        Phase     `json:"phase"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// EndpointKey reconstructs the record's durable identity.
func (r Record) EndpointKey() Key {
	return Key{
		NetworkName: r.NetworkName,
		ContainerID: r.ContainerID,
		IfName:      r.IfName,
	}
}

// Validate checks a fully populated record read from persistent state.
func (r Record) Validate() error {
	if err := r.EndpointKey().Validate(); err != nil {
		return err
	}
	if err := validateInterfaceName("host veth", r.HostVethName); err != nil {
		return err
	}
	if err := validateInterfaceName("bridge", r.Bridge); err != nil {
		return err
	}
	if r.MTU <= 0 || r.MTU > 65535 {
		return fmt.Errorf("endpoint MTU %d is outside 1..65535", r.MTU)
	}

	subnet, err := netip.ParsePrefix(r.Subnet)
	if err != nil || !subnet.IsValid() || !subnet.Addr().Is4() || subnet != subnet.Masked() {
		return fmt.Errorf("endpoint subnet %q is not a canonical IPv4 prefix", r.Subnet)
	}
	ip, err := netip.ParseAddr(r.ContainerIP)
	if err != nil || !ip.Is4() || !subnet.Contains(ip) {
		return fmt.Errorf("endpoint IP %q is not IPv4 inside %s", r.ContainerIP, subnet)
	}
	gateway, err := netip.ParseAddr(r.Gateway)
	if err != nil || !gateway.Is4() || !subnet.Contains(gateway) {
		return fmt.Errorf("endpoint gateway %q is not IPv4 inside %s", r.Gateway, subnet)
	}
	rangeStart, err := netip.ParseAddr(r.RangeStart)
	if err != nil || !rangeStart.Is4() || !subnet.Contains(rangeStart) {
		return fmt.Errorf("endpoint range start %q is not IPv4 inside %s", r.RangeStart, subnet)
	}
	rangeEnd, err := netip.ParseAddr(r.RangeEnd)
	if err != nil || !rangeEnd.Is4() || !subnet.Contains(rangeEnd) || rangeStart.Compare(rangeEnd) > 0 {
		return fmt.Errorf("endpoint range end %q does not form a valid range in %s", r.RangeEnd, subnet)
	}
	if r.Phase != PhasePending && r.Phase != PhaseReady {
		return fmt.Errorf("endpoint phase %q is invalid", r.Phase)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("endpoint createdAt is zero")
	}
	if r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("endpoint updatedAt is invalid")
	}
	return nil
}

func validateInterfaceName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", kind)
	}
	// Linux IFNAMSIZ includes the trailing NUL.
	if len(name) > 15 {
		return fmt.Errorf("%s name %q exceeds 15 bytes", kind, name)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/:\x00") {
		return fmt.Errorf("%s name %q contains forbidden characters", kind, name)
	}
	for _, r := range name {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%s name %q contains whitespace", kind, name)
		}
	}
	return nil
}
