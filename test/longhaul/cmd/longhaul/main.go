// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package main provides a standalone binary entry point for running
// long haul tests as a Kubernetes Deployment (without Ginkgo test framework).
// A Deployment is used (not a Job) so the kubelet auto-restarts the driver
// pod on crash; the canonical "did the test pass?" signal is the
// longhaul-report ConfigMap and the GitHub Actions annotations, not the pod
// exit status.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/documentdb/documentdb-operator/test/longhaul/backup"
	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
	"github.com/documentdb/documentdb-operator/test/longhaul/monitor"
	"github.com/documentdb/documentdb-operator/test/longhaul/operations"
	"github.com/documentdb/documentdb-operator/test/longhaul/report"
	"github.com/documentdb/documentdb-operator/test/longhaul/workload"

	shareddocdb "github.com/documentdb/documentdb-operator/test/shared/mongo"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[longhaul] ")

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	log.Printf("config loaded: duration=%s namespace=%s cluster=%s writers=%d",
		cfg.MaxDuration, cfg.Namespace, cfg.ClusterName, cfg.NumWriters)

	exitCode := run(cfg)
	os.Exit(exitCode)
}

func run(cfg config.Config) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.MaxDuration > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, cfg.MaxDuration)
		defer timeoutCancel()
	}

	// Initialize components.
	j := journal.New()
	metrics := workload.NewMetrics()

	// Connect to DocumentDB.
	if cfg.DocumentDBURI == "" {
		log.Fatal("LONGHAUL_DOCUMENTDB_URI must be set")
	}
	docdbClient, err := shareddocdb.NewFromURI(ctx, cfg.DocumentDBURI)
	if err != nil {
		log.Fatalf("failed to connect to DocumentDB: %v", err)
	}
	defer func() {
		disconnectCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = docdbClient.Disconnect(disconnectCtx)
	}()

	// Verify connectivity. The driver intentionally induces disruption
	// windows (scale/upgrade), so we use the shared retry-aware Ping
	// rather than a bare one-shot Ping — a single connection-refused
	// during gateway sidecar restart shouldn't abort the run.
	pingCtx, pingCancel := context.WithTimeout(ctx, 60*time.Second)
	defer pingCancel()
	if err := shareddocdb.PingWithRetry(pingCtx, docdbClient, 60*time.Second); err != nil {
		log.Fatalf("DocumentDB ping failed: %v", err)
	}
	log.Println("DocumentDB connection established")

	db := docdbClient.Database("longhaul")

	// Optionally drop previous test data. Disabled by default so that pod
	// restarts (Deployment auto-restart on crash) preserve durability
	// history for post-mortem; opt in with LONGHAUL_RESET_DATA=true for
	// local/dev iterations or fresh CI runs.
	if cfg.ResetData {
		if err := db.Collection(workload.CollectionName).Drop(ctx); err != nil {
			log.Fatalf("failed to drop collection: %v", err)
		}
		log.Println("workload collection dropped (LONGHAUL_RESET_DATA=true)")
	}

	// Create indexes.
	if err := workload.EnsureIndexes(ctx, db); err != nil {
		log.Fatalf("failed to create indexes: %v", err)
	}

	j.Info("main", "long haul test starting")

	// Initialize real k8s cluster client. The clientset built inside
	// K8sClusterClient is reused for ConfigMap operations (reporter) below
	// instead of building a second one against the same REST config.
	clusterClient, err := monitor.NewK8sClusterClient(monitor.K8sClientConfig{
		Namespace:   cfg.Namespace,
		ClusterName: cfg.ClusterName,
		Kubeconfig:  os.Getenv("KUBECONFIG"),
	})
	if err != nil {
		log.Fatalf("failed to initialize k8s client: %v", err)
	}
	k8sClientset := clusterClient.Clientset()
	j.Info("main", "k8s client initialized")

	// Start health monitor.
	healthMon := monitor.NewHealthMonitor(clusterClient, j, cfg.SteadyStateWait)
	go healthMon.Run(ctx)

	// Start leak detector.
	leakDetector := monitor.NewLeakDetector(j, 10.0, 10)

	// Start writers.
	writers := workload.StartWriters(ctx, cfg.NumWriters, db, metrics, j)
	j.Info("main", fmt.Sprintf("started %d writers", cfg.NumWriters))

	// Start verifier. A single verifier is sufficient — see StartVerifier
	// godoc. Writers are passed so the verifier can detect tail loss by
	// comparing each writer's acked tip against what's in the DB.
	verifier := workload.StartVerifier(ctx, db, writers, metrics, j)
	j.Info("main", "verifier started")

	// Start retention pruner (bounds the workload collection so an unbounded
	// write test cannot eventually exhaust the PVC). It prunes only documents
	// below the verifier's confirmed floor, so it never affects the durability
	// verdict. Disabled when RetainPerWriter == 0.
	if cfg.RetainPerWriter > 0 {
		workload.StartPruner(ctx, db.Collection(workload.CollectionName), writers, verifier, cfg.RetainPerWriter, cfg.PruneInterval, metrics, j)
		j.Info("main", "retention pruner started")
	} else {
		j.Info("main", "retention pruning disabled (LONGHAUL_RETAIN_PER_WRITER=0)")
	}

	// Build the operation registry once, then select the configured runner.
	registry, err := operations.NewDefaultRegistry(cfg, clusterClient, k8sClientset, healthMon, j)
	if err != nil {
		log.Fatalf("failed to build operation registry: %v", err)
	}
	opRunner, err := newOperationRunner(cfg, registry, healthMon, j)
	if err != nil {
		log.Fatalf("failed to configure operation runner: %v", err)
	}
	go opRunner.Run(ctx)

	// Start data-protection verifier (ScheduledBackup + retention). Runs
	// concurrently with the scheduler by design — backup is deliberately not
	// isolated from topology/chaos operations.
	backupMetrics := backup.NewMetrics()
	if cfg.BackupEnabled {
		verifier := backup.NewVerifier(clusterClient, j, backupMetrics, backup.Config{
			ScheduledBackupName: cfg.ClusterName + "-longhaul",
			Schedule:            cfg.BackupSchedule,
			RetentionDays:       cfg.BackupRetentionDays,
			VerifyInterval:      cfg.BackupVerifyInterval,
		})
		if err := verifier.Bootstrap(ctx); err != nil {
			// A bootstrap failure is not fatal to the whole run — the workload
			// and other operations still provide signal. Surface it loudly.
			j.Error("backup", fmt.Sprintf("ScheduledBackup bootstrap failed, backup verification disabled: %v", err))
		} else {
			go verifier.Run(ctx)
		}
	} else {
		j.Info("backup", "backup verification disabled via config")
	}

	// Start metrics sampling goroutine (feeds leak detector).
	go runMetricsSampling(ctx, clusterClient, leakDetector, j)

	// Start periodic checkpoint reporter.
	summaryFunc := func(final bool) report.Summary {
		return buildSummary(metrics, backupMetrics, leakDetector, opRunner, j, final)
	}
	reporter := report.NewCheckpointReporter(k8sClientset, cfg.Namespace, cfg.ReportInterval, summaryFunc)
	go reporter.Run(ctx)

	j.Info("main", "all components started, entering main loop")

	// Sequence mode is completion-driven: it exits as soon as its operations
	// have finished (or a failure occurs); MaxDuration is only its watchdog.
	// Random and disabled modes are duration-driven.
	completionDriven := cfg.OperationMode == config.OperationModeSequence
	if completionDriven {
		select {
		case <-opRunner.Done():
			j.Info("main", "operations finished")
		case <-ctx.Done():
			j.Info("main", fmt.Sprintf("operations watchdog fired: %v", ctx.Err()))
			if sr, ok := opRunner.(*operations.SequenceRunner); ok {
				sr.MarkIncomplete(
					fmt.Sprintf("operation sequence incomplete: watchdog fired: %v", ctx.Err()),
				)
			}
			<-opRunner.Done()
		}
	} else {
		// Random/disabled modes are duration-driven, but a terminal operation
		// failure also halts the runner early (Scheduler.Run returns and closes
		// Done). React to that too so the FAIL verdict is emitted promptly
		// instead of waiting out MaxDuration, which is unbounded in production.
		select {
		case <-ctx.Done():
			j.Info("main", fmt.Sprintf("test ending: %v", ctx.Err()))
		case <-opRunner.Done():
			j.Info("main", "operations halted early (failure)")
		}
		cancel()
		<-opRunner.Done()
	}
	cancel()

	// Allow goroutines to flush.
	time.Sleep(500 * time.Millisecond)

	// Emit exactly one terminal report synchronously before os.Exit. EmitFinal
	// prints the markdown, emits the GitHub Actions annotation, and persists the
	// authoritative verdict to the report ConfigMap.
	summary := reporter.EmitFinal()

	if summary.Result == report.ResultFail {
		log.Printf("TEST FAILED: %s", summary.FailReason)
		return 1
	}

	log.Println("TEST PASSED")
	return 0
}

