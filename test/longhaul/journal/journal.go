// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package journal provides an append-only event log for tracking test execution,
// disruption windows, and significant state changes during long haul tests.
package journal

import (
	"fmt"
	"sync"
	"time"
)

// Level represents the severity of a journal event.
type Level string

const (
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

// maxEvents bounds the in-memory event ring. We trim with headroom so the
// trim cost is amortized over many appends (one copy every trimHeadroom
// events), not paid on every append once we hit the cap.
const (
	maxEvents            = 10000
	trimHeadroom         = 1000
	maxDisruptionWindows = 1000
)

// Event represents a single journal entry.
type Event struct {
	Timestamp time.Time
	Level     Level
	Component string
	Message   string
}

// String returns a human-readable representation of the event.
func (e Event) String() string {
	return fmt.Sprintf("[%s] %s %s: %s",
		e.Timestamp.Format("15:04:05"), e.Level, e.Component, e.Message)
}

// Journal is a thread-safe, append-only event log.
type Journal struct {
	mu     sync.RWMutex
	events []Event

	// Active disruption window (nil if none).
	activeWindow *DisruptionWindow

	// All closed disruption windows.
	closedWindows []DisruptionWindow
}

// New creates a new empty Journal.
func New() *Journal {
	return &Journal{
		events: make([]Event, 0, 256),
	}
}

// Record appends a new event to the journal. Safe for concurrent use.
//
// To bound memory on multi-day runs the in-memory ring is capped at
// maxEvents; once exceeded by trimHeadroom, the oldest events are dropped.
// The rendered report only ever surfaces the last 20 events, so the cap is
// invisible to consumers.
func (j *Journal) Record(level Level, component, message string) {
	e := Event{
		Timestamp: time.Now(),
		Level:     level,
		Component: component,
		Message:   message,
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	j.events = append(j.events, e)
	if len(j.events) > maxEvents+trimHeadroom {
		trimmed := make([]Event, maxEvents)
		copy(trimmed, j.events[len(j.events)-maxEvents:])
		j.events = trimmed
	}
}

// Info records an info-level event.
func (j *Journal) Info(component, message string) {
	j.Record(LevelInfo, component, message)
}

// Warn records a warn-level event.
func (j *Journal) Warn(component, message string) {
	j.Record(LevelWarn, component, message)
}

// Error records an error-level event.
func (j *Journal) Error(component, message string) {
	j.Record(LevelError, component, message)
}

// OpenDisruptionWindow starts tracking a new disruption period.
func (j *Journal) OpenDisruptionWindow(operationName string, policy OutagePolicy) {
	j.mu.Lock()
	defer j.mu.Unlock()

	// Close any existing window first.
	if j.activeWindow != nil {
		j.activeWindow.EndTime = time.Now()
		j.appendClosedWindow(*j.activeWindow)
	}

	j.activeWindow = &DisruptionWindow{
		OperationName: operationName,
		StartTime:     time.Now(),
		Policy:        policy,
	}

	j.events = append(j.events, Event{
		Timestamp: time.Now(),
		Level:     LevelWarn,
		Component: "journal",
		Message:   fmt.Sprintf("disruption window opened: %s", operationName),
	})
}

// CloseDisruptionWindow ends the active disruption period and returns a copy.
func (j *Journal) CloseDisruptionWindow() *DisruptionWindow {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.activeWindow == nil {
		return nil
	}

	j.activeWindow.EndTime = time.Now()
	j.appendClosedWindow(*j.activeWindow)
	closed := *j.activeWindow

	j.events = append(j.events, Event{
		Timestamp: time.Now(),
		Level:     LevelInfo,
		Component: "journal",
		Message: fmt.Sprintf("disruption window closed: %s (duration: %s)",
			j.activeWindow.OperationName, j.activeWindow.Duration()),
	})

	j.activeWindow = nil
	return &closed
}

func (j *Journal) appendClosedWindow(window DisruptionWindow) {
	j.closedWindows = append(j.closedWindows, window)
	if len(j.closedWindows) > maxDisruptionWindows {
		copy(j.closedWindows, j.closedWindows[len(j.closedWindows)-maxDisruptionWindows:])
		j.closedWindows = j.closedWindows[:maxDisruptionWindows]
	}
}

// RecordWriteOutcome reports the result of a single write attempt to the active
// disruption window so it can measure the real write-outage duration from
// timestamps. attemptStart is when the write attempt began (before the driver
// call) and attemptEnd is when it returned; during an outage the driver call
// can block for the full server-selection timeout, so the two can be tens of
// seconds apart. A failure opens or extends the current outage; a success
// closes it, recording the first-failure -> recovering-success-completion span
// as a candidate for the window's longest observed outage. No-op when no window
// is active.
func (j *Journal) RecordWriteOutcome(attemptStart, attemptEnd time.Time, failed bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	w := j.activeWindow
	if w == nil {
		return
	}
	if failed {
		w.WriteFailures++
		// Track the earliest failing attempt of the current outage. Outcomes
		// arrive in completion order, so a failure recorded later may have
		// begun earlier than the one that opened the outage; keep the minimum
		// so the measured outage spans from the true first failing attempt.
		if w.WriteOutageStart.IsZero() || attemptStart.Before(w.WriteOutageStart) {
			w.WriteOutageStart = attemptStart
		}
		return
	}
	if !w.WriteOutageStart.IsZero() {
		// Writers are concurrent and each captures attemptStart before its
		// (possibly blocking) driver call, but outcomes arrive here in
		// completion order. A success whose attempt began at or before the
		// current outage start carries no information that the outage has
		// ended — it was already in flight when the outage opened — so it must
		// neither shrink the measured outage nor clear the marker. Only a
		// success that started strictly after the outage began proves the write
		// path recovered.
		if attemptStart.After(w.WriteOutageStart) {
			// The outage lasts until the recovering write actually completes,
			// not until it started: during a failover the successful call can
			// block in server selection for many seconds before returning.
			// Measure to attemptEnd so an over-budget outage is not undercounted.
			if gap := attemptEnd.Sub(w.WriteOutageStart); gap > w.MaxWriteOutageObserved {
				w.MaxWriteOutageObserved = gap
			}
			w.WriteOutageStart = time.Time{}
		}
	}
}

// ActiveWindow returns the current disruption window, or nil if none is active.
func (j *Journal) ActiveWindow() *DisruptionWindow {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.activeWindow == nil {
		return nil
	}
	// Return a copy to avoid data races.
	w := *j.activeWindow
	return &w
}

// HasPolicyViolation returns true if any disruption window exceeded its policy.
func (j *Journal) HasPolicyViolation() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if j.activeWindow != nil && j.activeWindow.ExceededPolicy() {
		return true
	}
	for i := range j.closedWindows {
		if j.closedWindows[i].ExceededPolicy() {
			return true
		}
	}
	return false
}

// Events returns a copy of all events recorded so far.
func (j *Journal) Events() []Event {
	j.mu.RLock()
	defer j.mu.RUnlock()
	result := make([]Event, len(j.events))
	copy(result, j.events)
	return result
}

// EventsSince returns events recorded after the given time.
func (j *Journal) EventsSince(t time.Time) []Event {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var result []Event
	for _, e := range j.events {
		if e.Timestamp.After(t) {
			result = append(result, e)
		}
	}
	return result
}

// Len returns the number of events in the journal.
func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.events)
}

// DisruptionWindows returns all closed disruption windows.
func (j *Journal) DisruptionWindows() []DisruptionWindow {
	j.mu.RLock()
	defer j.mu.RUnlock()
	result := make([]DisruptionWindow, len(j.closedWindows))
	copy(result, j.closedWindows)
	return result
}
