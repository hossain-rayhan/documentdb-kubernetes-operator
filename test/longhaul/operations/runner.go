// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	"context"
	"sync"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
)

// RunStatus is the bounded lifecycle state of the operation runner.
type RunStatus string

const (
	RunStatusPending    RunStatus = "PENDING"
	RunStatusRunning    RunStatus = "RUNNING"
	RunStatusComplete   RunStatus = "COMPLETE"
	RunStatusFailed     RunStatus = "FAILED"
	RunStatusIncomplete RunStatus = "INCOMPLETE"
	RunStatusDisabled   RunStatus = "DISABLED"
)

// OperationResultStatus is the state of one requested sequence operation.
type OperationResultStatus string

const (
	OperationPending OperationResultStatus = "PENDING"
	OperationRunning OperationResultStatus = "RUNNING"
	OperationPassed  OperationResultStatus = "PASSED"
	OperationFailed  OperationResultStatus = "FAILED"
)

// OperationResult is the single mutable result for one requested sequence item.
type OperationResult struct {
	Name   string                `json:"name"`
	Status OperationResultStatus `json:"status"`
	Error  string                `json:"error,omitempty"`
}

// OperationAggregate bounds random-mode history to counters per operation type.
type OperationAggregate struct {
	Name   string `json:"name"`
	Passed int    `json:"passed"`
	Failed int    `json:"failed"`
}

// RunSnapshot is a concurrency-safe value snapshot of operation execution.
type RunSnapshot struct {
	Mode          config.OperationMode `json:"mode"`
	Status        RunStatus            `json:"status"`
	Results       []OperationResult    `json:"results,omitempty"`
	Aggregates    []OperationAggregate `json:"aggregates,omitempty"`
	FailureReason string               `json:"failureReason,omitempty"`
}

// OpsExecuted returns the number of terminal operation attempts.
func (s RunSnapshot) OpsExecuted() int {
	if s.Mode == config.OperationModeSequence {
		count := 0
		for _, result := range s.Results {
			if result.Status == OperationPassed || result.Status == OperationFailed {
				count++
			}
		}
		return count
	}

	count := 0
	for _, aggregate := range s.Aggregates {
		count += aggregate.Passed + aggregate.Failed
	}
	return count
}

// HasFailure reports whether an operation attempt or sequence lifecycle failed.
func (s RunSnapshot) HasFailure() bool {
	if s.Status == RunStatusFailed || s.Status == RunStatusIncomplete {
		return true
	}
	for _, aggregate := range s.Aggregates {
		if aggregate.Failed > 0 {
			return true
		}
	}
	return false
}

// Runner is the common reporting and lifecycle surface for every operation mode.
type Runner interface {
	Run(ctx context.Context)
	Snapshot() RunSnapshot
	Done() <-chan struct{}
}

type runnerState struct {
	mu       sync.RWMutex
	snapshot RunSnapshot
	done     chan struct{}
	doneOnce sync.Once
}

func newRunnerState(snapshot RunSnapshot) runnerState {
	return runnerState{snapshot: snapshot, done: make(chan struct{})}
}

func (s *runnerState) Snapshot() RunSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := s.snapshot
	snapshot.Results = append([]OperationResult(nil), s.snapshot.Results...)
	snapshot.Aggregates = append([]OperationAggregate(nil), s.snapshot.Aggregates...)
	return snapshot
}

func (s *runnerState) Done() <-chan struct{} {
	return s.done
}

func (s *runnerState) closeDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

// DisabledRunner performs no operations and has no completion requirement.
type DisabledRunner struct {
	state runnerState
}

// NewDisabledRunner creates a runner for disabled operation mode.
func NewDisabledRunner() *DisabledRunner {
	return &DisabledRunner{state: newRunnerState(RunSnapshot{
		Mode:   config.OperationModeDisabled,
		Status: RunStatusDisabled,
	})}
}

// Run waits for shutdown without scheduling operations.
func (r *DisabledRunner) Run(ctx context.Context) {
	<-ctx.Done()
	r.state.closeDone()
}

// Snapshot returns the disabled runner state.
func (r *DisabledRunner) Snapshot() RunSnapshot {
	return r.state.Snapshot()
}

// Done closes when Run returns after cancellation.
func (r *DisabledRunner) Done() <-chan struct{} {
	return r.state.Done()
}
