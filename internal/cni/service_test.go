// 本文件以 fake NetworkOps 驱动 ADD/CHECK/DEL 编排，重点验证幂等、恢复顺序、critical 回滚屏障和日志，不依赖 root。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package cni

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudnet/cloudnet/internal/config"
	"github.com/cloudnet/cloudnet/internal/endpoint"
	"github.com/cloudnet/cloudnet/internal/ipam"
	"github.com/cloudnet/cloudnet/internal/network"
	"github.com/containernetworking/cni/pkg/skel"
)

const serviceTestConfig = `{
  "cniVersion": "1.1.0",
  "name": "cloudnet-v1",
  "type": "cloudnet",
  "bridge": "cni-br0",
  "mtu": 1500,
  "ipam": {
    "subnet": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "rangeStart": "10.77.0.10",
    "rangeEnd": "10.77.0.250"
  },
  "log": {"level": "info"}
}`

func TestBridgeSpecFromConfigCarriesNetworkOwnership(t *testing.T) {
	t.Parallel()

	conf, err := config.Parse([]byte(serviceTestConfig))
	if err != nil {
		t.Fatal(err)
	}
	spec := bridgeSpecFromConfig(conf)
	if spec.NetworkName != conf.Name {
		t.Fatalf("BridgeSpec.NetworkName = %q, want %q", spec.NetworkName, conf.Name)
	}
}

func TestServiceRepeatedAddReturnsSameAddressWithoutRecreate(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")

	first, err := service.Add(args)
	if err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	second, err := service.Add(args)
	if err != nil {
		t.Fatalf("second Add() error = %v", err)
	}
	if first.Address != second.Address || first.Address.Addr() != netip.MustParseAddr("10.77.0.10") {
		t.Fatalf("addresses first=%s second=%s, want stable 10.77.0.10/24", first.Address, second.Address)
	}
	if fake.createCalls != 1 {
		t.Fatalf("CreateEndpoint calls = %d, want 1", fake.createCalls)
	}
	if fake.checkEndpointCalls != 1 {
		t.Fatalf("duplicate ADD CheckEndpoint calls = %d, want 1", fake.checkEndpointCalls)
	}
	assertEndpointPhase(t, service.Store, args.ContainerID, endpoint.PhaseReady)
}

func TestServiceRepeatedAddInNewNetNSKeepsAddressAndReconciles(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	firstArgs := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	first, err := service.Add(firstArgs)
	if err != nil {
		t.Fatalf("first Add() error = %v", err)
	}

	// The path locates the actual endpoint but is not part of its IPAM key.
	fake.checkEndpointErr = errors.New("interface is missing in replacement netns")
	secondArgs := serviceArgs("container-a", "/run/netns/cloudnet-test-recreated")
	second, err := service.Add(secondArgs)
	if err != nil {
		t.Fatalf("second Add() error = %v", err)
	}
	if second.Address != first.Address {
		t.Fatalf("address changed across netns recreation: %s -> %s", first.Address, second.Address)
	}
	if fake.createCalls != 2 {
		t.Fatalf("CreateEndpoint calls = %d, want 2", fake.createCalls)
	}
	if fake.checkEndpointCalls != 1 {
		t.Fatalf("CheckEndpoint calls = %d, want 1", fake.checkEndpointCalls)
	}
	if fake.deleteCalls != 1 {
		t.Fatalf("DeleteEndpoint calls = %d, want 1", fake.deleteCalls)
	}
	if fake.lastDelete.NetNSPath != "" {
		t.Fatalf("reconcile delete netns = %q, want host-only cleanup", fake.lastDelete.NetNSPath)
	}

	key := endpoint.Key{NetworkName: "cloudnet-v1", ContainerID: secondArgs.ContainerID, IfName: secondArgs.IfName}
	if err := service.Store.WithLock("cloudnet-v1", func(locked *ipam.LockedStore) error {
		record, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("reconciled endpoint state is missing")
		}
		if record.NetNS != secondArgs.Netns {
			t.Errorf("persisted netns = %q, want %q", record.NetNS, secondArgs.Netns)
		}
		if record.ContainerIP != second.Address.Addr().String() {
			t.Errorf("persisted IP = %s, want %s", record.ContainerIP, second.Address.Addr())
		}
		return nil
	}); err != nil {
		t.Fatalf("verify reconciled state: %v", err)
	}
	fake.checkEndpointErr = nil
	if err := service.Check(secondArgs); err != nil {
		t.Fatalf("Check() after netns reconciliation error = %v", err)
	}
}

