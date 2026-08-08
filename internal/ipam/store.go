package ipam

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/cloudnet/cloudnet/internal/endpoint"
	"golang.org/x/sys/unix"
)

const DefaultStateRoot = "/var/lib/cloudnet"

var networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

// Store owns the root of all per-network state directories.
type Store struct {
	root string
}

// LockedStore is valid only during its Store.WithLock callback. All mutations
// are in memory until Commit or successful callback completion.
type LockedStore struct {
	networkName string
	statePath   string
	stateDir    *os.File
	state       persistedState
	dirty       bool
	active      bool
}

// AllocationRequest reserves an address and creates a pending endpoint record.
// ContainerIP, range fields, phase, and timestamps are filled by Allocate.
type AllocationRequest struct {
	Endpoint endpoint.Record
	Range    Range
}

func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("state root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve state root %q: %w", root, err)
	}
	return &Store{root: filepath.Clean(absolute)}, nil
}

// ValidateNetworkName ensures a network name can be used as exactly one safe
// path component. The conservative ASCII grammar also keeps operational paths
// and logs predictable.
func ValidateNetworkName(name string) error {
	if len(name) > 63 || !networkNamePattern.MatchString(name) {
		return fmt.Errorf("%w %q: use 1..63 ASCII letters, digits, dot, underscore, or hyphen; start and end alphanumeric", ErrInvalidNetworkName, name)
	}
	return nil
}

func (s *Store) networkDir(networkName string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("state store is nil")
	}
	if err := ValidateNetworkName(networkName); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "networks", networkName), nil
}

func (s *Store) StatePath(networkName string) (string, error) {
	dir, err := s.networkDir(networkName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFile), nil
}

func (s *Store) LockPath(networkName string) (string, error) {
	dir, err := s.networkDir(networkName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, lockFile), nil
}

