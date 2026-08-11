// Package logging 为 cloudnet 提供结构化的 JSON 日志输出。
//
// 为什么需要这个包？
//   - CNI 协议规定：stdout 只能承载 CNI Result（成功时）或 CNI Error（失败时）。
//     插件自己的诊断信息必须写到 stderr。
//   - 使用 Go 标准库的 log/slog 输出 JSON 格式日志，便于集成测试脚本用 jq 解析验证。
//   - 日志中包含关键字段（containerID、ifName、hostVeth、containerIP、phase、duration 等），
//     帮助运维人员在容器网络出问题时快速定位。
//
// 设计原则：
//   - 只写 stderr，绝不污染 stdout
//   - 使用 slog JSON handler，字段固定、易于机器解析
//   - 将默认的 "time" 键重命名为 "timestamp"，方便与常见日志系统对接
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// shortContainerIDLength 是日志中 containerID 的截断长度。
// 完整的 containerID 可能很长（如 sha256:64 个 hex 字符），
// 日志中只保留前 12 个字符用于关联，避免日志行太长。
const shortContainerIDLength = 12

// InvocationFields 是单次 CNI 命令调用相关的关键字段集合。
// 在命令开始时收集，在命令结束时的 completion 日志中统一输出。
//
// 各字段含义：
//   - Command：CNI 命令名，ADD/CHECK/DEL
//   - Network：CNI 网络名，固定为 cloudnet-v1
//   - ContainerID：容器 ID（会被截断为 ShortContainerID）
//   - IfName：容器内的接口名，通常为 eth0
//   - NetNS：容器 network namespace 路径
//   - Bridge：宿主机 Linux Bridge 名，固定为 cni-br0
//   - HostVeth：宿主机侧 veth 接口名（确定性生成，如 cn5665fb5b768ca）
//   - ContainerIP：分配给容器的 IPv4 地址，如 10.77.0.10
type InvocationFields struct {
	Command     string
	Network     string
	ContainerID string
	IfName      string
	NetNS       string
	Bridge      string
	HostVeth    string
	ContainerIP string
}

// New 创建一个 JSON 格式的 slog.Logger，输出到指定的 io.Writer。
// CNI 调用方传入 os.Stderr；接受 io.Writer 参数使得单元测试可以直接
// 捕获日志输出进行验证。
//
// 参数：
//   - level：日志级别，debug/info/warn/error，空字符串默认为 info
//   - output：日志输出目标，nil 会直接报错
//
// 返回的 logger 会将默认的 "time" key 重命名为 "timestamp"。
func New(level string, output io.Writer) (*slog.Logger, error) {
	if output == nil {
		return nil, fmt.Errorf("log output is nil")
	}
	parsed, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: parsed,
		// ReplaceAttr 将默认的 "time" key 重命名为 "timestamp"
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				attr.Key = "timestamp"
			}
			return attr
		},
	})
	return slog.New(handler), nil
}

// NewStderr 是 New(level, os.Stderr) 的便捷封装。
func NewStderr(level string) (*slog.Logger, error) {
	return New(level, os.Stderr)
}

// ParseLevel 将字符串转换为 slog.Level。
// 支持：debug、info（默认）、warn/warning、error。
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", level)
	}
}

// WithInvocation 给 logger 附加 CNI 调用相关的结构化字段。
// containerID 会被自动截断，防止日志行过长。
func WithInvocation(logger *slog.Logger, fields InvocationFields) *slog.Logger {
	return logger.With(
		"command", fields.Command,
		"network", fields.Network,
		"containerID", ShortContainerID(fields.ContainerID),
		"ifName", fields.IfName,
		"netns", fields.NetNS,
		"bridge", fields.Bridge,
		"hostVeth", fields.HostVeth,
		"containerIP", fields.ContainerIP,
	)
}

// ShortContainerID 截断过长的 containerID，只保留前 shortContainerIDLength 字节。
// 如果 containerID 本来就很短（如 cnitool 生成的），则原样返回。
func ShortContainerID(containerID string) string {
	if len(containerID) <= shortContainerIDLength {
		return containerID
	}
	return containerID[:shortContainerIDLength]
}

// OperationAttrs 构建命令结束日志的动态属性列表。
//
// 每条 CNI 命令完成时（无论成功失败），都会写一条 completion 日志，
// 包含以下字段：
//   - duration：命令总耗时
//   - phase：失败时指示在哪个阶段失败（如 "create-veth"、"persist-ready" 等）
//   - error：错误详情（成功时为 null）
//   - rollback：是否执行过回滚操作
func OperationAttrs(duration time.Duration, phase string, err error, rollback bool) []slog.Attr {
	return []slog.Attr{
		slog.Duration("duration", duration),
		slog.String("phase", phase),
		slog.Any("error", err),
		slog.Bool("rollback", rollback),
	}
}
