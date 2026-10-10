//go:build windows

// Package supervisor owns a winserial.Port and holds all the policy: what
// "healthy" means, when a link has degraded, and when to recover. The port
// itself holds no opinions and only counts, because a component that grades
// itself is trusting the thing that may be broken.
//
// It knows nothing about any UI. Everything it reports goes through a Sink,
// so the same policy drives a GUI, a CLI, a service or a test unchanged.
//
// # Three rules about not blocking the caller
//
// This package is typically driven from a GUI thread, and a GUI thread that
// waits on a serial syscall is an application that has hung.
//
//  1. THE MUTEX IS NEVER HELD ACROSS A SYSCALL. It protects fields, not
//     I/O. CreateFile against a re-enumerating USB adapter takes real time,
//     and holding the lock across it blocks State, Close, Write and Stop —
//     every entry point the caller has, including the one they would use to
//     escape. Commands that must do I/O publish their intent, release the
//     lock, do the I/O, then re-acquire and reconcile.
//
//  2. State IS PURE MEMORY. It returns the last snapshot the health loop
//     computed. Querying the device to answer "what is your state" is how a
//     status call ends up waiting on the dead thing it is reporting on.
//
//  3. THE EVENT PATH IS BOUNDED. A producer that can outrun its consumer
//     will eventually block inside its own emit, mid-loop, after which the
//     process still answers status while doing nothing on the wire. Events
//     are dropped and counted, never allowed to block. See limitedSink.
package supervisor

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"serialkit/winserial"
)

// Sink is how the supervisor talks to whatever is driving it. Every method
// may be called from a background goroutine; implementations must be safe
// for concurrent use and must not block for long (the reader goroutine and
// the 1 Hz health loop are on the other end).
//
// A nil Sink is legal — the supervisor then runs silently.
type Sink interface {
	// OnLog is a human-readable line. kind is "info" or "err".
	OnLog(kind, text string)
	// OnRx is a chunk received from the port (a copy — safe to keep).
	OnRx(b []byte)
	// OnTx is what was actually handed to the driver, n bytes of it.
	OnTx(b []byte, n int)
	// OnState is a snapshot emitted when something changed.
	OnState(st State)
	// OnHealth is the 1 Hz snapshot from the health loop.
	OnHealth(st State)
}

// Event-path budgets, per second. Generous for a human reading a monitor,
// and far below what a failing port can produce: a port failing every 200 ms
// across a 5 s degradation window, times a UI that re-renders on each line,
// is how the event path becomes the bottleneck. See limitedSink.
const (
	maxLogsPerSec = 40
	maxRxPerSec   = 200
	maxTxPerSec   = 200
)

// limitedSink bounds what reaches the real Sink, dropping and counting
// rather than blocking.
//
// A producer that can outrun its consumer eventually blocks inside its own
// emit call, mid-loop, after which it answers status perfectly while doing
// nothing on the wire. The counters and the caller's view then disagree,
// which is the only way such a stall is ever noticed. Any consumer that
// re-renders per event — a WebView bridge, a channel, a log sink — is the
// same back-pressure path.
// Suppressed counts are published in State so that silence which was CHOSEN
// stays distinguishable from silence that means breakage.
type limitedSink struct {
	inner Sink

	mu          sync.Mutex
	winStart    time.Time
	logs        int
	rx          int
	tx          int
	supLogs     uint64
	supRx       uint64
	supTx       uint64
	totSupessed atomic.Uint64
}

func newLimitedSink(inner Sink) *limitedSink {
	return &limitedSink{inner: inner, winStart: time.Now()}
}

