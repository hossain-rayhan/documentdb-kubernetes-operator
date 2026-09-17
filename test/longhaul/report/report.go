// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/documentdb/documentdb-operator/test/longhaul/backup"
	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
	"github.com/documentdb/documentdb-operator/test/longhaul/monitor"
	"github.com/documentdb/documentdb-operator/test/longhaul/operations"
	"github.com/documentdb/documentdb-operator/test/longhaul/workload"
)

// Result is the terminal verdict of the test run.
type Result string

const (
	ResultPass Result = "PASS"
	ResultFail Result = "FAIL"
)

// Summary is the full state needed to render a checkpoint or final report.
// It is a pure value snapshot — no live counters, no channels — so it can be
// passed across goroutines and re-rendered offline.
type Summary struct {
	// Result is the current verdict. It flips to FAIL for durability errors,
	// operation failures/incomplete sequences, or outage-policy violations.
	Result Result

	// Duration is wall-clock time since the run started (process StartTime),
	// not since the cluster was created. Resets on pod restart.
	Duration time.Duration

	// Metrics is a snapshot of the workload counters (writes attempted/acked/
	// failed, verify passes, gaps, checksum errors).
	Metrics workload.MetricsSnapshot

	// Backup is a snapshot of the data-protection counters (backups
	// scheduled/completed/failed, live population, retention leaks). A
	// retention leak flips Result to FAIL.
	Backup backup.MetricsSnapshot

	// LeakAnalysis is the operator-pod resource trend (memory/CPU slope over
	// the run); LeakAnalysis.HasLeak being true does NOT flip Result — it
	// only emits a warning annotation.
	LeakAnalysis monitor.LeakAnalysis

	// OpsExecuted is the count of terminal operation attempts since startup.
	OpsExecuted int

	// OperationRun is the bounded sequence result or random aggregate snapshot.
	OperationRun operations.RunSnapshot

	// Windows is the journal's bounded set of recent closed disruption windows,
	// in start order.
	Windows []journal.DisruptionWindow

	// Events is the journal's full event ring (info/warn/error log lines).
	// The renderer only includes the last 20 in the markdown body to keep
	// the ConfigMap value under the 1 MiB limit.
	Events []journal.Event

	// FailReason is a short human-readable cause when Result == FAIL
	// (e.g. "data loss: 17 gaps detected"). Empty when Result == PASS.
	FailReason string
}

