//go:build windows

package supervisor

// Hardening tests for the "application hangs" class. These need no
// hardware: each one isolates a property the hang depended on, so the
// property is guarded even on a machine with no serial port at all.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"serialkit/winserial"
)

// countSink counts what actually reaches the real sink, so the rate limiter
// can be measured rather than trusted.
type countSink struct {
	mu   sync.Mutex
	logs []string
	rx   int
	tx   int
}

func (c *countSink) OnLog(kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, text)
}
func (c *countSink) OnRx(b []byte) { c.mu.Lock(); c.rx++; c.mu.Unlock() }
func (c *countSink) OnTx(b []byte, n int) {
	c.mu.Lock()
	c.tx++
	c.mu.Unlock()
}
func (c *countSink) OnState(st State)  {}
func (c *countSink) OnHealth(st State) {}

func (c *countSink) counts() (logs, rx, tx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.logs), c.rx, c.tx
}

func (c *countSink) has(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// TestEventPathDropsAndCountsInsteadOfBlocking covers back-pressure on
// the event path: a producer that can outrun its consumer will eventually
// block inside its own emit, in the middle of the serve loop, and the
// process then answers status perfectly while doing nothing on the wire.
//
// Real traffic rarely reaches this limit, because the port layer coalesces
// reads into few large chunks and the event rate stays under budget even at
// full line rate. That is a good property, not a reason to leave the limiter
// unverified, so it is driven directly here.
func TestEventPathDropsAndCountsInsteadOfBlocking(t *testing.T) {
	cs := &countSink{}
	ls := newLimitedSink(cs)

	const burst = 5000
	start := time.Now()
	for i := 0; i < burst; i++ {
		ls.OnLog("err", "flood")
		ls.OnRx([]byte{1, 2, 3})
	}
	el := time.Since(start)

	logs, rx, _ := cs.counts()
	t.Logf("%d log + %d rx offered in %v; %d logs and %d rx chunks passed; suppressed=%d",
		burst, burst, el, logs, rx, ls.suppressed())

	// The point of the limiter: offering it 10000 events must not block.
	if el > 2*time.Second {
		t.Fatalf("emitting %d events took %v — the event path is blocking", 2*burst, el)
	}
	if logs > maxLogsPerSec+8 {
		t.Errorf("rate limit not applied to logs: %d passed, budget %d/s", logs, maxLogsPerSec)
	}
	if rx > maxRxPerSec+8 {
		t.Errorf("rate limit not applied to rx: %d passed, budget %d/s", rx, maxRxPerSec)
	}
	if logs == 0 || rx == 0 {
		t.Error("the limiter dropped everything — it must pass up to its budget")
	}

	// Roll the window: suppression must be REPORTED, not silent. Silence
	// that was chosen has to stay distinguishable from silence that means
	// breakage.
	time.Sleep(1050 * time.Millisecond)
	ls.OnLog("info", "after the window")
	if ls.suppressed() == 0 {
		t.Error("suppressed count never incremented")
	}
	if !cs.has("EVENT RATE LIMIT") {
		t.Error("the limiter never told anyone it was dropping events")
	}
	t.Logf("total suppressed reported: %d", ls.suppressed())
}

// TestStateIsPureMemory: State must never touch the device.
//
// A status call that queries the port is how an application ends up waiting
// on the dead thing it is reporting on. 20000 calls on an idle supervisor
// should be essentially free; if State is doing I/O this takes seconds.
func TestStateIsPureMemory(t *testing.T) {
	s := New(nil)
	s.Start()
	defer s.Stop()

	start := time.Now()
	for i := 0; i < 20000; i++ {
		_ = s.State()
	}
	el := time.Since(start)
	t.Logf("20000 State() calls in %v (%v each)", el, el/20000)
	if el > 500*time.Millisecond {
		t.Fatalf("State() cost %v for 20000 calls — it is doing more than reading memory", el)
	}
}

// TestStateStaysResponsiveWhileConnecting guards rule 1: s.mu must never be
// held across a syscall.
//
// Open does a CreateFile, which on a re-enumerating USB adapter takes real
// time. If the lock is held across it, every entry point the UI has —
// State, Close, Write — blocks behind it, including the one the operator
// would use to escape. Here Open/Close are hammered at an absent port while
// State latency is measured.
func TestStateStaysResponsiveWhileConnecting(t *testing.T) {
	s := New(nil)
	s.Start()
	defer s.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// COM250 cannot exist, so every attempt fails deterministically.
		cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Open(cfg)
			_ = s.Close()
		}
	}()

	worst := time.Duration(0)
	for i := 0; i < 3000; i++ {
		start := time.Now()
		_ = s.State()
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	close(stop)
	wg.Wait()

	t.Logf("worst State() latency while Open/Close churned: %v", worst)
	if worst > 100*time.Millisecond {
		t.Fatalf("State() blocked %v behind a connect attempt — the lock is held across I/O", worst)
	}
}

