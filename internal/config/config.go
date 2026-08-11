// Package config 负责解析和校验 cloudnet V1 的 CNI 网络配置。
//
// 为什么需要这个包？
//
//	CNI 插件从 stdin 接收 JSON 配置（通常来自 /etc/cni/net.d/ 下的配置文件）。
//	这个包做的事情：
//	  1. 严格解析 JSON：拒绝重复 key、拒绝未知字段、拒绝多余 JSON 值
//	  2. 校验 V1 固定配置：网络名、插件类型、网桥名、MTU、IPAM 参数都必须匹配
//	  3. 校验运行时参数：containerID、netns 路径、ifName 的格式和安全性
//
// 为什么这么"严格"？
//
//	cloudnet V1 是固定配置的：subnet 必须是 10.77.0.0/24，
//	gateway 必须是 10.77.0.1，bridge 必须是 cni-br0，MTU 必须是 1500。
//	任何偏离都意味着"这不是 cloudnet V1 的配置"，应该立即拒绝。
//	这样可以避免用错误的配置去解释已有的内核对象，造成误操作。
//
// 固定配置意味着什么？
//
//	你不需要写复杂的配置文件。cloudnet V1 的网络参数是写死在代码里的。
//	配置文件中仍然需要声明这些值（CNI 协议要求），但它们必须和代码中的常量一致。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
)

// ---- V1 固定配置常量 ----
// 这些值定义在代码中而不是配置文件中，意味着 cloudnet V1 不接收自定义值。
// 任何偏离都会在 Validate() 时被拒绝。

const (
	// NetworkName 是 V1 唯一支持的 CNI 网络名
	NetworkName = "cloudnet-v1"
	// PluginType 是 CNI 插件类型标识符
	PluginType = "cloudnet"
	// BridgeName 是宿主机上的 Linux Bridge 名称
	BridgeName = "cni-br0"
	// MTU 是所有接口的最大传输单元
	MTU = 1500

	// Subnet 是容器网络的 CIDR 子网
	Subnet = "10.77.0.0/24"
	// Gateway 是子网的网关地址，配置在 cni-br0 上
	Gateway = "10.77.0.1"
	// RangeStart 是 IPv4 地址池的起始地址（含）
	RangeStart = "10.77.0.10"
	// RangeEnd 是 IPv4 地址池的结束地址（含）
	RangeEnd = "10.77.0.250"

	// defaultLogLevel 是日志级别为空时的默认值
	defaultLogLevel = "info"
	// maxConfigBytes 限制 stdin 输入不超过 1 MiB
	maxConfigBytes = 1 << 20
)

// ErrInvalidConfig 是所有配置错误的根错误，用于 errors.Is 判断。
var ErrInvalidConfig = errors.New("invalid config")

// NetConf 是 cloudnet V1 的严格网络配置结构体。
//
// CNI 公共字段（cniVersion、name、type 等）被保留是为了：
//   - CHECK 命令可以核对 prevResult
//   - CNI runtime 可以传递标准的能力/参数数据
//   - 不因"不知道的字段"而削弱顶层 JSON 校验
type NetConf struct {
	// ---- CNI 协议必需字段 ----
	CNIVersion string `json:"cniVersion"` // CNI 规范版本：1.0.0 或 1.1.0
	Name       string `json:"name"`       // 网络名，必须为 cloudnet-v1
	Type       string `json:"type"`       // 插件类型，必须为 cloudnet

	// ---- CNI 协议可选字段（保留用于协议兼容） ----
	Args          map[string]json.RawMessage `json:"args,omitempty"`
	Capabilities  map[string]bool            `json:"capabilities,omitempty"`
	RuntimeConfig map[string]json.RawMessage `json:"runtimeConfig,omitempty"`
	DNS           types.DNS                  `json:"dns,omitempty"`

	// ---- CHECK/prevResult 相关 ----
	RawPrevResult    map[string]interface{} `json:"prevResult,omitempty"` // 原始 prevResult，CHECK 时校验
	PrevResult       types.Result           `json:"-"`
	ValidAttachments []types.GCAttachment   `json:"cni.dev/valid-attachments,omitempty"`

	// ---- cloudnet V1 配置 ----
	Bridge string     `json:"bridge"`        // Bridge 名，必须为 cni-br0
	MTU    int        `json:"mtu"`           // MTU 值，必须为 1500
	IPAM   IPAMConfig `json:"ipam"`          // IPAM 子配置
	Log    LogConfig  `json:"log,omitempty"` // 日志配置（目前只支持 level）
}

