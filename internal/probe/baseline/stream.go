package baseline

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

// stream is one probe stream in flight: the sender fills in sent times, the
// reader appends replies, both under mu.
type stream struct {
	mu       sync.Mutex
	probes   []probe
	arrivals []int
}

// reply records a reply to probe i that arrived at now; out-of-range indexes are
// ignored, so a corrupted or foreign reply cannot touch another probe.
func (s *stream) reply(i uint64, r reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= uint64(len(s.probes)) || s.probes[i].sent.IsZero() {
		return
	}
	if len(s.probes[i].replies) == 0 {
		s.arrivals = append(s.arrivals, int(i))
	}
	s.probes[i].replies = append(s.probes[i].replies, r)
}

// run sends one probe per offset, waits tmax after the last, then stops read by
// moving conn's read deadline to now — once, so no later deadline can undo it. It
// ends early with ctx, a send error or the reader, and returns when it stopped.
func (s *stream) run(ctx context.Context, conn net.Conn, offsets []time.Duration, tmax time.Duration, send func(i int, now time.Time) error, read func()) time.Time {
	s.probes = make([]probe, len(offsets))
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
		case <-timer.C:
		}
		now := time.Now()
		s.mu.Lock()
		s.probes[i].planned, s.probes[i].sent = planned, now
		s.mu.Unlock()
		if err := send(i, now); err != nil {
			s.mu.Lock()
			s.probes[i].sent = time.Time{} // never left: not counted at all
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
	var kept []probe
	keep := make([]int, len(s.probes))
	for i, p := range s.probes {
		keep[i] = -1
		if !p.sent.IsZero() {
			keep[i] = len(kept)
			kept = append(kept, p)
		}
	}
	var arrivals []int
	for _, i := range s.arrivals {
		if keep[i] >= 0 {
			arrivals = append(arrivals, keep[i])
		}
	}
	s.probes, s.arrivals = kept, arrivals
	return end
}
