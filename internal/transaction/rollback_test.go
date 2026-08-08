package transaction

import (
	"errors"
	"reflect"
	"testing"
)

func TestRollbackRunsInReverseOrder(t *testing.T) {
	t.Parallel()

	var got []string
	var stack Stack
	stack.Defer("first", func() error {
		got = append(got, "first")
		return nil
	})
	stack.Defer("second", func() error {
		got = append(got, "second")
		return nil
	})

	cause := errors.New("operation failed")
	if err := stack.Rollback(cause); !errors.Is(err, cause) {
		t.Fatalf("Rollback() error = %v, want wrapped cause", err)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback order = %v, want %v", got, want)
	}
}

func TestRollbackContinuesAfterFailure(t *testing.T) {
	t.Parallel()

	cause := errors.New("primary")
	cleanupErr := errors.New("cleanup")
	var got []string
	var stack Stack
	stack.Defer("first", func() error {
		got = append(got, "first")
		return nil
	})
	stack.Defer("second", func() error {
		got = append(got, "second")
		return cleanupErr
	})

	err := stack.Rollback(cause)
	if !errors.Is(err, cause) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Rollback() error = %v, want both primary and cleanup errors", err)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback order = %v, want %v", got, want)
	}
}

func TestRollbackCriticalFailureStopsDependentActions(t *testing.T) {
	t.Parallel()

	cause := errors.New("primary")
	cleanupErr := errors.New("endpoint still exists")
	var got []string
	var stack Stack
	stack.Defer("release allocation", func() error {
		got = append(got, "release")
		return nil
	})
	stack.DeferCritical("delete endpoint", func() error {
		got = append(got, "delete")
		return cleanupErr
	})

	err := stack.Rollback(cause)
	if !errors.Is(err, cause) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Rollback() error = %v, want both primary and cleanup errors", err)
	}
	if want := []string{"delete"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback actions = %v, want %v", got, want)
	}
}

func TestCommitDisarmsRollback(t *testing.T) {
	t.Parallel()

	called := false
	var stack Stack
	stack.Defer("cleanup", func() error {
		called = true
		return nil
	})
	stack.Commit()

	cause := errors.New("late failure")
	if err := stack.Rollback(cause); !errors.Is(err, cause) {
		t.Fatalf("Rollback() error = %v, want cause", err)
	}
	if called {
		t.Fatal("rollback ran after Commit")
	}
}
