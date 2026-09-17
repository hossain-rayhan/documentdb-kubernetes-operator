// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	"context"
	"fmt"
	"time"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
)

const defaultPreconditionPollInterval = time.Second

// SteadyStateGate is the health-monitor surface needed by sequence mode.
type SteadyStateGate interface {
	WaitForSteadyState(ctx context.Context) error
	InvalidateSteadyState()
}

type preconditionWaitFunc func(context.Context, Operation) error

// SequenceRunner executes each requested operation exactly once and in order.
type SequenceRunner struct {
	operations      []Operation
	steadyStateGate SteadyStateGate
	journal         *journal.Journal
	recoveryTimeout time.Duration
	state           runnerState

	waitForPrecondition preconditionWaitFunc
	terminal            bool
}

// NewSequenceRunner creates a deterministic sequential operation runner.
func NewSequenceRunner(
	ops []Operation,
	gate SteadyStateGate,
	j *journal.Journal,
	recoveryTimeout time.Duration,
) *SequenceRunner {
	results := make([]OperationResult, len(ops))
	for i, op := range ops {
		results[i] = OperationResult{Name: op.Name(), Status: OperationPending}
	}
	runner := &SequenceRunner{
		operations:      append([]Operation(nil), ops...),
		steadyStateGate: gate,
		journal:         j,
		recoveryTimeout: recoveryTimeout,
		state: newRunnerState(RunSnapshot{
			Mode:    config.OperationModeSequence,
			Status:  RunStatusPending,
			Results: results,
		}),
	}
	runner.waitForPrecondition = runner.pollPrecondition
	return runner
}

// Run executes the configured sequence and stops on the first failure.
func (r *SequenceRunner) Run(ctx context.Context) {
	defer r.state.closeDone()

	r.state.mu.Lock()
	if r.terminal {
		r.state.mu.Unlock()
		return
	}
	r.state.snapshot.Status = RunStatusRunning
	r.state.mu.Unlock()

	for i, op := range r.operations {
		if !r.setResult(i, OperationRunning, "") {
			return
		}
		if err := r.runOne(ctx, op); err != nil {
			status := RunStatusFailed
			reason := err.Error()
			if ctx.Err() != nil {
				status = RunStatusIncomplete
				reason = fmt.Sprintf("operation sequence incomplete during %s: %v", op.Name(), ctx.Err())
			}
			r.setFailure(i, status, reason)
			return
		}
		if !r.setResult(i, OperationPassed, "") {
			return
		}
	}

	r.state.mu.Lock()
	if !r.terminal {
		r.terminal = true
		r.state.snapshot.Status = RunStatusComplete
	}
	r.state.mu.Unlock()
}

