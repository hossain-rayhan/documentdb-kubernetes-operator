// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package report

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/documentdb/documentdb-operator/test/longhaul/operations"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapName is the name of the ConfigMap used to persist reports.
	ConfigMapName = "longhaul-report"

	// Durable incident-state keys latched into the report ConfigMap. Unlike
	// the transient `result` key — which a fresh process resets to RUNNING
	// after an automatic Deployment restart — these are sticky: once any
	// process observes a failure they persist across restarts so the incident
	// cannot be silently erased. Monitors should alert from these keys.
	keyIncidentLatched    = "incident-latched"
	keyIncidentCount      = "incident-count"
	keyFirstFailureReason = "first-failure-reason"
	keyFirstFailureTime   = "first-failure-time"
	keyLastFailureReason  = "last-failure-reason"
	keyLastFailureTime    = "last-failure-time"
)

// mergeIncidentState latches a monotonic failure record into data, carrying
// forward any prior incident recorded in the existing ConfigMap. This prevents
// an automatic Deployment restart from erasing a FAIL verdict: a failed process
// writes result=FAIL and exits, and the restarted process would otherwise
// overwrite that with a fresh RUNNING checkpoint. Once latched the incident is
// sticky across restarts; incident-count is monotonic across process lifetimes.
func mergeIncidentState(data, existing map[string]string, summary Summary) {
	priorLatched := existing[keyIncidentLatched] == "true"
	priorCount := parseInt64(existing[keyIncidentCount])

	// Always carry forward any previously latched incident, even when this
	// process is currently healthy (the common post-restart case).
	if priorLatched {
		data[keyIncidentLatched] = "true"
		data[keyIncidentCount] = strconv.FormatInt(priorCount, 10)
		data[keyFirstFailureReason] = existing[keyFirstFailureReason]
		data[keyFirstFailureTime] = existing[keyFirstFailureTime]
		data[keyLastFailureReason] = existing[keyLastFailureReason]
		data[keyLastFailureTime] = existing[keyLastFailureTime]
	}

	if summary.Result != ResultFail {
		return
	}

	reason := summary.FailReason
	if reason == "" {
		reason = summary.OperationRun.FailureReason
	}
	if reason == "" {
		reason = "run reported FAIL"
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if !priorLatched {
		// First observation of this incident (this process, or ever).
		data[keyIncidentLatched] = "true"
		data[keyIncidentCount] = strconv.FormatInt(priorCount+1, 10)
		data[keyFirstFailureReason] = reason
		data[keyFirstFailureTime] = now
	}
	// Always refresh the most-recent failure details.
	data[keyLastFailureReason] = reason
	data[keyLastFailureTime] = now
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// SummaryFunc is called to generate the current test summary. final is true
// only for the terminal emit, when incomplete sequence execution must fail.
type SummaryFunc func(final bool) Summary

// CheckpointReporter periodically generates and persists reports.
type CheckpointReporter struct {
	clientset   kubernetes.Interface
	namespace   string
	interval    time.Duration
	summaryFunc SummaryFunc

	emitMu       sync.Mutex
	finalEmitted bool
	finalSummary Summary
}

// NewCheckpointReporter creates a periodic reporter that writes to stdout and ConfigMap.
func NewCheckpointReporter(clientset kubernetes.Interface, namespace string, interval time.Duration, fn SummaryFunc) *CheckpointReporter {
	return &CheckpointReporter{
		clientset:   clientset,
		namespace:   namespace,
		interval:    interval,
		summaryFunc: fn,
	}
}

// Run starts the periodic reporting loop. Blocks until context is cancelled.
// On shutdown, callers should invoke EmitFinal() synchronously — Run no longer
// emits its own final report because the goroutine can be killed by os.Exit
// before the K8s Update returns.
func (r *CheckpointReporter) Run(ctx context.Context) {
	log.Printf("[checkpoint] periodic reporter started (interval=%s)", r.interval)
	defer log.Println("[checkpoint] periodic reporter stopped")

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.emit(ctx, false)
		}
	}
}

// EmitFinal writes a terminal summary (PASS or FAIL is persisted as itself,
// not as RUNNING) using a bounded context. Safe to call after the main
// context has been cancelled. Intended to be called synchronously from main
// just before exit so the verdict is durable in the ConfigMap.
func (r *CheckpointReporter) EmitFinal() Summary {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.emit(ctx, true)
}

