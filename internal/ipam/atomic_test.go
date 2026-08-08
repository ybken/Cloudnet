package ipam

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAtomicWriteRenameFailurePreservesPreviousState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFile)
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	previous := newPersistedState("cloudnet-v1")
	previous.UpdatedAt = time.Now().UTC()
	previous.Config = &persistedNetworkConfig{
		Subnet: "10.77.0.0/24", Gateway: "10.77.0.1",
		RangeStart: "10.77.0.10", RangeEnd: "10.77.0.250",
		Bridge: "cni-br0", MTU: 1500,
	}
	if err := writeStateAtomic(directory, path, previous); err != nil {
		t.Fatalf("write initial state: %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("injected rename failure")
	originalRename := renameStateFileAt
	renameStateFileAt = func(_ int, _ string, _ int, _ string) error { return sentinel }
	t.Cleanup(func() { renameStateFileAt = originalRename })

	next := previous
	next.UpdatedAt = next.UpdatedAt.Add(time.Second)
	err = writeStateAtomic(directory, path, next)
	if !errors.Is(err, sentinel) {
		t.Fatalf("writeStateAtomic() error = %v, want injected failure", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("failed atomic replacement changed the previous state")
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files remain after failed replacement: %v", temps)
	}
}