// IPAMConfig 是 IPAM 相关的配置，包含子网、网关和地址池范围。
// 带 "-" 标记的字段不参与 JSON 序列化，是解析后缓存的 netip 类型值。
type IPAMConfig struct {
	Subnet     string `json:"subnet"`     // CIDR 格式子网，如 "10.77.0.0/24"
	Gateway    string `json:"gateway"`    // 网关地址，如 "10.77.0.1"
	RangeStart string `json:"rangeStart"` // 地址池起始，如 "10.77.0.10"
	RangeEnd   string `json:"rangeEnd"`   // 地址池结束，如 "10.77.0.250"

	// 以下为解析后的 netip 类型缓存，不参与 JSON 序列化
	SubnetPrefix   netip.Prefix `json:"-"`
	GatewayAddr    netip.Addr   `json:"-"`
	RangeStartAddr netip.Addr   `json:"-"`
	RangeEndAddr   netip.Addr   `json:"-"`
}

// LogConfig 控制日志输出级别。
type LogConfig struct {
	Level string `json:"level,omitempty"` // debug/info/warn/error，默认为 info
}

// Parse 是严格的 JSON 配置解析入口。
//
// 它会：
//  1. 检查输入长度（不超过 1 MiB）
//  2. 先扫描 JSON token 流检测重复 key（encoding/json 默认识别不到这个）
//  3. 用 DisallowUnknownFields 反序列化，拒绝未知字段
//  4. 确认只有恰好一个 JSON 值（拒绝 "{} {}" 这样的输入）
//  5. 调用 Validate() 校验 V1 固定配置
//
// 如果任何一步失败，返回 wrapped ErrInvalidConfig。
func Parse(data []byte) (*NetConf, error) {
	if len(data) == 0 {
		return nil, invalidf("stdin is empty")
	}
	if len(data) > maxConfigBytes {
		return nil, invalidf("stdin exceeds %d bytes", maxConfigBytes)
	}
	// 第一步：扫描 token 流检测重复 key
	if err := checkUniqueJSON(data); err != nil {
		return nil, invalidf("malformed JSON: %v", err)
	}

	// 第二步：反序列化，拒绝未知字段
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var conf NetConf
	if err := decoder.Decode(&conf); err != nil {
		return nil, invalidf("decode JSON: %v", err)
	}
	// 第三步：确保没有多余的 JSON 值
	if err := requireEOF(decoder); err != nil {
		return nil, invalidf("decode JSON: %v", err)
	}
	// 第四步：校验 V1 固定配置
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	return &conf, nil
}

// Load 是 Parse 的别名，供命令处理器使用。
func Load(data []byte) (*NetConf, error) {
	return Parse(data)
}

// invalidf 包装一个格式化的错误消息为 ErrInvalidConfig。
func invalidf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// requireEOF 确保 JSON decoder 后面没有第二个 JSON 值。
// 例如 "{} 123" 或 "{}{}" 这样的输入应该被拒绝。
func requireEOF(decoder *json.Decoder) error {
	var extra interface{}
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

// checkUniqueJSON 在 typed decode 之前扫描 JSON token 流，
// 检测重复的 object key。Go 标准库的 json.Decoder 默认会
// 静默接受重复 key 并使用最后一个值，这可能掩盖配置错误。
func checkUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder); err != nil {
		return err
	}
	return requireTokenEOF(decoder)
}

// checkJSONValue 递归检查 JSON 值中的重复 object key。
func checkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		// 不是分隔符（{、[），是普通值（字符串、数字等），不需要递归
		return nil
	}

	switch delim {
	case '{':
		// 进入 JSON object：收集所有 key 并检测重复
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			// 递归检查 value
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("expected object end")
		}
	case '[':
		// 进入 JSON array：递归检查每个元素
		for decoder.More() {
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("expected array end")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return nil
}

func requireTokenEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

// normalizeLogLevel 规范化日志级别字符串。
func normalizeLogLevel(level string) (string, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return defaultLogLevel, nil
	}
	switch level {
	case "debug", "info", "warn", "error":
		return level, nil
	default:
		return "", fmt.Errorf("unsupported log level %q", level)
	}
}
