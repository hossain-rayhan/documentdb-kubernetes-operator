// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/documentdb/documentdb-operator/test/longhaul/backup"
	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
	"github.com/documentdb/documentdb-operator/test/longhaul/monitor"
	"github.com/documentdb/documentdb-operator/test/longhaul/operations"
	"github.com/documentdb/documentdb-operator/test/longhaul/report"
	"github.com/documentdb/documentdb-operator/test/longhaul/workload"
)

type snapshotRunner struct {
	snapshot operations.RunSnapshot
	done     chan struct{}
}

func (r *snapshotRunner) Run(context.Context)              {}
func (r *snapshotRunner) Snapshot() operations.RunSnapshot { return r.snapshot }
func (r *snapshotRunner) Done() <-chan struct{}            { return r.done }

func summaryFor(snapshot operations.RunSnapshot, final bool) report.Summary {
	j := journal.New()
	return buildSummary(
		workload.NewMetrics(),
		backup.NewMetrics(),
		monitor.NewLeakDetector(j, 10, 10),
		&snapshotRunner{snapshot: snapshot, done: make(chan struct{})},
		j,
		final,
	)
}

var _ = Describe("buildSummary operation verdicts", func() {
	It("fails random mode when any execution failed", func() {
		summary := summaryFor(operations.RunSnapshot{
			Mode:          config.OperationModeRandom,
			Status:        operations.RunStatusFailed,
			FailureReason: "operation scale-up execute failed: boom",
			Aggregates: []operations.OperationAggregate{
				{Name: "scale-up", Failed: 1},
			},
		}, true)

		Expect(summary.Result).To(Equal(report.ResultFail))
		Expect(summary.FailReason).To(ContainSubstring("execute failed"))
	})

	It("allows an in-progress sequence at a checkpoint", func() {
		summary := summaryFor(operations.RunSnapshot{
			Mode:   config.OperationModeSequence,
			Status: operations.RunStatusRunning,
			Results: []operations.OperationResult{
				{Name: "kill-operator-pod", Status: operations.OperationPending},
			},
		}, false)
		Expect(summary.Result).To(Equal(report.ResultPass))
	})

	It("fails an incomplete requested sequence at final shutdown", func() {
		summary := summaryFor(operations.RunSnapshot{
			Mode:   config.OperationModeSequence,
			Status: operations.RunStatusRunning,
			Results: []operations.OperationResult{
				{Name: "kill-operator-pod", Status: operations.OperationRunning},
				{Name: "kill-primary-pod", Status: operations.OperationPending},
			},
		}, true)

		Expect(summary.Result).To(Equal(report.ResultFail))
		Expect(summary.FailReason).To(ContainSubstring("operation sequence incomplete"))
	})

	It("does not impose an operation completion requirement in disabled mode", func() {
		summary := summaryFor(operations.RunSnapshot{
			Mode:   config.OperationModeDisabled,
			Status: operations.RunStatusDisabled,
		}, true)
		Expect(summary.Result).To(Equal(report.ResultPass))
	})
})