// GenerateMarkdown produces a human-readable markdown report.
func GenerateMarkdown(s Summary) string {
	var b strings.Builder

	b.WriteString("# Long Haul Test Report\n\n")

	// Header
	fmt.Fprintf(&b, "**Result:** %s\n", s.Result)
	fmt.Fprintf(&b, "**Duration:** %s\n", s.Duration.Round(time.Second))
	fmt.Fprintf(&b, "**Operations Executed:** %d\n", s.OpsExecuted)
	if s.FailReason != "" {
		fmt.Fprintf(&b, "**Failure Reason:** %s\n", s.FailReason)
	}
	b.WriteString("\n")

	switch s.OperationRun.Mode {
	case config.OperationModeSequence:
		b.WriteString("## Operation Results\n\n")
		b.WriteString("| # | Operation | Status | Error |\n")
		b.WriteString("|---|-----------|--------|-------|\n")
		for i, result := range s.OperationRun.Results {
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n",
				i+1, result.Name, result.Status, markdownCell(result.Error))
		}
		b.WriteString("\n")
	case config.OperationModeRandom:
		b.WriteString("## Operation Summary\n\n")
		b.WriteString("| Operation | Passed | Failed |\n")
		b.WriteString("|-----------|--------|--------|\n")
		for _, aggregate := range s.OperationRun.Aggregates {
			fmt.Fprintf(&b, "| %s | %d | %d |\n",
				aggregate.Name, aggregate.Passed, aggregate.Failed)
		}
		b.WriteString("\n")
	}

	// Data Plane Metrics
	b.WriteString("## Data Plane Metrics\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	fmt.Fprintf(&b, "| Writes Attempted | %d |\n", s.Metrics.WriteAttempted)
	fmt.Fprintf(&b, "| Writes Acknowledged | %d |\n", s.Metrics.WriteAcknowledged)
	fmt.Fprintf(&b, "| Writes Failed | %d |\n", s.Metrics.WriteFailed)
	fmt.Fprintf(&b, "| Write Success Rate | %.2f%% |\n", s.Metrics.WriteSuccessRate()*100)
	fmt.Fprintf(&b, "| Verify Passes | %d |\n", s.Metrics.VerifyPasses)
	fmt.Fprintf(&b, "| Gaps Detected | %d |\n", s.Metrics.GapsDetected)
	fmt.Fprintf(&b, "| Checksum Errors | %d |\n", s.Metrics.ChecksumErrors)
	fmt.Fprintf(&b, "| Docs Pruned | %d |\n", s.Metrics.DocsPruned)
	b.WriteString("\n")

	// Data Protection (ScheduledBackup + retention)
	b.WriteString("## Data Protection\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	fmt.Fprintf(&b, "| Backups Scheduled | %d |\n", s.Backup.Scheduled)
	fmt.Fprintf(&b, "| Backups Completed | %d |\n", s.Backup.Completed)
	fmt.Fprintf(&b, "| Backups Failed | %d |\n", s.Backup.Failed)
	fmt.Fprintf(&b, "| Backups Skipped | %d |\n", s.Backup.Skipped)
	fmt.Fprintf(&b, "| Live Backup Count | %d |\n", s.Backup.LastChildCount)
	fmt.Fprintf(&b, "| Retention Leaks | %d |\n", s.Backup.RetentionLeaks)
	fmt.Fprintf(&b, "| Max Scheduled Without Completion | %d |\n", s.Backup.MaxScheduledWithoutCompletion)
	if !s.Backup.LastScheduled.IsZero() {
		fmt.Fprintf(&b, "| Last Scheduled | %s |\n", s.Backup.LastScheduled.Format(time.RFC3339))
	}
	b.WriteString("\n")

	// Disruption Windows
	if len(s.Windows) > 0 {
		b.WriteString("## Disruption Windows\n\n")
		b.WriteString("| Operation | Duration | Write Failures | Est. Write Outage | Policy Exceeded |\n")
		b.WriteString("|-----------|----------|----------------|-------------------|------------------|\n")
		for _, w := range s.Windows {
			exceeded := "No"
			if w.ExceededPolicy() {
				exceeded = "**YES**"
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n",
				w.OperationName, w.Duration().Round(time.Second), w.WriteFailures,
				w.EstimatedWriteOutage().Round(time.Millisecond), exceeded)
		}
		b.WriteString("\n")
	}

	// Leak Analysis
	if s.LeakAnalysis.SampleCount > 0 {
		b.WriteString("## Resource Leak Analysis\n\n")
		fmt.Fprintf(&b, "- Samples: %d over %s\n",
			s.LeakAnalysis.SampleCount, s.LeakAnalysis.Duration.Round(time.Minute))
		fmt.Fprintf(&b, "- Memory trend: %.2f MB/hour\n", s.LeakAnalysis.MemorySlopeMB)
		fmt.Fprintf(&b, "- CPU trend: %.4f cores/hour\n", s.LeakAnalysis.CPUSlopeCores)
		if s.LeakAnalysis.HasLeak {
			b.WriteString("- **⚠️ Memory leak suspected**\n")
		}
		b.WriteString("\n")
	}

	// Recent Events (last 20)
	b.WriteString("## Recent Events\n\n")
	b.WriteString("```\n")
	events := s.Events
	start := 0
	if len(events) > 20 {
		start = len(events) - 20
	}
	for _, e := range events[start:] {
		b.WriteString(e.String() + "\n")
	}
	b.WriteString("```\n")

	return b.String()
}

func markdownCell(value string) string {
	if value == "" {
		return "—"
	}
	value = strings.ReplaceAll(value, "|", "\\|")
	return strings.ReplaceAll(value, "\n", " ")
}
