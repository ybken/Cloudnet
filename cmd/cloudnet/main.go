package main

import (
	cloudnetcni "github.com/cloudnet/cloudnet/internal/cni"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
)

const buildDescription = "CNI plugin cloudnet V1"

func main() {
	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add:   cloudnetcni.CmdAdd,
			Check: cloudnetcni.CmdCheck,
			Del:   cloudnetcni.CmdDel,
		},
		version.PluginSupports("1.0.0", "1.1.0"),
		buildDescription,
	)
}