// admit returns whether this event may pass, rolling the 1 s window and
// emitting a single summary line for whatever was dropped in the window
// that just ended.
func (l *limitedSink) admit(kind string) bool {
	l.mu.Lock()
	if time.Since(l.winStart) >= time.Second {
		dropped := l.supLogs + l.supRx + l.supTx
		sl, sr, stx := l.supLogs, l.supRx, l.supTx
		l.winStart = time.Now()
		l.logs, l.rx, l.tx = 0, 0, 0
		l.supLogs, l.supRx, l.supTx = 0, 0, 0
		l.mu.Unlock()
		if dropped > 0 && l.inner != nil {
			l.totSupessed.Add(dropped)
			// Reports its own suppression, and is itself inside the next
			// window's budget, so it cannot become the firehose.
			l.inner.OnLog("err", fmt.Sprintf(
				"EVENT RATE LIMIT: dropped %d event(s) in the last second (%d log, %d rx, %d tx) — "+
					"the port is producing faster than this UI can render; counters are unaffected",
				dropped, sl, sr, stx))
		}
		l.mu.Lock()
	}
	ok := false
	switch kind {
	case "log":
		if l.logs < maxLogsPerSec {
			l.logs++
			ok = true
		} else {
			l.supLogs++
		}
	case "rx":
		if l.rx < maxRxPerSec {
			l.rx++
			ok = true
		} else {
			l.supRx++
		}
	case "tx":
		if l.tx < maxTxPerSec {
			l.tx++
			ok = true
		} else {
			l.supTx++
		}
	}
	l.mu.Unlock()
	return ok
}

func (l *limitedSink) suppressed() uint64 { return l.totSupessed.Load() }

func (l *limitedSink) OnLog(kind, text string) {
	if l.inner != nil && l.admit("log") {
		l.inner.OnLog(kind, text)
	}
}

func (l *limitedSink) OnRx(b []byte) {
	if l.inner != nil && l.admit("rx") {
		l.inner.OnRx(b)
	}
}

func (l *limitedSink) OnTx(b []byte, n int) {
	if l.inner != nil && l.admit("tx") {
		l.inner.OnTx(b, n)
	}
}

// State and health snapshots are never dropped: they are small, they are
// already rate-limited by the 1 Hz loop, and they are what the operator
// reads to find out that events are being dropped.
func (l *limitedSink) OnState(st State) {
	if l.inner != nil {
		l.inner.OnState(st)
	}
}

func (l *limitedSink) OnHealth(st State) {
	if l.inner != nil {
		l.inner.OnHealth(st)
	}
}

// State is a full snapshot. It is computed by the health loop and by each
// command, and served from memory by State() — never computed by querying
// the device on the caller's thread (rule 2).
type State struct {
	Open        bool            `json:"open"`
	Port        string          `json:"port"`
	Config      string          `json:"config"`
	DTR         bool            `json:"dtr"`
	RTS         bool            `json:"rts"`
	Modem       map[string]bool `json:"modem"`
	Stats       winserial.Stats `json:"stats"`
	Healthy     bool            `json:"healthy"`
	WdAdvancing bool            `json:"wdAdvancing"`
	Degraded    bool            `json:"degraded"`
	AutoRecycle bool            `json:"autoRecycle"`
	Recycles    int             `json:"recycles"`
	UptimeS     int             `json:"uptimeS"`
	PID         int             `json:"pid"`
	// Opening is true while a connect attempt is in flight. Without it the
	// UI cannot tell "not connected" from "connecting", and an operator
	// clicking Open twice on a slow adapter gets a confusing refusal.
	Opening bool `json:"opening"`
	// Present is the registry's answer to "does this port exist right now".
	// It is the authoritative unplug signal; everything else is inference.
	Present bool `json:"present"`
	// Vanished means the port was open and has since disappeared from the
	// machine. Distinct from Degraded, which is inferred from failures.
	Vanished bool `json:"vanished"`
	// Wedged means the port's handle could not be cancelled and was leaked.
	Wedged bool `json:"wedged"`
	// LeakedHandles is process-lifetime. A number that climbs is a driver
	// that will not cancel I/O.
	LeakedHandles int `json:"leakedHandles"`
	// EventsSuppressed is how many events the rate limiter dropped. Silence
	// that was chosen must be distinguishable from silence that is breakage.
	EventsSuppressed uint64 `json:"eventsSuppressed"`
	// TxQueued is the depth of the async transmit queue.
	TxQueued int `json:"txQueued"`
	// TxDropped counts writes refused because that queue was full.
	TxDropped uint64 `json:"txDropped"`
}

// txDepth is the async transmit queue. Deep enough to absorb a burst of
// operator sends or a repeat-timer, shallow enough that a stalled port
// refuses new work promptly instead of hoarding it.
const txDepth = 64

