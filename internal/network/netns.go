package network

import (
	"errors"
	"fmt"
	"net"
	"os"

	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

func openNetNS(path string) (cnins.NetNS, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("namespace open failed for %q: %w", path, err)
	}
	netNS, err := cnins.GetNS(path)
	if err != nil {
		return nil, fmt.Errorf("namespace open failed for %q: %w", path, err)
	}
	return netNS, nil
}

func namespaceMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, fmt.Errorf("inspect netns %q: %w", path, err)
}

func preflightContainerName(netNS cnins.NetNS, ifName string) error {
	return netNS.Do(func(_ cnins.NetNS) error {
		_, found, err := linkByName(ifName)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("target interface conflict: %q already exists in netns", ifName)
		}
		return nil
	})
}

func configureContainer(netNS cnins.NetNS, spec EndpointSpec) error {
	return netNS.Do(func(_ cnins.NetNS) error {
		peer, found, err := linkByName(spec.PeerVethName)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("moved peer %q is missing in target netns", spec.PeerVethName)
		}
		if err := VerifyVethOwnership(peer, spec.Alias); err != nil {
			return err
		}
		if _, found, err := linkByName(spec.IfName); err != nil {
			return err
		} else if found {
			return fmt.Errorf("target interface conflict: %q already exists in netns", spec.IfName)
		}
		if err := netlink.LinkSetName(peer, spec.IfName); err != nil {
			return fmt.Errorf("rename peer %q to %q: %w", spec.PeerVethName, spec.IfName, err)
		}
		containerLink, found, err := linkByName(spec.IfName)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("container interface %q missing after rename", spec.IfName)
		}
		if err := netlink.LinkSetMTU(containerLink, spec.MTU); err != nil {
			return fmt.Errorf("set container interface %q MTU: %w", spec.IfName, err)
		}
		addresses, err := netlink.AddrList(containerLink, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("list container interface %q addresses: %w", spec.IfName, err)
		}
		state, err := classifyIPv4Addresses(addresses, spec.Address)
		if err != nil {
			return err
		}
		if state == addressConflict {
			return fmt.Errorf("container interface %q has conflicting IPv4 addresses", spec.IfName)
		}
		if state == addressAbsent {
			if err := netlink.AddrAdd(containerLink, &netlink.Addr{IPNet: ipNetFromPrefix(spec.Address)}); err != nil {
				return fmt.Errorf("configure container address %s on %q: %w", spec.Address, spec.IfName, err)
			}
		}
		if err := netlink.LinkSetUp(containerLink); err != nil {
			return fmt.Errorf("set container interface %q up: %w", spec.IfName, err)
		}
		if err := addDefaultRoute(containerLink, spec.Gateway); err != nil {
			return err
		}
		loopback, found, err := linkByName("lo")
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("loopback interface is missing in target netns")
		}
		if loopback.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(loopback); err != nil {
				return fmt.Errorf("set loopback up: %w", err)
			}
		}
		return nil
	})
}
