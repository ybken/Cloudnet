package ipam_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudnet/cloudnet/internal/endpoint"
	"github.com/cloudnet/cloudnet/internal/ipam"
)

const testNetwork = "cloudnet-v1"

func TestStoreAllocatePersistsPendingAndReadyEndpoint(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	var allocated endpoint.Record
	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var created bool
		var err error
		allocated, created, err = locked.Allocate(allocationRequest(t, key, "veth-a"))
		if err != nil {
			return err
		}
		if !created {
			t.Error("Allocate() created = false, want true")
		}
		if allocated.Phase != endpoint.PhasePending {
			t.Errorf("phase = %q, want pending", allocated.Phase)
		}

		// Commit is intentionally usable before the callback returns. CNI ADD can
		// persist pending state and keep the process lock across netlink work.
		if err := locked.Commit(); err != nil {
			return err
		}
		ready, err := locked.MarkReady(key)
		if err != nil {
			return err
		}
		if ready.Phase != endpoint.PhaseReady {
			t.Errorf("phase = %q, want ready", ready.Phase)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
	if got, want := allocated.ContainerIP, "10.77.0.10"; got != want {
		t.Fatalf("ContainerIP = %q, want %q", got, want)
	}

	err = store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		got, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("GetEndpoint() did not find persisted endpoint")
		}
		if got.Phase != endpoint.PhaseReady {
			t.Errorf("persisted phase = %q, want ready", got.Phase)
		}
		if got.ContainerIP != allocated.ContainerIP {
			t.Errorf("persisted IP = %q, want %q", got.ContainerIP, allocated.ContainerIP)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("second WithLock() error = %v", err)
	}
}

func TestStoreMarkPendingIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		if _, _, err := locked.Allocate(allocationRequest(t, key, "veth-a")); err != nil {
			return err
		}
		_, err := locked.MarkReady(key)
		return err
	}); err != nil {
		t.Fatalf("seed ready endpoint: %v", err)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		first, err := locked.MarkPending(key)
		if err != nil {
			return err
		}
		second, err := locked.MarkPending(key)
		if err != nil {
			return err
		}
		if first.Phase != endpoint.PhasePending || second.Phase != endpoint.PhasePending {
			t.Errorf("MarkPending phases = %q, %q", first.Phase, second.Phase)
		}
		if !second.UpdatedAt.Equal(first.UpdatedAt) {
			t.Error("idempotent MarkPending changed updatedAt")
		}
		return nil
	}); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		record, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok || record.Phase != endpoint.PhasePending {
			t.Fatalf("persisted endpoint = %+v, found=%v; want pending", record, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify pending endpoint: %v", err)
	}
}

func TestStoreRepeatedAllocationReturnsSameAddress(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	var first, second endpoint.Record
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var err error
		first, _, err = locked.Allocate(allocationRequest(t, key, "veth-a"))
		return err
	}); err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var created bool
		var err error
		second, created, err = locked.Allocate(allocationRequest(t, key, "veth-a"))
		if created {
			t.Error("repeated Allocate() created = true, want false")
		}
		return err
	}); err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	if first.ContainerIP != second.ContainerIP {
		t.Fatalf("repeated allocation changed IP: %s -> %s", first.ContainerIP, second.ContainerIP)
	}
}

func TestStoreRepeatedAllocationUpdatesNetNSWithoutChangingAddress(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	var first endpoint.Record
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var err error
		first, _, err = locked.Allocate(allocationRequest(t, key, "veth-a"))
		if err != nil {
			return err
		}
		_, err = locked.MarkReady(key)
		return err
	}); err != nil {
		t.Fatalf("first allocation: %v", err)
	}

	wantNetNS := "/run/netns/recreated"
	var second endpoint.Record
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		req := allocationRequest(t, key, "veth-a")
		req.Endpoint.NetNS = wantNetNS
		var created bool
		var err error
		second, created, err = locked.Allocate(req)
		if created {
			t.Error("repeated Allocate() created = true, want false")
		}
		return err
	}); err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	if second.ContainerIP != first.ContainerIP {
		t.Fatalf("repeated allocation changed IP: %s -> %s", first.ContainerIP, second.ContainerIP)
	}
	if second.NetNS != wantNetNS {
		t.Fatalf("returned netns = %q, want %q", second.NetNS, wantNetNS)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		persisted, ok, err := locked.GetEndpoint(key)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("updated endpoint is missing")
		}
		if persisted.NetNS != wantNetNS {
			t.Errorf("persisted netns = %q, want %q", persisted.NetNS, wantNetNS)
		}
		if persisted.ContainerIP != first.ContainerIP {
			t.Errorf("persisted IP = %s, want %s", persisted.ContainerIP, first.ContainerIP)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify allocation: %v", err)
	}
}

