package errs

import (
	"errors"
	"fmt"
)

type Kind string

const (
	KindInvalidConfig       Kind = "invalid config"
	KindBridgeConflict      Kind = "bridge conflict"
	KindAllocationExhausted Kind = "allocation exhausted"
	KindNamespaceOpen       Kind = "namespace open failed"
	KindVethCreate          Kind = "veth create failed"
	KindEndpointCleanup     Kind = "endpoint cleanup failed"
	KindRouteConfiguration  Kind = "route configuration failed"
	KindStatePersistence    Kind = "state persistence failed"
	KindCheckMismatch       Kind = "check mismatch"
	KindRollback            Kind = "rollback failed"
)

type Error struct {
	Kind Kind
	Op   string
	Err  error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Kind, e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func Wrap(kind Kind, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Err: err}
}

func New(kind Kind, op, message string) error {
	return &Error{Kind: kind, Op: op, Err: errors.New(message)}
}

func IsKind(err error, kind Kind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}