// Supervisor owns exactly one port at a time.
type Supervisor struct {
	sink *limitedSink

	mu       sync.Mutex
	port     *winserial.Port
	lastCfg  winserial.Config
	dtr, rts bool
	// gen is bumped by Open and Close. A recycleLoop captures gen at start
	// and re-checks it on every attempt; a stale generation means the port
	// has since been closed or reopened out from under it, so the loop must
	// stop rather than race to reopen a port nobody is expecting.
	gen int
	// opening is set while an Open is in flight with the lock RELEASED, so
	// that a second Open, or a recycle attempt, can see a connect is
	// already happening (rule 1).
	opening bool

	// wdBase is added to the current port's raw Watchdog counter so that
	// State.Stats.Watchdog survives a recycle. Each winserial.Port starts
	// its own counter at 0, so without this a consumer watching the number
	// would see it drop on every auto-recycle — indistinguishable from a
	// crash loop that keeps resetting to "looks fine". A reset reads as
	// "the program restarted", a louder and wronger claim than whatever
	// actually happened. wdBase accumulates the closed port's last count on
	// every recycle, and is reset to 0 only by an explicit Open, which is a
	// deliberate new life rather than a recovery.
	wdBase uint64

	// health / AND-gate inputs
	prevWd      uint64
	wdAdvancing bool
	degraded    bool
	vanished    bool
	autoRecycle bool
	recycling   bool
	recycles    int
	errWindow   []time.Time // timestamps of recent errors for the streak lever
	startedAt   time.Time

	// snap is the last computed snapshot. State() serves this and touches
	// no device (rule 2).
	snap State

	txq       chan []byte
	txDropped atomic.Uint64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New returns a supervisor with auto-recovery enabled. Call Start to begin
// the health loop and the transmit writer.
func New(sink Sink) *Supervisor {
	s := &Supervisor{
		sink:        newLimitedSink(sink),
		autoRecycle: true,
		startedAt:   time.Now(),
		stop:        make(chan struct{}),
		txq:         make(chan []byte, txDepth),
	}
	s.snap = State{PID: os.Getpid(), Modem: map[string]bool{}, AutoRecycle: true}
	return s
}

// ListPorts enumerates the machine's COM ports (adapter-agnostic).
func ListPorts() ([]string, error) { return winserial.ListPorts() }

// Start launches the 1 Hz health loop and the async transmit writer.
func (s *Supervisor) Start() {
	s.wg.Add(2)
	go s.healthLoop()
	go s.writerLoop()
}

// Stop halts the loops and closes any open port. After Stop the Supervisor
// is done; create a new one to start again.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	p := s.port
	s.port = nil
	s.gen++
	s.mu.Unlock()
	if p != nil {
		_ = p.Close()
	}
	// Bounded: a wedged port's Close already returns within its own
	// deadlines, but shutdown must not depend on that being true.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.log("err", "SHUTDOWN: supervisor loops did not exit within 5 s")
	}
}

// ---- sink helpers ------------------------------------------------------

func (s *Supervisor) log(kind, text string) { s.sink.OnLog(kind, text) }

func (s *Supervisor) emitState(st State) { s.sink.OnState(st) }

// ---- commands ----------------------------------------------------------

