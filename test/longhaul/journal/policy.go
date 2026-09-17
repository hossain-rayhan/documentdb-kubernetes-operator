// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package journal

import "time"

// OutagePolicy defines acceptable disruption bounds for an operation. Its two
// fields assert on different properties of the managed cluster and fail
// independently (ExceededPolicy trips if either is exceeded): MaxWriteOutage
// bounds client-visible write availability, while MustRecoverWithin bounds the
// cluster's return to its full declared topology (all pods Ready, CR Ready).
// Operation execution errors also fail the run independently of this policy.
// Each can be violated while the other is fine — e.g. after a failover writes
// resume quickly (MaxWriteOutage happy) yet the cluster stays degraded until a
// replacement standby rejoins, which only MustRecoverWithin catches.
type OutagePolicy struct {
	// MaxWriteOutage bounds how long the write path (client -> gateway ->
	// primary) may be unavailable during the window. It is measured from write
	// timestamps as the longest span from the first failing write attempt to
	// the first subsequent successful write (see
	// DisruptionWindow.EstimatedWriteOutage), so it reflects real wall-clock
	// unavailability regardless of how many writer goroutines
	// (LONGHAUL_NUM_WRITERS) are configured or how the driver batches its
	// retries.
	MaxWriteOutage time.Duration

	// MustRecoverWithin is the maximum time from operation start to full cluster
	// recovery (steady state).
	MustRecoverWithin time.Duration
}

// DefaultOutagePolicy returns a conservative policy suitable for most operations.
func DefaultOutagePolicy() OutagePolicy {
	return OutagePolicy{
		MaxWriteOutage:    5 * time.Second,
		MustRecoverWithin: 5 * time.Minute,
	}
}

// NoOutageWriteOutageCushion is the tiny write-outage budget granted to
// operations that are expected NOT to disrupt the data plane. It is not a
// tolerance for real outages: the write-outage is measured as the span from the
// first failing write to the next successful one, so a lone transient failure
// (one writer, recovered on the next ~100ms tick) maps to roughly one
// writeInterval of outage. This ~3-tick cushion absorbs unrelated background
// noise (a client reconnect, service-endpoint churn) without tolerating a
// genuine primary outage. Centralized so it can be recalibrated against real
// long-haul runs in one place.
const NoOutageWriteOutageCushion = 300 * time.Millisecond

// NoOutagePolicy is the outage budget for operations that keep the write path
// up throughout and therefore must not cause a write outage. It is shared by
// every "no data-plane impact" operation:
//   - control-plane faults, e.g. an operator pod restart, and
//   - scaling that only adds or removes a standby replica (the primary, and
//     thus the write path, is never touched).
//
// recovery bounds how long the cluster may take to return to steady state.
func NoOutagePolicy(recovery time.Duration) OutagePolicy {
	return OutagePolicy{
		MaxWriteOutage:    NoOutageWriteOutageCushion,
		MustRecoverWithin: recovery,
	}
}

// PrimaryHandoverWriteOutage is the write-outage budget for kill-primary-pod:
// an *ungraceful* failover that detects the lost pod, then promotes a standby.
// The write path is interrupted for exactly one primary handover.
//
// Unlike a graceful switchover, an ungraceful failover cannot begin promoting a
// standby until CNPG notices the primary pod is gone, so the detection latency
// is added on top of the promotion itself. Calibrated against a real
// kill-primary on a resource-constrained kind runner, which measured a ~30.3s
// handover outage; the budget carries headroom over that so a healthy failover
// on a slow CI runner is not flagged while a gross regression (a multi-minute
// write stall) still is. The longer, full-topology recovery is bounded
// separately by MustRecoverWithin.
const PrimaryHandoverWriteOutage = 60 * time.Second

// PrimaryHandoverPolicy is the outage budget for operations whose write path is
// interrupted for a single primary handover (see PrimaryHandoverWriteOutage).
// recovery bounds how long the cluster may take to return to full topology,
// which can legitimately differ per operation (a rolling upgrade restarts every
// pod and takes longer than a single failover).
func PrimaryHandoverPolicy(recovery time.Duration) OutagePolicy {
	return OutagePolicy{
		MaxWriteOutage:    PrimaryHandoverWriteOutage,
		MustRecoverWithin: recovery,
	}
}

