// Package cni 把 CNI 协议入口连接到 cloudnet 的状态与网络编排服务。
package cni

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
)

// CmdAdd 是 skel 调用的 ADD 适配器。Service.Add 只构造 ResultData，
// 真正按请求的 CNI 版本序列化到 stdout 的工作在这里完成。
func CmdAdd(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	result, err := service.Add(args)
	if err != nil {
		return err
	}
	// PrintResult 必须是成功路径最后的 stdout 写入，避免污染 CNI 协议通道。
	if err := PrintResult(result); err != nil {
		return fmt.Errorf("print CNI result: %w", err)
	}
	return nil
}

// CmdCheck 验证状态但不产生 Result；成功时应保持 stdout 为空。
func CmdCheck(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	return service.Check(args)
}

// CmdDel 执行幂等清理。按 CNI 约定，DEL 即使 netns 已消失也应尽量成功。
func CmdDel(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	return service.Del(args)
}