// Open opens the port. Everything the port needs is in cfg — nothing is
// read from a config file or assumed (idle until commanded, with all
// commanded, with all parameters carried in the command).
//
// The CreateFile happens with s.mu RELEASED (rule 1). An "opening" flag and
// the generation counter are published first so that a concurrent Open, a
// Close, or an in-flight recycle can all see what is happening and
// reconcile afterwards.
func (s *Supervisor) Open(cfg winserial.Config) error {
	s.mu.Lock()
	if s.port != nil {
		name := s.port.Config().Name
		s.mu.Unlock()
		return fmt.Errorf("already open on %s", name)
	}
	if s.opening {
		s.mu.Unlock()
		return fmt.Errorf("a connect to %s is already in progress", s.lastCfg.Name)
	}
	s.opening = true
	s.lastCfg = cfg
	// Claim the generation now: this invalidates any auto-recycle attempt
	// already in flight, so it cannot install a port behind this one.
	s.gen++
	gen := s.gen
	s.mu.Unlock()

	s.refreshSnap()

	// No lock held across this call.
	p, err := winserial.Open(cfg, s.onData, s.onErr)

	s.mu.Lock()
	s.opening = false
	if err != nil {
		s.mu.Unlock()
		s.log("err", err.Error())
		s.refreshSnap()
		return err
	}
	if s.gen != gen || s.port != nil {
		// Someone acted while CreateFile was in flight. Discard rather than
		// install a port nobody is expecting.
		s.mu.Unlock()
		_ = p.Close()
		s.log("info", "OPEN: discarding "+cfg.Name+" (superseded while connecting)")
		s.refreshSnap()
		return fmt.Errorf("open of %s superseded", cfg.Name)
	}
	s.port = p
	s.dtr, s.rts = cfg.DTR, cfg.RTS
	s.degraded = false
	s.vanished = false
	s.errWindow = nil
	s.prevWd = 0
	s.wdBase = 0 // a deliberate new life, not a recovery
	s.mu.Unlock()

	s.log("info", fmt.Sprintf("OPEN %s %d %d%s%s hs=%s dtr=%v rts=%v gap=%dms wto=%dms",
		cfg.Name, cfg.Baud, cfg.DataBits, ParityLetter(cfg.Parity), cfg.StopBits,
		cfg.Handshake, cfg.DTR, cfg.RTS, cfg.ReadGapMs, cfg.WriteTimeoutMs))
	st := s.refreshSnap()
	s.emitState(st)
	return nil
}

// Close is the operator's explicit disconnect. Bounded: winserial.Close
// cannot block indefinitely, and a port it could not prove idle is reported
// as wedged rather than silently closed.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	p := s.port
	s.port = nil
	s.degraded = false
	if p != nil {
		// Carry this life's count forward so the watchdog stalls rather than
		// drops while nothing is open. Only an operator Open resets it, which
		// is a deliberate new life rather than a recovery.
		s.wdBase += p.Stats().Watchdog
	}
	s.vanished = false
	s.gen++
	s.mu.Unlock()
	if p == nil {
		s.refreshSnap()
		return nil
	}
	err := p.Close()
	if err != nil {
		s.log("err", "CLOSE "+p.Config().Name+": "+err.Error())
	} else {
		s.log("info", "CLOSE "+p.Config().Name)
	}
	st := s.refreshSnap()
	s.emitState(st)
	return err
}

// Write transmits b synchronously and reports the result. The bytes are
// already framed by the caller — the supervisor adds nothing to the wire.
//
// This blocks for as long as the bytes need on the wire (8 KiB at 9600 baud
// is ~9.4 s, legitimately). Do NOT call it from a UI thread; use WriteAsync.
func (s *Supervisor) Write(b []byte) (int, error) {
	s.mu.Lock()
	p := s.port
	s.mu.Unlock()
	if p == nil {
		return 0, fmt.Errorf("port not open")
	}
	n, err := p.Write(b)
	s.sink.OnTx(b[:n], n)
	if err != nil {
		s.log("err", "TX "+err.Error())
	}
	return n, err
}

// WriteAsync queues b for transmission and returns at once. It is the only
// write a UI thread should use.
//
// The error reports whether the bytes were ACCEPTED, never whether they
// went out; the outcome arrives through the Sink, which is where every
// other asynchronous fact about the port already arrives. A full queue is
// refused and counted rather than waited on: waiting is how the operator's
// next click ends up behind a 10-second write to a port that is gone.
func (s *Supervisor) WriteAsync(b []byte) error {
	s.mu.Lock()
	open := s.port != nil
	s.mu.Unlock()
	if !open {
		return fmt.Errorf("port not open")
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case s.txq <- cp:
		return nil
	default:
		n := s.txDropped.Add(1)
		return fmt.Errorf("transmit queue full (%d deep) — %d dropped so far; the port is not draining", txDepth, n)
	}
}

