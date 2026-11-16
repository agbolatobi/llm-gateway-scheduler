package scheduler

import "time"

// Lower priorities are selected first; stable sorting preserves FIFO within a
// priority. Aging promotes old requests ahead of new interactive work. Capacity
// is checked separately, so a busy runtime cannot block unrelated ready work.
func schedulingPriority(queuedRequest *job, config Config, now time.Time) int {
	if config.Policy == FIFO {
		return 0
	}
	if now.Sub(queuedRequest.entered) >= time.Duration(config.AgingSeconds)*time.Second {
		return 0
	}
	if queuedRequest.class == "background" {
		return 2
	}
	return 1
}
