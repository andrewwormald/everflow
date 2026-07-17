// Package reconciler detects Runs stuck on a lost in-memory event: the
// sync.Cond EventStreamer (ADR-0033) has no durable queue, so an event
// dropped between a daemon restart and its delivery leaves the Run's
// AgentState in StatusWorking or StatusDiscovering forever, with nothing
// to wake it back up. See DESIGN.md § "The state machine".
package reconciler

import (
	"time"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
)

// IsStuck reports whether a Run sitting in status since lastProgress has
// gone stale: only StatusWorking and StatusDiscovering are in-flight states
// that depend on an in-memory event to advance, so any other status is
// never considered stuck. now and lastProgress are passed in rather than
// read from time.Now() so callers (and tests) control elapsed time.
func IsStuck(status refactorsweep.AgentStatus, lastProgress time.Time, now time.Time, threshold time.Duration) bool {
	switch status {
	case refactorsweep.StatusWorking, refactorsweep.StatusDiscovering:
	default:
		return false
	}
	return now.Sub(lastProgress) >= threshold
}
