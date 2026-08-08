package network

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// CreateEndpoint creates, moves, configures, and attaches one owned veth pair.
// Any failure after LinkAdd removes that exact newly-created pair.
func CreateEndpoint(spec EndpointSpec) error {
	if err := validateEndpointSpec(spec); err != nil {
		return fmt.Errorf("invalid endpoint configuration: %w", err)
	}
	bridge, err := requireBridge(spec.BridgeName)
	if err != nil {
		return err
	}
	netNS, err := openNetNS(spec.NetNSPath)
	if err != nil {
		return err
	}
	defer netNS.Close()
	if err := preflightContainerName(netNS, spec.IfName); err != nil {
		return fmt.Errorf("preflight target netns: %w", err)
	}
	for _, name := range []string{spec.HostVethName, spec.PeerVethName} {
		if _, found, err := linkByName(name); err != nil {
			return err
		} else if found {
			return fmt.Errorf("veth create failed: host namespace link %q already exists", name)
		}
	}

	attrs := netlink.NewLinkAttrs()
	attrs.Name = spec.HostVethName
	attrs.MTU = spec.MTU
	pair := &netlink.Veth{LinkAttrs: attrs, PeerName: spec.PeerVethName}
	if err := netlink.LinkAdd(pair); err != nil {
		return fmt.Errorf("veth create failed for %q/%q: %w", spec.HostVethName, spec.PeerVethName, err)
	}
	var createdHost netlink.Link
	fail := func(original error) error {
		target := createdHost
		if target == nil {
			var lookupErr error
			var targetFound bool
			target, targetFound, lookupErr = linkByName(spec.HostVethName)
			if lookupErr != nil {
				return errors.Join(original, fmt.Errorf("rollback look up veth %q: %w", spec.HostVethName, lookupErr))
			}
			if !targetFound {
				return original
			}
		}
		if err := netlink.LinkDel(target); err != nil {
			_, stillPresent, lookupErr := linkByName(spec.HostVethName)
			if lookupErr != nil || stillPresent {
				return errors.Join(original, fmt.Errorf("rollback veth %q: %w", spec.HostVethName, err))
			}
		}
		return original
	}

	host, found, err := linkByName(spec.HostVethName)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("host veth %q missing after create", spec.HostVethName)
		}
		return fail(err)
	}
	createdHost = host
	peer, found, err := linkByName(spec.PeerVethName)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("peer veth %q missing after create", spec.PeerVethName)
		}
		return fail(err)
	}
	if err := netlink.LinkSetMTU(host, spec.MTU); err != nil {
		return fail(fmt.Errorf("set host veth %q MTU: %w", spec.HostVethName, err))
	}
	if err := netlink.LinkSetMTU(peer, spec.MTU); err != nil {
		return fail(fmt.Errorf("set peer veth %q MTU: %w", spec.PeerVethName, err))
	}
	if err := netlink.LinkSetAlias(host, spec.Alias); err != nil {
		return fail(fmt.Errorf("set host veth %q ownership alias: %w", spec.HostVethName, err))
	}
	if err := netlink.LinkSetAlias(peer, spec.Alias); err != nil {
		return fail(fmt.Errorf("set peer veth %q ownership alias: %w", spec.PeerVethName, err))
	}
	if err := VerifyVethOwnership(host, spec.Alias); err != nil {
		return fail(err)
	}
	if err := netlink.LinkSetNsFd(peer, int(netNS.Fd())); err != nil {
		return fail(fmt.Errorf("move peer %q to netns %q: %w", spec.PeerVethName, spec.NetNSPath, err))
	}
	if err := VerifyVethOwnership(host, spec.Alias); err != nil {
		return fail(err)
	}
	if err := netlink.LinkSetMaster(host, bridge); err != nil {
		return fail(fmt.Errorf("attach owned host veth %q to bridge %q: %w", spec.HostVethName, spec.BridgeName, err))
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return fail(fmt.Errorf("set host veth %q up: %w", spec.HostVethName, err))
	}
	if err := configureContainer(netNS, spec); err != nil {
		return fail(fmt.Errorf("configure container endpoint: %w", err))
	}
	if err := checkHostEndpoint(spec, bridge); err != nil {
		return fail(fmt.Errorf("verify host endpoint after create: %w", err))
	}
	return nil
}