// WithLock holds a process-level exclusive flock for the callback's entire
// duration. A successful callback auto-commits dirty state. A failing callback
// does not commit mutations that were not explicitly committed already.
func (s *Store) WithLock(networkName string, callback func(*LockedStore) error) error {
	if callback == nil {
		return fmt.Errorf("state lock callback is nil")
	}
	dir, err := s.networkDir(networkName)
	if err != nil {
		return err
	}
	dirHandle, err := openNetworkStateDir(s.root, networkName)
	if err != nil {
		return err
	}
	defer dirHandle.Close()

	lockPath := filepath.Join(dir, lockFile)
	lock, err := openRegularFileAt(dirHandle, lockFile, lockPath, unix.O_CREAT|unix.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open permanent network lock %s: %w", lockPath, err)
	}
	defer lock.Close()
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod permanent network lock %s: %w", lockPath, err)
	}
	if err := flockExclusive(lock); err != nil {
		return fmt.Errorf("lock network state %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	statePath := filepath.Join(dir, stateFile)
	state, err := loadState(dirHandle, statePath, networkName)
	if err != nil {
		return err
	}
	locked := &LockedStore{
		networkName: networkName,
		statePath:   statePath,
		stateDir:    dirHandle,
		state:       state,
		active:      true,
	}
	defer func() { locked.active = false }()

	if err := callback(locked); err != nil {
		return err
	}
	if err := locked.Commit(); err != nil {
		return fmt.Errorf("commit network %q state: %w", networkName, err)
	}
	return nil
}

func flockExclusive(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (s *LockedStore) ensureActive() error {
	if s == nil || !s.active {
		return ErrLockedStoreClosed
	}
	return nil
}

// Commit durably writes current mutations while retaining the flock. This is
// how ADD records pending state before beginning namespace and veth work.
func (s *LockedStore) Commit() error {
	if err := s.ensureActive(); err != nil {
		return err
	}
	if !s.dirty {
		return nil
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := validatePersistedState(s.state, s.networkName); err != nil {
		return fmt.Errorf("refuse to persist invalid state: %w", err)
	}
	if err := writeStateAtomic(s.stateDir, s.statePath, s.state); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *LockedStore) Allocate(request AllocationRequest) (endpoint.Record, bool, error) {
	if err := s.ensureActive(); err != nil {
		return endpoint.Record{}, false, err
	}
	if err := validateRequest(request, s.networkName); err != nil {
		return endpoint.Record{}, false, err
	}

	key := request.Endpoint.EndpointKey()
	id := key.ID()
	networkConfig := networkConfigFromRequest(request)
	if s.state.Config != nil && *s.state.Config != networkConfig {
		return endpoint.Record{}, false, fmt.Errorf("%w for network %q", ErrNetworkConflict, s.networkName)
	}
	if existing, ok := s.state.Endpoints[id]; ok {
		if err := matchesRequest(existing, request); err != nil {
			return endpoint.Record{}, false, err
		}
		if existing.NetNS != request.Endpoint.NetNS {
			existing.NetNS = request.Endpoint.NetNS
			existing.UpdatedAt = time.Now().UTC()
			s.state.Endpoints[id] = existing
			s.dirty = true
		}
		return existing, false, nil
	}

	used := make(map[netip.Addr]struct{}, len(s.state.Allocations))
	for raw := range s.state.Allocations {
		// State invariants were checked on load and after every mutation.
		used[netip.MustParseAddr(raw)] = struct{}{}
	}
	addr, err := request.Range.NextAvailable(used)
	if err != nil {
		return endpoint.Record{}, false, fmt.Errorf("allocate endpoint %s: %w", id, err)
	}

	now := time.Now().UTC()
	record := request.Endpoint
	record.ContainerIP = addr.String()
	record.Subnet = request.Range.Subnet().String()
	record.Gateway = request.Range.Gateway().String()
	record.RangeStart = request.Range.Start().String()
	record.RangeEnd = request.Range.End().String()
	record.Phase = endpoint.PhasePending
	record.CreatedAt = now
	record.UpdatedAt = now
	if err := record.Validate(); err != nil {
		return endpoint.Record{}, false, fmt.Errorf("build pending endpoint: %w", err)
	}
	s.state.Endpoints[id] = record
	s.state.Allocations[record.ContainerIP] = id
	if s.state.Config == nil {
		s.state.Config = &networkConfig
	}
	s.dirty = true
	return record, true, nil
}

func networkConfigFromRequest(request AllocationRequest) persistedNetworkConfig {
	return persistedNetworkConfig{
		Subnet:     request.Range.Subnet().String(),
		Gateway:    request.Range.Gateway().String(),
		RangeStart: request.Range.Start().String(),
		RangeEnd:   request.Range.End().String(),
		Bridge:     request.Endpoint.Bridge,
		MTU:        request.Endpoint.MTU,
	}
}

func validateRequest(request AllocationRequest, networkName string) error {
	key := request.Endpoint.EndpointKey()
	if err := key.Validate(); err != nil {
		return fmt.Errorf("invalid allocation endpoint: %w", err)
	}
	if key.NetworkName != networkName {
		return fmt.Errorf("%w: endpoint network %q does not match locked network %q", ErrEndpointConflict, key.NetworkName, networkName)
	}
	if err := ValidateNetworkName(key.NetworkName); err != nil {
		return err
	}
	if err := request.Range.Validate(); err != nil {
		return err
	}
	if request.Endpoint.ContainerIP != "" || request.Endpoint.Phase != "" ||
		!request.Endpoint.CreatedAt.IsZero() || !request.Endpoint.UpdatedAt.IsZero() {
		return fmt.Errorf("allocation request contains store-managed endpoint fields")
	}
	if request.Endpoint.Subnet != "" && request.Endpoint.Subnet != request.Range.Subnet().String() {
		return fmt.Errorf("%w: endpoint subnet %q differs from range %s", ErrEndpointConflict, request.Endpoint.Subnet, request.Range.Subnet())
	}
	if request.Endpoint.Gateway != "" && request.Endpoint.Gateway != request.Range.Gateway().String() {
		return fmt.Errorf("%w: endpoint gateway %q differs from range %s", ErrEndpointConflict, request.Endpoint.Gateway, request.Range.Gateway())
	}
	if request.Endpoint.HostVethName == "" || request.Endpoint.Bridge == "" || request.Endpoint.MTU <= 0 {
		return fmt.Errorf("allocation endpoint is missing host veth, bridge, or MTU metadata")
	}
	return nil
}

func matchesRequest(existing endpoint.Record, request AllocationRequest) error {
	want := request.Endpoint
	want.Subnet = request.Range.Subnet().String()
	want.Gateway = request.Range.Gateway().String()
	want.RangeStart = request.Range.Start().String()
	want.RangeEnd = request.Range.End().String()
	if existing.NetworkName != want.NetworkName ||
		existing.ContainerID != want.ContainerID ||
		existing.IfName != want.IfName ||
		existing.HostVethName != want.HostVethName ||
		existing.Subnet != want.Subnet ||
		existing.Gateway != want.Gateway ||
		existing.RangeStart != want.RangeStart ||
		existing.RangeEnd != want.RangeEnd ||
		existing.Bridge != want.Bridge ||
		existing.MTU != want.MTU ||
		!request.Range.Contains(netip.MustParseAddr(existing.ContainerIP)) {
		return fmt.Errorf("%w for endpoint %s", ErrEndpointConflict, existing.EndpointKey().ID())
	}
	return nil
}

func (s *LockedStore) GetEndpoint(key endpoint.Key) (endpoint.Record, bool, error) {
	if err := s.ensureActive(); err != nil {
		return endpoint.Record{}, false, err
	}
	if err := key.Validate(); err != nil {
		return endpoint.Record{}, false, err
	}
	if key.NetworkName != s.networkName {
		return endpoint.Record{}, false, fmt.Errorf("endpoint network %q does not match locked network %q", key.NetworkName, s.networkName)
	}
	record, ok := s.state.Endpoints[key.ID()]
	return record, ok, nil
}

func (s *LockedStore) ListEndpoints() ([]endpoint.Record, error) {
	if err := s.ensureActive(); err != nil {
		return nil, err
	}
	return sortedRecords(s.state.Endpoints), nil
}

// MarkPending records that an existing endpoint is about to be reconciled.
// Persisting this transition before destructive network work makes a process
// interruption recoverable by the next ADD or DEL. The operation is idempotent.
func (s *LockedStore) MarkPending(key endpoint.Key) (endpoint.Record, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, err
	}
	if !ok {
		return endpoint.Record{}, fmt.Errorf("endpoint %s does not exist", key.ID())
	}
	if record.Phase == endpoint.PhasePending {
		return record, nil
	}
	record.Phase = endpoint.PhasePending
	record.UpdatedAt = time.Now().UTC()
	s.state.Endpoints[key.ID()] = record
	s.dirty = true
	return record, nil
}

// MarkReady is idempotent.
func (s *LockedStore) MarkReady(key endpoint.Key) (endpoint.Record, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, err
	}
	if !ok {
		return endpoint.Record{}, fmt.Errorf("endpoint %s does not exist", key.ID())
	}
	if record.Phase == endpoint.PhaseReady {
		return record, nil
	}
	record.Phase = endpoint.PhaseReady
	record.UpdatedAt = time.Now().UTC()
	s.state.Endpoints[key.ID()] = record
	s.dirty = true
	return record, nil
}

// Release removes an endpoint and its address in one state mutation. Missing
// endpoints are successful no-ops, making DEL naturally idempotent.
func (s *LockedStore) Release(key endpoint.Key) (endpoint.Record, bool, error) {
	record, ok, err := s.GetEndpoint(key)
	if err != nil {
		return endpoint.Record{}, false, err
	}
	if !ok {
		return endpoint.Record{}, false, nil
	}
	id := key.ID()
	allocatedTo, ok := s.state.Allocations[record.ContainerIP]
	if !ok || allocatedTo != id {
		return endpoint.Record{}, false, &CorruptStateError{
			Path: s.statePath,
			Err:  fmt.Errorf("endpoint %s allocation %s is inconsistent", id, record.ContainerIP),
		}
	}
	delete(s.state.Allocations, record.ContainerIP)
	delete(s.state.Endpoints, id)
	s.dirty = true
	return record, true, nil
}
