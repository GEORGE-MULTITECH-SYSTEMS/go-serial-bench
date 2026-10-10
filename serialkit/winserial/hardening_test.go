//go:build windows

package winserial

// Hardening tests: the unplug-hang class. None of them requires a human to
// pull a cable — each isolates one property that the hang depended on.

import (
	"bytes"
	"testing"
	"time"
)

// TestStatsNeverBlocks: Stats must be pure memory.
//
// A status call that queries the device is how an application ends up
// waiting on the dead thing it is reporting on. Here a long write is left
// pending (peer closed, so it cannot complete quickly) and Stats is sampled
// underneath it: the write holds the write gate and a read-lock on the
// handle for seconds, and Stats must not care.
func TestStatsNeverBlocks(t *testing.T) {
	havePair(t)
	p, err := Open(Config{Name: "COM11", Baud: 9600, WriteTimeoutMs: 500}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// 8 KiB at 9600 baud is ~9.4 s of wire time: this write will be pending
	// for a long time with COM12 deliberately not opened.
	go p.Write(bytes.Repeat([]byte{0x55}, 8192))
	time.Sleep(400 * time.Millisecond) // let it go pending

	worst := time.Duration(0)
	for i := 0; i < 500; i++ {
		start := time.Now()
		_ = p.Stats()
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	t.Logf("worst Stats() latency with a write pending: %v", worst)
	if worst > 50*time.Millisecond {
		t.Fatalf("Stats() blocked for %v behind in-flight I/O — a status call must never wait on the device", worst)
	}
}

// TestClosePreemptsBlockedWrite: Close must be bounded even when a write is
// in flight, because the caller is usually the operator trying to escape a
// port that has just stopped responding.
//
// It also asserts the handle was NOT leaked: CancelIoEx is expected to bring
// the pending write back, so Close can prove the handle idle and close it
// properly. A leak here would mean the cancel path is not working.
func TestClosePreemptsBlockedWrite(t *testing.T) {
	havePair(t)
	leakedBefore := LeakedHandles()

	p, err := Open(Config{Name: "COM11", Baud: 9600, WriteTimeoutMs: 500}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	wrote := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = p.Write(bytes.Repeat([]byte{0x55}, 8192))
		wrote <- time.Since(start)
	}()
	time.Sleep(400 * time.Millisecond) // the write is now pending

	start := time.Now()
	cerr := p.Close()
	closeEl := time.Since(start)
	t.Logf("Close with a write in flight: %v (err=%v)", closeEl, cerr)

	// closeJoinMs + writeGateWaitMs + writeGateWaitMs = 6 s absolute worst
	// case; anything near that means a wait is not being cancelled.
	if closeEl > 6*time.Second {
		t.Fatalf("Close blocked %v — it must be bounded", closeEl)
	}

	select {
	case el := <-wrote:
		t.Logf("the pending write returned after %v", el)
		// Without CancelIoEx this would have run the full ~9.9 s deadline.
		if el > 8*time.Second {
			t.Errorf("pending write took %v — Close did not preempt it", el)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("the pending write never returned")
	}

	if got := LeakedHandles(); got != leakedBefore {
		t.Errorf("handle was leaked (%d -> %d): the cancel path should have proved it idle",
			leakedBefore, got)
	}
}

// TestCloseIsIdempotentAndBounded: Close is called from shutdown paths, from
// the operator, and from the auto-recycle. Calling it twice must be safe and
// must not block.
func TestCloseIsIdempotentAndBounded(t *testing.T) {
	havePair(t)
	p, err := Open(Config{Name: "COM11", Baud: 38400}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("two Closes took %v", el)
	}
	// Every operation on a closed port must refuse, not panic and not touch
	// the (now invalid) handle.
	if _, err := p.Write([]byte{1}); err == nil {
		t.Error("Write on a closed port must fail")
	}
	if err := p.Purge(); err == nil {
		t.Error("Purge on a closed port must fail")
	}
	if _, err := p.Modem(); err == nil {
		t.Error("Modem on a closed port must fail")
	}
	if _, err := p.CheckLine(); err == nil {
		t.Error("CheckLine on a closed port must fail")
	}
	if err := p.SetDTR(true); err == nil {
		t.Error("SetDTR on a closed port must fail")
	}
	if st := p.Stats(); st.Open {
		t.Error("Stats().Open must be false after Close")
	}
}

// TestPresentIsAuthoritative: presence is the only non-inferential answer to
// "has the adapter been unplugged", and the detector depends on it being
// exact for both a port that exists and one that cannot.
func TestPresentIsAuthoritative(t *testing.T) {
	havePair(t)
	if ok, err := Present("COM11"); err != nil || !ok {
		t.Fatalf("Present(COM11) = %v, %v — want true", ok, err)
	}
	if ok, err := Present("com11"); err != nil || !ok {
		t.Fatalf("Present must not be case-sensitive: %v, %v", ok, err)
	}
	if ok, err := Present("COM250"); err != nil || ok {
		t.Fatalf("Present(COM250) = %v, %v — want false", ok, err)
	}
}

// TestCheckLineReportsCleanLine: on a working pair ClearCommError must
// succeed and report no line errors. The point is that the counters exist
// and are wired at all — without this call, a line corrupting every byte is
// indistinguishable from a quiet one.
func TestCheckLineReportsCleanLine(t *testing.T) {
	havePair(t)
	a, err := Open(Config{Name: "COM11", Baud: 38400}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(Config{Name: "COM12", Baud: 38400}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := a.CheckLine(); err != nil {
		t.Fatalf("CheckLine on a healthy port: %v", err)
	}
	st := a.Stats()
	if st.Line.Any() {
		t.Errorf("line errors on a matched-baud virtual pair: %+v", st.Line)
	}
	if st.Line.Probes != 0 {
		t.Errorf("ClearCommError itself failed %d time(s)", st.Line.Probes)
	}
	t.Logf("line counters on a clean link: %+v", st.Line)
}

// TestProbeFailuresDoNotInflateTheStreak is the reason ProbeErrors exists.
//
// The streak detector fires at 10 consecutive DATA-PATH failures. A 1 Hz
// health probe that failed against a vanished adapter would, if it counted
// into the same streak, manufacture the very signature the detector is
// watching for — and "the data path is dead" could no longer be told apart
// from "my own polling is failing". Here the port is closed, so every probe
// fails, and the data-path counters must stay untouched.
func TestProbeFailuresDoNotInflateTheStreak(t *testing.T) {
	havePair(t)
	p, err := Open(Config{Name: "COM11", Baud: 38400}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	before := p.Stats()
	for i := 0; i < 20; i++ {
		_, _ = p.Modem()
		_, _ = p.CheckLine()
	}
	after := p.Stats()
	if after.Errors != before.Errors {
		t.Errorf("probe failures leaked into Errors: %d -> %d", before.Errors, after.Errors)
	}
	if after.ErrorStreak != before.ErrorStreak {
		t.Errorf("probe failures leaked into ErrorStreak: %d -> %d", before.ErrorStreak, after.ErrorStreak)
	}
	t.Logf("after 40 failed probes on a closed port: Errors=%d ErrorStreak=%d ProbeErrors=%d",
		after.Errors, after.ErrorStreak, after.ProbeErrors)
}

// TestWatchdogAdvancesMonotonically: the watchdog is the one field a
// supervisor compares across samples, so it must never go backwards. A
// reset reads as "the program restarted", which is a louder and wronger
// claim than whatever actually happened.
func TestWatchdogAdvancesMonotonically(t *testing.T) {
	havePair(t)
	p, err := Open(Config{Name: "COM11", Baud: 38400}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	last := uint64(0)
	for i := 0; i < 20; i++ {
		wd := p.Stats().Watchdog
		if wd < last {
			t.Fatalf("watchdog went backwards: %d -> %d", last, wd)
		}
		last = wd
		time.Sleep(25 * time.Millisecond)
	}
	if last == 0 {
		t.Fatal("watchdog never advanced on an idle line — a frozen counter cannot be told from a wedge")
	}
	t.Logf("watchdog reached %d over 500 ms idle", last)
}
