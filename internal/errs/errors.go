// Package errs 定义了 cloudnet 中所有可分类的错误类型。
//
// 为什么需要这个包？
//
//	cloudnet 作为 CNI 插件，在 ADD/CHECK/DEL 命令执行过程中可能遇到各种
//	不同类型的失败：配置格式错误、网桥冲突、IP 地址耗尽、namespace 打不开、
//	veth 创建失败、路由配置失败、状态持久化失败等。
//
//	调用方（如 cni/service.go）需要知道失败的具体"种类"（Kind），
//	以便决定回滚策略和日志记录方式。普通的 fmt.Errorf 无法携带这种
//	结构化信息，所以设计了带 Kind 的错误包装类型。
//
// 设计原则：
//   - 每种错误对应一个 Kind 常量，便于分类和处理
//   - Error 类型同时保留 Kind、操作名（Op）和原始错误（Err），支持 errors.Unwrap 链式解包
//   - Wrap 函数非侵入式地将现有 error 包装为带 Kind 的错误
//   - IsKind 允许调用方用 errors.As 判断错误种类，而无需关心嵌套层次
package errs

import (
	"errors"
	"fmt"
)

// Kind 是错误类别的标识符，每一个 Kind 对应 cloudnet 可能遇到的一类故障。
// 在日志和 CNI Error 返回中会用到这些字符串。
type Kind string

const (
	// KindInvalidConfig 表示 CNI 配置或运行时参数格式/值不合法。
	// 例如：stdin 不是合法 JSON、cniVersion 不支持、network name 包含非法字符。
	KindInvalidConfig Kind = "invalid config"

	// KindBridgeConflict 表示宿主机上已存在名为 cni-br0 的对象，
	// 但它不是 Linux Bridge，或它的 MTU/IP 地址/端口与 cloudnet V1 契约不一致。
	// 插件绝不会静默接管或修改这种冲突对象。
	KindBridgeConflict Kind = "bridge conflict"

	// KindAllocationExhausted 表示 IP 地址池（10.77.0.10..10.77.0.250）中的所有地址
	// 都已被分配，无法为新 endpoint 分配地址。
	KindAllocationExhausted Kind = "allocation exhausted"

	// KindNamespaceOpen 表示无法打开容器对应的 network namespace。
	// 可能原因：netns 路径不存在、权限不足、已被 runtime 删除。
	KindNamespaceOpen Kind = "namespace open failed"

	// KindVethCreate 表示创建 veth pair 失败，或设置 MTU/alias/master 等属性失败。
	// 这是数据面操作的核心错误类型。
	KindVethCreate Kind = "veth create failed"

	// KindEndpointCleanup 表示在 DEL 或回滚过程中，删除已创建的 veth 或其他
	// endpoint 资源时失败。这种错误可能导致资源泄漏。
	KindEndpointCleanup Kind = "endpoint cleanup failed"

	// KindRouteConfiguration 表示在容器 netns 内部配置默认路由（0.0.0.0/0 via 10.77.0.1）
	// 失败。可能原因：已存在其他默认路由、路由表操作权限不足。
	KindRouteConfiguration Kind = "route configuration failed"

	// KindStatePersistence 表示读取、解析、写入或校验磁盘状态文件（state.json）失败。
	// 这是最严重的错误类型之一，因为磁盘状态是 IPAM 的唯一事实来源。
	KindStatePersistence Kind = "state persistence failed"

	// KindCheckMismatch 表示 CHECK 命令发现实际网络状态与预期不一致。
	// CHECK 只检测不修复，这个错误告诉 runtime 该 endpoint 需要被重新 ADD。
	KindCheckMismatch Kind = "check mismatch"

	// KindRollback 表示在 ADD 失败后的回滚（rollback）过程中，
	// 清理已创建资源时也失败了。原始错误和回滚错误会被 Join 在一起。
	KindRollback Kind = "rollback failed"
)

// Error 是 cloudnet 的结构化错误类型，它实现了 error 接口和 errors.Unwrap。
//
// 每个 Error 包含：
//   - Kind：错误的类别（见上面常量定义）
//   - Op：出错时正在执行的操作名（如 "allocate endpoint IP"、"ensure shared bridge"）
//   - Err：原始的低层错误（可以是 nil）
//
// 这样在日志中可以看到完整的信息链：哪个操作 → 什么种类 → 具体原因。
type Error struct {
	Kind Kind   // 错误类别
	Op   string // 正在执行的操作
	Err  error  // 原始错误
}

// Error 实现 error 接口，格式为 "kind: op: err" 或 "kind: err"（当 Op 为空时）。
func (e *Error) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Kind, e.Op, e.Err)
}

// Unwrap 实现 errors.Unwrap，使 errors.Is / errors.As 可以穿透 Error 找到原始错误。
func (e *Error) Unwrap() error { return e.Err }

// Wrap 将一个现有的 error 包装为带 Kind 的 Error。
// 如果 err 为 nil，返回 nil（不做无意义的包装）。
//
// 用法示例：
//
//	errs.Wrap(errs.KindVethCreate, "create endpoint", err)
func Wrap(kind Kind, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Err: err}
}

// New 创建一个带 Kind 的新错误（不需要原始 error 时使用）。
func New(kind Kind, op, message string) error {
	return &Error{Kind: kind, Op: op, Err: errors.New(message)}
}

// IsKind 检查 err 链中是否存在指定的 Kind。
// 内部使用 errors.As 递归查找 *Error 类型。
//
// 用法示例：
//
//	if errs.IsKind(err, errs.KindCheckMismatch) { ... }
func IsKind(err error, kind Kind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}
