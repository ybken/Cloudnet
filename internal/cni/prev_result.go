package cni

import (
	"fmt"
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

// ValidatePrevResult 将 runtime 缓存的 ADD 结果与刚从 Linux 状态生成的 expected
// 对照。prevResult 可选；缺失时 CHECK 仍会验证持久状态和内核。
func ValidatePrevResult(raw map[string]interface{}, cniVersion string, expected ResultData) error {
	if raw == nil {
		return nil
	}

	// ParsePrevResult 可能改写 RawPrevResult，先浅拷贝顶层 map 避免改变配置对象。
	pluginConf := &cnitypes.PluginConf{
		CNIVersion:    cniVersion,
		RawPrevResult: cloneMap(raw),
	}
	if err := version.ParsePrevResult(pluginConf); err != nil {
		return fmt.Errorf("parse prevResult: %w", err)
	}
	// 各 CNI 版本先规范化为 types/100，后续只维护一套比较逻辑。
	result, err := current.NewResultFromResult(pluginConf.PrevResult)
	if err != nil {
		return fmt.Errorf("normalize prevResult: %w", err)
	}

	// 以 (name, sandbox) 唯一定位；只按 eth0 名称会误匹配其他 namespace。
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
	// 链式 Result 可含其他插件的地址。这里只检查绑定到本容器接口的 IPv4；
	// 其他接口和 IPv6 不属于 cloudnet V1 的责任范围。
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

	// route 没有 Interface 索引可核对，故要求整个 Result 恰好一个 IPv4 default。
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

// cloneMap 复制顶层容器；本函数不会直接改写嵌套值。
func cloneMap(input map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

// findInterface 要求唯一匹配；重复项也代表 prevResult 自相矛盾。
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

// verifyInterface 只检查 expected 中存在的观测值；空 MAC/非正 MTU 表示未知。
func verifyInterface(actual *current.Interface, expectedMAC string, expectedMTU int) error {
	if expectedMAC != "" && actual.Mac != expectedMAC {
		return fmt.Errorf("MAC is %q, want %q", actual.Mac, expectedMAC)
	}
	if expectedMTU > 0 && actual.Mtu != expectedMTU {
		return fmt.Errorf("MTU is %d, want %d", actual.Mtu, expectedMTU)
	}
	return nil
}

// isIPv4Default 接受 nil/未指定地址，但必须是 32 位地址族的 /0。
func isIPv4Default(dst net.IPNet) bool {
	ones, bits := dst.Mask.Size()
	return bits == 32 && ones == 0 && (dst.IP == nil || dst.IP.IsUnspecified())
}