func newOperationRunner(
	cfg config.Config,
	registry *operations.Registry,
	health *monitor.HealthMonitor,
	j *journal.Journal,
) (operations.Runner, error) {
	switch cfg.OperationMode {
	case config.OperationModeRandom:
		return operations.NewScheduler(registry.All(), health, j, cfg.OpCooldown), nil
	case config.OperationModeSequence:
		ops, err := registry.Resolve(cfg.OperationSequence)
		if err != nil {
			return nil, err
		}
		return operations.NewSequenceRunner(ops, health, j, cfg.RecoveryTimeout), nil
	case config.OperationModeDisabled:
		return operations.NewDisabledRunner(), nil
	default:
		return nil, fmt.Errorf("unsupported operation mode %q", cfg.OperationMode)
	}
}

// buildSummary constructs a report.Summary from current state.
func buildSummary(
	metrics *workload.Metrics,
	backupMetrics *backup.Metrics,
	leakDetector *monitor.LeakDetector,
	opRunner operations.Runner,
	j *journal.Journal,
	final bool,
) report.Summary {
	snap := metrics.Snapshot()
	backupSnap := backupMetrics.Snapshot()
	leakAnalysis := leakDetector.Analyze()
	operationRun := opRunner.Snapshot()

	result := report.ResultPass
	failReason := ""

	if snap.HasDataLoss() {
		result = report.ResultFail
		failReason = appendFailReason(failReason, fmt.Sprintf("data loss: %d gaps, %d checksum errors",
			snap.GapsDetected, snap.ChecksumErrors))
	}
	if operationRun.HasFailure() {
		result = report.ResultFail
		failReason = appendFailReason(failReason, operationRun.FailureReason)
		if operationRun.FailureReason == "" {
			failReason = appendFailReason(failReason, "operation execution failed")
		}
	}
	if final &&
		operationRun.Mode == config.OperationModeSequence &&
		operationRun.Status != operations.RunStatusComplete &&
		!operationRun.HasFailure() {
		result = report.ResultFail
		failReason = appendFailReason(failReason,
			fmt.Sprintf("operation sequence incomplete (status %s)", operationRun.Status))
	}
	if j.HasPolicyViolation() {
		result = report.ResultFail
		failReason = appendFailReason(failReason, "outage policy violated")
	}
	if backupSnap.HasRetentionLeak() {
		result = report.ResultFail
		failReason = appendFailReason(failReason, fmt.Sprintf("backup retention leak: %d expired backups not collected",
			backupSnap.RetentionLeaks))
	}
	if backupSnap.HasCompletionStall() {
		result = report.ResultFail
		failReason = appendFailReason(failReason, fmt.Sprintf("backup completion stalled: %d backups scheduled with no completion",
			backupSnap.MaxScheduledWithoutCompletion))
	}

	return report.Summary{
		Result:       result,
		Duration:     snap.Elapsed,
		Metrics:      snap,
		Backup:       backupSnap,
		LeakAnalysis: leakAnalysis,
		OpsExecuted:  operationRun.OpsExecuted(),
		OperationRun: operationRun,
		Windows:      j.DisruptionWindows(),
		Events:       j.Events(),
		FailReason:   failReason,
	}
}

