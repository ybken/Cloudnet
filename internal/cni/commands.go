package cni

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
)

func CmdAdd(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	result, err := service.Add(args)
	if err != nil {
		return err
	}
	if err := PrintResult(result); err != nil {
		return fmt.Errorf("print CNI result: %w", err)
	}
	return nil
}

func CmdCheck(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	return service.Check(args)
}

func CmdDel(args *skel.CmdArgs) error {
	service, err := NewDefaultService()
	if err != nil {
		return fmt.Errorf("initialize cloudnet service: %w", err)
	}
	return service.Del(args)
}
