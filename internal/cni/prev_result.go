package cni

import (
	"fmt"
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

// ValidatePrevResult verifies the cached ADD result used by CNI CHECK against
// the endpoint state that was just observed from Linux.
func ValidatePrevResult(raw map[string]interface{}, cniVersion string, expected ResultData) error {
	if raw == nil {
		return nil
	}

	pluginConf := &cnitypes.PluginConf{
		CNIVersion:    cniVersion,
		RawPrevResult: cloneMap(raw),
	}
	if err := version.ParsePrevResult(pluginConf); err != nil {
		return fmt.Errorf("parse prevResult: %w", err)
	}
	result, err := current.NewResultFromResult(pluginConf.PrevResult)
	if err != nil {
		return fmt.Errorf("normalize prevResult: %w", err)
	}

	containerIndex, err := findInterface(result.Interfaces, expected.IfName, expected.NetNS)
	if err != nil {
		return fmt.Errorf("prevResult container interface: %w", err)
	}
	if err := verifyInterface(result.Interfaces[containerIndex], expected.ContainerMAC, expected.MTU); err != nil {
		return fmt.Errorf("prevResult container interface: %w", err)
	}
	if expected.BridgeName != "" {
		bridgeIndex, err := findInterface(result.Interfaces, expected.BridgeName, "")
		if err != nil {
			return fmt.Errorf("prevResult bridge interface: %w", err)
		}
		if err := verifyInterface(result.Interfaces[bridgeIndex], expected.BridgeMAC, expected.MTU); err != nil {
			return fmt.Errorf("prevResult bridge interface: %w", err)
		}
	}
	if expected.HostName != "" {
		hostIndex, err := findInterface(result.Interfaces, expected.HostName, "")
		if err != nil {
			return fmt.Errorf("prevResult host interface: %w", err)
		}
		if err := verifyInterface(result.Interfaces[hostIndex], expected.HostMAC, expected.MTU); err != nil {
			return fmt.Errorf("prevResult host interface: %w", err)
		}
	}

	containerIPv4s := make([]*current.IPConfig, 0, 1)
	for _, candidate := range result.IPs {
		if candidate == nil || candidate.Interface == nil || *candidate.Interface != containerIndex {
			continue
		}
		if candidate.Address.IP.To4() == nil {
			continue
		}
		containerIPv4s = append(containerIPv4s, candidate)
	}
	// A chained result can contain addresses owned by other plugins. Cloudnet
	// validates only IPv4 configurations explicitly bound to its container
	// interface; entries for other interfaces and IPv6 entries are outside V1.
	if len(containerIPv4s) != 1 {
		return fmt.Errorf("prevResult container IPv4 address count is %d, want 1", len(containerIPv4s))
	}
	containerIPv4 := containerIPv4s[0]
	if containerIPv4.Address.String() != expected.Address.String() {
		return fmt.Errorf("prevResult address is %s, want %s", containerIPv4.Address.String(), expected.Address)
	}
	if !containerIPv4.Gateway.Equal(net.IP(expected.Gateway.AsSlice())) {
		return fmt.Errorf("prevResult gateway is %s, want %s", containerIPv4.Gateway, expected.Gateway)
	}

	defaultRoutes := 0
	for _, route := range result.Routes {
		if route == nil || !isIPv4Default(route.Dst) {
			continue
		}
		defaultRoutes++
		if !route.GW.Equal(net.IP(expected.Gateway.AsSlice())) {
			return fmt.Errorf("prevResult default route gateway is %s, want %s", route.GW, expected.Gateway)
		}
	}
	if defaultRoutes != 1 {
		return fmt.Errorf("prevResult default route count is %d, want 1", defaultRoutes)
	}
	return nil
}

func cloneMap(input map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func findInterface(interfaces []*current.Interface, name, sandbox string) (int, error) {
	found := -1
	for index, candidate := range interfaces {
		if candidate == nil || candidate.Name != name || candidate.Sandbox != sandbox {
			continue
		}
		if found >= 0 {
			return -1, fmt.Errorf("multiple interfaces match name %q and sandbox %q", name, sandbox)
		}
		found = index
	}
	if found < 0 {
		return -1, fmt.Errorf("interface name %q and sandbox %q is missing", name, sandbox)
	}
	return found, nil
}

func verifyInterface(actual *current.Interface, expectedMAC string, expectedMTU int) error {
	if expectedMAC != "" && actual.Mac != expectedMAC {
		return fmt.Errorf("MAC is %q, want %q", actual.Mac, expectedMAC)
	}
	if expectedMTU > 0 && actual.Mtu != expectedMTU {
		return fmt.Errorf("MTU is %d, want %d", actual.Mtu, expectedMTU)
	}
	return nil
}

func isIPv4Default(dst net.IPNet) bool {
	ones, bits := dst.Mask.Size()
	return bits == 32 && ones == 0 && (dst.IP == nil || dst.IP.IsUnspecified())
}
