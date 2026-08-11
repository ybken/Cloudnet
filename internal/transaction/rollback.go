// Package transaction 为 cloudnet 提供 LIFO（后进先出）补偿操作栈。
//
// 为什么需要这个包？
//
//	cloudnet 的 ADD 操作会经历多个步骤：持久化 pending 状态 → 创建 Bridge
//	→ 创建 veth pair → 配置地址和路由 → 持久化 ready 状态。
//	如果中间某一步失败，必须把前面已完成的操作撤销（"回滚"），避免留下垃圾资源。
//
//	举个例子：
//	  1. IPAM 已经分配了 10.77.0.10，并写入了 state.json (pending)
//	  2. cni-br0 已经创建
//	  3. veth pair 创建成功
//	  4. 把 veth peer 移入 container netns 时失败了
//	  → 此时需要删除刚创建的 veth pair → 释放 10.77.0.10 → 但 cni-br0 保留（共享资源）
//
//	这个"当第 N 步失败时，逆序执行前 N-1 步的清理"就是 Stack 要做的事情。
//
// 设计原则：
//   - LIFO 顺序：后注册的回滚动作先执行（"后进先出"），符合依赖关系
//   - 普通动作 vs Critical 动作：Critical 动作失败会中止整个回滚链，
//     防止在不安全的情况下继续执行更早的回滚
//   - Commit 表示正向操作完全成功，所有回滚动作被丢弃
//   - RollbackError 同时携带原始错误和所有回滚失败，不会丢失诊断信息
package transaction

import (
	"errors"
	"fmt"
	"strings"
)

// action 是回滚栈中的单个条目。
//   - name：动作名称，用于错误报告（如 "delete owned veth"）
//   - fn：实际执行的回滚函数，返回 nil 表示清理成功
//   - stopOnFailure：如果为 true，该动作失败会阻止后续（更早注册的）回滚执行
type action struct {
	name          string
	fn            func() error
	stopOnFailure bool
}

// Stack 记录正向操作过程中注册的补偿动作，并在失败时按 LIFO 顺序执行。
//
// 典型用法：
//
//	stack := &Stack{}
//	stack.Defer("delete veth", func() error { return netlink.LinkDel(veth) })
//	// ... 做可能失败的事情 ...
//	if err != nil {
//	    return stack.Rollback(err)  // 逆序执行所有注册的回滚
//	}
//	stack.Commit()  // 一切成功，丢弃回滚动作
//
// Stack 不是并发安全的：它被设计为在单个 goroutine 中使用。
type Stack struct {
	actions   []action
	committed bool
}

// Defer 注册一个普通的补偿动作。回滚时如果该动作失败，
// 会记录失败但继续执行剩余动作（尽力清理）。
//
// 参数：
//   - name：动作名称，用于错误报告
//   - fn：回滚函数，如果清理已经不需要做（如资源已不存在），应返回 nil
func (s *Stack) Defer(name string, fn func() error) {
	s.deferAction(name, fn, false)
}

// DeferCritical 注册一个"关键"补偿动作。如果该动作在回滚时失败，
// 会停止执行更早注册的回滚动作。
//
// 为什么需要 DeferCritical？
//
//	在 ADD 中，veth 创建之后的回滚动作是"删除 veth"。
//	如果这个删除失败了，说明 host veth 可能还残留在系统中。
//	此时不应该释放 IP 地址（更早注册的动作），因为一个残留的 veth
//	可能会和重分配该 IP 的新 veth 产生冲突。
//
// 在 ADD 中的使用顺序：
//  1. Defer("release allocation", ...)          ← 最早注册
//  2. DeferCritical("delete owned veth", ...)    ← 最后注册，但在回滚中先执行
//     如果 delete veth 失败 → 不回滚到 release allocation，IP 保留在 pending 状态隔离
func (s *Stack) DeferCritical(name string, fn func() error) {
	s.deferAction(name, fn, true)
}

func (s *Stack) deferAction(name string, fn func() error, stopOnFailure bool) {
	if s.committed || fn == nil {
		return
	}
	s.actions = append(s.actions, action{name: name, fn: fn, stopOnFailure: stopOnFailure})
}

// Commit 标记正向操作完全成功。所有已注册的回滚动作被丢弃，
// 后续对 Rollback 的调用将变成 no-op。
func (s *Stack) Commit() {
	s.committed = true
	s.actions = nil
}

// Rollback 逆序执行所有已注册的回滚动作，返回 RollbackError。
//
// 如果 Stack 已被 Commit，直接返回原始错误不做任何回滚。
// 如果所有回滚动作都成功，返回原始错误（不带 rollback 失败信息）。
// 如果有回滚动作失败，返回 RollbackError 包含原始错误 + 所有回滚失败。
//
// 参数 cause 是触发回滚的原始错误。
func (s *Stack) Rollback(cause error) error {
	if s.committed {
		return cause
	}

	rollbackErr := &RollbackError{Cause: cause}
	// LIFO：从最后一个注册的动作开始，逆序执行
	for i := len(s.actions) - 1; i >= 0; i-- {
		entry := s.actions[i]
		if err := entry.fn(); err != nil {
			rollbackErr.Failures = append(rollbackErr.Failures, Failure{
				Action: entry.name,
				Err:    err,
			})
			// Critical 动作失败 → 中止回滚链
			if entry.stopOnFailure {
				break
			}
		}
	}
	s.actions = nil

	if len(rollbackErr.Failures) == 0 {
		return cause
	}
	return rollbackErr
}

// Failure 记录单个回滚动作的失败详情。
type Failure struct {
	Action string // 回滚动作名称
	Err    error  // 具体错误
}

// RollbackError 包含触发回滚的原始错误和回滚过程中的所有失败。
// 这确保了原始错误（为什么操作失败）不会因为回滚错误（清理也失败了）而被掩盖。
type RollbackError struct {
	Cause    error     // 触发回滚的原始错误
	Failures []Failure // 回滚过程中的失败列表
}

// Error 将所有错误信息合并为一条人类可读的字符串。
// 格式："原始错误; rollback 动作1: 错误1; rollback 动作2: 错误2"
func (e *RollbackError) Error() string {
	parts := make([]string, 0, len(e.Failures)+1)
	if e.Cause != nil {
		parts = append(parts, e.Cause.Error())
	}
	for _, failure := range e.Failures {
		parts = append(parts, fmt.Sprintf("rollback %s: %v", failure.Action, failure.Err))
	}
	return strings.Join(parts, "; ")
}

// Unwrap 返回所有错误（原始 + 各回滚失败）的 Join，支持 errors.Is/As 链式查找。
func (e *RollbackError) Unwrap() error {
	result := make([]error, 0, len(e.Failures)+1)
	if e.Cause != nil {
		result = append(result, e.Cause)
	}
	for _, failure := range e.Failures {
		result = append(result, failure.Err)
	}
	return errors.Join(result...)
}

// JoinedFailure 只返回回滚失败的错误组合（不含原始 cause）。
// 用于只需要知道回滚本身是否成功而无需关注原始错误的场景。
func (e *RollbackError) JoinedFailure() error {
	result := make([]error, 0, len(e.Failures))
	for _, failure := range e.Failures {
		result = append(result, fmt.Errorf("%s: %w", failure.Action, failure.Err))
	}
	return errors.Join(result...)
}