func TestServiceReadyReconcileDeleteFailureQuarantinesAllocation(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	first, err := service.Add(args)
	if err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if got := first.Address.Addr(); got != netip.MustParseAddr("10.77.0.10") {
		t.Fatalf("first address = %s, want 10.77.0.10", got)
	}

	fake.checkEndpointErr = errors.New("injected endpoint mismatch")
	cleanupErr := errors.New("injected reconcile delete failure")
	fake.deleteErr = cleanupErr
	_, err = service.Add(args)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("reconcile Add() error = %v, want cleanup failure", err)
	}
	assertEndpointPhase(t, service.Store, args.ContainerID, endpoint.PhasePending)

	fake.checkEndpointErr = nil
	fake.deleteErr = nil
	result, err := service.Add(serviceArgs("container-b", "/run/netns/cloudnet-test-b"))
	if err != nil {
		t.Fatalf("Add() beside reconcile quarantine error = %v", err)
	}
	if got := result.Address.Addr(); got != netip.MustParseAddr("10.77.0.11") {
		t.Fatalf("address beside reconcile quarantine = %s, want 10.77.0.11", got)
	}
}

func TestServiceAddFailureReleasesPendingAllocation(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	fake.createErr = errors.New("injected veth failure")
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")

	if _, err := service.Add(args); err == nil || !strings.Contains(err.Error(), "veth create failed") {
		t.Fatalf("Add() error = %v, want classified veth error", err)
	}
	assertEndpointMissing(t, service.Store, args.ContainerID)

	fake.createErr = nil
	result, err := service.Add(serviceArgs("container-b", "/run/netns/cloudnet-test-b"))
	if err != nil {
		t.Fatalf("Add() after rollback error = %v", err)
	}
	if got := result.Address.Addr(); got != netip.MustParseAddr("10.77.0.10") {
		t.Fatalf("address after rollback = %s, want released 10.77.0.10", got)
	}
}

func TestServiceAddPreflightFailureDoesNotInspectUnrelatedContainerInterface(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	createErr := errors.New("target interface eth0 already exists")
	unrelatedErr := errors.New("unrelated container interface ownership mismatch")
	fake.createErr = createErr
	fake.deleteFunc = func(spec network.DeleteSpec) error {
		if spec.NetNSPath != "" {
			return unrelatedErr
		}
		return nil
	}
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-target-conflict")

	_, err := service.Add(args)
	if !errors.Is(err, createErr) {
		t.Fatalf("Add() error = %v, want create failure", err)
	}
	if errors.Is(err, unrelatedErr) {
		t.Fatalf("Add() inspected unrelated container interface: %v", err)
	}
	if fake.lastDelete.NetNSPath != "" {
		t.Fatalf("rollback delete netns = %q, want host-only cleanup", fake.lastDelete.NetNSPath)
	}
	assertEndpointMissing(t, service.Store, args.ContainerID)

	fake.createErr = nil
	result, err := service.Add(serviceArgs("container-b", "/run/netns/cloudnet-test-b"))
	if err != nil {
		t.Fatalf("Add() after preflight rollback error = %v", err)
	}
	if got := result.Address.Addr(); got != netip.MustParseAddr("10.77.0.10") {
		t.Fatalf("address after preflight rollback = %s, want released 10.77.0.10", got)
	}
}