func TestStoreRejectsRepeatedEndpointWithConflictingConfiguration(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		_, _, err := locked.Allocate(allocationRequest(t, key, "veth-a"))
		return err
	}); err != nil {
		t.Fatalf("first allocation: %v", err)
	}

	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		req := allocationRequest(t, key, "different-veth")
		_, _, err := locked.Allocate(req)
		return err
	})
	if !errors.Is(err, ipam.ErrEndpointConflict) {
		t.Fatalf("conflicting Allocate() error = %v, want ErrEndpointConflict", err)
	}
}

func TestStoreRetainsNetworkConfigurationAfterLastRelease(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	firstKey := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		if _, _, err := locked.Allocate(allocationRequest(t, firstKey, "veth-a")); err != nil {
			return err
		}
		_, _, err := locked.Release(firstKey)
		return err
	}); err != nil {
		t.Fatalf("allocate and release: %v", err)
	}

	secondKey := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-b", IfName: "eth0"}
	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		req := allocationRequest(t, secondKey, "veth-b")
		different := mustRange(t, "10.77.0.0/24", "10.77.0.1", "10.77.0.20", "10.77.0.250")
		req.Range = different
		req.Endpoint.Subnet = different.Subnet().String()
		req.Endpoint.Gateway = different.Gateway().String()
		_, _, err := locked.Allocate(req)
		return err
	})
	if !errors.Is(err, ipam.ErrNetworkConflict) {
		t.Fatalf("Allocate() error = %v, want ErrNetworkConflict", err)
	}
}

func TestStoreReleaseIsIdempotentAndReusesAddress(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	firstKey := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	secondKey := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-b", IfName: "eth0"}
	var first endpoint.Record
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var err error
		first, _, err = locked.Allocate(allocationRequest(t, firstKey, "veth-a"))
		return err
	}); err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		_, released, err := locked.Release(firstKey)
		if !released {
			t.Error("first Release() released = false, want true")
		}
		return err
	}); err != nil {
		t.Fatalf("first Release() error = %v", err)
	}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		_, released, err := locked.Release(firstKey)
		if released {
			t.Error("second Release() released = true, want false")
		}
		return err
	}); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}

	var second endpoint.Record
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		var err error
		second, _, err = locked.Allocate(allocationRequest(t, secondKey, "veth-b"))
		return err
	}); err != nil {
		t.Fatalf("replacement Allocate() error = %v", err)
	}
	if second.ContainerIP != first.ContainerIP {
		t.Fatalf("released IP was not reused: first=%s second=%s", first.ContainerIP, second.ContainerIP)
	}
}

func TestStoreExhaustion(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	rng := mustRange(t, "10.77.0.0/29", "10.77.0.1", "10.77.0.2", "10.77.0.3")
	for i := 0; i < 2; i++ {
		key := endpoint.Key{NetworkName: testNetwork, ContainerID: fmt.Sprintf("container-%d", i), IfName: "eth0"}
		err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
			req := allocationRequest(t, key, fmt.Sprintf("veth-%d", i))
			req.Range = rng
			req.Endpoint.Subnet = rng.Subnet().String()
			req.Endpoint.Gateway = rng.Gateway().String()
			_, _, err := locked.Allocate(req)
			return err
		})
		if err != nil {
			t.Fatalf("allocation %d error = %v", i, err)
		}
	}

	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-full", IfName: "eth0"}
		req := allocationRequest(t, key, "veth-full")
		req.Range = rng
		req.Endpoint.Subnet = rng.Subnet().String()
		req.Endpoint.Gateway = rng.Gateway().String()
		_, _, err := locked.Allocate(req)
		return err
	})
	if !errors.Is(err, ipam.ErrExhausted) {
		t.Fatalf("exhausted Allocate() error = %v, want ErrExhausted", err)
	}
}

func TestStoreConcurrentAllocationsHaveNoDuplicates(t *testing.T) {
	store := newStore(t)
	const count = 50

	start := make(chan struct{})
	results := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := endpoint.Key{NetworkName: testNetwork, ContainerID: fmt.Sprintf("container-%03d", i), IfName: "eth0"}
			err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
				record, _, err := locked.Allocate(allocationRequest(t, key, fmt.Sprintf("veth%010d", i)))
				if err == nil {
					results <- record.ContainerIP
				}
				return err
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Allocate() error = %v", err)
		}
	}
	seen := make(map[string]struct{}, count)
	for addr := range results {
		if _, ok := seen[addr]; ok {
			t.Fatalf("duplicate allocation %s", addr)
		}
		seen[addr] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("unique allocations = %d, want %d", len(seen), count)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		records, err := locked.ListEndpoints()
		if err != nil {
			return err
		}
		if len(records) != count {
			t.Errorf("persisted endpoints = %d, want %d", len(records), count)
		}
		return nil
	}); err != nil {
		t.Fatalf("verification WithLock() error = %v", err)
	}
}