// writerLoop is the single owner of the transmit path: one writer, so the
// wire is the lock and there is never a second writer racing it.
func (s *Supervisor) writerLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case b := <-s.txq:
			s.mu.Lock()
			p := s.port
			s.mu.Unlock()
			if p == nil {
				s.log("err", "TX dropped: port not open")
				continue
			}
			n, err := p.Write(b)
			s.sink.OnTx(b[:n], n)
			if err != nil {
				s.log("err", "TX "+err.Error())
			}
		}
	}
}

func (s *Supervisor) SetDTR(on bool) error {
	s.mu.Lock()
	p := s.port
	s.dtr = on
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.SetDTR(on)
}

func (s *Supervisor) SetRTS(on bool) error {
	s.mu.Lock()
	p := s.port
	s.rts = on
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.SetRTS(on)
}

func (s *Supervisor) SendBreak(d time.Duration) error {
	s.mu.Lock()
	p := s.port
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("port not open")
	}
	s.log("info", fmt.Sprintf("BREAK %d ms", d.Milliseconds()))
	return p.SendBreak(d)
}

func (s *Supervisor) Purge() error {
	s.mu.Lock()
	p := s.port
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Purge()
}

// SetAutoRecycle turns the bounded auto-recovery lever on or off.
func (s *Supervisor) SetAutoRecycle(on bool) {
	s.mu.Lock()
	s.autoRecycle = on
	s.snap.AutoRecycle = on
	s.mu.Unlock()
}

// State returns the most recent snapshot. Pure memory: no syscall, no
// device query, no lock held by anything doing I/O (rule 2). Safe to call
// from a UI thread while the port is dead, wedged or vanished.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// refreshSnap recomputes the snapshot and stores it. The device queries it
// makes (modem status, presence) happen with s.mu RELEASED; only the
// assignment is locked.
func (s *Supervisor) refreshSnap() State {
	s.mu.Lock()
	st := s.stateLocked()
	p := s.port
	name := st.Port
	if name == "" {
		name = s.lastCfg.Name
	}
	s.mu.Unlock()

	// Out-of-band queries, outside the lock.
	if p != nil {
		st.Modem = modemMap(p)
	}
	if name != "" {
		if present, err := winserial.Present(name); err == nil {
			st.Present = present
		}
	}
	st.LeakedHandles = winserial.LeakedHandles()
	st.EventsSuppressed = s.sink.suppressed()
	st.TxQueued = len(s.txq)
	st.TxDropped = s.txDropped.Load()

	s.mu.Lock()
	s.snap = st
	s.mu.Unlock()
	return st
}

// modemMap queries modem status without holding s.mu — see the comment in
// stateLocked for why that matters.
func modemMap(p *winserial.Port) map[string]bool {
	if ms, err := p.Modem(); err == nil {
		return map[string]bool{"cts": ms.CTS, "dsr": ms.DSR, "ri": ms.RI, "dcd": ms.DCD}
	}
	return map[string]bool{}
}

func (s *Supervisor) stateLocked() State {
	st := State{
		AutoRecycle: s.autoRecycle, Recycles: s.recycles,
		UptimeS:     int(time.Since(s.startedAt).Seconds()),
		WdAdvancing: s.wdAdvancing, Degraded: s.degraded,
		Modem: map[string]bool{},
		DTR:   s.dtr, RTS: s.rts,
		PID:      os.Getpid(),
		Opening:  s.opening,
		Vanished: s.vanished,
	}
	if s.port != nil {
		c := s.port.Config()
		st.Open = true
		st.Port = c.Name
		st.Config = fmt.Sprintf("%d %d%s%s", c.Baud, c.DataBits, ParityLetter(c.Parity), c.StopBits)
		st.Stats = s.port.Stats()
		st.Stats.Watchdog += s.wdBase
		st.Wedged = st.Stats.Wedged
		// Modem is deliberately NOT queried here: stateLocked runs with s.mu
		// held, and Port.Modem() can synchronously invoke onErr on failure
		// (e.g. GetCommModemStatus on a vanished device), which tries to
		// re-lock s.mu — a self-deadlock that then wedges Close/Write/Stop
		// for everyone. A virtual COM pair never fails this way, so it only
		// appears against real hardware. Callers fetch
		// it via modemMap after releasing the lock.
		//
		// healthy = open AND watchdog advancing AND not degraded AND not
		// degraded AND not vanished AND not wedged. The AND is here and
		// nowhere else; winserial.Stats deliberately has no Healthy field.
		st.Healthy = st.Open && s.wdAdvancing && !s.degraded && !s.vanished && !st.Wedged
	} else {
		// The port is gone — closed, or swapped out mid-recycle. Stats is
		// the zero value here, so Watchdog would read 0 and a consumer
		// comparing samples would see it DROP, which reads as "the program
		// restarted" rather than "the adapter was unplugged". Carrying
		// wdBase makes the counter STALL across the outage instead of
		// resetting; wdAdvancing already reports that it is not moving.
		st.Stats.Watchdog = s.wdBase
		// Name the port even though none is installed. A caller recovering
		// a lost link still needs to know which port is being waited on, and
		// Present needs a name to report against; leaving this empty blanks
		// the one authoritative unplug signal there is.
		st.Port = s.lastCfg.Name
	}
	return st
}

