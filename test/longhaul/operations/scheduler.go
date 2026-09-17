// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package operations implements operation runners and individual disruptive
// operations for long haul tests.
package operations

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
	"github.com/documentdb/documentdb-operator/test/longhaul/monitor"
)

// errRunInterrupted signals that an operation did not complete because the
// parent run context was cancelled — e.g. a bounded random-mode run reached its
// configured duration while an operation was still waiting for recovery. This
// is a normal shutdown, not an operation failure, and must never produce a FAIL
// verdict.
var errRunInterrupted = errors.New("run interrupted by context cancellation")

// Operation defines the interface for a disruptive operation.
type Operation interface {
	// Name returns a human-readable identifier for this operation.
	Name() string

	// Weight returns the relative probability of selection (higher = more likely).
	Weight() int

	// Precondition checks if the operation can be executed in the current state.
	Precondition(ctx context.Context) (bool, string)

	// Execute performs the operation and returns when complete.
	Execute(ctx context.Context) error

	// OutagePolicy returns the disruption budget for this operation.
	OutagePolicy() journal.OutagePolicy
}

// Scheduler selects and executes operations based on weighted random selection,
// preconditions, cooldowns, and steady-state gates.
type Scheduler struct {
	operations    []Operation
	healthMonitor *monitor.HealthMonitor
	journal       *journal.Journal
	cooldown      time.Duration

	mu          sync.Mutex
	lastOpTime  time.Time
	opsExecuted int
	inProgress  bool

	state          runnerState
	aggregateIndex map[string]int
}

// NewScheduler creates an operation scheduler.
func NewScheduler(
	ops []Operation,
	health *monitor.HealthMonitor,
	j *journal.Journal,
	cooldown time.Duration,
) *Scheduler {
	aggregates := make([]OperationAggregate, 0, len(ops))
	aggregateIndex := make(map[string]int, len(ops))
	for _, op := range ops {
		if _, exists := aggregateIndex[op.Name()]; exists {
			continue
		}
		aggregateIndex[op.Name()] = len(aggregates)
		aggregates = append(aggregates, OperationAggregate{Name: op.Name()})
	}
	return &Scheduler{
		operations:    ops,
		healthMonitor: health,
		journal:       j,
		cooldown:      cooldown,
		state: newRunnerState(RunSnapshot{
			Mode:       config.OperationModeRandom,
			Status:     RunStatusPending,
			Aggregates: aggregates,
		}),
		aggregateIndex: aggregateIndex,
	}
}

// Run starts the scheduler loop. It blocks until context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.journal.Info("scheduler", "operation scheduler started")
	s.state.mu.Lock()
	s.state.snapshot.Status = RunStatusRunning
	s.state.mu.Unlock()
	defer func() {
		s.state.mu.Lock()
		if s.state.snapshot.Status == RunStatusRunning {
			s.state.snapshot.Status = RunStatusComplete
		}
		s.state.mu.Unlock()
		s.state.closeDone()
		s.journal.Info("scheduler", "operation scheduler stopped")
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.tryExecute(ctx); err != nil {
				if errors.Is(err, errRunInterrupted) {
					// A bounded run reached its configured duration during an
					// operation. That is a clean shutdown, not a failure, so
					// exit quietly without emitting a FAIL-inducing error.
					return
				}
				// A terminal operation failure ends the run immediately so the
				// FAIL verdict is emitted promptly. In production MaxDuration is
				// unbounded, so without this the loop would run forever and the
				// failure would never surface.
				s.journal.Error("scheduler", fmt.Sprintf("halting run after operation failure: %v", err))
				return
			}
		}
	}
}

