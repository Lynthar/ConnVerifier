package load

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const (
	chunk = 64 << 10
	// tlsPayload is the share of a full TLS 1.3 record that is payload: 16384
	// bytes plus 22 of header, content type and AEAD tag. HTTP/2 frame headers,
	// under 0.06% of a 16 KiB frame, are not taken off.
	tlsPayload = 16384.0 / 16406
	// An upload connection's kernel keeps at most 5 ms of its throughput unsent
	// (§6.1.1), so this host's own buffer adds little to the round trips of the
	// requests that share it; before a throughput is known, firstLowat bytes.
	lowatTime  = 5 * time.Millisecond
	firstLowat = 64 << 10
	minLowat   = 4 << 10
	// uploadGrace is how long a stopped phase waits for the node's last report of
	// what it read before cutting the uploads off.
	uploadGrace = 2 * time.Second
)

var zeros = make([]byte, chunk)

// Why a phase stopped.
const (
	stopSettled     = "settled"     // responsiveness settled after goodput did
	stopTime        = "time"        // the phase's time limit
	stopBudget      = "budget"      // the phase's byte limit
	stopNodeBudget  = "node_budget" // the node's grant ran out
	stopError       = "error"       // a load connection failed
	stopInterrupted = "interrupted" // the run was cancelled
)

// phaseSpec names a phase and the directions it loads.
type phaseSpec struct {
	name     string
	down, up bool
}

var phases = []phaseSpec{{"download", true, false}, {"upload", false, true}, {"bidirectional", true, true}}

// target is a session's load endpoints on one node address.
type target struct {
	addr, token, session string
	pin                  [32]byte
	dial                 nodeclient.DialFunc
	timeout              time.Duration
}

// transport opens connections like the control client does: the run's dialer,
// trust in the pin only, no proxy. A load transport holds one connection, so its
// load stream and the self probes on it share it; a probe transport keeps none.
func (t *target) transport(load bool, dial nodeclient.DialFunc) *http.Transport {
	tr := &http.Transport{
		DialContext:            func(ctx context.Context, network, _ string) (net.Conn, error) { return dial(ctx, network, t.addr) },
		TLSClientConfig:        protocol.ClientTLS(t.pin),
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    t.timeout,
		ResponseHeaderTimeout:  t.timeout,
		MaxResponseHeaderBytes: 8 << 10,
		DisableCompression:     true,
	}
	if load {
		tr.MaxConnsPerHost = 1
	} else {
		tr.DisableKeepAlives = true
	}
	return tr
}

func (t *target) request(ctx context.Context, method, endpoint string, body io.Reader) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, method, "https://"+t.addr+protocol.LoadPath(t.session, endpoint), body)
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept-Encoding", "identity") // §7: the server must not compress
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return req
}

// direction is one way of a phase's load. Goodput is counted where the bytes
// arrive: download bytes read from this host's sockets (TLS records, so payload
// is their tlsPayload share), upload body bytes the node reports having read.
// Counting elsewhere is wrong both ways — above the socket HTTP/2 holds a
// download's data before the body yields it, and bytes written on the sending
// side include whatever is still in flight, which grows with a deep queue.
// pending, delivered and total are written by its connections, the rest only by
// the interval loop.
type direction struct {
	down      bool
	payload   float64      // share of a counted byte that is payload
	pending   atomic.Int64 // counted bytes since the last interval boundary
	delivered atomic.Int64 // counted bytes in all
	atEnd     int64        // delivered when the phase stopped, or for uploads once the node\'s last report came
	total     atomic.Int64 // bytes on this host's sockets: the traffic
	series    []int64      // bytes per completed interval
	ma        []float64    // moving averages over full windows, bit/s
	settled   bool
	conns     int          // load connections opened, including those that never carried their stream
	failed    atomic.Int64 // of those, how many never did
	started   time.Time
	finalBPS  float64
	wires     []*wireConn // guarded by phaseRun.mu
}

// established is how many of d's connections carried, or may still carry, their
// stream.
func (d *direction) established() int { return d.conns - int(d.failed.Load()) }

