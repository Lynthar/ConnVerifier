package echo

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// UDPRun is a STAMP stream's raw observations.
type UDPRun struct {
	Stream
	End        time.Time
	SendErrors uint64
}

// Run sends authenticated STAMP packets on conn at offsets until stop is closed
// (nil: until the offsets run out) and reads the reflections into r. Replies that
// fail authentication or name another session are ignored; they cannot be the
// node's answer to this stream. Only r's Stream may be read before Run returns.
func (r *UDPRun) Run(ctx context.Context, conn net.Conn, secret []byte, ssid uint16, offsets []time.Duration, stop <-chan struct{}, tmax time.Duration) {
	key := protocol.StampKey(secret)
	var sendErrors atomic.Uint64
	send := func(i int, now time.Time) error {
		b := protocol.StampSender{Seq: uint32(i), Timestamp: protocol.NTPTime(now), SSID: ssid}.Marshal(key)
		if _, err := conn.Write(b[:]); err != nil {
			sendErrors.Add(1)
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			return errSkip
		}
		return nil
	}
	read := func() {
		buf := make([]byte, protocol.StampLen+1)
		for {
			n, err := conn.Read(buf)
			at := time.Now()
			if err != nil {
				if errors.Is(err, net.ErrClosed) || isTimeout(err) {
					return
				}
				continue // an ICMP error surfacing on a connected socket
			}
			p, err := protocol.ParseStampReflector(buf[:n], key)
			if err != nil || p.SSID != ssid {
				continue
			}
			r.add(uint64(p.SenderSeq), Reply{At: at, ReflSeq: p.Seq, Residence: p.Timestamp.Sub(p.Received)})
		}
	}
	r.End = r.run(ctx, conn, offsets, stop, tmax, send, read)
	r.SendErrors = sendErrors.Load()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
