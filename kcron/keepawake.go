package main

import (
	"os/exec"
	"time"
)

// The suspend/wake roundtrip on a Kindle costs real wall-clock time: between
// readyToSuspend and a job actually running after wakeupFromSuspend, the device
// spends a few seconds entering suspend, waiting on the RTC and letting powerd
// bring the system back up. Kernel RTC resume latency alone is ~2-3s, and the
// powerd readyToSuspend cluster adds a handful more. So the correctness floor (the
// closest a job can be and still survive a suspend/wake cycle) is on the order of
// seconds, not minutes.
//
// Jobs due sooner than the threshold keep the device awake (abortSuspend) instead
// of suspending; jobs further out suspend normally. Default 3 minutes, overridable
// with -keepawake; 'off' disables the keep-awake mode entirely (always suspend).

// keepAwakeThreshold is the cutoff: a job due sooner than this keeps the device
// awake instead of suspending. Set from -keepawake. The default sits safely above
// the few-second roundtrip floor (so no job is ever missed) and in the range where
// staying awake briefly is competitive with a suspend/resume cycle.
var keepAwakeThreshold = 3 * time.Minute

// keepAwakeDisabled is the sentinel for "mode off": never keep awake, always
// suspend. Selected by -keepawake off.
const keepAwakeDisabled = -1 * time.Second

// activeThreshold returns the effective threshold, or 0 when the mode is off.
func activeThreshold() time.Duration {
	if keepAwakeThreshold == keepAwakeDisabled {
		return 0
	}
	return keepAwakeThreshold
}

// abortSuspend is a write-only Int trigger (powerd exposes it as "w Int"): writing
// to it aborts the in-progress suspend transition and restarts the short
// ReadyToSuspend countdown WITHOUT kicking the machine back to Active. The device
// stays right at the edge of sleep instead of fully waking, so power use is far
// lower than deferSuspend, and a short-interval job no longer prevents the device
// from eventually sleeping once the job stops being imminent.
const propAbortSuspend = "abortSuspend"

// requestAbortSuspend tells powerd to abort the in-progress suspend and restart the
// ReadyToSuspend countdown. Must be called during readyToSuspend. Returns an error
// if powerd rejects it (wrong state, or unavailable on this firmware).
func requestAbortSuspend() error {
	return exec.Command("lipc-set-prop", "-i", powerd, propAbortSuspend, "1").Run()
}