// ParityLetter renders a parity name as its single-letter form.
func ParityLetter(p string) string {
	switch strings.ToLower(p) {
	case "odd":
		return "O"
	case "even":
		return "E"
	case "mark":
		return "M"
	case "space":
		return "S"
	}
	return "N"
}

// ---- callbacks from the port goroutines --------------------------------

func (s *Supervisor) onData(b []byte) { s.sink.OnRx(b) }

// onErr runs on the port's own goroutines. It must not call back into the
// port, and it must not do I/O: it records and classifies, nothing more.
func (s *Supervisor) onErr(err error) {
	s.log("err", err.Error())
	s.mu.Lock()
	t := time.Now()
	s.errWindow = append(s.errWindow, t)
	// keep a 5 s window
	cut := t.Add(-5 * time.Second)
	i := 0
	for i < len(s.errWindow) && s.errWindow[i].Before(cut) {
		i++
	}
	s.errWindow = s.errWindow[i:]
	// Adapter-vanish signature: >= 10 errors within 5 s. A STREAK, never an
	// error code. Two real CP210x pulls produced 995 -> 5 -> 2 within ~31 ms
	// while an earlier recorded pull on the same bench produced a stream of
	// 22 and neither live pull reproduced it; a code-matching detector would
	// have missed both pulls, and the streak rule caught both.
	if len(s.errWindow) >= 10 && !s.degraded {
		s.degraded = true
		s.log("err", "LINK DEGRADED (10 errors in 5000 ms) — connected flag no longer trusted")
	}
	s.mu.Unlock()
}

// ---- 1 Hz health loop: heartbeat + watchdog + bounded auto-recycle -----

func (s *Supervisor) healthLoop() {
	defer s.wg.Done()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}

		s.mu.Lock()
		p := s.port
		name := s.lastCfg.Name
		s.mu.Unlock()

		// Probes, outside the lock. CheckLine is the cheap honest liveness
		// test on the handle itself; Present is the authoritative answer to
		// "is the adapter still plugged in".
		gone := false
		if p != nil {
			if _, err := p.CheckLine(); err != nil {
				gone = true
			}
			if present, err := winserial.Present(name); err == nil && !present {
				gone = true
			}
			if p.Wedged() {
				gone = true
			}
		}

		s.mu.Lock()
		if s.port != nil {
			wd := s.port.Stats().Watchdog
			s.wdAdvancing = wd != s.prevWd
			s.prevWd = wd

			// An unplugged adapter is a FACT once the port has left the
			// machine, not an inference from a failure streak — so it does
			// not wait for ten errors in five seconds.
			if gone && !s.vanished {
				s.vanished = true
				s.degraded = true
				s.log("err", "PORT VANISHED: "+name+" is no longer present on this machine — "+
					"open handle is dead regardless of what it reports")
			}

			// Bounded auto-recycle (adapter-vanish lever): close the dead
			// handle and reopen; a failed reopen is retried every 5 s.
			if s.degraded && s.autoRecycle && !s.recycling && !s.opening {
				s.recycling = true
				old := s.port
				s.port = nil
				s.wdBase += old.Stats().Watchdog
				cfg := s.lastCfg
				s.gen++
				gen := s.gen
				s.mu.Unlock()
				if err := old.Close(); err != nil {
					s.log("err", "RECYCLE: close of "+cfg.Name+" was not clean: "+err.Error())
				}
				s.log("info", "RECYCLE: close+reopen "+cfg.Name)
				go s.recycleLoop(cfg, gen)
				s.mu.Lock()
			}
		} else {
			s.wdAdvancing = false
			// A link that was LOST keeps trying to come back, and recovery is
			// driven from here rather than from whoever happened to start the
			// last retry goroutine.
			//
			// The retry loop can legitimately die: Open bumps the generation
			// before calling CreateFile so that an in-flight recycle cannot
			// install a port behind a caller who is connecting. That alone
			// would let a FAILED Open during an outage orphan recovery for
			// good. Restarting from the health loop makes recovery
			// impossible to orphan, whatever ended the previous attempt.
			//
			// Gated on vanished/degraded, which an explicit Close clears, so
			// this never reopens a port somebody deliberately disconnected.
			if (s.vanished || s.degraded) && s.autoRecycle && !s.recycling &&
				!s.opening && s.lastCfg.Name != "" {
				s.recycling = true
				cfg := s.lastCfg
				s.gen++
				gen := s.gen
				s.mu.Unlock()
				s.log("info", "RECYCLE: resuming recovery of "+cfg.Name)
				go s.recycleLoop(cfg, gen)
				s.mu.Lock()
			}
		}
		s.mu.Unlock()

		st := s.refreshSnap()
		s.sink.OnHealth(st)
	}
}

