package cni

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/cloudnet/cloudnet/internal/config"
	"github.com/cloudnet/cloudnet/internal/endpoint"
	"github.com/cloudnet/cloudnet/internal/errs"
	"github.com/cloudnet/cloudnet/internal/ipam"
	"github.com/cloudnet/cloudnet/internal/logging"
	"github.com/cloudnet/cloudnet/internal/network"
	"github.com/cloudnet/cloudnet/internal/transaction"
	"github.com/containernetworking/cni/pkg/skel"
)

type NetworkOps interface {
	EnsureBridge(network.BridgeSpec) (bool, error)
	CheckBridge(network.BridgeSpec) error
	CreateEndpoint(network.EndpointSpec) error
	CheckEndpoint(network.EndpointSpec) error
	DeleteEndpoint(network.DeleteSpec) error
}

type linuxNetworkOps struct{}

func (linuxNetworkOps) EnsureBridge(spec network.BridgeSpec) (bool, error) {
	return network.EnsureBridge(spec)
}

func (linuxNetworkOps) CheckBridge(spec network.BridgeSpec) error {
	return network.CheckBridge(spec)
}

func (linuxNetworkOps) CreateEndpoint(spec network.EndpointSpec) error {
	return network.CreateEndpoint(spec)
}

func (linuxNetworkOps) CheckEndpoint(spec network.EndpointSpec) error {
	return network.CheckEndpoint(spec)
}

func (linuxNetworkOps) DeleteEndpoint(spec network.DeleteSpec) error {
	return network.DeleteEndpoint(spec)
}

type Service struct {
	Store     *ipam.Store
	Network   NetworkOps
	LogWriter io.Writer
}

func NewDefaultService() (*Service, error) {
	store, err := ipam.NewStore(ipam.DefaultStateRoot)
	if err != nil {
		return nil, err
	}
	return &Service{Store: store, Network: linuxNetworkOps{}, LogWriter: os.Stderr}, nil
}

