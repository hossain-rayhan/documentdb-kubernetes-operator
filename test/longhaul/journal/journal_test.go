// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package journal

import (
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Journal", func() {
	Describe("Record and Events", func() {
		It("preserves levels and length", func() {
			j := New()
			j.Info("test", "first")
			j.Warn("test", "second")
			j.Error("test", "third")

			events := j.Events()
			Expect(events).To(HaveLen(3))
			Expect(events[0].Level).To(Equal(LevelInfo))
			Expect(events[1].Level).To(Equal(LevelWarn))
			Expect(events[2].Level).To(Equal(LevelError))
			Expect(j.Len()).To(Equal(3))
		})

		It("returns only events after a cutoff", func() {
			j := New()
			j.Info("test", "before")
			cutoff := time.Now()
			time.Sleep(2 * time.Millisecond)
			j.Info("test", "after1")
			j.Info("test", "after2")

			Expect(j.EventsSince(cutoff)).To(HaveLen(2))
		})
	})

	Describe("DisruptionWindow lifecycle", func() {
		It("opens, records failures, and closes correctly", func() {
			j := New()
			policy := OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second}

			Expect(j.ActiveWindow()).To(BeNil())

			j.OpenDisruptionWindow("scale-up", policy)
			w := j.ActiveWindow()
			Expect(w).NotTo(BeNil())
			Expect(w.OperationName).To(Equal("scale-up"))
			Expect(w.IsActive()).To(BeTrue())

			j.RecordWriteOutcome(time.Now(), time.Now(), true)
			j.RecordWriteOutcome(time.Now(), time.Now(), true)
			j.RecordWriteOutcome(time.Now(), time.Now(), true)
			Expect(j.ActiveWindow().WriteFailures).To(Equal(int64(3)))

			j.CloseDisruptionWindow()
			Expect(j.ActiveWindow()).To(BeNil())
			closed := j.DisruptionWindows()
			Expect(closed).To(HaveLen(1))
			Expect(closed[0].WriteFailures).To(Equal(int64(3)))
			Expect(closed[0].IsActive()).To(BeFalse())
		})

		It("measures the write outage as first-failure to first-success", func() {
			j := New()
			j.OpenDisruptionWindow("kill-primary", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Minute})

			start := time.Now()
			// Writes fail across a ~200ms span, then recover.
			j.RecordWriteOutcome(start, start, true)
			j.RecordWriteOutcome(start.Add(100*time.Millisecond), start.Add(100*time.Millisecond), true)
			j.RecordWriteOutcome(start.Add(200*time.Millisecond), start.Add(200*time.Millisecond), false) // recovery

			w := j.ActiveWindow()
			Expect(w.WriteFailures).To(Equal(int64(2)))
			Expect(w.WriteOutageStart.IsZero()).To(BeTrue(), "outage should be closed after a success")
			Expect(w.EstimatedWriteOutage()).To(BeNumerically("~", 200*time.Millisecond, 5*time.Millisecond))

			// A single lost write that blocks for the full server-selection
			// timeout must not undercount: one failure whose next success is
			// 30s later still measures the real 30s span (the bug this fixes).
			j.RecordWriteOutcome(start.Add(1*time.Second), start.Add(1*time.Second), true)
			j.RecordWriteOutcome(start.Add(31*time.Second), start.Add(31*time.Second), false)
			Expect(j.ActiveWindow().EstimatedWriteOutage()).To(BeNumerically("~", 30*time.Second, 5*time.Millisecond))
		})

		It("measures the outage to the recovering write's completion, not its start", func() {
			j := New()
			j.OpenDisruptionWindow("kill-primary", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Minute})

			start := time.Now()
			// First write fails, opening the outage at t0.
			j.RecordWriteOutcome(start, start, true)
			// The recovering write starts just 100ms later but blocks in server
			// selection for the full failover, only completing 25s after it
			// began. The outage lasted until that completion (~25.1s), not until
			// the attempt started (~100ms). Measuring from attemptStart would
			// let an over-budget failover slip past the gate.
			recoverStart := start.Add(100 * time.Millisecond)
			recoverEnd := recoverStart.Add(25 * time.Second)
			j.RecordWriteOutcome(recoverStart, recoverEnd, false)

			w := j.ActiveWindow()
			Expect(w.WriteOutageStart.IsZero()).To(BeTrue(), "outage should be closed after a success")
			Expect(w.EstimatedWriteOutage()).To(BeNumerically("~", 25100*time.Millisecond, 5*time.Millisecond))
		})

		It("ignores an out-of-order success that predates the outage start", func() {
			j := New()
			j.OpenDisruptionWindow("kill-primary", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Minute})

			base := time.Now()
			// A slow writer began before the outage (attemptStart = base) but
			// its success is recorded later, after a fast failure opened the
			// outage at base+1s. The stale success must NOT clear the outage.
			j.RecordWriteOutcome(base.Add(1*time.Second), base.Add(1*time.Second), true) // opens outage at +1s
			j.RecordWriteOutcome(base, base, false)                                      // earlier start, later completion

			w := j.ActiveWindow()
			Expect(w.WriteOutageStart.IsZero()).To(BeFalse(), "stale pre-outage success must not close the outage")
			Expect(w.WriteOutageStart).To(Equal(base.Add(1 * time.Second)))

			// The outage remains open and is measured to a genuine later recovery.
			j.RecordWriteOutcome(base.Add(31*time.Second), base.Add(31*time.Second), false)
			Expect(j.ActiveWindow().EstimatedWriteOutage()).To(BeNumerically("~", 30*time.Second, 5*time.Millisecond))
		})

		It("measures from the earliest failing attempt when failures arrive out of order", func() {
			j := New()
			j.OpenDisruptionWindow("kill-primary", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Minute})

			base := time.Now()
			// A failure that began earlier is recorded after one that began
			// later; the outage must span from the earliest failing attempt.
			j.RecordWriteOutcome(base.Add(2*time.Second), base.Add(2*time.Second), true)
			j.RecordWriteOutcome(base, base, true)
			Expect(j.ActiveWindow().WriteOutageStart).To(Equal(base))

			j.RecordWriteOutcome(base.Add(10*time.Second), base.Add(10*time.Second), false)
			Expect(j.ActiveWindow().EstimatedWriteOutage()).To(BeNumerically("~", 10*time.Second, 5*time.Millisecond))
		})

		It("opening a new window closes the previous active window", func() {
			j := New()
			j.OpenDisruptionWindow("op1", DefaultOutagePolicy())
			j.OpenDisruptionWindow("op2", DefaultOutagePolicy())
			Expect(j.ActiveWindow().OperationName).To(Equal("op2"))
			closed := j.DisruptionWindows()
			Expect(closed).To(HaveLen(1))
			Expect(closed[0].OperationName).To(Equal("op1"))
		})

		It("RecordWriteOutcome without an active window is a no-op", func() {
			j := New()
			Expect(func() { j.RecordWriteOutcome(time.Now(), time.Now(), true) }).NotTo(Panic())
		})

		It("bounds closed disruption-window diagnostics to the newest entries", func() {
			j := New()
			total := maxDisruptionWindows + 5
			for i := 0; i < total; i++ {
				j.OpenDisruptionWindow(fmt.Sprintf("op-%d", i), DefaultOutagePolicy())
				j.CloseDisruptionWindow()
			}

			windows := j.DisruptionWindows()
			Expect(windows).To(HaveLen(maxDisruptionWindows))
			Expect(windows[0].OperationName).To(Equal("op-5"))
			Expect(windows[len(windows)-1].OperationName).To(Equal(fmt.Sprintf("op-%d", total-1)))
		})
	})

	Describe("HasPolicyViolation", func() {
		It("returns false on empty journal", func() {
			Expect(New().HasPolicyViolation()).To(BeFalse())
		})

		It("returns false on a closed window within budget", func() {
			j := New()
			j.OpenDisruptionWindow("op", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second})
			j.CloseDisruptionWindow()
			Expect(j.HasPolicyViolation()).To(BeFalse())
		})

		It("returns true on a closed window over write-outage budget", func() {
			j := New()
			j.OpenDisruptionWindow("op", OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: 10 * time.Millisecond})
			// Writes started failing 50ms ago and never recovered, so the
			// outage spans start->close (~50ms), exceeding the 10ms budget.
			j.RecordWriteOutcome(time.Now().Add(-50*time.Millisecond), time.Now().Add(-50*time.Millisecond), true)
			j.CloseDisruptionWindow()
			Expect(j.HasPolicyViolation()).To(BeTrue())
		})

		It("returns true on an active window over time budget", func() {
			j := New()
			j.OpenDisruptionWindow("op", OutagePolicy{MustRecoverWithin: time.Nanosecond, MaxWriteOutage: time.Second})
			time.Sleep(1 * time.Millisecond)
			Expect(j.HasPolicyViolation()).To(BeTrue())
		})
	})

	It("appends concurrently without races (run with -race)", func() {
		j := New()
		var wg sync.WaitGroup
		const writers = 8
		const perWriter = 100
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < perWriter; k++ {
					j.Info("c", "x")
				}
			}()
		}
		wg.Wait()
		Expect(j.Len()).To(Equal(writers * perWriter))
	})

	It("caps the in-memory event ring and keeps the most recent entries", func() {
		// Exceed maxEvents + trimHeadroom so the trim path fires at least once.
		j := New()
		total := maxEvents + trimHeadroom + 500
		for i := 0; i < total; i++ {
			j.Info("c", fmt.Sprintf("%d", i))
		}
		// After amortized trim, length is between maxEvents and maxEvents+trimHeadroom.
		Expect(j.Len()).To(BeNumerically(">=", maxEvents))
		Expect(j.Len()).To(BeNumerically("<=", maxEvents+trimHeadroom))

		events := j.Events()
		// Oldest surviving message is total - len(events); newest is total-1.
		Expect(events[0].Message).To(Equal(fmt.Sprintf("%d", total-len(events))))
		Expect(events[len(events)-1].Message).To(Equal(fmt.Sprintf("%d", total-1)))
	})
})
