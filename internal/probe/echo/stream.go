// Package echo runs the probe streams that measure round trips to a node: STAMP
// over UDP and PING/PONG on an admitted data-plane connection, each sent on a
// Poisson schedule and resolved against a waiting time.
package echo

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// errSkip from a send function means that probe never left, and sending goes on;
// any other error ends the stream.
var errSkip = errors.New("probe not sent")

// Stream is one probe stream: Probes in send order, and Arrivals, the indexes of
// probes in the order their first reply came. While it runs, the sender and the
// reader share it under mu; once run returns it is the caller's.
type Stream struct {
	mu       sync.Mutex
	Probes   []Probe
	Arrivals []int
}

// add records a reply to probe i; out-of-range indexes are ignored, so a corrupted
// or foreign reply cannot touch another probe.
func (s *Stream) add(i uint64, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= uint64(len(s.Probes)) || s.Probes[i].Sent.IsZero() {
		return
	}
	if len(s.Probes[i].Replies) == 0 {
		s.Arrivals = append(s.Arrivals, int(i))
	}
	s.Probes[i].Replies = append(s.Probes[i].Replies, r)
}

// RTTs returns the round trips, to the first reply, of the probes sent at or
// after since that have one. It may be called while the stream runs.
func (s *Stream) RTTs(since time.Time) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var d []time.Duration
	for _, p := range s.Probes {
		if !p.Sent.IsZero() && !p.Sent.Before(since) && len(p.Replies) > 0 {
			d = append(d, p.Replies[0].At.Sub(p.Sent))
		}
	}
	return d
}

// run sends one probe per offset until stop is closed, waits tmax after the last,
// then stops read by moving conn's read deadline to now — once, so no later
// deadline can undo it. ctx, a send error or the reader end it at once. It drops
// probes that never left and returns when it stopped.
func (s *Stream) run(ctx context.Context, conn net.Conn, offsets []time.Duration, stop <-chan struct{}, tmax time.Duration, send func(i int, now time.Time) error, read func()) time.Time {
	s.mu.Lock()
	s.Probes = make([]Probe, len(offsets))
	s.mu.Unlock()
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); read() }()

	start := time.Now()
	last := start
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C
sending:
	for i, off := range offsets {
		planned := start.Add(off)
		timer.Reset(time.Until(planned))
		select {
		case <-ctx.Done():
			break sending
		case <-stop:
			break sending
		case <-timer.C:
		}
		now := time.Now()
		s.mu.Lock()
		s.Probes[i].Planned, s.Probes[i].Sent = planned, now
		s.mu.Unlock()
		if err := send(i, now); err != nil {
			s.mu.Lock()
			s.Probes[i].Sent = time.Time{} // never left: not counted at all
			s.mu.Unlock()
			if !errors.Is(err, errSkip) {
				break
			}
			continue
		}
		last = now
	}
	timer.Reset(time.Until(last.Add(tmax)))
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-readerDone:
	}
	end := time.Now()
	conn.SetReadDeadline(end)
	<-readerDone

	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []Probe
	keep := make([]int, len(s.Probes))
	for i, p := range s.Probes {
		keep[i] = -1
		if !p.Sent.IsZero() {
			keep[i] = len(kept)
			kept = append(kept, p)
		}
	}
	var arrivals []int
	for _, i := range s.Arrivals {
		if keep[i] >= 0 {
			arrivals = append(arrivals, keep[i])
		}
	}
	s.Probes, s.Arrivals = kept, arrivals
	return end
}