func TestServiceAddCleanupFailureQuarantinesPendingAllocation(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	createErr := errors.New("injected veth setup failure")
	internalRollbackErr := errors.New("injected CreateEndpoint rollback failure")
	cleanupErr := errors.New("injected outer endpoint cleanup failure")
	fake.createErr = errors.Join(createErr, internalRollbackErr)
	fake.deleteErr = cleanupErr
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")

	_, err := service.Add(args)
	for _, want := range []error{createErr, internalRollbackErr, cleanupErr} {
		if !errors.Is(err, want) {
			t.Fatalf("Add() error = %v, want wrapped %v", err, want)
		}
	}
	if fake.deleteCalls != 1 {
		t.Fatalf("DeleteEndpoint calls = %d, want outer cleanup attempt", fake.deleteCalls)
	}
	assertEndpointPhase(t, service.Store, args.ContainerID, endpoint.PhasePending)

	// Retrying recovery must also preserve the allocation when exact-owned
	// deletion cannot prove that the endpoint is gone.
	_, err = service.Add(args)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("recover pending Add() error = %v, want cleanup failure", err)
	}
	if fake.deleteCalls != 2 {
		t.Fatalf("DeleteEndpoint calls after recovery = %d, want 2", fake.deleteCalls)
	}
	assertEndpointPhase(t, service.Store, args.ContainerID, endpoint.PhasePending)

	// The quarantined address must not be assigned to a different endpoint.
	fake.createErr = nil
	fake.deleteErr = nil
	result, err := service.Add(serviceArgs("container-b", "/run/netns/cloudnet-test-b"))
	if err != nil {
		t.Fatalf("Add() beside quarantined endpoint error = %v", err)
	}
	if got := result.Address.Addr(); got != netip.MustParseAddr("10.77.0.11") {
		t.Fatalf("address beside quarantine = %s, want 10.77.0.11", got)
	}
}

func TestServicePersistReadyCleanupFailureQuarantinesAllocation(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	statePath, err := service.Store.StatePath("cloudnet-v1")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Dir(statePath)
	var chmodErr error
	fake.createHook = func() {
		chmodErr = os.Chmod(stateDir, 0o500)
	}
	cleanupErr := errors.New("injected persist-ready endpoint cleanup failure")
	fake.deleteErr = cleanupErr
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")

	_, addErr := service.Add(args)
	if chmodErr != nil {
		t.Fatalf("make state directory read-only: %v", chmodErr)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("restore state directory permissions: %v", err)
	}
	if addErr == nil || !strings.Contains(addErr.Error(), "persist ready endpoint") {
		t.Fatalf("Add() error = %v, want persist-ready failure", addErr)
	}
	if !errors.Is(addErr, cleanupErr) {
		t.Fatalf("Add() error = %v, want wrapped cleanup failure", addErr)
	}
	assertEndpointPhase(t, service.Store, args.ContainerID, endpoint.PhasePending)

	fake.createHook = nil
	fake.deleteErr = nil
	result, err := service.Add(serviceArgs("container-b", "/run/netns/cloudnet-test-b"))
	if err != nil {
		t.Fatalf("Add() beside persist-ready quarantine error = %v", err)
	}
	if got := result.Address.Addr(); got != netip.MustParseAddr("10.77.0.11") {
		t.Fatalf("address beside persist-ready quarantine = %s, want 10.77.0.11", got)
	}
}

func TestServiceDelAllowsMissingNetNSAndIsIdempotent(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	addArgs := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	if _, err := service.Add(addArgs); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	delArgs := serviceArgs("container-a", "")
	if err := service.Del(delArgs); err != nil {
		t.Fatalf("first Del() error = %v", err)
	}
	if err := service.Del(delArgs); err != nil {
		t.Fatalf("second Del() error = %v", err)
	}
	if fake.deleteCalls != 2 {
		t.Fatalf("DeleteEndpoint calls = %d, want state-backed then derived cleanup", fake.deleteCalls)
	}
	if fake.lastDelete.NetNSPath != "" {
		t.Fatalf("DEL trusted stale state netns %q, want empty", fake.lastDelete.NetNSPath)
	}
	assertEndpointMissing(t, service.Store, addArgs.ContainerID)
}

