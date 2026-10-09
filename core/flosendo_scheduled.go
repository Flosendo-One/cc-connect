package core

import "time"

// Flosendo: a scheduled prompt (timer or cron job) that fires while its session is mid-turn waits for
// that turn to end and then runs, instead of being dropped with "session is busy". Upstream dropped it
// after its "⏰" start notice had already posted, so an overnight nudge that met a working session
// announced itself and did nothing (9 Oct 2026). The start notice now posts only once the turn starts.
//
// Declared limit: the scheduler's own execution timeout (TimeoutMins, default 30m) still runs on the
// clock and may log "timed out" while the job waits; the job keeps waiting up to scheduledBusyWaitMax
// and runs when the session frees.
var (
	scheduledLockPoll    = 5 * time.Second
	scheduledBusyWaitMax = 3 * time.Hour
)

// waitForScheduledTurnLock takes the session's turn lock, polling until it is free or max has passed.
func waitForScheduledTurnLock(s *Session, max, poll time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if s.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}

// timerStillScheduled reports whether a timer that waited for its session is still wanted: a timer
// deleted while it waited does not run (the wait must not outlive a cancellation).
func (e *Engine) timerStillScheduled(job *TimerJob) bool {
	if e.timerScheduler == nil || e.timerScheduler.store == nil {
		return true
	}
	return e.timerScheduler.store.Get(job.ID) != nil
}

// cronStillScheduled is the same for a cron job: deleted, or switched off while it waited (a job that
// was already disabled when it started is a manual run and stays allowed).
func (e *Engine) cronStillScheduled(job *CronJob) bool {
	if e.cronScheduler == nil || e.cronScheduler.store == nil {
		return true
	}
	cur := e.cronScheduler.store.Get(job.ID)
	if cur == nil {
		return false
	}
	return !(job.Enabled && !cur.Enabled)
}