func (s *Service) Add(args *skel.CmdArgs) (result ResultData, retErr error) {
	started := time.Now()
	phase := "parse"
	rolledBack := false
	fields := invocationFields("ADD", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, rolledBack)
	}()

	if err := s.validate(); err != nil {
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "initialize ADD service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)

	phase = "validate"
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, true); err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}
	allocationRange, err := ipam.NewRange(
		conf.IPAM.SubnetPrefix,
		conf.IPAM.GatewayAddr,
		conf.IPAM.RangeStartAddr,
		conf.IPAM.RangeEndAddr,
	)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindInvalidConfig, "build allocation range", err)
	}

	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	bridgeSpec := bridgeSpecFromConfig(conf)
	alias := network.HostVethAlias(conf.Name, args.ContainerID, args.IfName)
	hostName := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	peerName := network.PeerVethName(conf.Name, args.ContainerID, args.IfName)
	// ADD cleanup is deliberately host-only. If the deterministic host side is
	// absent, its veth peer cannot still exist; entering a reused target netns
	// could only collide with an unrelated interface that happens to use ifName.
	rollbackDeleteSpec := network.DeleteSpec{
		IfName:       args.IfName,
		HostVethName: hostName,
		Alias:        alias,
	}
	var record endpoint.Record

	phase = "transaction"
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		request := ipam.AllocationRequest{
			Range: allocationRange,
			Endpoint: endpoint.Record{
				NetworkName:  conf.Name,
				ContainerID:  args.ContainerID,
				IfName:       args.IfName,
				NetNS:        args.Netns,
				HostVethName: hostName,
				Subnet:       conf.IPAM.SubnetPrefix.String(),
				Gateway:      conf.IPAM.GatewayAddr.String(),
				Bridge:       conf.Bridge,
				MTU:          conf.MTU,
			},
		}
		allocated, created, err := locked.Allocate(request)
		if err != nil {
			if errors.Is(err, ipam.ErrExhausted) {
				return errs.Wrap(errs.KindAllocationExhausted, "allocate endpoint IP", err)
			}
			return errs.Wrap(errs.KindStatePersistence, "allocate endpoint IP", err)
		}
		record = allocated
		fields.ContainerIP = allocated.ContainerIP

		var rollback transaction.Stack
		registerRelease := func() {
			rollback.Defer("release endpoint allocation", func() error {
				if _, _, err := locked.Release(key); err != nil {
					return err
				}
				return locked.Commit()
			})
		}
		fail := func(cause error) error {
			rolledBack = true
			return rollback.Rollback(cause)
		}

		if created {
			phase = "persist-pending"
			if err := locked.Commit(); err != nil {
				return errs.Wrap(errs.KindStatePersistence, "persist pending endpoint", err)
			}
			registerRelease()
		} else if allocated.Phase == endpoint.PhasePending {
			registerRelease()
		}

		// A persisted pending endpoint may be the remainder of a process that
		// died at any point during veth setup. Confirm it is gone before any
		// later rollback is allowed to release its address.
		if !created && allocated.Phase == endpoint.PhasePending {
			phase = "recover-pending"
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				rolledBack = true
				return errs.Wrap(errs.KindEndpointCleanup, "clean interrupted endpoint", err)
			}
		}

		phase = "ensure-bridge"
		if _, err := s.Network.EnsureBridge(bridgeSpec); err != nil {
			return fail(errs.Wrap(errs.KindBridgeConflict, "ensure shared bridge", err))
		}

		endpointSpec, err := endpointSpecFromRecord(allocated, args.Netns, peerName, alias)
		if err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "read allocated endpoint", err))
		}
		if !created && allocated.Phase == endpoint.PhaseReady {
			phase = "idempotency-check"
			if err := s.Network.CheckEndpoint(endpointSpec); err == nil {
				rollback.Commit()
				return nil
			}
			phase = "reconcile-pending"
			pending, err := locked.MarkPending(key)
			if err != nil {
				return errs.Wrap(errs.KindStatePersistence, "mark endpoint pending for reconciliation", err)
			}
			record = pending
			if err := locked.Commit(); err != nil {
				return errs.Wrap(errs.KindStatePersistence, "persist pending endpoint for reconciliation", err)
			}
			phase = "reconcile-delete"
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				rolledBack = true
				return errs.Wrap(errs.KindEndpointCleanup, "remove inconsistent owned endpoint", err)
			}
			registerRelease()
		}

		phase = "create-veth"
		// CreateEndpoint can fail after creating one or both veth endpoints.
		// This critical compensation therefore has to be armed before the call.
		// Its failure stops rollback so the allocation remains quarantined.
		rollback.DeferCritical("delete owned veth", func() error {
			if err := s.Network.DeleteEndpoint(rollbackDeleteSpec); err != nil {
				return errs.Wrap(errs.KindEndpointCleanup, "rollback owned endpoint", err)
			}
			return nil
		})
		if err := s.Network.CreateEndpoint(endpointSpec); err != nil {
			return fail(classifySetupError("create endpoint", err))
		}

		phase = "persist-ready"
		ready, err := locked.MarkReady(key)
		if err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "mark endpoint ready", err))
		}
		record = ready
		if err := locked.Commit(); err != nil {
			return fail(errs.Wrap(errs.KindStatePersistence, "persist ready endpoint", err))
		}
		rollback.Commit()
		return nil
	})
	if err != nil {
		var classified *errs.Error
		if errors.As(err, &classified) {
			return ResultData{}, err
		}
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "run ADD transaction", err)
	}

	phase = "result"
	result, err = resultFromRecord(conf.CNIVersion, record, args.Netns)
	if err != nil {
		return ResultData{}, errs.Wrap(errs.KindStatePersistence, "build CNI result", err)
	}
	return result, nil
}

func (s *Service) Check(args *skel.CmdArgs) (retErr error) {
	started := time.Now()
	phase := "parse"
	fields := invocationFields("CHECK", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, false)
	}()

	if err := s.validate(); err != nil {
		return errs.Wrap(errs.KindCheckMismatch, "initialize CHECK service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, true); err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}

	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	phase = "check-state"
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		record, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("allocation state for endpoint %s is missing", key.ID())
		}
		fields.ContainerIP = record.ContainerIP
		if record.Phase != endpoint.PhaseReady {
			return allocationStateFieldMismatch("phase", record.Phase, endpoint.PhaseReady)
		}
		if err := recordMatchesInvocation(record, conf, args); err != nil {
			return err
		}

		phase = "check-bridge"
		if err := s.Network.CheckBridge(bridgeSpecFromConfig(conf)); err != nil {
			return err
		}
		endpointSpec, err := endpointSpecFromRecord(
			record,
			args.Netns,
			network.PeerVethName(conf.Name, args.ContainerID, args.IfName),
			network.HostVethAlias(conf.Name, args.ContainerID, args.IfName),
		)
		if err != nil {
			return err
		}
		phase = "check-endpoint"
		if err := s.Network.CheckEndpoint(endpointSpec); err != nil {
			return err
		}
		observed, err := resultFromRecord(conf.CNIVersion, record, args.Netns)
		if err != nil {
			return err
		}
		phase = "check-prev-result"
		return ValidatePrevResult(conf.RawPrevResult, conf.CNIVersion, observed)
	})
	if err != nil {
		return errs.Wrap(errs.KindCheckMismatch, "verify endpoint", err)
	}
	return nil
}

