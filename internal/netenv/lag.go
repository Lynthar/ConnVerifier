package netenv

import (
	"sync/atomic"
	"time"
)

// Lag watches whether this process gets the CPU when it asks for it: a timer
// wakes every period and counts how often it woke more than limit late. Throttling
// by a cgroup or by other processes shows here even when CPU use looks low.
type Lag struct {
	ticks, over atomic.Uint64
	stop, done  chan struct{}
}

// StartLag starts the timer; Stop ends it.
func StartLag(period, limit time.Duration) *Lag {
	l := &Lag{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		t := time.NewTimer(period)
		defer t.Stop()
		planned := time.Now().Add(period)
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
			}
			now := time.Now()
			l.ticks.Add(1)
			if now.Sub(planned) > limit {
				l.over.Add(1)
			}
			planned = now.Add(period)
			t.Reset(period)
		}
	}()
	return l
}

// Counts returns the wake-ups so far and how many of them were late.
func (l *Lag) Counts() (ticks, over uint64) { return l.ticks.Load(), l.over.Load() }

// Stop ends the timer and returns the final counts. It may be called more than once.
func (l *Lag) Stop() (ticks, over uint64) {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
	return l.Counts()
}
