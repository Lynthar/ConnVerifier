package netenv

import (
	"testing"
	"time"
)

func TestLagCountsLateWakeups(t *testing.T) {
	relaxed := StartLag(time.Millisecond, time.Hour)
	strict := StartLag(time.Millisecond, -time.Hour) // every wake-up is "late"
	time.Sleep(50 * time.Millisecond)
	ticks, over := relaxed.Stop()
	if ticks == 0 || over != 0 {
		t.Errorf("relaxed: %d ticks, %d late; want some ticks and none late", ticks, over)
	}
	ticks, over = strict.Stop()
	if ticks == 0 || over != ticks {
		t.Errorf("strict: %d ticks, %d late; want all late", ticks, over)
	}
	if again, _ := strict.Stop(); again != ticks {
		t.Errorf("second Stop: %d ticks, want %d", again, ticks)
	}
}

func TestProcessCPUGrowsWithWork(t *testing.T) {
	before, ok := ProcessCPU()
	if !ok {
		t.Skip("CPU time unknown on this platform")
	}
	for end := time.Now().Add(50 * time.Millisecond); time.Now().Before(end); {
	}
	if after, _ := ProcessCPU(); after-before < 10*time.Millisecond {
		t.Errorf("50 ms of busy work added %v of CPU time", after-before)
	}
}