func (s *Service) Del(args *skel.CmdArgs) (retErr error) {
	started := time.Now()
	phase := "parse"
	fields := invocationFields("DEL", args)
	logger := s.newLogger("info")
	defer func() {
		logCompletion(logger, fields, time.Since(started), phase, retErr, false)
	}()

	if err := s.validate(); err != nil {
		return errs.Wrap(errs.KindStatePersistence, "initialize DEL service", err)
	}
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "parse CNI stdin", err)
	}
	logger = s.newLogger(conf.Log.Level)
	fields.Network = conf.Name
	fields.Bridge = conf.Bridge
	fields.HostVeth = network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if err := config.ValidateRuntime(args.ContainerID, args.Netns, args.IfName, false); err != nil {
		return errs.Wrap(errs.KindInvalidConfig, "validate CNI environment", err)
	}

	key := endpoint.Key{NetworkName: conf.Name, ContainerID: args.ContainerID, IfName: args.IfName}
	expectedHost := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	alias := network.HostVethAlias(conf.Name, args.ContainerID, args.IfName)
	phase = "delete-endpoint"
	err = s.Store.WithLock(conf.Name, func(locked *ipam.LockedStore) error {
		record, found, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		hostName := expectedHost
		if found {
			fields.ContainerIP = record.ContainerIP
			if record.HostVethName != expectedHost {
				return fmt.Errorf(
					"persisted host veth %q does not match deterministic name %q",
					record.HostVethName,
					expectedHost,
				)
			}
			hostName = record.HostVethName
		}
		if err := s.Network.DeleteEndpoint(network.DeleteSpec{
			NetNSPath:    args.Netns,
			IfName:       args.IfName,
			HostVethName: hostName,
			Alias:        alias,
		}); err != nil {
			return errs.Wrap(errs.KindEndpointCleanup, "delete owned endpoint", err)
		}
		if found {
			phase = "release-allocation"
			if _, _, err := locked.Release(key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var classified *errs.Error
		if errors.As(err, &classified) {
			return err
		}
		return errs.Wrap(errs.KindStatePersistence, "delete endpoint transaction", err)
	}
	return nil
}

func (s *Service) validate() error {
	if s == nil {
		return fmt.Errorf("service is nil")
	}
	if s.Store == nil {
		return fmt.Errorf("state store is nil")
	}
	if s.Network == nil {
		s.Network = linuxNetworkOps{}
	}
	return nil
}

func (s *Service) newLogger(level string) *slog.Logger {
	output := s.LogWriter
	if output == nil {
		output = os.Stderr
	}
	logger, err := logging.New(level, output)
	if err != nil {
		logger, _ = logging.New("info", output)
	}
	return logger
}

func bridgeSpecFromConfig(conf *config.NetConf) network.BridgeSpec {
	return network.BridgeSpec{
		NetworkName: conf.Name,
		Name:        conf.Bridge,
		Subnet:      conf.IPAM.SubnetPrefix,
		Gateway:     conf.IPAM.GatewayAddr,
		MTU:         conf.MTU,
	}
}

func endpointSpecFromRecord(record endpoint.Record, netns, peerName, alias string) (network.EndpointSpec, error) {
	ip, err := netip.ParseAddr(record.ContainerIP)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted container IP %q: %w", record.ContainerIP, err)
	}
	subnet, err := netip.ParsePrefix(record.Subnet)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted subnet %q: %w", record.Subnet, err)
	}
	gateway, err := netip.ParseAddr(record.Gateway)
	if err != nil {
		return network.EndpointSpec{}, fmt.Errorf("parse persisted gateway %q: %w", record.Gateway, err)
	}
	return network.EndpointSpec{
		BridgeName:   record.Bridge,
		NetNSPath:    netns,
		IfName:       record.IfName,
		HostVethName: record.HostVethName,
		PeerVethName: peerName,
		Alias:        alias,
		Address:      netip.PrefixFrom(ip, subnet.Bits()),
		Gateway:      gateway,
		MTU:          record.MTU,
	}, nil
}

