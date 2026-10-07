package baseline

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

// dataFailure is why no data-plane connection was admitted; kind is "dial",
// "handshake" or "reject", and reject holds the node's REJECT.
type dataFailure struct {
	kind   string
	reject protocol.Frame
	err    error
}

// openData dials the node and presents the session ticket; it returns the
// admitted connection or why there is none.
func openData(ctx context.Context, dial nodeclient.DialFunc, addr string, id [16]byte, secret []byte, timeout time.Duration) (net.Conn, *dataFailure) {
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, &dataFailure{kind: "dial", err: err}
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
		return nil, &dataFailure{kind: "reject", reject: f}
	default:
		err = protocol.ErrMalformed
	}
	conn.Close()
	return nil, &dataFailure{kind: "handshake", err: err}
}

// tcpRun is an echo stream's raw observations; broke is set when the connection
// ended before the run did.
type tcpRun struct {
	stream
	end   time.Time
	broke error
}

// runTCP pipelines PINGs on an admitted connection at offsets — it never waits for
// a PONG before the next PING — and pairs PONGs by sequence number.
func runTCP(ctx context.Context, conn net.Conn, offsets []time.Duration, tmax time.Duration) *tcpRun {
	r := &tcpRun{}
	var once sync.Once
	broke := func(err error) { once.Do(func() { r.broke = err }) }
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
				r.reply(f.Seq, reply{at: at})
			case protocol.TypeClose:
				broke(fmt.Errorf("closed by the node: %s", f.Reason))
				return
			default:
				broke(protocol.ErrMalformed)
				return
			}
		}
	}
	r.end = r.run(ctx, conn, offsets, tmax, send, read)
	return r
}
