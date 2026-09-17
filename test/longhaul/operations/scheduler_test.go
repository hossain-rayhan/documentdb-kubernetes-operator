// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
)

// fakeOp is a minimal Operation for scheduler tests.
type fakeOp struct {
	name      string
	weight    int
	available bool
	executed  int
	err       error
}

func (f *fakeOp) Name() string { return f.name }
func (f *fakeOp) Weight() int  { return f.weight }
func (f *fakeOp) Precondition(_ context.Context) (bool, string) {
	if f.available {
		return true, ""
	}
	return false, "precondition not met"
}
func (f *fakeOp) Execute(_ context.Context) error {
	f.executed++
	return f.err
}
func (f *fakeOp) OutagePolicy() journal.OutagePolicy { return journal.DefaultOutagePolicy() }

// cancelOp cancels the run context from inside Execute and then returns the
// resulting context error, mimicking a bounded random-mode run whose configured
// duration expires while an operation is still waiting for recovery.
type cancelOp struct {
	name   string
	cancel context.CancelFunc
}

func (c *cancelOp) Name() string                                { return c.name }
func (c *cancelOp) Weight() int                                 { return 1 }
func (c *cancelOp) Precondition(context.Context) (bool, string) { return true, "" }
func (c *cancelOp) Execute(ctx context.Context) error {
	c.cancel()
	return ctx.Err()
}
func (c *cancelOp) OutagePolicy() journal.OutagePolicy { return journal.DefaultOutagePolicy() }

func newSchedulerForTest(ops ...Operation) *Scheduler {
	return &Scheduler{
		operations: ops,
		journal:    journal.New(),
		cooldown:   time.Hour,
	}
}

var _ = Describe("Scheduler", func() {
	Describe("selectOperation", func() {
		It("returns nil when no candidates pass precondition", func() {
			s := newSchedulerForTest(&fakeOp{name: "a", weight: 1, available: false})
			Expect(s.selectOperation(context.Background())).To(BeNil())
		})

		It("returns nil when total weight is zero", func() {
			a := &fakeOp{name: "a", weight: 0, available: true}
			s := newSchedulerForTest(a)
			Expect(s.selectOperation(context.Background())).To(BeNil())
		})

		It("picks only operations whose precondition passes", func() {
			a := &fakeOp{name: "a", weight: 1, available: false}
			b := &fakeOp{name: "b", weight: 1, available: true}
			s := newSchedulerForTest(a, b)
			for i := 0; i < 50; i++ {
				got := s.selectOperation(context.Background())
				Expect(got).NotTo(BeNil(), "iter %d", i)
				Expect(got.Name()).To(Equal("b"))
			}
		})

		It("respects relative weights (a:1, b:9 -> b ~ 90%)", func() {
			a := &fakeOp{name: "a", weight: 1, available: true}
			b := &fakeOp{name: "b", weight: 9, available: true}
			s := newSchedulerForTest(a, b)

			const trials = 2000
			bCount := 0
			for i := 0; i < trials; i++ {
				if s.selectOperation(context.Background()).Name() == "b" {
					bCount++
				}
			}
			// Expected 1800; allow generous +/-10% (180) for randomness.
			Expect(bCount).To(BeNumerically(">=", 1620))
			Expect(bCount).To(BeNumerically("<=", 1980))
		})
	})

	Describe("executeOp", func() {
		It("opens and closes a disruption window around the call", func() {
			op := &fakeOp{name: "op", weight: 1, available: true}
			s := newSchedulerForTest(op)
			s.executeOp(context.Background(), op)
			Expect(op.executed).To(Equal(1))
			Expect(s.journal.ActiveWindow()).To(BeNil())
			closed := s.journal.DisruptionWindows()
			Expect(closed).To(HaveLen(1))
			Expect(closed[0].OperationName).To(Equal("op"))
		})

		It("records an ERROR event when Execute fails", func() {
			op := &fakeOp{name: "boom", weight: 1, available: true, err: errors.New("kaboom")}
			s := newSchedulerForTest(op)
			err := s.executeOp(context.Background(), op)
			Expect(err).To(HaveOccurred(), "executeOp must surface the failure so Run can halt the run")

			var sawError bool
			for _, e := range s.journal.Events() {
				if e.Level == journal.LevelError && e.Component == "scheduler" {
					sawError = true
				}
			}
			Expect(sawError).To(BeTrue(), "expected scheduler ERROR event on Execute failure")
		})

		It("classifies parent-context cancellation as a clean interruption, not a failure", func() {
			// A bounded run whose duration expires mid-operation cancels the
			// parent context, which surfaces from Execute as a context error.
			// That normal shutdown must not be reported as an operation failure.
			ctx, cancel := context.WithCancel(context.Background())
			op := &cancelOp{name: "drain", cancel: cancel}
			s := newSchedulerForTest(op)

			err := s.executeOp(ctx, op)
			Expect(errors.Is(err, errRunInterrupted)).To(BeTrue(),
				"a bounded run ending during an operation must not be a failure")

			// It is logged informationally, never as a scheduler ERROR that
			// would surface as a FAIL verdict.
			for _, e := range s.journal.Events() {
				Expect(e.Level).NotTo(Equal(journal.LevelError),
					"parent-context cancellation must not emit a scheduler ERROR")
			}
		})

		It("does not record an interrupted operation as a failure in run state", func() {
			ctx, cancel := context.WithCancel(context.Background())
			op := &cancelOp{name: "drain", cancel: cancel}
			s := NewScheduler([]Operation{op}, nil, journal.New(), time.Hour)

			// Mirror tryExecute's terminal branch: an errRunInterrupted result
			// must skip recordExecution so run state stays non-failed.
			err := s.executeOp(ctx, op)
			Expect(errors.Is(err, errRunInterrupted)).To(BeTrue())
			if !errors.Is(err, errRunInterrupted) {
				s.recordExecution(op.Name(), err)
			}

			snapshot := s.Snapshot()
			Expect(snapshot.Status).NotTo(Equal(RunStatusFailed))
			Expect(snapshot.HasFailure()).To(BeFalse())
		})
	})

	It("OpsExecuted mirrors the internal counter", func() {
		s := newSchedulerForTest()
		Expect(s.OpsExecuted()).To(Equal(0))
		s.opsExecuted = 7
		Expect(s.OpsExecuted()).To(Equal(7))
	})

	It("keeps bounded aggregate counters and exposes failures", func() {
		a := &fakeOp{name: "a"}
		b := &fakeOp{name: "b"}
		s := NewScheduler([]Operation{a, b}, nil, journal.New(), time.Hour)

		for i := 0; i < 1000; i++ {
			s.recordExecution("a", nil)
		}
		s.recordExecution("b", errors.New("first failure"))
		s.recordExecution("b", errors.New("second failure"))

		snapshot := s.Snapshot()
		Expect(snapshot.Aggregates).To(Equal([]OperationAggregate{
			{Name: "a", Passed: 1000},
			{Name: "b", Failed: 2},
		}))
		Expect(snapshot.Status).To(Equal(RunStatusFailed))
		Expect(snapshot.HasFailure()).To(BeTrue())
		Expect(snapshot.FailureReason).To(ContainSubstring("first failure"))
	})
})
