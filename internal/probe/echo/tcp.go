package echo

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// DataFailure is why no data-plane connection was admitted; Kind is "dial",
// "handshake" or "reject", and Reject holds the node's REJECT.
type DataFailure struct {
	Kind   string
	Reject protocol.Frame
	Err    error
}

// OpenData dials the node and presents the session ticket; it returns the
// admitted connection or why there is none.
func OpenData(ctx context.Context, dial nodeclient.DialFunc, addr string, id [16]byte, secret []byte, timeout time.Duration) (net.Conn, *DataFailure) {
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, &DataFailure{Kind: "dial", Err: err}
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	conn.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) }) // after, or it is overwritten
	defer stop()
	_, err = conn.Write(protocol.NewHello(secret, id, nonce).MarshalBinary())
	var f protocol.Frame
	if err == nil {
		f, err = protocol.ReadFrame(conn)
	}
	switch {
	case err != nil:
	case f.Type == protocol.TypeAccept:
		conn.SetDeadline(time.Time{})
		return conn, nil
	case f.Type == protocol.TypeReject:
		conn.Close()
		return nil, &DataFailure{Kind: "reject", Reject: f}
	default:
		err = protocol.ErrMalformed
	}
	conn.Close()
	return nil, &DataFailure{Kind: "handshake", Err: err}
}

// TCPRun is an echo stream's raw observations; Broke is set when the connection
// ended before the run did.
type TCPRun struct {
	Stream
	End   time.Time
	Broke error
}

// Run pipelines PINGs on an admitted connection at offsets until stop is closed
// (nil: until the offsets run out) — it never waits for a PONG before the next
// PING — and pairs PONGs by sequence number into r. Only r's Stream may be read
// before Run returns.
func (r *TCPRun) Run(ctx context.Context, conn net.Conn, offsets []time.Duration, stop <-chan struct{}, tmax time.Duration) {
	var once sync.Once
	broke := func(err error) { once.Do(func() { r.Broke = err }) }
	send := func(i int, now time.Time) error {
		conn.SetWriteDeadline(now.Add(tmax))
		err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.TypePing, Seq: uint64(i)})
		if err != nil {
			broke(err)
		}
		return err
	}
	read := func() {
		var fr protocol.FrameReader
		for {
			f, err := fr.Next(conn)
			at := time.Now()
			if err != nil {
				if !isTimeout(err) { // a timeout is the stop, not a break
					broke(err)
				}
				return
			}
			switch f.Type {
			case protocol.TypePong:
				r.add(f.Seq, Reply{At: at})
			case protocol.TypeClose:
				broke(fmt.Errorf("closed by the node: %s", f.Reason))
				return
			default:
				broke(protocol.ErrMalformed)
				return
			}
		}
	}
	r.End = r.run(ctx, conn, offsets, stop, tmax, send, read)
}
