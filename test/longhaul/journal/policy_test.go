// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package journal

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DisruptionWindow", func() {
	Describe("IsActive", func() {
		It("returns true for an open window (no end time)", func() {
			w := DisruptionWindow{StartTime: time.Now()}
			Expect(w.IsActive()).To(BeTrue())
		})
		It("returns false once EndTime is set", func() {
			now := time.Now()
			w := DisruptionWindow{StartTime: now, EndTime: now.Add(time.Second)}
			Expect(w.IsActive()).To(BeFalse())
		})
	})

	Describe("Duration", func() {
		It("returns end-start for a closed window", func() {
			start := time.Now()
			end := start.Add(7 * time.Second)
			w := DisruptionWindow{StartTime: start, EndTime: end}
			Expect(w.Duration()).To(Equal(7 * time.Second))
		})
		It("returns at-least-since-start for an active window", func() {
			start := time.Now().Add(-3 * time.Second)
			w := DisruptionWindow{StartTime: start}
			Expect(w.Duration()).To(BeNumerically(">=", 3*time.Second))
		})
	})

	DescribeTable("ExceededPolicy",
		func(w DisruptionWindow, want bool) {
			Expect(w.ExceededPolicy()).To(Equal(want))
		},
		Entry("within all budgets",
			DisruptionWindow{
				StartTime:              time.Now().Add(-10 * time.Second),
				EndTime:                time.Now(),
				MaxWriteOutageObserved: 100 * time.Millisecond,
				Policy:                 OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, false),
		Entry("exceeds MustRecoverWithin",
			DisruptionWindow{
				StartTime:              time.Now().Add(-2 * time.Minute),
				EndTime:                time.Now(),
				MaxWriteOutageObserved: 10 * time.Millisecond,
				Policy:                 OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, true),
		Entry("exceeds MaxWriteOutage",
			DisruptionWindow{
				StartTime:              time.Now().Add(-10 * time.Second),
				EndTime:                time.Now(),
				MaxWriteOutageObserved: 2 * time.Second,
				Policy:                 OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, true),
		Entry("boundary: observed outage equal to budget is allowed",
			DisruptionWindow{
				StartTime:              time.Now().Add(-10 * time.Second),
				EndTime:                time.Now(),
				MaxWriteOutageObserved: time.Second,
				Policy:                 OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, false),
		Entry("no write failure means no outage",
			DisruptionWindow{
				StartTime: time.Now().Add(-10 * time.Second),
				EndTime:   time.Now(),
				Policy:    OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, false),
		Entry("still-open outage on a closed window is measured to EndTime",
			DisruptionWindow{
				StartTime:        time.Now().Add(-10 * time.Second),
				EndTime:          time.Now(),
				WriteOutageStart: time.Now().Add(-3 * time.Second),
				Policy:           OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, true),
		Entry("active window also evaluated against MustRecoverWithin",
			DisruptionWindow{
				StartTime: time.Now().Add(-2 * time.Minute),
				Policy:    OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, true),
		Entry("active window with an open outage measured to now",
			DisruptionWindow{
				StartTime:        time.Now().Add(-3 * time.Second),
				WriteOutageStart: time.Now().Add(-2 * time.Second),
				Policy:           OutagePolicy{MustRecoverWithin: time.Minute, MaxWriteOutage: time.Second},
			}, true),
	)

	It("DefaultOutagePolicy returns no zero-valued field", func() {
		p := DefaultOutagePolicy()
		Expect(p.MustRecoverWithin).NotTo(BeZero())
		Expect(p.MaxWriteOutage).NotTo(BeZero())
	})

	It("NoOutagePolicy grants the near-zero cushion and echoes recovery", func() {
		p := NoOutagePolicy(3 * time.Minute)
		Expect(p.MaxWriteOutage).To(Equal(NoOutageWriteOutageCushion))
		Expect(p.MustRecoverWithin).To(Equal(3 * time.Minute))
	})

	It("NoOutagePolicy is far tighter than DefaultOutagePolicy", func() {
		Expect(NoOutageWriteOutageCushion).To(BeNumerically("<", DefaultOutagePolicy().MaxWriteOutage))
	})
})
