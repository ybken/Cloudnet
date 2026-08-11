package network

import (
	"crypto/sha256"
	"encoding/hex"
)

// Linux 接口名最长 15 字节；cn/cp 区分 host 端与临时 peer，
// alias 则保留完整 digest 作为删除许可。
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

// OwnsHostVeth 使用完整字符串相等，不接受前缀或截断匹配。
func OwnsHostVeth(alias, networkName, containerID, ifName string) bool {
	return alias == HostVethAlias(networkName, containerID, ifName)
}

// IsOwnedHostVeth 是语义更直观的兼容别名，规则仍集中在 OwnsHostVeth。
func IsOwnedHostVeth(alias, networkName, containerID, ifName string) bool {
	return OwnsHostVeth(alias, networkName, containerID, ifName)
}

// endpointDigest 用 NUL 分隔身份 tuple，避免不同字段边界拼成同一字节串。
// 上层校验保证字段自身不含 NUL。
func endpointDigest(networkName, containerID, ifName string) [sha256.Size]byte {
	// NUL separators make tuple boundaries unambiguous. Validated CNI identity
	// fields cannot contain NUL.
	return sha256.Sum256([]byte(networkName + "\x00" + containerID + "\x00" + ifName))
}