func (r *SequenceRunner) runOne(ctx context.Context, op Operation) error {
	if r.steadyStateGate == nil {
		return fmt.Errorf("operation %s steady-state gate is nil", op.Name())
	}

	steadyCtx, cancelSteady := context.WithTimeout(ctx, r.recoveryTimeout)
	err := r.steadyStateGate.WaitForSteadyState(steadyCtx)
	cancelSteady()
	if err != nil {
		return fmt.Errorf("operation %s initial steady-state gate failed: %w", op.Name(), err)
	}

	preconditionCtx, cancelPrecondition := context.WithTimeout(ctx, r.recoveryTimeout)
	err = r.waitForPrecondition(preconditionCtx, op)
	cancelPrecondition()
	if err != nil {
		return fmt.Errorf("operation %s precondition timeout: %w", op.Name(), err)
	}

	r.journal.Info("sequence", fmt.Sprintf("executing operation: %s", op.Name()))
	r.journal.OpenDisruptionWindow(op.Name(), op.OutagePolicy())
	// Reset the steady-state epoch so the post-recovery gate (and the
	// operation's own internal steady-state wait) must observe a health
	// sample taken after the disruption is opened, rather than being
	// satisfied instantly by the pre-operation steadySince.
	r.steadyStateGate.InvalidateSteadyState()

	executeCtx, cancelExecute := context.WithTimeout(ctx, r.recoveryTimeout)
	executeErr := op.Execute(executeCtx)
	cancelExecute()
	window := r.journal.CloseDisruptionWindow()

	if executeErr != nil {
		r.journal.Error("sequence", fmt.Sprintf("operation %s failed: %v", op.Name(), executeErr))
		return fmt.Errorf("operation %s execute failed: %w", op.Name(), executeErr)
	}
	if window == nil {
		return fmt.Errorf("operation %s closed without a disruption window", op.Name())
	}
	if window.ExceededPolicy() {
		err := fmt.Errorf("operation %s exceeded its outage policy", op.Name())
		r.journal.Error("sequence", err.Error())
		return err
	}

	recoveryCtx, cancelRecovery := context.WithTimeout(ctx, r.recoveryTimeout)
	err = r.steadyStateGate.WaitForSteadyState(recoveryCtx)
	cancelRecovery()
	if err != nil {
		return fmt.Errorf("operation %s post-recovery steady-state gate failed: %w", op.Name(), err)
	}

	r.journal.Info("sequence", fmt.Sprintf("operation %s completed successfully", op.Name()))
	return nil
}

func (r *SequenceRunner) pollPrecondition(ctx context.Context, op Operation) error {
	ticker := time.NewTicker(defaultPreconditionPollInterval)
	defer ticker.Stop()

	lastReason := "precondition not met"
	for {
		ok, reason := op.Precondition(ctx)
		if ok {
			return nil
		}
		if reason != "" {
			lastReason = reason
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", lastReason, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *SequenceRunner) setResult(index int, status OperationResultStatus, reason string) bool {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if r.terminal {
		return false
	}
	r.state.snapshot.Results[index].Status = status
	r.state.snapshot.Results[index].Error = reason
	return true
}

func (r *SequenceRunner) setFailure(index int, status RunStatus, reason string) {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if r.terminal {
		return
	}
	r.terminal = true
	r.state.snapshot.Status = status
	r.state.snapshot.FailureReason = reason
	r.state.snapshot.Results[index].Status = OperationFailed
	r.state.snapshot.Results[index].Error = reason
}

// MarkIncomplete terminally fails a sequence whose watchdog or shutdown
// cancellation fired before Run could publish its own terminal snapshot.
func (r *SequenceRunner) MarkIncomplete(reason string) {
	r.state.mu.Lock()
	if r.terminal {
		r.state.mu.Unlock()
		return
	}
	allPassed := len(r.state.snapshot.Results) > 0
	for _, result := range r.state.snapshot.Results {
		if result.Status != OperationPassed {
			allPassed = false
			break
		}
	}
	if allPassed {
		r.terminal = true
		r.state.snapshot.Status = RunStatusComplete
		r.state.mu.Unlock()
		r.state.closeDone()
		return
	}
	r.terminal = true
	r.state.snapshot.Status = RunStatusIncomplete
	r.state.snapshot.FailureReason = reason
	for i := range r.state.snapshot.Results {
		if r.state.snapshot.Results[i].Status == OperationRunning {
			r.state.snapshot.Results[i].Status = OperationFailed
			r.state.snapshot.Results[i].Error = reason
			break
		}
		if r.state.snapshot.Results[i].Status == OperationPending {
			r.state.snapshot.Results[i].Status = OperationFailed
			r.state.snapshot.Results[i].Error = reason
			break
		}
	}
	r.state.mu.Unlock()
	r.state.closeDone()
}

// Snapshot returns a deterministic copy ordered by the configured sequence.
func (r *SequenceRunner) Snapshot() RunSnapshot {
	return r.state.Snapshot()
}

// Done closes when the sequence completes or stops on failure/cancellation.
func (r *SequenceRunner) Done() <-chan struct{} {
	return r.state.Done()
}
