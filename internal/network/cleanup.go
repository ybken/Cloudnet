package network

import (
	"errors"
	"fmt"
	"os"

	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// DeleteEndpoint is idempotent for missing links and a missing netns. A link
// that exists is deleted only after its type and alias prove exact ownership.
func DeleteEndpoint(spec DeleteSpec) error {
	if err := validateDeleteSpec(spec); err != nil {
		return fmt.Errorf("invalid endpoint delete configuration: %w", err)
	}
	host, found, err := linkByName(spec.HostVethName)
	if err != nil {
		return err
	}
	if found {
		if err := VerifyVethOwnership(host, spec.Alias); err != nil {
			return err
		}
		if err := netlink.LinkDel(host); err != nil {
			return fmt.Errorf("delete owned host veth %q: %w", spec.HostVethName, err)
		}
		return nil
	}
	if spec.NetNSPath == "" {
		return nil
	}
	missing, err := namespaceMissing(spec.NetNSPath)
	if err != nil {
		return err
	}
	if missing {
		return nil
	}
	netNS, err := openNetNS(spec.NetNSPath)
	if err != nil {
		// The namespace may disappear between stat and GetNS during DEL.
		if _, statErr := os.Stat(spec.NetNSPath); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer netNS.Close()
	return netNS.Do(func(_ cnins.NetNS) error {
		containerLink, found, err := linkByName(spec.IfName)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := VerifyVethOwnership(containerLink, spec.Alias); err != nil {
			return err
		}
		if err := netlink.LinkDel(containerLink); err != nil {
			return fmt.Errorf("delete owned container veth %q: %w", spec.IfName, err)
		}
		return nil
	})
}
