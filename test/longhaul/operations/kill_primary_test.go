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

type successfulSteadyGate struct {
	calls  int
	onWait func()
}

func (g *successfulSteadyGate) WaitForSteadyState(context.Context) error {
	g.calls++
	if g.onWait != nil {
		g.onWait()
	}
	return nil
}

func (g *successfulSteadyGate) InvalidateSteadyState() {}

var _ = Describe("KillPrimaryPod", func() {
	It("Name is kill-primary-pod and Weight is 2", func() {
		k := NewKillPrimaryPod(&fakeClient{}, nil, time.Minute)
		Expect(k.Name()).To(Equal("kill-primary-pod"))
		Expect(k.Weight()).To(Equal(2))
	})

	It("OutagePolicy shares the single-primary-handover budget with upgrade", func() {
		k := NewKillPrimaryPod(&fakeClient{}, nil, 3*time.Minute)
		p := k.OutagePolicy()
		Expect(p.MaxWriteOutage).To(Equal(journal.PrimaryHandoverWriteOutage))
		Expect(p.MustRecoverWithin).To(Equal(3 * time.Minute))
	})

	DescribeTable("Precondition",
		func(ipn int, ipnErr error, wantOK bool, wantReasonHas string) {
			c := &fakeClient{instancesPerNode: ipn, ipnErr: ipnErr}
			k := NewKillPrimaryPod(c, nil, time.Minute)

			ok, reason := k.Precondition(context.Background())
			Expect(ok).To(Equal(wantOK), "reason=%q", reason)
			if wantReasonHas != "" {
				Expect(reason).To(ContainSubstring(wantReasonHas))
			}
		},
		Entry("single-instance: ipn=1 -> skip", 1, nil, false, "no HA standby"),
		Entry("read error -> skip", 0, errors.New("boom"), false, "cannot read instancesPerNode"),
		Entry("HA: ipn=2 -> eligible", 2, nil, true, ""),
		Entry("HA: ipn=3 -> eligible", 3, nil, true, ""),
	)

	It("Execute deletes the original primary and verifies a different primary", func() {
		c := &fakeClient{
			instancesPerNode:   2,
			primary:            "cluster-1",
			replacementPrimary: "cluster-2",
		}
		gate := &successfulSteadyGate{}
		k := NewKillPrimaryPod(c, gate, time.Second)

		Expect(k.Execute(context.Background())).To(Succeed())
		c.mu.Lock()
		defer c.mu.Unlock()
		Expect(c.deletedPods).To(ConsistOf("cluster-1"))
		Expect(c.primary).To(Equal("cluster-2"))
		Expect(gate.calls).To(Equal(1))
	})

	It("fails when CNPG keeps reporting the deleted primary", func() {
		c := &fakeClient{instancesPerNode: 2, primary: "cluster-1"}
		gate := &successfulSteadyGate{}
		k := NewKillPrimaryPod(c, gate, 20*time.Millisecond)
		k.primaryPollInterval = time.Millisecond

		err := k.Execute(context.Background())
		Expect(err).To(MatchError(ContainSubstring(`primary did not change from "cluster-1"`)))
		Expect(gate.calls).To(Equal(0), "steady-state recovery must wait until primary change is proven")
	})

	It("fails if the recovered cluster reports the original primary again", func() {
		c := &fakeClient{
			instancesPerNode:   2,
			primary:            "cluster-1",
			replacementPrimary: "cluster-2",
		}
		gate := &successfulSteadyGate{onWait: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.primary = "cluster-1"
		}}
		k := NewKillPrimaryPod(c, gate, time.Second)

		err := k.Execute(context.Background())
		Expect(err).To(MatchError(ContainSubstring("expected a non-empty primary different")))
	})

	It("aborts without deleting if the primary changes before the delete", func() {
		c := &fakeClient{
			instancesPerNode:   2,
			primary:            "cluster-1",
			replacementPrimary: "cluster-9",
		}
		// Simulate an unrelated failover landing between the initial read and
		// the pre-delete re-read: the first GetPrimaryInstance returns
		// "cluster-1" and then promotes "cluster-2", so the re-read sees the
		// changed primary. The operation must abort rather than delete the
		// former primary and mistake the pre-existing promotion for its own.
		c.getPrimaryHook = func() {
			c.primary = "cluster-2"
			c.getPrimaryHook = nil
		}
		gate := &successfulSteadyGate{}
		k := NewKillPrimaryPod(c, gate, time.Second)

		err := k.Execute(context.Background())
		Expect(err).To(MatchError(ContainSubstring("unexpected external failover")))
		c.mu.Lock()
		defer c.mu.Unlock()
		Expect(c.deletedPods).To(BeEmpty(), "must not delete a pod that is no longer the primary")
		Expect(gate.calls).To(Equal(0))
	})

	It("Execute fails without deleting when the primary is unknown", func() {
		c := &fakeClient{instancesPerNode: 2, primaryErr: errors.New("no primary")}
		k := NewKillPrimaryPod(c, nil, time.Second)

		err := k.Execute(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("get primary instance"))
		c.mu.Lock()
		defer c.mu.Unlock()
		Expect(c.deletedPods).To(BeEmpty())
	})

	It("Execute fails without deleting when the primary name is empty", func() {
		c := &fakeClient{instancesPerNode: 2, primary: ""}
		k := NewKillPrimaryPod(c, nil, time.Second)

		err := k.Execute(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty primary pod name"))
		c.mu.Lock()
		defer c.mu.Unlock()
		Expect(c.deletedPods).To(BeEmpty())
	})
})
