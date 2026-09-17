// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
)

type sequenceTestOp struct {
	name         string
	execute      func(context.Context) error
	precondition func(context.Context) (bool, string)
	policy       journal.OutagePolicy
}

func (o *sequenceTestOp) Name() string { return o.name }
func (o *sequenceTestOp) Weight() int  { return 1 }
func (o *sequenceTestOp) Precondition(ctx context.Context) (bool, string) {
	if o.precondition != nil {
		return o.precondition(ctx)
	}
	return true, ""
}
func (o *sequenceTestOp) Execute(ctx context.Context) error {
	if o.execute != nil {
		return o.execute(ctx)
	}
	return nil
}
func (o *sequenceTestOp) OutagePolicy() journal.OutagePolicy {
	if o.policy.MustRecoverWithin != 0 || o.policy.MaxWriteOutage != 0 {
		return o.policy
	}
	return journal.DefaultOutagePolicy()
}

type sequenceTestGate struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (g *sequenceTestGate) WaitForSteadyState(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.err
}

func (g *sequenceTestGate) InvalidateSteadyState() {}

func runSequence(runner *SequenceRunner, ctx context.Context) RunSnapshot {
	go runner.Run(ctx)
	Eventually(runner.Done()).Should(BeClosed())
	return runner.Snapshot()
}

var _ = Describe("SequenceRunner", func() {
	It("executes each operation exactly once in exact order", func() {
		var order []string
		ops := []Operation{
			&sequenceTestOp{name: "first", execute: func(context.Context) error {
				order = append(order, "first")
				return nil
			}},
			&sequenceTestOp{name: "second", execute: func(context.Context) error {
				order = append(order, "second")
				return nil
			}},
		}
		gate := &sequenceTestGate{}
		snapshot := runSequence(NewSequenceRunner(ops, gate, journal.New(), time.Second), context.Background())

		Expect(order).To(Equal([]string{"first", "second"}))
		Expect(snapshot.Status).To(Equal(RunStatusComplete))
		Expect(snapshot.Results).To(Equal([]OperationResult{
			{Name: "first", Status: OperationPassed},
			{Name: "second", Status: OperationPassed},
		}))
		Expect(gate.calls).To(Equal(4), "initial and post-recovery gate for each operation")
	})

	It("records an execute error and stops before later operations", func() {
		var order []string
		ops := []Operation{
			&sequenceTestOp{name: "first", execute: func(context.Context) error {
				order = append(order, "first")
				return nil
			}},
			&sequenceTestOp{name: "broken", execute: func(context.Context) error {
				order = append(order, "broken")
				return errors.New("kaboom")
			}},
			&sequenceTestOp{name: "never", execute: func(context.Context) error {
				order = append(order, "never")
				return nil
			}},
		}
		snapshot := runSequence(
			NewSequenceRunner(ops, &sequenceTestGate{}, journal.New(), time.Second),
			context.Background(),
		)

		Expect(order).To(Equal([]string{"first", "broken"}))
		Expect(snapshot.Status).To(Equal(RunStatusFailed))
		Expect(snapshot.FailureReason).To(ContainSubstring("kaboom"))
		Expect(snapshot.Results).To(Equal([]OperationResult{
			{Name: "first", Status: OperationPassed},
			{Name: "broken", Status: OperationFailed, Error: snapshot.FailureReason},
			{Name: "never", Status: OperationPending},
		}))
	})

	It("fails deterministically when a precondition times out", func() {
		op := &sequenceTestOp{name: "blocked"}
		runner := NewSequenceRunner([]Operation{op}, &sequenceTestGate{}, journal.New(), time.Second)
		runner.waitForPrecondition = func(context.Context, Operation) error {
			return context.DeadlineExceeded
		}

		snapshot := runSequence(runner, context.Background())
		Expect(snapshot.Status).To(Equal(RunStatusFailed))
		Expect(snapshot.Results[0].Status).To(Equal(OperationFailed))
		Expect(snapshot.FailureReason).To(ContainSubstring("precondition timeout"))
	})

	It("marks cancellation as incomplete and leaves later operations pending", func() {
		started := make(chan struct{})
		op := &sequenceTestOp{name: "cancelled", execute: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}
		runner := NewSequenceRunner(
			[]Operation{op, &sequenceTestOp{name: "never"}},
			&sequenceTestGate{},
			journal.New(),
			time.Minute,
		)
		ctx, cancel := context.WithCancel(context.Background())
		go runner.Run(ctx)
		Eventually(started).Should(BeClosed())
		cancel()
		Eventually(runner.Done()).Should(BeClosed())

		snapshot := runner.Snapshot()
		Expect(snapshot.Status).To(Equal(RunStatusIncomplete))
		Expect(snapshot.Results[0].Status).To(Equal(OperationFailed))
		Expect(snapshot.Results[1].Status).To(Equal(OperationPending))
		Expect(snapshot.FailureReason).To(ContainSubstring("incomplete"))
	})

	It("publishes a terminal incomplete snapshot when the watchdog wins", func() {
		runner := NewSequenceRunner(
			[]Operation{
				&sequenceTestOp{name: "first"},
				&sequenceTestOp{name: "second"},
			},
			&sequenceTestGate{},
			journal.New(),
			time.Minute,
		)

		runner.MarkIncomplete("watchdog fired")
		Expect(runner.Done()).To(BeClosed())
		snapshot := runner.Snapshot()
		Expect(snapshot.Status).To(Equal(RunStatusIncomplete))
		Expect(snapshot.FailureReason).To(Equal("watchdog fired"))
		Expect(snapshot.Results).To(Equal([]OperationResult{
			{Name: "first", Status: OperationFailed, Error: "watchdog fired"},
			{Name: "second", Status: OperationPending},
		}))
	})

	It("preserves completion when the watchdog races after every operation passed", func() {
		runner := NewSequenceRunner(
			[]Operation{&sequenceTestOp{name: "done"}},
			&sequenceTestGate{},
			journal.New(),
			time.Second,
		)
		runner.state.snapshot.Results[0].Status = OperationPassed

		runner.MarkIncomplete("watchdog fired")

		snapshot := runner.Snapshot()
		Expect(snapshot.Status).To(Equal(RunStatusComplete))
		Expect(snapshot.Results).To(Equal([]OperationResult{{
			Name:   "done",
			Status: OperationPassed,
		}}))
	})

	It("fails when the closed disruption window exceeds policy", func() {
		op := &sequenceTestOp{
			name: "policy",
			policy: journal.OutagePolicy{
				MaxWriteOutage:    time.Hour,
				MustRecoverWithin: -time.Nanosecond,
			},
		}
		snapshot := runSequence(
			NewSequenceRunner([]Operation{op}, &sequenceTestGate{}, journal.New(), time.Second),
			context.Background(),
		)

		Expect(snapshot.Status).To(Equal(RunStatusFailed))
		Expect(snapshot.FailureReason).To(ContainSubstring("exceeded its outage policy"))
	})
})