func TestServiceCheckReportsNetworkMismatch(t *testing.T) {
	t.Parallel()

	service, fake := newTestService(t)
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	if _, err := service.Add(args); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	fake.checkBridgeErr = errors.New("bridge MTU is 1400, want 1500")

	err := service.Check(args)
	if err == nil || !strings.Contains(err.Error(), "check mismatch") || !strings.Contains(err.Error(), "bridge MTU") {
		t.Fatalf("Check() error = %v, want specific classified bridge mismatch", err)
	}
}

func TestServiceCheckReportsPersistedFieldValues(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	if _, err := service.Add(args); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	statePath, err := service.Store.StatePath("cloudnet-v1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]interface{}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	endpoints := state["endpoints"].(map[string]interface{})
	for _, value := range endpoints {
		value.(map[string]interface{})["netns"] = "/run/netns/cloudnet-test-stale"
	}
	updated, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, append(updated, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = service.Check(args)
	for _, want := range []string{
		`field "netns"`,
		`actual="/run/netns/cloudnet-test-stale"`,
		`expected="/run/netns/cloudnet-test-a"`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Check() error = %v, want substring %q", err, want)
		}
	}
}

func TestRecordMatchesInvocationReportsEveryField(t *testing.T) {
	t.Parallel()

	args := serviceArgs("container-a", "/run/netns/cloudnet-test-a")
	conf, err := config.Parse(args.StdinData)
	if err != nil {
		t.Fatal(err)
	}
	record := endpoint.Record{
		NetworkName:  conf.Name,
		ContainerID:  args.ContainerID,
		IfName:       args.IfName,
		NetNS:        args.Netns,
		HostVethName: network.HostVethName(conf.Name, args.ContainerID, args.IfName),
		Subnet:       conf.IPAM.SubnetPrefix.String(),
		Gateway:      conf.IPAM.GatewayAddr.String(),
		RangeStart:   conf.IPAM.RangeStartAddr.String(),
		RangeEnd:     conf.IPAM.RangeEndAddr.String(),
		Bridge:       conf.Bridge,
		MTU:          conf.MTU,
	}

	tests := []struct {
		name     string
		field    string
		actual   string
		expected string
		mutate   func(*endpoint.Record)
	}{
		{"network name", "networkName", "cloudnet-other", conf.Name, func(r *endpoint.Record) { r.NetworkName = "cloudnet-other" }},
		{"container ID", "containerID", "container-stale", args.ContainerID, func(r *endpoint.Record) { r.ContainerID = "container-stale" }},
		{"interface name", "ifName", "eth9", args.IfName, func(r *endpoint.Record) { r.IfName = "eth9" }},
		{"namespace", "netns", "/run/netns/stale", args.Netns, func(r *endpoint.Record) { r.NetNS = "/run/netns/stale" }},
		{"host veth", "hostVethName", "cn0000000000000", record.HostVethName, func(r *endpoint.Record) { r.HostVethName = "cn0000000000000" }},
		{"subnet", "subnet", "10.78.0.0/24", conf.IPAM.SubnetPrefix.String(), func(r *endpoint.Record) { r.Subnet = "10.78.0.0/24" }},
		{"gateway", "gateway", "10.77.0.2", conf.IPAM.GatewayAddr.String(), func(r *endpoint.Record) { r.Gateway = "10.77.0.2" }},
		{"range start", "rangeStart", "10.77.0.11", conf.IPAM.RangeStartAddr.String(), func(r *endpoint.Record) { r.RangeStart = "10.77.0.11" }},
		{"range end", "rangeEnd", "10.77.0.249", conf.IPAM.RangeEndAddr.String(), func(r *endpoint.Record) { r.RangeEnd = "10.77.0.249" }},
		{"bridge", "bridge", "cni-br1", conf.Bridge, func(r *endpoint.Record) { r.Bridge = "cni-br1" }},
		{"MTU", "mtu", "1400", "1500", func(r *endpoint.Record) { r.MTU = 1400 }},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changed := record
			tc.mutate(&changed)
			err := recordMatchesInvocation(changed, conf, args)
			for _, want := range []string{
				`field "` + tc.field + `"`,
				"actual=" + strconv.Quote(tc.actual),
				"expected=" + strconv.Quote(tc.expected),
			} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("recordMatchesInvocation() error = %v, want substring %q", err, want)
				}
			}
		})
	}
}