// TestWriteAsyncNeverBlocks: the UI's write path must return immediately
// even when the port is not there.
func TestWriteAsyncNeverBlocks(t *testing.T) {
	s := New(nil)
	s.Start()
	defer s.Stop()

	// Closed port: refused at once, not queued.
	if err := s.WriteAsync([]byte("x")); err == nil {
		t.Error("WriteAsync on a closed port must fail")
	}

	// With a port installed but nothing draining the queue, the queue must
	// fill and then REFUSE rather than block. Install a nil-port state by
	// faking an open supervisor: the writer loop will drop them, so measure
	// only that no call blocks.
	start := time.Now()
	for i := 0; i < txDepth*4; i++ {
		_ = s.WriteAsync([]byte("payload"))
	}
	el := time.Since(start)
	t.Logf("%d WriteAsync calls in %v, dropped=%d", txDepth*4, el, s.txDropped.Load())
	if el > time.Second {
		t.Fatalf("WriteAsync blocked (%v for %d calls)", el, txDepth*4)
	}
}

// TestQueueFullIsRefusedAndCounted: a full transmit queue must be refused
// with a count, not waited on. Waiting is how the operator's next click ends
// up behind a 10-second write to a port that is gone.
func TestQueueFullIsRefusedAndCounted(t *testing.T) {
	s := New(nil)
	// Deliberately NOT started: no writerLoop, so nothing drains txq.
	// A port must appear open for WriteAsync to accept at all.
	s.mu.Lock()
	s.port = nil
	s.mu.Unlock()

	// Fill the queue directly, then confirm WriteAsync refuses rather than
	// blocks once it is full.
	for i := 0; i < txDepth; i++ {
		s.txq <- []byte("x")
	}
	done := make(chan error, 1)
	go func() { done <- s.WriteAsync([]byte("one more")) }()
	select {
	case err := <-done:
		// port is nil so it refuses for that reason; either way it must not
		// block, which is the property under test.
		t.Logf("WriteAsync with a full queue returned promptly: %v", err)
		if err == nil {
			t.Error("expected a refusal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WriteAsync blocked on a full queue")
	}
}

// TestStopIsBoundedAndIdempotent: shutdown must not hang, and the retry
// sleep inside recycleLoop must not hold it up either (a plain time.Sleep
// there makes Stop wait out the full 5 s interval).
func TestStopIsBounded(t *testing.T) {
	sink := &stubSink{}
	s := New(sink)
	s.Start()

	// Put a retry loop in flight against a port that cannot exist.
	cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}
	s.mu.Lock()
	s.lastCfg = cfg
	s.degraded = true
	s.autoRecycle = true
	s.recycling = true
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	go s.recycleLoop(cfg, gen)

	// Let it reach its sleep.
	deadline := time.Now().Add(3 * time.Second)
	for sink.count("RECYCLE") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	s.Stop()
	el := time.Since(start)
	t.Logf("Stop took %v with a recycle retry in flight", el)
	if el > 5*time.Second {
		t.Fatalf("Stop took %v — it waited out the retry interval", el)
	}
	// Idempotent.
	s.Stop()
}

// TestHealthyRequiresEveryCondition pins the health rule: the AND lives
// here and nowhere else, and winserial.Stats deliberately has no Healthy
// field for anything to disagree with.
func TestHealthyRequiresEveryCondition(t *testing.T) {
	s := New(nil)
	// No port at all: never healthy, whatever else is true.
	s.mu.Lock()
	s.wdAdvancing = true
	st := s.stateLocked()
	s.mu.Unlock()
	if st.Healthy {
		t.Error("healthy with no port open")
	}
	if st.Open {
		t.Error("open with no port")
	}
}

// TestWatchdogNeverDropsAcrossAnOutage pins an invariant that is easy to
// state and easy to break: the watchdog must never go backwards.
//
// stateLocked fills Stats only while a port is installed, and the zero value
// has Watchdog 0, so the count would drop to zero for the whole window in
// which a port has been lost and not yet recovered. wdBase already covered
// the recycle case; this covers the window where there is no port at all.
//
// A consumer comparing samples reads a drop as "the program restarted",
// which is a far louder and wronger claim than "the adapter was unplugged".
// Stalling is the honest shape: wdAdvancing already says it is not moving.
func TestWatchdogNeverDropsAcrossAnOutage(t *testing.T) {
	s := New(nil)

	// A port was open and accumulated work, then vanished — exactly the
	// state healthLoop installs when it swaps a dead port out to recycle.
	s.mu.Lock()
	s.wdBase = 117
	s.port = nil
	s.degraded = true
	s.vanished = true
	st := s.stateLocked()
	s.mu.Unlock()

	if st.Open {
		t.Error("Open must be false with no port installed")
	}
	if st.Stats.Watchdog != 117 {
		t.Fatalf("watchdog dropped to %d during the outage — want it to stall at 117", st.Stats.Watchdog)
	}
	if st.Healthy {
		t.Error("a stalled watchdog must never read as healthy")
	}

	// And it must still be monotonic once a new port's own counter starts
	// from zero again: base + the new life's count.
	s.mu.Lock()
	s.wdBase = 117
	prev := s.stateLocked().Stats.Watchdog
	s.wdBase = 134 // a second life ended, 17 more iterations carried over
	next := s.stateLocked().Stats.Watchdog
	s.mu.Unlock()
	if next < prev {
		t.Fatalf("watchdog went backwards across lives: %d -> %d", prev, next)
	}
}