// emit writes the current summary to stdout, GH Actions annotations, and the
// status ConfigMap. final=true means this is the shutdown emit, in which case
// PASS is persisted as "PASS" (not "RUNNING") so consumers can distinguish a
// finished clean run from an in-flight checkpoint.
func (r *CheckpointReporter) emit(ctx context.Context, final bool) Summary {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if final && r.finalEmitted {
		return r.finalSummary
	}
	if !final && r.finalEmitted {
		return Summary{}
	}

	summary := r.summaryFunc(final)
	if final {
		r.finalEmitted = true
		r.finalSummary = summary
	}

	// Intermediate PASS checkpoints surface as RUNNING; the final emit
	// preserves the true PASS/FAIL outcome.
	resultStr := string(summary.Result)
	if summary.Result == ResultPass && !final {
		resultStr = "RUNNING"
	}

	markdown := GenerateMarkdown(summary)

	// Print to stdout with clear delimiter.
	fmt.Println("\n=== CHECKPOINT REPORT ===")
	fmt.Println(markdown)
	fmt.Print("=== END CHECKPOINT ===\n\n")

	// Emit GitHub Actions annotations.
	EmitAnnotation(summary)

	// Persist to ConfigMap.
	if r.clientset == nil {
		return summary
	}

	data := map[string]string{
		"latest-report":     markdown,
		"last-updated":      time.Now().UTC().Format(time.RFC3339),
		"result":            resultStr,
		"operation-status":  string(summary.OperationRun.Status),
		"operation-results": marshalOperationResults(summary.OperationRun.Results),
		"docs-pruned":       strconv.FormatInt(summary.Metrics.DocsPruned, 10),
	}
	if len(summary.OperationRun.Aggregates) > 0 {
		data["operation-aggregates"] = marshalOperationAggregates(summary.OperationRun.Aggregates)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "longhaul-test",
				"app.kubernetes.io/part-of": "documentdb-operator",
			},
		},
	}

	existing, err := r.clientset.CoreV1().ConfigMaps(r.namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		mergeIncidentState(data, nil, summary)
		cm.Data = data
		_, err = r.clientset.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			log.Printf("[checkpoint] failed to create ConfigMap: %v", err)
		} else {
			log.Println("[checkpoint] ConfigMap created")
		}
	} else if err == nil {
		// Latch durable incident state from the prior revision so a restart
		// cannot overwrite a FAIL verdict with a fresh RUNNING checkpoint.
		mergeIncidentState(data, existing.Data, summary)
		existing.Data = data
		_, err = r.clientset.CoreV1().ConfigMaps(r.namespace).Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			log.Printf("[checkpoint] failed to update ConfigMap: %v", err)
		} else {
			log.Println("[checkpoint] ConfigMap updated")
		}
	} else {
		log.Printf("[checkpoint] failed to get ConfigMap: %v", err)
	}

	// Also log the summary as JSON for structured log consumers.
	summaryJSON, _ := json.Marshal(map[string]any{
		"result":           resultStr,
		"elapsed":          summary.Duration.String(),
		"writes":           summary.Metrics.WriteAttempted,
		"gaps":             summary.Metrics.GapsDetected,
		"ops":              summary.OpsExecuted,
		"memory_leak":      summary.LeakAnalysis.HasLeak,
		"memory_slope":     fmt.Sprintf("%.2f MB/h", summary.LeakAnalysis.MemorySlopeMB),
		"operation_status": summary.OperationRun.Status,
		"checkpoint_time":  time.Now().UTC().Format(time.RFC3339),
	})
	log.Printf("[checkpoint] %s", string(summaryJSON))
	return summary
}

func marshalOperationResults(results []operations.OperationResult) string {
	if results == nil {
		results = []operations.OperationResult{}
	}
	data, _ := json.Marshal(results)
	return string(data)
}

func marshalOperationAggregates(aggregates []operations.OperationAggregate) string {
	if aggregates == nil {
		aggregates = []operations.OperationAggregate{}
	}
	data, _ := json.Marshal(aggregates)
	return string(data)
}
