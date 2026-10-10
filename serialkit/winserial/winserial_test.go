//go:build windows

package winserial

// Live com0com test — COM11 <-> COM12 must exist and be free.
// Run:  go test ./winserial -run TestCom0com -v
// Skipped automatically if the pair is not present.

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func havePair(t *testing.T) {
	ports, err := ListPorts()
	if err != nil {
		t.Fatalf("ListPorts: %v", err)
	}
	has := map[string]bool{}
	for _, p := range ports {
		has[p] = true
	}
	if !has["COM11"] || !has["COM12"] {
		t.Skip("com0com COM11/COM12 not present")
	}
}

func TestCom0comLoop(t *testing.T) {
	havePair(t)
	var gotA, gotB syncBuf
	var errsA, errsB atomic.Int32
	cfg := func(n string) Config {
		return Config{Name: n, Baud: 38400, DataBits: 8, Parity: "none", StopBits: "1", ReadGapMs: 20}
	}
	a, err := Open(cfg("COM11"), func(b []byte) { gotA.Write(b) }, func(error) { errsA.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(cfg("COM12"), func(b []byte) { gotB.Write(b) }, func(error) { errsB.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// settle: com0com delivers bytes a previous holder left in flight toward
	// us (PurgeComm clears OUR driver queue, not the pair's buffer). Drain
	// and discard before measuring — same rule as any real bench.
	time.Sleep(150 * time.Millisecond)
	if gotA.Len()+gotB.Len() > 0 {
		t.Logf("discarded %d stale byte(s) left in the pair by a previous holder", gotA.Len()+gotB.Len())
	}
	gotA.Reset()
	gotB.Reset()

	// exclusivity: a second open of COM11 must fail with the error-5 message
	if _, err := Open(cfg("COM11"), nil, nil); err == nil {
		t.Fatal("second open of COM11 succeeded — exclusivity broken")
	} else {
		t.Logf("second open correctly refused: %v", err)
	}

	// watchdog must advance on an idle line
	w0 := a.Stats().Watchdog
	time.Sleep(300 * time.Millisecond)
	if w1 := a.Stats().Watchdog; w1 == w0 {
		t.Fatalf("watchdog frozen on idle line: %d", w0)
	} else {
		t.Logf("idle watchdog %d -> %d in 300 ms", w0, w1)
	}

	// A -> B, B -> A, 200 frames each way, mixed sizes
	var wantA, wantB bytes.Buffer
	for i := 0; i < 200; i++ {
		fa := []byte{0x01, 0x03, byte(i), 0x00, 0x00, 0x02, 0xC4, byte(0x0B + i)}
		fb := bytes.Repeat([]byte{byte(0xA0 + i%16)}, 1+i%37)
		if _, err := a.Write(fa); err != nil {
			t.Fatalf("A write %d: %v", i, err)
		}
		if _, err := b.Write(fb); err != nil {
			t.Fatalf("B write %d: %v", i, err)
		}
		wantB.Write(fa)
		wantA.Write(fb)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gotB.Len() >= wantB.Len() && gotA.Len() >= wantA.Len() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Equal(gotB.Bytes(), wantB.Bytes()) {
		t.Fatalf("A->B mismatch: got %d want %d bytes", gotB.Len(), wantB.Len())
	}
	if !bytes.Equal(gotA.Bytes(), wantA.Bytes()) {
		t.Fatalf("B->A mismatch: got %d want %d bytes", gotA.Len(), wantA.Len())
	}
	sa, sb := a.Stats(), b.Stats()
	t.Logf("A: tx=%d rx=%d reads=%d writes=%d errs=%d wd=%d", sa.TxBytes, sa.RxBytes, sa.Reads, sa.Writes, sa.Errors, sa.Watchdog)
	t.Logf("B: tx=%d rx=%d reads=%d writes=%d errs=%d wd=%d", sb.TxBytes, sb.RxBytes, sb.Reads, sb.Writes, sb.Errors, sb.Watchdog)
	if sa.Errors != 0 || sb.Errors != 0 || errsA.Load() != 0 || errsB.Load() != 0 {
		t.Fatalf("unexpected errors A=%d B=%d", sa.Errors, sb.Errors)
	}
}

// The com0com WriteFile-forever wedge: with the peer end CLOSED, writes
// must fail by timeout, never block, and the watchdog must keep ticking.
func TestWriteTimeoutPeerClosed(t *testing.T) {
	havePair(t)
	a, err := Open(Config{Name: "COM11", Baud: 38400, WriteTimeoutMs: 300}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// COM12 deliberately NOT opened.
	big := bytes.Repeat([]byte{0x55}, 8192)
	// expected bound: 300 ms constant + wire time of 8192 bytes @38400
	// (8192*11/38400 = 2.35 s) + a little slack
	bound := 300*time.Millisecond + time.Duration(a.wireTimeMs(len(big)))*time.Millisecond + 500*time.Millisecond
	start := time.Now()
	n, werr := a.Write(big)
	el := time.Since(start)
	t.Logf("write with peer closed: n=%d err=%v elapsed=%v (bound %v)", n, werr, el, bound)
	if el > bound {
		t.Fatalf("write blocked %v > %v — the wedge is back", el, bound)
	}
	// Loop a few more; must stay bounded, and watchdog must keep advancing
	w0 := a.Stats().Watchdog
	for i := 0; i < 3; i++ {
		_, _ = a.Write(big)
	}
	if w1 := a.Stats().Watchdog; w1 == w0 {
		t.Fatalf("watchdog frozen during blocked writes")
	}
	t.Logf("stats after: %+v", a.Stats())
	if werr == nil {
		t.Log("NOTE: com0com accepted the bytes into its buffer without a peer (buffer not yet full) — bounded either way")
	}
	if errors.Is(werr, ErrClosed) {
		t.Fatal("unexpected ErrClosed")
	}
}

func TestListPorts(t *testing.T) {
	p, err := ListPorts()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ports: %v", p)
}

// syncBuf is a bytes.Buffer with a lock. The onData callback runs on the
// port's reader goroutine while the test body polls Len() and compares
// Bytes(), so a plain bytes.Buffer here is a data race — one the suite
// never saw because it had only ever been run without -race. It reported
// octet-exact results the whole time, which is exactly why this matters:
// the comparison was correct and the measurement of it was not.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// Bytes returns a COPY: the caller compares it while the reader goroutine
// may still be appending, and bytes.Buffer.Bytes() aliases live storage.
func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}