func resultFromRecord(cniVersion string, record endpoint.Record, netns string) (ResultData, error) {
	spec, err := endpointSpecFromRecord(
		record,
		netns,
		network.PeerVethName(record.NetworkName, record.ContainerID, record.IfName),
		network.HostVethAlias(record.NetworkName, record.ContainerID, record.IfName),
	)
	if err != nil {
		return ResultData{}, err
	}
	return ResultData{
		CNIVersion: cniVersion,
		NetNS:      netns,
		BridgeName: record.Bridge,
		HostName:   record.HostVethName,
		IfName:     record.IfName,
		MTU:        record.MTU,
		Address:    spec.Address,
		Gateway:    spec.Gateway,
	}, nil
}

func recordMatchesInvocation(record endpoint.Record, conf *config.NetConf, args *skel.CmdArgs) error {
	expectedHost := network.HostVethName(conf.Name, args.ContainerID, args.IfName)
	if record.NetworkName != conf.Name {
		return allocationStateFieldMismatch("networkName", record.NetworkName, conf.Name)
	}
	if record.ContainerID != args.ContainerID {
		return allocationStateFieldMismatch("containerID", record.ContainerID, args.ContainerID)
	}
	if record.IfName != args.IfName {
		return allocationStateFieldMismatch("ifName", record.IfName, args.IfName)
	}
	if record.NetNS != args.Netns {
		return allocationStateFieldMismatch("netns", record.NetNS, args.Netns)
	}
	if record.HostVethName != expectedHost {
		return allocationStateFieldMismatch("hostVethName", record.HostVethName, expectedHost)
	}
	if expected := conf.IPAM.SubnetPrefix.String(); record.Subnet != expected {
		return allocationStateFieldMismatch("subnet", record.Subnet, expected)
	}
	if expected := conf.IPAM.GatewayAddr.String(); record.Gateway != expected {
		return allocationStateFieldMismatch("gateway", record.Gateway, expected)
	}
	if expected := conf.IPAM.RangeStartAddr.String(); record.RangeStart != expected {
		return allocationStateFieldMismatch("rangeStart", record.RangeStart, expected)
	}
	if expected := conf.IPAM.RangeEndAddr.String(); record.RangeEnd != expected {
		return allocationStateFieldMismatch("rangeEnd", record.RangeEnd, expected)
	}
	if record.Bridge != conf.Bridge {
		return allocationStateFieldMismatch("bridge", record.Bridge, conf.Bridge)
	}
	if record.MTU != conf.MTU {
		return allocationStateFieldMismatch("mtu", record.MTU, conf.MTU)
	}
	return nil
}

func allocationStateFieldMismatch(field string, actual, expected interface{}) error {
	return fmt.Errorf(
		"allocation state field %q mismatch: actual=%q expected=%q",
		field,
		fmt.Sprint(actual),
		fmt.Sprint(expected),
	)
}

func classifySetupError(operation string, err error) error {
	kind := errs.KindVethCreate
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "namespace open failed") {
		kind = errs.KindNamespaceOpen
	} else if strings.Contains(message, "route configuration failed") ||
		strings.Contains(message, "default route") {
		kind = errs.KindRouteConfiguration
	}
	return errs.Wrap(kind, operation, err)
}

func invocationFields(command string, args *skel.CmdArgs) logging.InvocationFields {
	fields := logging.InvocationFields{Command: command}
	if args != nil {
		fields.ContainerID = args.ContainerID
		fields.IfName = args.IfName
		fields.NetNS = args.Netns
	}
	return fields
}

func logCompletion(
	logger *slog.Logger,
	fields logging.InvocationFields,
	duration time.Duration,
	phase string,
	err error,
	rollback bool,
) {
	logger = logging.WithInvocation(logger, fields)
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
	}
	logger.LogAttrs(
		context.Background(),
		level,
		"CNI command completed",
		logging.OperationAttrs(duration, phase, err, rollback)...,
	)
}