func TestStoreSerializesWholeCallback(t *testing.T) {
	store := newStore(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithLock(testNetwork, func(_ *ipam.LockedStore) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.WithLock(testNetwork, func(_ *ipam.LockedStore) error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second callback entered while first still held the network lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first WithLock() error = %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second callback did not enter after lock release")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second WithLock() error = %v", err)
	}
}

func TestStoreCallbackErrorDoesNotCommitUncommittedMutation(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	sentinel := errors.New("network setup failed")
	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		if _, _, err := locked.Allocate(allocationRequest(t, key, "veth-a")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock() error = %v, want sentinel", err)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		_, ok, err := locked.GetEndpoint(key)
		if ok {
			t.Error("uncommitted endpoint was persisted")
		}
		return err
	}); err != nil {
		t.Fatalf("verification WithLock() error = %v", err)
	}
}

func TestStoreExplicitCommitSurvivesLaterCallbackError(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	sentinel := errors.New("network setup failed")
	err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		if _, _, err := locked.Allocate(allocationRequest(t, key, "veth-a")); err != nil {
			return err
		}
		if err := locked.Commit(); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock() error = %v, want sentinel", err)
	}

	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		got, ok, err := locked.GetEndpoint(key)
		if !ok {
			t.Error("explicitly committed endpoint is missing")
		} else if got.Phase != endpoint.PhasePending {
			t.Errorf("phase = %q, want pending", got.Phase)
		}
		return err
	}); err != nil {
		t.Fatalf("verification WithLock() error = %v", err)
	}
}

func TestStoreCorruptStateIsPreservedAndCallbackIsNotRun(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	statePath, err := store.StatePath(testNetwork)
	if err != nil {
		t.Fatalf("StatePath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{ definitely-not-json\n")
	if err := os.WriteFile(statePath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	err = store.WithLock(testNetwork, func(_ *ipam.LockedStore) error {
		called = true
		return nil
	})
	var corrupt *ipam.CorruptStateError
	if !errors.As(err, &corrupt) {
		t.Fatalf("WithLock() error = %v, want CorruptStateError", err)
	}
	if called {
		t.Error("callback ran with corrupt state")
	}
	got, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("corrupt state was changed: got %q, want %q", got, original)
	}
}

func TestStoreStrictStateJSONIsPreservedAndCallbackIsNotRun(t *testing.T) {
	t.Parallel()

	mutations := map[string]func([]byte) []byte{
		"trailing document": func(raw []byte) []byte { return append(raw, []byte("{}")...) },
		"unknown field": func(raw []byte) []byte {
			return append([]byte(`{"unexpected":true,`), raw[1:]...)
		},
		"duplicate field": func(raw []byte) []byte {
			return append([]byte(`{"version":1,`), raw[1:]...)
		},
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newStore(t)
			key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
			if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
				_, _, err := locked.Allocate(allocationRequest(t, key, "veth-a"))
				return err
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			statePath, _ := store.StatePath(testNetwork)
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := mutate(raw)
			if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}

			called := false
			err = store.WithLock(testNetwork, func(_ *ipam.LockedStore) error {
				called = true
				return nil
			})
			var stateErr *ipam.CorruptStateError
			if !errors.As(err, &stateErr) {
				t.Fatalf("WithLock() error = %v, want CorruptStateError", err)
			}
			if called {
				t.Error("callback ran with non-strict state JSON")
			}
			preserved, readErr := os.ReadFile(statePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(preserved) != string(corrupt) {
				t.Fatal("rejected state JSON was modified")
			}
		})
	}
}

