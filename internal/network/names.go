package network

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	MaxInterfaceNameLength = 15
	HostVethPrefix         = "cn"
	PeerVethPrefix         = "cp"
	HostVethAliasPrefix    = "cloudnet:v1:"
)

// HostVethName is derivable without endpoint state so DEL can recover from a
// missing state file. Thirteen hex digits retain 52 bits of the endpoint hash.
func HostVethName(networkName, containerID, ifName string) string {
	digest := endpointDigest(networkName, containerID, ifName)
	return HostVethPrefix + hex.EncodeToString(digest[:])[:MaxInterfaceNameLength-len(HostVethPrefix)]
}

// PeerVethName is the temporary name used before the peer moves into the
// target namespace and is renamed to CNI_IFNAME.
func PeerVethName(networkName, containerID, ifName string) string {
	digest := endpointDigest(networkName, containerID, ifName)
	return PeerVethPrefix + hex.EncodeToString(digest[:])[:MaxInterfaceNameLength-len(PeerVethPrefix)]
}

// HostVethAlias is the exact ownership proof checked before deleting a link.
// It keeps the complete digest while remaining below Linux's alias limit.
func HostVethAlias(networkName, containerID, ifName string) string {
	digest := endpointDigest(networkName, containerID, ifName)
	return HostVethAliasPrefix + networkName + ":" + hex.EncodeToString(digest[:])
}

func OwnsHostVeth(alias, networkName, containerID, ifName string) bool {
	return alias == HostVethAlias(networkName, containerID, ifName)
}

func IsOwnedHostVeth(alias, networkName, containerID, ifName string) bool {
	return OwnsHostVeth(alias, networkName, containerID, ifName)
}

func endpointDigest(networkName, containerID, ifName string) [sha256.Size]byte {
	// NUL separators make tuple boundaries unambiguous. Validated CNI identity
	// fields cannot contain NUL.
	return sha256.Sum256([]byte(networkName + "\x00" + containerID + "\x00" + ifName))
}