func (s *Scheduler) tryExecute(ctx context.Context) error {
	s.mu.Lock()
	if s.inProgress {
		s.mu.Unlock()
		return nil
	}

	// Check cooldown.
	if !s.lastOpTime.IsZero() && time.Since(s.lastOpTime) < s.cooldown {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Check steady-state gate.
	if !s.healthMonitor.IsSteadyState() {
		return nil
	}

	// Select an operation.
	op := s.selectOperation(ctx)
	if op == nil {
		return nil
	}

	// Execute.
	s.mu.Lock()
	s.inProgress = true
	s.mu.Unlock()

	err := s.executeOp(ctx, op)

	s.mu.Lock()
	s.inProgress = false
	s.lastOpTime = time.Now()
	s.opsExecuted++
	s.mu.Unlock()

	if errors.Is(err, errRunInterrupted) {
		// Normal shutdown that landed mid-operation: do not record it as an
		// operation failure. Propagate so the run loop exits quietly.
		return err
	}
	s.recordExecution(op.Name(), err)
	return err
}

func (s *Scheduler) selectOperation(ctx context.Context) Operation {
	// Filter by preconditions and build weighted list.
	type candidate struct {
		op     Operation
		weight int
	}
	var candidates []candidate
	totalWeight := 0

	for _, op := range s.operations {
		ok, _ := op.Precondition(ctx)
		if ok {
			w := op.Weight()
			candidates = append(candidates, candidate{op: op, weight: w})
			totalWeight += w
		}
	}

	if len(candidates) == 0 || totalWeight == 0 {
		return nil
	}

	// Weighted random selection.
	r := rand.IntN(totalWeight)
	for _, c := range candidates {
		r -= c.weight
		if r < 0 {
			return c.op
		}
	}
	return candidates[len(candidates)-1].op
}

func (s *Scheduler) executeOp(ctx context.Context, op Operation) error {
	s.journal.Info("scheduler", fmt.Sprintf("executing operation: %s", op.Name()))
	s.journal.OpenDisruptionWindow(op.Name(), op.OutagePolicy())
	// Reset the steady-state epoch so the operation's internal recovery wait
	// and the next scheduler-tick steady-state gate must observe a health
	// sample taken after this disruption, not a stale pre-operation one.
	if s.healthMonitor != nil {
		s.healthMonitor.InvalidateSteadyState()
	}

	err := op.Execute(ctx)
	window := s.journal.CloseDisruptionWindow()

	if err != nil {
		// A bounded run that reaches its configured duration cancels the parent
		// context, which propagates into the operation's recovery wait as an
		// error. Distinguish that normal shutdown from a genuine operation
		// failure: a healthy timed run must not FAIL merely because its
		// deadline landed while an operation was in flight.
		if ctx.Err() != nil {
			s.journal.Info("scheduler", fmt.Sprintf("operation %s interrupted by shutdown: %v", op.Name(), err))
			return errRunInterrupted
		}
		s.journal.Error("scheduler", fmt.Sprintf("operation %s failed: %v", op.Name(), err))
		return fmt.Errorf("operation %s execute failed: %w", op.Name(), err)
	}
	if window == nil {
		err = fmt.Errorf("operation %s closed without a disruption window", op.Name())
		s.journal.Error("scheduler", err.Error())
		return err
	}
	if window.ExceededPolicy() {
		err = fmt.Errorf("operation %s exceeded its outage policy", op.Name())
		s.journal.Error("scheduler", err.Error())
		return err
	}

	s.journal.Info("scheduler", fmt.Sprintf("operation %s completed successfully", op.Name()))
	return nil
}

func (s *Scheduler) recordExecution(name string, err error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	index, ok := s.aggregateIndex[name]
	if !ok {
		return
	}
	if err != nil {
		s.state.snapshot.Aggregates[index].Failed++
		s.state.snapshot.Status = RunStatusFailed
		if s.state.snapshot.FailureReason == "" {
			s.state.snapshot.FailureReason = err.Error()
		}
		return
	}
	s.state.snapshot.Aggregates[index].Passed++
}

// OpsExecuted returns the number of operations completed.
func (s *Scheduler) OpsExecuted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opsExecuted
}

// Snapshot returns bounded aggregate counters in registration order.
func (s *Scheduler) Snapshot() RunSnapshot {
	return s.state.Snapshot()
}

// Done closes when the scheduler stops after context cancellation.
func (s *Scheduler) Done() <-chan struct{} {
	return s.state.Done()
}