func TestServiceLogContainsDiagnosticSchema(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	var output bytes.Buffer
	service.LogWriter = &output
	args := serviceArgs(strings.Repeat("a", 64), "/run/netns/cloudnet-test-log")
	if _, err := service.Add(args); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	var entry map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("log is not one JSON document: %v\n%s", err, output.String())
	}
	for _, key := range []string{
		"timestamp", "level", "command", "network", "containerID", "ifName",
		"netns", "bridge", "hostVeth", "containerIP", "duration", "phase",
		"error", "rollback",
	} {
		if _, ok := entry[key]; !ok {
			t.Errorf("log field %q is missing: %v", key, entry)
		}
	}
	if got, want := entry["containerID"], strings.Repeat("a", 12); got != want {
		t.Errorf("containerID = %v, want shortened %q", got, want)
	}
	if got := entry["command"]; got != "ADD" {
		t.Errorf("command = %v, want ADD", got)
	}
}

type fakeNetworkOps struct {
	ensureErr          error
	createErr          error
	checkBridgeErr     error
	checkEndpointErr   error
	deleteErr          error
	deleteFunc         func(network.DeleteSpec) error
	createHook         func()
	createCalls        int
	checkEndpointCalls int
	deleteCalls        int
	lastDelete         network.DeleteSpec
}

func (f *fakeNetworkOps) EnsureBridge(network.BridgeSpec) (bool, error) {
	return true, f.ensureErr
}

func (f *fakeNetworkOps) CheckBridge(network.BridgeSpec) error {
	return f.checkBridgeErr
}

func (f *fakeNetworkOps) CreateEndpoint(network.EndpointSpec) error {
	f.createCalls++
	if f.createHook != nil {
		f.createHook()
	}
	return f.createErr
}

func (f *fakeNetworkOps) CheckEndpoint(network.EndpointSpec) error {
	f.checkEndpointCalls++
	return f.checkEndpointErr
}

func (f *fakeNetworkOps) DeleteEndpoint(spec network.DeleteSpec) error {
	f.deleteCalls++
	f.lastDelete = spec
	if f.deleteFunc != nil {
		return f.deleteFunc(spec)
	}
	return f.deleteErr
}

func newTestService(t *testing.T) (*Service, *fakeNetworkOps) {
	t.Helper()
	store, err := ipam.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeNetworkOps{}
	return &Service{Store: store, Network: fake, LogWriter: io.Discard}, fake
}

func serviceArgs(containerID, netns string) *skel.CmdArgs {
	return &skel.CmdArgs{
		ContainerID: containerID,
		Netns:       netns,
		IfName:      "eth0",
		StdinData:   []byte(serviceTestConfig),
	}
}

func assertEndpointPhase(t *testing.T, store *ipam.Store, containerID string, want endpoint.Phase) {
	t.Helper()
	key := endpoint.Key{NetworkName: "cloudnet-v1", ContainerID: containerID, IfName: "eth0"}
	if err := store.WithLock("cloudnet-v1", func(locked *ipam.LockedStore) error {
		record, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("endpoint is missing")
		}
		if record.Phase != want {
			t.Fatalf("phase = %q, want %q", record.Phase, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertEndpointMissing(t *testing.T, store *ipam.Store, containerID string) {
	t.Helper()
	key := endpoint.Key{NetworkName: "cloudnet-v1", ContainerID: containerID, IfName: "eth0"}
	if err := store.WithLock("cloudnet-v1", func(locked *ipam.LockedStore) error {
		_, ok, err := locked.GetEndpoint(key)
		if ok {
			t.Fatal("endpoint still exists")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