// UpgradeWriteOutage is the write-outage budget for upgrade-documentdb. A
// cross-version rolling upgrade still interrupts writes for a single primary
// switchover (the standby restarts do NOT interrupt writes), but that
// switchover is heavier than a plain failover: it coincides with the extension
// version migration running under live write load, and the newly promoted
// primary must come up on the new image before it accepts writes. Calibrated
// against a real 0.110.0 -> 0.113.0 upgrade on a resource-constrained kind
// runner, which measured a ~33s switchover outage; the budget carries headroom
// over that so genuine cross-version upgrades are not flagged while a gross
// regression (a multi-minute write stall) still is. The upgrade's longer,
// whole-topology restart is bounded separately by MustRecoverWithin.
const UpgradeWriteOutage = 90 * time.Second

// UpgradeOutagePolicy is the outage budget for a cross-version DocumentDB
// upgrade (see UpgradeWriteOutage). recovery bounds how long the whole-topology
// rolling restart may take to return to full topology.
func UpgradeOutagePolicy(recovery time.Duration) OutagePolicy {
	return OutagePolicy{
		MaxWriteOutage:    UpgradeWriteOutage,
		MustRecoverWithin: recovery,
	}
}

// DisruptionWindow represents an active or closed disruption period.
type DisruptionWindow struct {
	// OperationName identifies which operation opened this window.
	OperationName string

	// StartTime is when the disruption began.
	StartTime time.Time

	// EndTime is when the disruption ended. Zero means still active.
	EndTime time.Time

	// Policy is the outage budget for this window.
	Policy OutagePolicy

	// WriteFailures counts individual failed write attempts observed during
	// this window. Retained for reporting only; the outage budget is evaluated
	// from timestamps (see EstimatedWriteOutage), not this count.
	WriteFailures int64

	// WriteOutageStart is the attempt-start time of the first failing write of
	// the currently-open outage, or zero when writes are not currently failing.
	// Set on the first failure after writes were healthy and cleared once a
	// write succeeds again.
	WriteOutageStart time.Time

	// MaxWriteOutageObserved is the longest completed outage seen so far in this
	// window: the span from a first failing write to the first subsequent
	// success. EstimatedWriteOutage combines it with any still-open outage.
	MaxWriteOutageObserved time.Duration
}

// EstimatedWriteOutage returns the longest span during this window for which
// the write path was unavailable, measured from write timestamps as
// first-failing-attempt -> first-subsequent-success. If writes are still
// failing when this is evaluated, the currently-open outage is measured up to
// the window end (or now, for an active window). Returns 0 when no write ever
// failed during the window.
func (w *DisruptionWindow) EstimatedWriteOutage() time.Duration {
	outage := w.MaxWriteOutageObserved
	if !w.WriteOutageStart.IsZero() {
		end := w.EndTime
		if end.IsZero() {
			end = time.Now()
		}
		if open := end.Sub(w.WriteOutageStart); open > outage {
			outage = open
		}
	}
	return outage
}

// IsActive returns true if the disruption window has not been closed.
func (w *DisruptionWindow) IsActive() bool {
	return w.EndTime.IsZero()
}

// Duration returns the elapsed time of the disruption window.
// For active windows, this is time since start.
func (w *DisruptionWindow) Duration() time.Duration {
	if w.IsActive() {
		return time.Since(w.StartTime)
	}
	return w.EndTime.Sub(w.StartTime)
}

// ExceededPolicy returns true if the window has violated its outage policy.
func (w *DisruptionWindow) ExceededPolicy() bool {
	if w.Duration() > w.Policy.MustRecoverWithin {
		return true
	}
	if w.EstimatedWriteOutage() > w.Policy.MaxWriteOutage {
		return true
	}
	return false
}
