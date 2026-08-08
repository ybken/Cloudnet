package errs

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapPreservesCauseAndContext(t *testing.T) {
	t.Parallel()

	cause := errors.New("permission denied")
	err := Wrap(KindStatePersistence, "write endpoint state", cause)

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
	if !IsKind(err, KindStatePersistence) {
		t.Fatalf("IsKind(%v, %q) = false", err, KindStatePersistence)
	}
	for _, want := range []string{"state persistence failed", "write endpoint state", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestWrapNilReturnsNil(t *testing.T) {
	t.Parallel()

	if err := Wrap(KindInvalidConfig, "parse", nil); err != nil {
		t.Fatalf("Wrap(..., nil) = %v, want nil", err)
	}
}