// phaseRun is one phase in progress.
type phaseRun struct {
	t             *target
	id            time.Duration
	limit         int64 // bytes in all directions
	maxConns      int
	ctx           context.Context
	cancel        context.CancelFunc
	dirs          []*direction
	total         atomic.Int64
	goodput       atomic.Uint64 // math.Float64bits of the latest aggregate goodput, bytes/s
	stopCh        chan struct{}
	stopOnce      sync.Once
	stopWhy       string
	stopErr       error
	conns         sync.WaitGroup
	uploads       sync.WaitGroup // the upload connections, also in conns
	probes        sync.WaitGroup
	foreignFailed atomic.Int64
	mu            sync.Mutex        // guards the fields below
	connErr       error             // why the first load connection that failed to open did
	raw           []net.Conn        // every connection the phase's load dialed, closed when it ends
	ready         []*http.Transport // load connections carrying their stream: where self probes go
	foreign       []foreignProbe
	self          []selfProbe
}

// phaseResult is what one phase measured.
type phaseResult struct {
	spec          phaseSpec
	ran           bool
	start, end    time.Time
	settledAt     time.Time // goodput settled; zero if it never did
	stopped       string
	err           error
	dirs          []*direction
	conns         int
	waited        time.Duration // spent waiting for the last phase's queue to drain
	drained       bool          // and it did
	connFailed    int           // load connections that never carried their stream
	foreignFailed int           // foreign probes that did not complete
	capped        bool          // the grant, not the method, kept connections from being added
	bytes         int64
	rpm           responsiveness
	rpmConf       int
	gpConf        []int // per direction
	self          []time.Duration
	foreign       []time.Duration // TCP + TLS + HTTP of each foreign probe
	lagTicks      uint64
	lagOver       uint64
	cpuBusy       float64
	cpuKnown      bool
}

func (p *phaseRun) stop(why string, err error) {
	p.stopOnce.Do(func() {
		p.stopWhy, p.stopErr = why, err
		close(p.stopCh)
	})
}

// fail ends the phase with err unless the phase is already being torn down,
// when every connection's error is just the teardown.
func (p *phaseRun) fail(err error) {
	if p.ctx.Err() == nil {
		p.stop(stopError, err)
	}
}

// connectFailed records a load connection that never carried its stream. The
// phase goes on with the others: a path too slow to open a connection while
// loaded is a finding about the path, not a failure of the measurement.
func (p *phaseRun) connectFailed(d *direction, err error) {
	if p.ctx.Err() != nil {
		return
	}
	d.failed.Add(1)
	p.mu.Lock()
	if p.connErr == nil {
		p.connErr = err
	}
	p.mu.Unlock()
}

func (p *phaseRun) stopped() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

// spend adds n bytes on this host's sockets in direction d and stops the phase
// at its byte limit.
func (p *phaseRun) spend(d *direction, n int) {
	d.total.Add(int64(n))
	if p.total.Add(int64(n)) >= p.limit {
		p.stop(stopBudget, nil)
	}
}

// deliver adds n bytes that arrived in direction d to its goodput.
func deliver(d *direction, n int64) {
	d.pending.Add(n)
	d.delivered.Add(n)
}

func (p *phaseRun) markReady(tr *http.Transport) {
	p.mu.Lock()
	p.ready = append(p.ready, tr)
	p.mu.Unlock()
}

// wireConn counts what a load connection moves in its direction once armed,
// that is once it carries its load: the handshakes before do not count.
type wireConn struct {
	net.Conn
	p     *phaseRun
	d     *direction
	armed atomic.Bool
}

func (c *wireConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.d.down && n > 0 && c.armed.Load() {
		c.p.spend(c.d, n)
		deliver(c.d, int64(n))
	}
	return n, err
}

func (c *wireConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if !c.d.down && n > 0 && c.armed.Load() {
		c.p.spend(c.d, n)
	}
	return n, err
}

// open starts a load connection in direction d. Its socket is kept so the phase
// can close it at its end: a transport keeps a connection whose streams are still
// being torn down, and it would outlive the phase and its share of the grant.
func (p *phaseRun) open(d *direction) {
	var wc *wireConn
	lowat := d.unsentLimit(p.id, d.established()+1)
	tr := p.t.transport(true, func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := p.t.dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !d.down {
			netx.SetNotSentLowat(c, lowat)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.raw = append(p.raw, c)
		wc = &wireConn{Conn: c, p: p, d: d}
		d.wires = append(d.wires, wc)
		return wc, nil
	})
	arm := func() {
		p.mu.Lock()
		if wc != nil {
			wc.armed.Store(true)
		}
		p.mu.Unlock()
		p.markReady(tr)
	}
	d.conns++
	if !d.down {
		p.uploads.Add(1)
	}
	p.conns.Go(func() {
		if d.down {
			p.download(tr, d, arm)
		} else {
			defer p.uploads.Done()
			p.upload(tr, d, arm)
		}
	})
}