func appendFailReason(existing, reason string) string {
	if reason == "" {
		return existing
	}
	if existing == "" {
		return reason
	}
	if existing == reason {
		return existing
	}
	return existing + "; " + reason
}

// runMetricsSampling periodically collects pod resource metrics and feeds the leak detector.
func runMetricsSampling(ctx context.Context, client *monitor.K8sClusterClient, ld *monitor.LeakDetector, j *journal.Journal) {
	if !client.MetricsAvailable() {
		j.Info("metrics", "metrics-server not available, leak detection sampling disabled")
		return
	}
	j.Info("metrics", "metrics sampling started (60s interval)")

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			podMetrics, err := client.GetPodMetrics(ctx)
			if err != nil {
				j.Warn("metrics", fmt.Sprintf("metrics query error: %v", err))
				continue
			}
			if podMetrics == nil {
				// Metrics became unavailable.
				j.Warn("metrics", "metrics-server became unavailable, stopping sampling")
				return
			}

			// Sum memory and CPU across all DocumentDB pods.
			var totalMem, totalCPU float64
			for _, pm := range podMetrics {
				totalMem += pm.MemoryMB
				totalCPU += pm.CPUCores
			}

			ld.AddSample(monitor.ResourceSample{
				Timestamp: time.Now(),
				MemoryMB:  totalMem,
				CPUCores:  totalCPU,
			})
		}
	}
}
