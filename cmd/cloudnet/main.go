// cloudnet 是 CNI 插件的可执行程序入口。
//
// CNI runtime 不会通过命令行参数调用插件，而是把 CNI_COMMAND、
// CNI_CONTAINERID、CNI_NETNS 等信息放进环境变量，并把网络配置写入 stdin。
// skel 包负责读取这些协议输入，再分派到 ADD、CHECK、DEL 或 VERSION。
package main

import (
	cloudnetcni "github.com/cloudnet/cloudnet/internal/cni"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
)

const buildDescription = "CNI plugin cloudnet V1"

func main() {
	// PluginMainFuncs 还会统一处理错误输出：成功的 ADD Result 与失败的
	// CNI Error 都写 stdout；插件自己的诊断日志则由 internal/logging 写 stderr。
	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add:   cloudnetcni.CmdAdd,
			Check: cloudnetcni.CmdCheck,
			Del:   cloudnetcni.CmdDel,
		},
		// VERSION 请求通过这里声明插件支持的 CNI 规范版本。
		version.PluginSupports("1.0.0", "1.1.0"),
		buildDescription,
	)
}