// recycleLoop tries to reopen immediately, then every 5 s, until it
// succeeds or auto-recycle is turned off / something else is opened. gen is
// the generation this loop was spawned under; if Open or Close bumps the
// generation (the operator acted while this loop was retrying), the loop
// abandons the retry instead of reopening a port nobody asked for anymore.
//
// Recovery genuinely needs the retry: on both real adapter pulls, reopening
// on the same live process worked, but only on the SECOND successful
// attempt, because the device was still re-enumerating. A one-shot cure
// would have failed both times.
func (s *Supervisor) recycleLoop(cfg winserial.Config, gen int) {
	defer func() {
		s.mu.Lock()
		s.recycling = false
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		if s.port != nil || !s.autoRecycle || s.gen != gen || s.opening {
			s.degraded = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		select {
		case <-s.stop:
			return
		default:
		}

		// Do not hammer CreateFile at a port that is not there: presence is
		// cheap and conclusive, and an absent port is a configuration or
		// physical fact, not a transient to spin on.
		if present, err := winserial.Present(cfg.Name); err == nil && !present {
			s.log("info", "RECYCLE: "+cfg.Name+" still absent — waiting for it to reappear")
			if !s.sleepOrStop(5 * time.Second) {
				return
			}
			continue
		}

		p, err := winserial.Open(cfg, s.onData, s.onErr)
		if err != nil {
			s.log("err", "RECYCLE FAILED: "+err.Error()+" — retry in 5 s")
			s.emitState(s.refreshSnap())
			if !s.sleepOrStop(5 * time.Second) {
				return
			}
			continue
		}
		s.mu.Lock()
		if s.gen != gen {
			// Operator acted while this attempt was in flight (opening a
			// port takes real time). Discard the just-opened port instead
			// of installing it behind the operator's back.
			s.mu.Unlock()
			_ = p.Close()
			s.log("info", "RECYCLE: discarding stale reopen of "+cfg.Name+" (operator acted first)")
			return
		}
		s.port = p
		s.degraded = false
		s.vanished = false
		s.errWindow = nil
		s.prevWd = 0
		s.recycles++
		n := s.recycles
		s.mu.Unlock()
		s.log("info", fmt.Sprintf("LINK RECYCLED OK (#%d)", n))
		s.emitState(s.refreshSnap())
		return
	}
}

// sleepOrStop waits d, or returns false if the supervisor is shutting down.
// A plain time.Sleep here makes Stop wait for the full retry interval.
func (s *Supervisor) sleepOrStop(d time.Duration) bool {
	select {
	case <-s.stop:
		return false
	case <-time.After(d):
		return true
	}
}