func TestStoreRejectsSymlinkedStatePaths(t *testing.T) {
	t.Parallel()

	t.Run("root", func(t *testing.T) {
		base := t.TempDir()
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "state-root")
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		store, err := ipam.NewStore(root)
		if err != nil {
			t.Fatalf("NewStore() error = %v", err)
		}
		assertLockCallbackRejected(t, store, "symbolic link")
		if _, err := os.Stat(filepath.Join(outside, "networks")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink target was modified: %v", err)
		}
	})

	t.Run("networks parent", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "networks")); err != nil {
			t.Fatal(err)
		}
		store, err := ipam.NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		assertLockCallbackRejected(t, store, "symbolic link")
		if _, err := os.Stat(filepath.Join(outside, testNetwork)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink target was modified: %v", err)
		}
	})

	t.Run("network directory", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "networks"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "networks", testNetwork)); err != nil {
			t.Fatal(err)
		}
		store, err := ipam.NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		assertLockCallbackRejected(t, store, "symbolic link")
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("symlink target was modified: %v", entries)
		}
	})

	t.Run("lock file", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		dir := filepath.Join(root, "networks", testNetwork)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("unchanged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(dir, ".lock")); err != nil {
			t.Fatal(err)
		}
		store, err := ipam.NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		assertLockCallbackRejected(t, store, "symbolic link")
		assertFileContents(t, victim, "unchanged")
	})

	t.Run("state file", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		dir := filepath.Join(root, "networks", testNetwork)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(dir, "state.json")); err != nil {
			t.Fatal(err)
		}
		store, err := ipam.NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		assertLockCallbackRejected(t, store, "symbolic link")
		assertFileContents(t, victim, "{}")
	})
}

func TestStoreRejectsNonRegularStateFile(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	statePath, err := store.StatePath(testNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	assertLockCallbackRejected(t, store, "regular file")
}

func TestStateAndPermanentLockPermissionsAndAtomicCleanup(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	key := endpoint.Key{NetworkName: testNetwork, ContainerID: "container-a", IfName: "eth0"}
	if err := store.WithLock(testNetwork, func(locked *ipam.LockedStore) error {
		_, _, err := locked.Allocate(allocationRequest(t, key, "veth-a"))
		return err
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}

	statePath, _ := store.StatePath(testNetwork)
	lockPath, _ := store.LockPath(testNetwork)
	for _, path := range []string{statePath, lockPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q): %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode of %s = %#o, want 0600", path, got)
		}
	}
	dirInfo, err := os.Stat(filepath.Dir(statePath))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("network state directory mode = %#o, want 0700", got)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Version     int    `json:"version"`
		NetworkName string `json:"networkName"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if header.Version != ipam.StateVersion || header.NetworkName != testNetwork {
		t.Fatalf("state header = %+v", header)
	}

	temps, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		sort.Strings(temps)
		t.Fatalf("atomic-write temporary files remain: %v", temps)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("permanent lock disappeared: %v", err)
	}
}

func TestNetworkNameTraversalIsRejected(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	invalid := []string{"", ".", "..", "../escape", "a/b", "/absolute", "white space", "-leading", "trailing-", "network\\name", "网络", strings.Repeat("a", 64)}
	for _, name := range invalid {
		name := name
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			t.Parallel()
			if err := ipam.ValidateNetworkName(name); !errors.Is(err, ipam.ErrInvalidNetworkName) {
				t.Fatalf("ValidateNetworkName() error = %v, want ErrInvalidNetworkName", err)
			}
			if _, err := store.StatePath(name); !errors.Is(err, ipam.ErrInvalidNetworkName) {
				t.Fatalf("StatePath() error = %v, want ErrInvalidNetworkName", err)
			}
		})
	}

	for _, name := range []string{"a", "cloudnet-v1", "team_a.network-01"} {
		if err := ipam.ValidateNetworkName(name); err != nil {
			t.Errorf("ValidateNetworkName(%q) error = %v", name, err)
		}
	}
}

func newStore(t *testing.T) *ipam.Store {
	t.Helper()
	store, err := ipam.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

func allocationRequest(t *testing.T, key endpoint.Key, hostVeth string) ipam.AllocationRequest {
	t.Helper()
	rng := mustRange(t, "10.77.0.0/24", "10.77.0.1", "10.77.0.10", "10.77.0.250")
	return ipam.AllocationRequest{
		Range: rng,
		Endpoint: endpoint.Record{
			NetworkName:  key.NetworkName,
			ContainerID:  key.ContainerID,
			IfName:       key.IfName,
			NetNS:        "/run/netns/test",
			HostVethName: hostVeth,
			Subnet:       rng.Subnet().String(),
			Gateway:      rng.Gateway().String(),
			Bridge:       "cni-br0",
			MTU:          1500,
		},
	}
}

func assertLockCallbackRejected(t *testing.T, store *ipam.Store, message string) {
	t.Helper()
	called := false
	err := store.WithLock(testNetwork, func(_ *ipam.LockedStore) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("WithLock() error = %v, want text %q", err, message)
	}
	if called {
		t.Fatal("callback ran after unsafe path was detected")
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("contents of %s = %q, want %q", path, raw, want)
	}
}
