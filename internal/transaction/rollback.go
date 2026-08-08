package transaction

import (
	"errors"
	"fmt"
	"strings"
)

type action struct {
	name          string
	fn            func() error
	stopOnFailure bool
}

// Stack records compensating actions and runs them in last-in, first-out order.
type Stack struct {
	actions   []action
	committed bool
}

func (s *Stack) Defer(name string, fn func() error) {
	s.deferAction(name, fn, false)
}

// DeferCritical records a compensation that must succeed before earlier
// actions are safe to run. A failure stops rollback at this action.
func (s *Stack) DeferCritical(name string, fn func() error) {
	s.deferAction(name, fn, true)
}

func (s *Stack) deferAction(name string, fn func() error, stopOnFailure bool) {
	if s.committed || fn == nil {
		return
	}
	s.actions = append(s.actions, action{name: name, fn: fn, stopOnFailure: stopOnFailure})
}

func (s *Stack) Commit() {
	s.committed = true
	s.actions = nil
}

func (s *Stack) Rollback(cause error) error {
	if s.committed {
		return cause
	}

	rollbackErr := &RollbackError{Cause: cause}
	for i := len(s.actions) - 1; i >= 0; i-- {
		entry := s.actions[i]
		if err := entry.fn(); err != nil {
			rollbackErr.Failures = append(rollbackErr.Failures, Failure{
				Action: entry.name,
				Err:    err,
			})
			if entry.stopOnFailure {
				break
			}
		}
	}
	s.actions = nil

	if len(rollbackErr.Failures) == 0 {
		return cause
	}
	return rollbackErr
}

type Failure struct {
	Action string
	Err    error
}

type RollbackError struct {
	Cause    error
	Failures []Failure
}

func (e *RollbackError) Error() string {
	parts := make([]string, 0, len(e.Failures)+1)
	if e.Cause != nil {
		parts = append(parts, e.Cause.Error())
	}
	for _, failure := range e.Failures {
		parts = append(parts, fmt.Sprintf("rollback %s: %v", failure.Action, failure.Err))
	}
	return strings.Join(parts, "; ")
}

func (e *RollbackError) Unwrap() error {
	result := make([]error, 0, len(e.Failures)+1)
	if e.Cause != nil {
		result = append(result, e.Cause)
	}
	for _, failure := range e.Failures {
		result = append(result, failure.Err)
	}
	return errors.Join(result...)
}

func (e *RollbackError) JoinedFailure() error {
	result := make([]error, 0, len(e.Failures))
	for _, failure := range e.Failures {
		result = append(result, fmt.Errorf("%s: %w", failure.Action, failure.Err))
	}
	return errors.Join(result...)
}
