package baseline

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// udpRun is a STAMP stream's raw observations.
type udpRun struct {
	stream
	end        time.Time
	sendErrors uint64
}

// runUDP sends authenticated STAMP packets on conn at offsets and reads the
// reflections. Replies that fail authentication or name another session are
// ignored; they cannot be the node's answer to this stream.
func runUDP(ctx context.Context, conn net.Conn, secret []byte, ssid uint16, offsets []time.Duration, tmax time.Duration) *udpRun {
	key := protocol.StampKey(secret)
	r := &udpRun{}
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
			r.reply(uint64(p.SenderSeq), reply{at: at, reflSeq: p.Seq, residence: p.Timestamp.Sub(p.Received)})
		}
	}
	r.end = r.run(ctx, conn, offsets, tmax, send, read)
	r.sendErrors = sendErrors.Load()
	return r
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
