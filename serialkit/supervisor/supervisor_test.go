//go:build windows

package supervisor

import (
	"strings"
	"sync"
	"testing"
	"time"

	"serialkit/winserial"
)

// stubSink records OnLog calls so the test can inspect what the supervisor
// said, without needing a UI.
type stubSink struct {
	mu   sync.Mutex
	logs []string
}

func (s *stubSink) OnLog(kind, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, kind+": "+text)
}
func (s *stubSink) OnRx(b []byte)        {}
func (s *stubSink) OnTx(b []byte, n int) {}
func (s *stubSink) OnState(st State)     {}
func (s *stubSink) OnHealth(st State)    {}

func (s *stubSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.logs {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// TestCloseDuringRecycleStopsRetrying is the repro for the documented gap:
// "An explicit Close() during an in-flight auto-recycle does not silence
// the retry timer". cfg.Name points at a COM port that cannot exist, so
// every winserial.Open attempt fails deterministically (error 2) — no
// hardware or com0com pair required to observe whether the retry loop
// keeps firing after the operator has explicitly closed.
//
// It counts any "RECYCLE" line rather than "RECYCLE FAILED" specifically:
// recycleLoop now checks Present() before calling winserial.Open, so an
// absent port logs "still absent — waiting for it to reappear" instead of a
// failed open. Spinning CreateFile at a port that is not on the machine was
// pointless work; the property under test is that the retry STOPS after an
// operator Close, not which sentence the retry prints.
func TestCloseDuringRecycleStopsRetrying(t *testing.T) {
	sink := &stubSink{}
	s := New(sink)
	defer close(s.stop)

	cfg := winserial.Config{Name: "COM250", Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1"}

	// Put the supervisor into the same state healthLoop would when it
	// decides to recycle: no port, degraded, recycling in flight.
	s.mu.Lock()
	s.port = nil
	s.lastCfg = cfg
	s.degraded = true
	s.autoRecycle = true
	s.recycling = true
	s.gen++
	gen := s.gen
	s.mu.Unlock()

	go s.recycleLoop(cfg, gen)

	// Let the first (immediate) attempt land.
	deadline := time.Now().Add(2 * time.Second)
	for sink.count("RECYCLE") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sink.count("RECYCLE") == 0 {
		t.Fatal("expected at least one failed recycle attempt before Close")
	}

	before := sink.count("RECYCLE")

	// Operator disconnects while the retry timer is in flight.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s.mu.Lock()
	recyclingAfterClose := s.recycling
	s.mu.Unlock()
	if !recyclingAfterClose {
		t.Log("recycling flag cleared immediately by Close (unexpected but not the bug under test)")
	}

	// Wait past the next 5 s retry tick.
	time.Sleep(5500 * time.Millisecond)
	after := sink.count("RECYCLE")

	if after > before {
		t.Fatalf("BUG: recycleLoop kept retrying after operator Close() — "+
			"%d attempts before Close, %d after; the retry timer was not silenced", before, after)
	}
}