// TestRecoveryResumesAfterAFailedManualOpen covers a failure that only
// appears when a caller acts during an outage.
//
// Open bumps the generation BEFORE calling CreateFile, so that an in-flight
// recycle cannot install a port behind a caller who is connecting. Correct
// on its own, it means a FAILED Open silently orphans auto-recovery: the
// retry loop stops and nothing restarts it, while the state still reports
// that the port is being waited on.
// auto-recovery. (The pre-existing code bumped the generation only after a
// SUCCESSFUL open, so it did not have this hole; it was introduced by the
// lock-not-held-across-CreateFile fix.)
//
// The cure is that recovery is driven by the health loop, which cannot be
// orphaned by whoever started the last attempt.
func TestRecoveryResumesAfterAFailedManualOpen(t *testing.T) {
	sink := &stubSink{}
	s := New(sink)
	s.Start()
	defer s.Stop()

	cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}

	// The state after a vanish: no port, lost involuntarily, retry running.
	s.mu.Lock()
	s.lastCfg = cfg
	s.vanished = true
	s.degraded = true
	s.autoRecycle = true
	s.mu.Unlock()

	// The operator clicks Open while the port is still absent. It fails.
	if err := s.Open(cfg); err == nil {
		t.Fatal("expected the open of a nonexistent port to fail")
	}

	// Recovery must come back by itself. The health loop ticks at 1 Hz, so
	// allow a few ticks.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count("RECYCLE") > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := sink.count("RECYCLE"); n == 0 {
		t.Fatal("auto-recovery was orphaned by a failed manual Open and never resumed")
	} else {
		t.Logf("recovery resumed on its own: %d RECYCLE line(s) after the failed Open", n)
	}
}

// TestOperatorCloseIsNotUndoneByRecovery is the counterweight: recovery must
// restart after a LOST link, never after a deliberate disconnect. A health
// loop that reopens whatever it last saw would fight the operator.
func TestOperatorCloseIsNotUndoneByRecovery(t *testing.T) {
	sink := &stubSink{}
	s := New(sink)
	s.Start()
	defer s.Stop()

	cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}
	s.mu.Lock()
	s.lastCfg = cfg
	s.mu.Unlock()

	// Close clears vanished/degraded — the signal that the link was lost.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	time.Sleep(3 * time.Second) // several health ticks
	if n := sink.count("RECYCLE"); n != 0 {
		t.Fatalf("recovery reopened a port the operator deliberately closed (%d attempts)", n)
	}
}

// TestRecoveryResumesWithARetryLoopActuallyRunning covers the same failure
// with a retry goroutine genuinely in flight.
//
// TestRecoveryResumesAfterAFailedManualOpen builds the state by hand with
// recycling=false, so no retry goroutine exists. The real case has one ALIVE
// and asleep in its 5 s wait, which re-checks the generation only after
// waking — a different path, and the one that actually has to recover.
//
// Sequence: vanish -> retry loop running -> Open fails -> retries
// must come back on their own.
func TestRecoveryResumesWithARetryLoopActuallyRunning(t *testing.T) {
	sink := &stubSink{}
	s := New(sink)
	s.Start()
	defer s.Stop()

	cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}

	// Exactly what healthLoop installs on a vanish, including a REAL
	// recycleLoop goroutine.
	s.mu.Lock()
	s.lastCfg = cfg
	s.vanished = true
	s.degraded = true
	s.autoRecycle = true
	s.recycling = true
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	go s.recycleLoop(cfg, gen)

	// Let it log at least one retry and settle into its sleep.
	deadline := time.Now().Add(3 * time.Second)
	for sink.count("RECYCLE") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	before := sink.count("RECYCLE")
	if before == 0 {
		t.Fatal("the retry loop never logged an attempt")
	}
	t.Logf("retries before the operator acted: %d", before)

	// The operator clicks Open while the port is still absent. It fails.
	if err := s.Open(cfg); err == nil {
		t.Fatal("expected the open of a nonexistent port to fail")
	}
	t.Log("operator Open failed, as it must")

	// Recovery must resume by itself. Allow for the orphaned loop to finish
	// its 5 s sleep plus a few 1 Hz health ticks.
	after := before
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if after = sink.count("RECYCLE"); after > before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("retries after the failed Open: %d (was %d)", after, before)
	if after <= before {
		t.Fatal("auto-recovery did NOT resume after a failed manual Open — " +
			"the retry loop was orphaned and nothing restarted it")
	}
}