// download reads the large object; its end means the node's grant ran out. A
// connection that fails before the response begins never carried its stream.
func (p *phaseRun) download(tr *http.Transport, d *direction, arm func()) {
	resp, err := tr.RoundTrip(p.t.request(p.ctx, http.MethodGet, protocol.LoadLarge, nil))
	if err != nil {
		p.connectFailed(d, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.fail(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	arm()
	buf := make([]byte, chunk)
	for {
		_, err := resp.Body.Read(buf)
		switch {
		case err == io.EOF:
			p.stop(stopNodeBudget, nil)
			return
		case err != nil:
			p.fail(err)
			return
		}
	}
}

// uploadBody is an endless body of zeros that ends when the phase stops; its
// first read means the request is on its way, so the connection is armed.
type uploadBody struct {
	p     *phaseRun
	arm   func()
	armed atomic.Bool
}

func (b *uploadBody) Read(buf []byte) (int, error) {
	if b.p.stopped() || b.p.ctx.Err() != nil {
		return 0, io.EOF
	}
	if !b.armed.Load() {
		b.arm()
		b.armed.Store(true)
	}
	return copy(buf, zeros), nil
}

// upload posts until the phase stops and counts as goodput what the node reports
// it has read. A connection that fails before the node answers never carried its
// stream; the node's answer ending while the phase runs means its grant ran out.
func (p *phaseRun) upload(tr *http.Transport, d *direction, arm func()) {
	body := &uploadBody{p: p, arm: arm}
	resp, err := tr.RoundTrip(p.t.request(p.ctx, http.MethodPost, protocol.LoadUpload, body))
	switch {
	case err != nil && body.armed.Load():
		p.fail(err)
		return
	case err != nil:
		p.connectFailed(d, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.fail(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	var record [8]byte
	var read uint64
	for {
		_, err := io.ReadFull(resp.Body, record[:])
		switch {
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			if !p.stopped() {
				p.stop(stopNodeBudget, nil)
			}
			return
		case err != nil:
			p.fail(err)
			return
		}
		if v := binary.BigEndian.Uint64(record[:]); v > read {
			deliver(d, int64(v-read))
			read = v
		}
	}
}

// selfProbe fetches the small object on a random load connection (§5.3), with no
// priority over the load.
func (p *phaseRun) selfProbe() {
	p.mu.Lock()
	var tr *http.Transport
	if len(p.ready) > 0 {
		tr = p.ready[rand.IntN(len(p.ready))]
	}
	p.mu.Unlock()
	if tr == nil {
		return
	}
	start := time.Now()
	resp, err := tr.RoundTrip(p.t.request(p.ctx, http.MethodGet, protocol.LoadSmall, nil))
	if err != nil {
		return
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	done := time.Now()
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	p.mu.Lock()
	p.self = append(p.self, selfProbe{done: done, http: done.Sub(start)})
	p.mu.Unlock()
}

// foreignProbe fetches the small object on a new connection and times its TCP
// handshake, its TLS handshake and the exchange separately (§5.3).
func (p *phaseRun) foreignProbe() {
	var mu sync.Mutex
	var dialStart, dialDone, tlsStart, tlsDone, gotConn time.Time
	var conn net.Conn
	tr := p.t.transport(false, func(ctx context.Context, network, addr string) (net.Conn, error) {
		start := time.Now()
		c, err := p.t.dial(ctx, network, addr)
		mu.Lock()
		dialStart, dialDone, conn = start, time.Now(), c
		mu.Unlock()
		return c, err
	})
	defer func() {
		tr.CloseIdleConnections()
		mu.Lock()
		if conn != nil {
			conn.Close()
		}
		mu.Unlock()
	}()
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { mu.Lock(); tlsStart = time.Now(); mu.Unlock() },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { mu.Lock(); tlsDone = time.Now(); mu.Unlock() },
		GotConn:           func(httptrace.GotConnInfo) { mu.Lock(); gotConn = time.Now(); mu.Unlock() },
	}
	req := p.t.request(httptrace.WithClientTrace(p.ctx, trace), http.MethodGet, protocol.LoadSmall, nil)
	failed := func() {
		if p.ctx.Err() == nil { // not the phase's teardown
			p.foreignFailed.Add(1)
		}
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		failed()
		return
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	done := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if err != nil || resp.StatusCode != http.StatusOK || dialDone.IsZero() || tlsDone.IsZero() || gotConn.IsZero() {
		failed()
		return
	}
	f := foreignProbe{done: done, tcp: dialDone.Sub(dialStart), tls: tlsDone.Sub(tlsStart), http: done.Sub(gotConn)}
	p.mu.Lock()
	p.foreign = append(p.foreign, f)
	p.mu.Unlock()
}

// probeLoop alternates foreign and self probes, at most mps pairs a second and
// at most ptc of the latest goodput (§5.3), until the phase stops.
func (p *phaseRun) probeLoop() {
	slots := make(chan struct{}, probeHeadroom)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for n := 0; ; {
		gap := p.id
		if pairs := min(mps, ptc*math.Float64frombits(p.goodput.Load())/probePairBytes); pairs > 0 {
			gap = time.Duration(float64(time.Second) / (2 * pairs))
		}
		timer.Reset(gap)
		select {
		case <-p.stopCh:
			return
		case <-p.ctx.Done():
			return
		case <-timer.C:
		}
		if math.Float64frombits(p.goodput.Load()) == 0 {
			continue // no estimate yet, so no probe budget
		}
		if n%2 == 1 {
			p.probes.Go(p.selfProbe)
		} else {
			select {
			case slots <- struct{}{}:
				p.probes.Go(func() { defer func() { <-slots }(); p.foreignProbe() })
			default: // every foreign slot busy: skip rather than queue
			}
		}
		n++
	}
}

// window returns the probes that completed in (from, to].
func (p *phaseRun) window(from, to time.Time) ([]foreignProbe, []selfProbe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var f []foreignProbe
	var s []selfProbe
	for _, x := range p.foreign {
		if x.done.After(from) && !x.done.After(to) {
			f = append(f, x)
		}
	}
	for _, x := range p.self {
		if x.done.After(from) && !x.done.After(to) {
			s = append(s, x)
		}
	}
	return f, s
}

// runPhase runs §5.4 for one phase: a load connection per direction to start,
// one more per direction each interval, goodput settled first and then
// responsiveness, or until the time limit, the byte limit, an error or ctx.
func runPhase(ctx context.Context, t *target, spec phaseSpec, id, maxTime time.Duration, bytes int64, maxConns int) phaseResult {
	pctx, cancel := context.WithCancel(ctx)
	p := &phaseRun{t: t, id: id, limit: bytes, maxConns: maxConns, ctx: pctx, cancel: cancel, stopCh: make(chan struct{})}
	start := time.Now()
	if spec.down {
		p.dirs = append(p.dirs, &direction{down: true, payload: tlsPayload, started: start})
	}
	if spec.up {
		p.dirs = append(p.dirs, &direction{payload: 1, started: start})
	}
	r := phaseResult{spec: spec, ran: true, start: start, dirs: p.dirs}
	for _, d := range p.dirs {
		for range inp {
			p.open(d)
		}
	}
	p.probes.Go(p.probeLoop)

	tick := time.NewTicker(id)
	defer tick.Stop()
	deadline := time.NewTimer(maxTime)
	defer deadline.Stop()
	var rpms []float64
loop:
	for {
		select {
		case <-ctx.Done():
			p.stop(stopInterrupted, nil)
			break loop
		case <-p.stopCh:
			break loop
		case <-deadline.C:
			p.stop(stopTime, nil)
			break loop
		case now := <-tick.C:
			var bps float64
			settled := true
			for _, d := range p.dirs {
				d.series = append(d.series, d.pending.Swap(0))
				if k := len(d.series); k >= mad {
					var sum int64
					for _, b := range d.series[k-mad:] {
						sum += b
					}
					d.ma = append(d.ma, float64(sum)*8*d.payload/(mad*id.Seconds()))
				}
				d.settled = d.settled || stable(d.ma)
				settled = settled && d.settled
				bps += d.rate(id)
			}
			p.goodput.Store(math.Float64bits(bps / 8))
			p.limitUnsent()
			if settled && r.settledAt.IsZero() {
				r.settledAt = now
			}
			if !r.settledAt.IsZero() {
				if rr := rpmOf(p.window(now.Add(-mad*id), now)); rr.ok {
					r.rpm = rr
					rpms = append(rpms, rr.rpm)
				}
				if stable(rpms) {
					p.stop(stopSettled, nil)
					break loop
				}
			}
			for _, d := range p.dirs {
				for range inc {
					switch {
					case d.established() >= mnp:
					case p.totalConns() >= p.maxConns:
						r.capped = true
					default:
						p.open(d)
					}
				}
			}
		}
	}
	r.end = time.Now()
	for _, d := range p.dirs {
		if d.down {
			d.atEnd = d.delivered.Load()
		}
	}
	p.awaitUploads()
	for _, d := range p.dirs {
		if !d.down {
			d.atEnd = d.delivered.Load()
		}
	}
	cancel()
	p.conns.Wait()
	p.probes.Wait()
	p.mu.Lock()
	for _, c := range p.raw {
		c.Close()
	}
	p.mu.Unlock()

	r.stopped, r.err = p.stopWhy, p.stopErr
	r.bytes = p.total.Load()
	r.conns = p.totalConns()
	r.foreignFailed = int(p.foreignFailed.Load())
	for _, d := range p.dirs {
		r.connFailed += int(d.failed.Load())
	}
	if r.conns == 0 && r.stopped != stopError && r.stopped != stopInterrupted {
		r.stopped, r.err = stopError, p.connErr // not one load connection opened
	}
	for _, d := range p.dirs {
		d.finalBPS = d.rate(id)
		if len(d.series) == 0 { // stopped within the first interval
			if el := r.end.Sub(d.started).Seconds(); el > 0 {
				d.finalBPS = float64(d.atEnd) * 8 * d.payload / el
			}
		}
		r.gpConf = append(r.gpConf, confidence(len(d.series), d.settled))
	}
	if r.settledAt.IsZero() || !r.rpm.ok {
		r.rpm = rpmOf(p.window(r.end.Add(-mad*id), r.end))
	}
	r.rpmConf = confidence(len(rpms), r.stopped == stopSettled) // no values before goodput settled: low
	from := r.settledAt
	if from.IsZero() {
		from = r.end.Add(-mad * id)
	}
	f, s := p.window(from, r.end)
	for _, x := range f {
		r.foreign = append(r.foreign, x.tcp+x.tls+x.http)
	}
	for _, x := range s {
		r.self = append(r.self, x.http)
	}
	return r
}

// awaitUploads lets the uploads of a stopped phase end on their own, their bodies
// ended by the stop, so the node's last report of what it read arrives; at most
// uploadGrace.
func (p *phaseRun) awaitUploads() {
	done := make(chan struct{})
	go func() { p.uploads.Wait(); close(done) }() // ends once the uploads do, after cancel at the latest
	t := time.NewTimer(uploadGrace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-p.ctx.Done():
	}
}

// limitUnsent sets each upload connection's unsent limit to 5 ms of its share of
// the latest goodput. Platforms without the option keep their own buffering.
func (p *phaseRun) limitUnsent() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.dirs {
		if d.down || len(d.wires) == 0 {
			continue
		}
		n := d.unsentLimit(p.id, len(d.wires))
		for _, wc := range d.wires {
			netx.SetNotSentLowat(wc.Conn, n)
		}
	}
}

// unsentLimit is 5 ms of one of conns connections' share of d's latest goodput,
// or firstLowat before there is one. Only the interval loop may call it.
func (d *direction) unsentLimit(id time.Duration, conns int) int {
	bps := d.rate(id)
	if bps <= 0 {
		return firstLowat
	}
	return max(int(bps/8/float64(max(conns, 1))*lowatTime.Seconds()), minLowat)
}

// rate is the latest goodput of d in bit/s: the moving average once a full
// window exists, before that the mean of the intervals so far.
func (d *direction) rate(id time.Duration) float64 {
	if len(d.ma) > 0 {
		return d.ma[len(d.ma)-1]
	}
	if len(d.series) == 0 {
		return 0
	}
	var sum int64
	for _, b := range d.series {
		sum += b
	}
	return float64(sum) * 8 * d.payload / (float64(len(d.series)) * id.Seconds())
}

// totalConns counts the load connections that carried, or may still carry, their
// stream.
func (p *phaseRun) totalConns() int {
	n := 0
	for _, d := range p.dirs {
		n += d.established()
	}
	return n
}
