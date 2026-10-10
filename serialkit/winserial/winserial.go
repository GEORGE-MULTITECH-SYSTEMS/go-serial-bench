//go:build windows

// Package winserial is an instance-based Windows COM-port layer built
// directly on golang.org/x/sys/windows. No cgo, no third-party serial
// library, no spawned helper process.
//
// What it provides:
//
//   - exclusive open (dwShareMode 0) with the \\.\ prefix, so COM10+ opens
//   - every DCB and COMMTIMEOUTS field stated explicitly at open
//   - a bounded write deadline on every write
//   - full duplex over FILE_FLAG_OVERLAPPED: a reader goroutine with one
//     pending ReadFile, and overlapped writes
//   - line-error counters from ClearCommError
//   - a monotonic watchdog counter, so a wedged reader is distinguishable
//     from an idle line
//   - a consecutive-failure streak, so a caller can detect a dead port
//
// # Surviving surprise removal
//
// Unplugging a USB adapter is the fault the obvious implementation handles
// worst, and four rules here exist for it alone.
//
//  1. NO UNBOUNDED WAIT. Every wait on an overlapped completion has a
//     deadline. COMMTIMEOUTS are enforced by the driver; once the driver's
//     device is gone, nothing enforces them, and a GetOverlappedResult with
//     bWait=true can wait forever. The timeouts are the normal mechanism;
//     the deadline is the backstop.
//
//  2. THE HANDLE IS GUARDED. hMu is read-held by any goroutine inside a
//     syscall on the handle, and write-held by Close before CloseHandle.
//     Rule 1 is what makes this affordable: no reader holds it longer than
//     one bounded wait.
//
//  3. A HANDLE THAT CANNOT BE PROVED IDLE IS LEAKED, NOT CLOSED. Windows
//     reuses handle values. Closing a handle while a goroutine is still
//     blocked in a syscall on it, or while the kernel still owns a pending
//     request pointing into our buffer, allows the next CreateFile to be
//     handed the same value — after which a stale reader consumes the new
//     port's bytes and charges them to the dead one. A leaked handle is a
//     number in a report; a recycled handle is silent cross-talk. See
//     leakHandle and LeakedHandles.
//
//  4. THE KERNEL'S MEMORY IS PINNED. While a request is pending the kernel
//     holds raw pointers to the OVERLAPPED and to the read buffer. Goroutine
//     stacks move when they grow, so neither may be stack-resident; both are
//     heap-allocated once per Port and never reallocated.
//
// # Two Windows defaults worth knowing
//
// A newly opened port does not have defaults. It inherits whatever the last
// program left in the DCB, and COMMTIMEOUTS inherit across processes — a
// port left with infinite timeouts by another program will hang the first
// read of the next one. Every field is therefore stated at open.
//
// Bytes written while the far end is closed are held on the writer's side
// and delivered once a reader opens. PurgeComm at open is therefore not a
// clean slate: stale bytes can still arrive after it.
package winserial

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Config is everything a Hercules-style UI exposes for a port.
type Config struct {
	Name      string // "COM13"
	Baud      uint32 // 9600, 38400, 115200 ...
	DataBits  byte   // 5..8
	Parity    string // "none" | "odd" | "even" | "mark" | "space"
	StopBits  string // "1" | "1.5" | "2"
	Handshake string // "off" | "rts/cts" | "xon/xoff" | "dtr/dsr"
	DTR       bool   // initial DTR (default false; see applyDCB)
	RTS       bool   // initial RTS (default false; see applyDCB)
	// ReadGapMs is the inter-byte silence that ends one Read (COMMTIMEOUTS
	// ReadIntervalTimeout). 20 ms is a sane default. Note that a
	// default FTDI VCP batches at ~16 ms regardless, so this is a
	// "generous" boundary, not real t3.5.
	ReadGapMs uint32
	// WriteTimeoutMs bounds every WriteFile. 0 is NOT allowed (that is the
	// wedge); the constructor forces 500 ms if unset.
	WriteTimeoutMs uint32
}

// Timing constants. These are backstops for a driver that has stopped
// honouring COMMTIMEOUTS because its device is gone (rule 1 above); in
// normal operation none of them is ever reached.
const (
	// readTotalMs is the COMMTIMEOUTS read constant: how long the driver
	// waits with nothing arriving before completing the read empty. It sets
	// the watchdog tick rate on an idle line.
	readTotalMs = 50
	// readWaitMs bounds our own wait for that completion. Generously above
	// readTotalMs: exceeding it does not mean "slow", it means the driver is
	// not completing our I/O at all.
	readWaitMs = 500
	// cancelWaitMs bounds the wait for a CANCELLED request to come back.
	// CancelIoEx is asynchronous: it asks, it does not guarantee.
	cancelWaitMs = 1000
	// closeJoinMs bounds Close's wait for the reader goroutine before it
	// gives up and leaks the handle instead (rule 3).
	closeJoinMs = 2000
	// writeGateWaitMs bounds Close's wait for an in-flight Write to finish.
	// Close has already issued CancelIoEx, so a pending write should abort
	// almost at once.
	writeGateWaitMs = 2000
	// writeDeadlineCapMs caps the computed per-write deadline. The deadline
	// is constant + wire time, and wire time is unbounded in the payload:
	// 8 KiB at 9600 baud is legitimately ~9.4 s. Correct, and far too long
	// to let a UI thread sit inside. Callers that must stay responsive use
	// the supervisor's async writer; this cap is the hard ceiling.
	writeDeadlineCapMs = 10000
)

// ModemStatus mirrors GetCommModemStatus.
type ModemStatus struct {
	CTS, DSR, RI, DCD bool
}

// LineErrors are the counters only ClearCommError can report. Without it a
// corrupt line is indistinguishable from a quiet one: framing and parity
// damage silently becomes "bytes received", and an overrun becomes "bytes
// that never arrived" with no error recorded anywhere.
type LineErrors struct {
	Frame   uint64 // CE_FRAME   — bad stop bit: wrong baud, or noise
	Parity  uint64 // CE_RXPARITY
	Overrun uint64 // CE_OVERRUN — a character lost in the UART itself
	RxOver  uint64 // CE_RXOVER  — the driver's input queue overflowed
	Break   uint64 // CE_BREAK
	Probes  uint64 // ClearCommError calls that themselves failed
}

// Any reports whether any real line error has ever been seen.
func (l LineErrors) Any() bool {
	return l.Frame|l.Parity|l.Overrun|l.RxOver|l.Break != 0
}

// Stats is a snapshot for the health/AND-gate evaluator.
//
// There is deliberately no Healthy field. Grading belongs to the supervisor,
// the only party that can see both "is it responding" and "is it doing its
// job"; a worker that grades itself is trusting the thing that may be
// broken to report on itself.
type Stats struct {
	Open        bool
	Watchdog    uint64 // increments once per reader-loop iteration
	RxBytes     uint64
	TxBytes     uint64
	Reads       uint64
	Writes      uint64
	Errors      uint64 // data-path read/write syscall failures
	ErrorStreak uint32 // consecutive data-path failures, no success between
	LastError   string
	LastErrorAt time.Time
	Line        LineErrors
	// ProbeErrors counts failures of out-of-band queries (modem status,
	// ClearCommError, purge). Kept OUT of Errors and ErrorStreak on purpose:
	// a 1 Hz health probe failing against a vanished adapter would otherwise
	// manufacture the very error streak the streak detector is watching for,
	// and the supervisor could no longer tell a dead data path from its own
	// polling.
	ProbeErrors uint64
	// Wedged means a request could not be cancelled and the handle has been
	// leaked rather than closed (rule 3). The port is finished.
	Wedged bool
	// TimedOutWaits counts completions that missed readWaitMs — the direct
	// signature of a driver that has stopped honouring its own timeouts.
	TimedOutWaits uint64
}

// Port is one open COM port. All state lives here — never file-scope
// (two ports in one process must work).
type Port struct {
	cfg Config

	// h is guarded by hMu: read-held while any goroutine is inside a syscall
	// on it, write-held by Close before CloseHandle (rule 2).
	hMu sync.RWMutex
	h   windows.Handle

	closed atomic.Bool
	wedged atomic.Bool

	// Kernel-visible memory, heap-allocated once and never reallocated,
	// because the kernel holds raw pointers into it while a request is
	// pending and goroutine stacks move (rule 4).
	rov  *windows.Overlapped
	wov  *windows.Overlapped
	rbuf []byte

	wd            atomic.Uint64
	rxBytes       atomic.Uint64
	txBytes       atomic.Uint64
	reads         atomic.Uint64
	writes        atomic.Uint64
	errs          atomic.Uint64
	errStreak     atomic.Uint32
	probeErrs     atomic.Uint64
	timedOutWaits atomic.Uint64

	ceFrame   atomic.Uint64
	ceParity  atomic.Uint64
	ceOverrun atomic.Uint64
	ceRxOver  atomic.Uint64
	ceBreak   atomic.Uint64
	ceProbes  atomic.Uint64

	lastErrMu sync.Mutex
	lastErr   string
	lastErrAt time.Time

	// writeGate is a one-slot semaphore rather than a Mutex so Close can
	// wait for an in-flight write WITH A DEADLINE. sync.Mutex cannot be
	// waited on with a timeout, and an unbounded wait here would reintroduce
	// exactly the hang this file exists to prevent.
	writeGate chan struct{}

	readEvt    windows.Handle
	writeEvt   windows.Handle
	stopReader chan struct{}
	readerDone chan struct{}
	onData     func([]byte)
	onErr      func(error)
}

// GetCommModemStatus bits (winbase.h) — not exported by x/sys/windows.
const (
	msCTSOn  = 0x0010
	msDSROn  = 0x0020
	msRingOn = 0x0040
	msRLSDOn = 0x0080
)

// ClearCommError error bits (winbase.h) — not exported by x/sys/windows.
const (
	ceRxOverBit   = 0x0001
	ceOverrunBit  = 0x0002
	ceRxParityBit = 0x0004
	ceFrameBit    = 0x0008
	ceBreakBit    = 0x0010
)

var (
	ErrClosed = errors.New("port is closed")
	// ErrWedged means a request could not be cancelled, so the handle was
	// leaked instead of closed (rule 3). Nothing about this port will work
	// again; open a fresh one.
	ErrWedged = errors.New("port wedged: pending I/O could not be cancelled, handle leaked deliberately")
)

// leakedPs keeps wedged Ports reachable forever. This is not a mistake and
// not a cache: while an uncancellable request is pending, the kernel still
// owns pointers to that Port's rov and rbuf, so the Port must never be
// collected and the handle must never be closed. Growth here is the honest
// cost of a driver that would not cancel, and it is reported by
// LeakedHandles.
var (
	leakedMu sync.Mutex
	leakedPs []*Port
)

func leakHandle(p *Port) {
	if p.wedged.Swap(true) {
		return // already leaked; count it once
	}
	leakedMu.Lock()
	leakedPs = append(leakedPs, p)
	n := len(leakedPs)
	leakedMu.Unlock()
	if p.onErr != nil {
		p.onErr(fmt.Errorf("%s WEDGED: pending I/O would not cancel; handle LEAKED, not closed "+
			"(closing it risks the next CreateFile being handed the same value) — %d leaked so far",
			p.cfg.Name, n))
	}
}

// LeakedHandles reports how many handles have been deliberately leaked by
// rule 3 for the lifetime of the process. A supervisor should surface it: a
// number that climbs is a driver that will not cancel I/O, which is a fact
// about the machine, not about this code.
func LeakedHandles() int {
	leakedMu.Lock()
	defer leakedMu.Unlock()
	return len(leakedPs)
}

// ListPorts enumerates HKLM\HARDWARE\DEVICEMAP\SERIALCOMM — the same
// registry path Windows serial tools conventionally read. It is
// adapter-agnostic: every driver stack registers here.
func ListPorts() ([]string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return []string{}, nil // no serial devices at all
		}
		return nil, err
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err == nil && v != "" {
			out = append(out, v)
		}
	}
	sortCom(out)
	return out, nil
}

// Present reports whether name is registered RIGHT NOW.
//
// This is the only authoritative, non-inferential answer to "has the adapter
// been unplugged". Everything else is inference from failures, and the
// failures are driver-specific: two real CP210x pulls produced
// 995 -> 5 -> 2 within ~31 ms, while an earlier recorded pull on the same
// bench produced a stream of 22 and neither live pull reproduced it. Hence
// the standing rule that a dead port is classified by BEHAVIOUR (a failure
// streak) and never by matching an error code. Presence is better than
// both: it is a fact rather than a classification.
//
// A false return is conclusive. A true return is not a guarantee the device
// answers — the registry entry can outlive the hardware briefly — so it is
// used to confirm a vanish, never to overrule a working data path.
func Present(name string) (bool, error) {
	ports, err := ListPorts()
	if err != nil {
		return false, err
	}
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, p := range ports {
		if strings.EqualFold(p, name) {
			return true, nil
		}
	}
	return false, nil
}

// sortCom orders COM1, COM2, ... COM10 numerically, not lexically.
func sortCom(s []string) {
	num := func(x string) int {
		n := 0
		for _, c := range strings.TrimPrefix(strings.ToUpper(x), "COM") {
			if c < '0' || c > '9' {
				return 1 << 30
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && num(s[j]) < num(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Open opens and configures the port. onData is called from the reader
// goroutine with each chunk (a copy — safe to keep). onErr is called on
// every syscall-level failure; the caller decides what a streak means.
//
// Open performs blocking syscalls — CreateFile on a re-enumerating USB
// adapter is not quick. Callers must NOT hold a lock across this call that a
// UI thread also needs.
func Open(cfg Config, onData func([]byte), onErr func(error)) (*Port, error) {
	if cfg.Name == "" {
		return nil, errors.New("empty port name")
	}
	if len(cfg.Name) >= 32 {
		return nil, errors.New("port name too long")
	}
	if cfg.ReadGapMs == 0 {
		cfg.ReadGapMs = 20
	}
	if cfg.WriteTimeoutMs == 0 {
		cfg.WriteTimeoutMs = 500 // never zero: that is the wedge
	}
	if cfg.DataBits == 0 {
		cfg.DataBits = 8
	}

	path, err := windows.UTF16PtrFromString(`\\.\` + cfg.Name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // exclusive — mandatory for a serial port
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, classifyOpenErr(cfg.Name, err)
	}

	p := &Port{
		h: h, cfg: cfg, onData: onData, onErr: onErr,
		rov:       new(windows.Overlapped), // heap-pinned (rule 4)
		wov:       new(windows.Overlapped),
		rbuf:      make([]byte, 4096),
		writeGate: make(chan struct{}, 1),
	}
	if err := p.applyDCB(); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetCommState: %w", err)
	}
	if err := p.applyTimeouts(); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetCommTimeouts: %w", err)
	}
	// generous driver queues; bursts must not be dropped by the driver
	_ = windows.SetupComm(h, 1<<16, 1<<16)
	// Whatever the previous holder left in the driver buffers is not ours.
	// Note this is NOT a clean slate: bytes written while our end was closed
	// are held on the WRITER's side and delivered after we open, arriving
	// after this purge. Draining one end of a pair proves nothing.
	_ = windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR|windows.PURGE_RXABORT|windows.PURGE_TXABORT)

	p.readEvt, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	p.writeEvt, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(p.readEvt)
		windows.CloseHandle(h)
		return nil, err
	}
	p.stopReader = make(chan struct{})
	p.readerDone = make(chan struct{})
	go p.readerLoop()
	return p, nil
}

func classifyOpenErr(name string, err error) error {
	var e windows.Errno
	if errors.As(err, &e) {
		switch e {
		case windows.ERROR_ACCESS_DENIED: // 5
			return fmt.Errorf("%s is held by another process (error 5) — do not retry-spin", name)
		case windows.ERROR_FILE_NOT_FOUND: // 2
			return fmt.Errorf("%s does not exist (error 2) — configuration, not transient", name)
		}
	}
	return fmt.Errorf("open %s: %w", name, err)
}

func (p *Port) applyDCB() error {
	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb))
	if err := windows.GetCommState(p.h, &dcb); err != nil {
		return err
	}
	dcb.BaudRate = p.cfg.Baud
	dcb.ByteSize = p.cfg.DataBits

	switch strings.ToLower(p.cfg.Parity) {
	case "odd":
		dcb.Parity = windows.ODDPARITY
	case "even":
		dcb.Parity = windows.EVENPARITY
	case "mark":
		dcb.Parity = windows.MARKPARITY
	case "space":
		dcb.Parity = windows.SPACEPARITY
	default:
		dcb.Parity = windows.NOPARITY
	}
	switch p.cfg.StopBits {
	case "1.5":
		dcb.StopBits = windows.ONE5STOPBITS
	case "2":
		dcb.StopBits = windows.TWOSTOPBITS
	default:
		dcb.StopBits = windows.ONESTOPBIT
	}

	// Flags bitfield (winbase.h DCB): bit0 fBinary, bit1 fParity,
	// bit2 fOutxCtsFlow, bit3 fOutxDsrFlow, bits4-5 fDtrControl,
	// bit6 fDsrSensitivity, bit7 fTXContinueOnXoff, bit8 fOutX,
	// bit9 fInX, bit10 fErrorChar, bit11 fNull, bits12-13 fRtsControl,
	// bit14 fAbortOnError.
	//
	// Rebuilt from scratch, never inherited: a newly opened port does NOT
	// have defaults. It carries whatever the last program to touch it left
	// in the DCB, so any field not stated here makes this port's behaviour
	// depend on what ran before us.
	var f uint32 = 1 // fBinary — always
	if dcb.Parity != windows.NOPARITY {
		f |= 1 << 1 // fParity: enable parity checking
	}
	// Explicit line policy, DISABLED unless asked for. On many
	// USB-serial dev boards DTR and RTS are wired to reset and boot-select.
	if p.cfg.DTR {
		f |= windows.DTR_CONTROL_ENABLE
	}
	if p.cfg.RTS {
		f |= windows.RTS_CONTROL_ENABLE
	}
	switch strings.ToLower(p.cfg.Handshake) {
	case "rts/cts":
		f |= 1 << 2 // fOutxCtsFlow
		f = (f &^ 0x3000) | windows.RTS_CONTROL_HANDSHAKE
	case "dtr/dsr":
		f |= 1 << 3 // fOutxDsrFlow
		f = (f &^ 0x30) | windows.DTR_CONTROL_HANDSHAKE
	case "xon/xoff":
		f |= (1 << 8) | (1 << 9) // fOutX | fInX
		dcb.XonChar = 0x11
		dcb.XoffChar = 0x13
		dcb.XonLim = 2048
		dcb.XoffLim = 512
	}
	dcb.Flags = f
	return windows.SetCommState(p.h, &dcb)
}

func (p *Port) applyTimeouts() error {
	return p.setTimeouts(p.cfg.WriteTimeoutMs)
}

// setTimeouts installs the read policy plus a write deadline. The write
// deadline is set PER WRITE (see Write) as constant + wire time, because
// WriteTotalTimeoutMultiplier is "ms per byte" — far too coarse (1 ms/byte
// = 8 s for 8 KiB at any baud). Multiplier stays 0.
//
// Timeouts must be stated at open, before any I/O, and they are the worse
// half of the inheritance problem: they leak across PROCESSES. A port left
// with infinite timeouts by a previous program will hang the first read of
// the next one.
func (p *Port) setTimeouts(writeMs uint32) error {
	ct := windows.CommTimeouts{
		// A pending read returns after ReadGapMs of silence following data,
		// or after ReadTotalTimeoutConstant if nothing arrives (a short
		// constant, so the reader loop notices Close promptly and the
		// watchdog keeps advancing on an idle line).
		ReadIntervalTimeout:         p.cfg.ReadGapMs,
		ReadTotalTimeoutMultiplier:  0,
		ReadTotalTimeoutConstant:    readTotalMs,
		WriteTotalTimeoutMultiplier: 0,
		WriteTotalTimeoutConstant:   writeMs,
	}
	return windows.SetCommTimeouts(p.h, &ct)
}

// wireTimeMs is how long n bytes occupy the line. 11 bits per
// character is the conservative (parity or 2-stop) figure.
func (p *Port) wireTimeMs(n int) uint32 {
	if p.cfg.Baud == 0 {
		return 0
	}
	return uint32((uint64(n)*11*1000)/uint64(p.cfg.Baud)) + 1
}

// awaitOverlapped waits for a pending request to complete, with a deadline,
// and returns the bytes transferred.
//
// This is rule 1 in one function. A pending overlapped request on a serial
// handle is normally completed by the driver's own COMMTIMEOUTS; waiting for
// it without a deadline is therefore "correct" right up until the device is
// unplugged, at which point there is no driver state machine left to enforce
// the timeout and the wait becomes permanent. That is the unplug hang.
//
// On deadline: ask for cancellation and wait again, briefly. If the request
// still does not come back, the kernel still owns our buffer and the handle
// is unsafe to close — report ErrWedged and let the caller leak it (rule 3).
func (p *Port) awaitOverlapped(ov *windows.Overlapped, evt windows.Handle, waitMs uint32) (uint32, error) {
	var n uint32
	ev, err := windows.WaitForSingleObject(evt, waitMs)
	if err != nil {
		return 0, fmt.Errorf("WaitForSingleObject: %w", err)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		p.timedOutWaits.Add(1)
		// The driver did not honour its own timeout. Nothing about the
		// device can be assumed from here on.
		_ = windows.CancelIoEx(p.h, ov)
		ev2, err2 := windows.WaitForSingleObject(evt, cancelWaitMs)
		if err2 != nil || ev2 == uint32(windows.WAIT_TIMEOUT) {
			return 0, ErrWedged
		}
		// Completed as cancelled; fall through and collect the result.
	}
	// bWait=false: the event is already signalled, so this only collects the
	// result and can never block.
	if err := windows.GetOverlappedResult(p.h, ov, &n, false); err != nil {
		return n, err
	}
	return n, nil
}

// readerLoop keeps one pending overlapped ReadFile per iteration.
// The timeout constant makes it wake at least every readTotalMs so the
// watchdog advances while idle — a counter that only moves when data arrives
// cannot be told from a wedged one on a quiet line, which is the whole point
// of having it.
func (p *Port) readerLoop() {
	defer close(p.readerDone)
	for {
		select {
		case <-p.stopReader:
			return
		default:
		}
		p.wd.Add(1)

		n, err := p.readOnce()
		switch {
		case errors.Is(err, ErrWedged):
			// The handle can no longer be closed safely and the kernel may
			// still write into rbuf. Leak the Port — which keeps rov and
			// rbuf reachable forever — and stop reading.
			leakHandle(p)
			return
		case errors.Is(err, ErrClosed):
			return
		case err != nil:
			p.fail(err)
			if p.closed.Load() {
				return
			}
			// Pace a failing handle: no hot spin, and no log firehose
			// either — the consumer of onErr is usually a UI.
			select {
			case <-p.stopReader:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if n > 0 {
			p.ok()
			p.reads.Add(1)
			p.rxBytes.Add(uint64(n))
			chunk := make([]byte, n)
			copy(chunk, p.rbuf[:n])
			if p.onData != nil {
				p.onData(chunk)
			}
		}
		// n == 0: silence. A third outcome, and NOT an error.
		// Collapsing silence into failure is correct for a master (both mean
		// "no reply") and catastrophic for a monitor, which would then
		// report a dead port on every quiet line.
	}
}

// readOnce issues one bounded overlapped read. The handle guard is taken and
// released per iteration, which is what lets Close ever acquire it.
func (p *Port) readOnce() (uint32, error) {
	p.hMu.RLock()
	defer p.hMu.RUnlock()
	if p.closed.Load() {
		return 0, ErrClosed
	}

	*p.rov = windows.Overlapped{HEvent: p.readEvt}
	_ = windows.ResetEvent(p.readEvt)
	var n uint32
	err := windows.ReadFile(p.h, p.rbuf, &n, p.rov)
	if err == nil {
		return n, nil // completed inline
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, fmt.Errorf("ReadFile: %w", err)
	}
	n, err = p.awaitOverlapped(p.rov, p.readEvt, readWaitMs)
	if err != nil {
		if errors.Is(err, ErrWedged) {
			return 0, err
		}
		if p.closed.Load() {
			return 0, ErrClosed
		}
		return 0, fmt.Errorf("ReadFile: %w", err)
	}
	return n, nil
}

// Write blocks until the bytes are handed to the driver or the write
// deadline expires. Returns nil only if every byte went out. Callers must
// NOT infer "on the wire" from success — success means the driver
// accepted them.
//
// The deadline is bounded but can legitimately reach writeDeadlineCapMs, so
// this must not be called from a thread that has to stay responsive. Use the
// supervisor's async writer for that.
func (p *Port) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, ErrClosed
	}
	if p.wedged.Load() {
		return 0, ErrWedged
	}
	// One writer at a time: the wire is the lock.
	select {
	case p.writeGate <- struct{}{}:
	case <-time.After(writeDeadlineCapMs * time.Millisecond):
		return 0, errors.New("write gate busy: another write is still in flight")
	}
	defer func() { <-p.writeGate }()

	p.hMu.RLock()
	defer p.hMu.RUnlock()
	if p.closed.Load() {
		return 0, ErrClosed
	}

	// deadline = user constant + the time these bytes legitimately need on
	// the wire at this baud; anything beyond that is a stalled driver.
	wire := p.wireTimeMs(len(b))
	deadline := p.cfg.WriteTimeoutMs + wire
	capped := ""
	if deadline > writeDeadlineCapMs {
		deadline = writeDeadlineCapMs
		capped = ", CAPPED"
	}
	if err := p.setTimeouts(deadline); err != nil {
		p.fail(fmt.Errorf("SetCommTimeouts: %w", err))
		return 0, err
	}

	*p.wov = windows.Overlapped{HEvent: p.writeEvt}
	_ = windows.ResetEvent(p.writeEvt)
	var n uint32
	err := windows.WriteFile(p.h, b, &n, p.wov)
	if err != nil {
		if !errors.Is(err, windows.ERROR_IO_PENDING) {
			p.fail(fmt.Errorf("WriteFile: %w", err))
			return 0, err
		}
		// Backstop beyond the driver's own deadline, for the same reason as
		// the read: COMMTIMEOUTS need a live device to be enforced by.
		n, err = p.awaitOverlapped(p.wov, p.writeEvt, deadline+cancelWaitMs)
		if err != nil {
			if errors.Is(err, ErrWedged) {
				leakHandle(p)
				return int(n), ErrWedged
			}
			p.fail(fmt.Errorf("WriteFile: %w", err))
			return int(n), err
		}
	}
	if int(n) != len(b) {
		e := fmt.Errorf("short write %d/%d (deadline %d ms = %d const + wire %d%s)",
			n, len(b), deadline, p.cfg.WriteTimeoutMs, wire, capped)
		p.fail(e)
		return int(n), e
	}
	p.ok()
	p.writes.Add(1)
	p.txBytes.Add(uint64(n))
	return int(n), nil
}

// guard runs a short out-of-band syscall under the handle guard. Failures
// here count as probes, not as data-path errors (see Stats.ProbeErrors).
func (p *Port) guard(what string, fn func(h windows.Handle) error) error {
	if p.closed.Load() {
		return ErrClosed
	}
	if p.wedged.Load() {
		return ErrWedged
	}
	p.hMu.RLock()
	defer p.hMu.RUnlock()
	if p.closed.Load() {
		return ErrClosed
	}
	if err := fn(p.h); err != nil {
		p.probe(fmt.Errorf("%s: %w", what, err))
		return err
	}
	return nil
}

// SetDTR and SetRTS drive the modem-control outputs live. Destructive on
// many dev boards, where these lines are wired to reset and boot-select.
func (p *Port) SetDTR(on bool) error {
	return p.guard("EscapeCommFunction(DTR)", func(h windows.Handle) error {
		if on {
			return windows.EscapeCommFunction(h, windows.SETDTR)
		}
		return windows.EscapeCommFunction(h, windows.CLRDTR)
	})
}

func (p *Port) SetRTS(on bool) error {
	return p.guard("EscapeCommFunction(RTS)", func(h windows.Handle) error {
		if on {
			return windows.EscapeCommFunction(h, windows.SETRTS)
		}
		return windows.EscapeCommFunction(h, windows.CLRRTS)
	})
}

// SendBreak holds TX low for d then releases it.
// The handle guard is dropped during the hold so a long break cannot delay
// Close, and the hold is abandoned if the port closes under it.
func (p *Port) SendBreak(d time.Duration) error {
	if err := p.guard("SETBREAK", func(h windows.Handle) error {
		return windows.EscapeCommFunction(h, windows.SETBREAK)
	}); err != nil {
		return err
	}
	select {
	case <-p.stopReader:
	case <-time.After(d):
	}
	return p.guard("CLRBREAK", func(h windows.Handle) error {
		return windows.EscapeCommFunction(h, windows.CLRBREAK)
	})
}

// Modem reads CTS/DSR/RI/DCD.
func (p *Port) Modem() (ModemStatus, error) {
	var ms ModemStatus
	var st uint32
	err := p.guard("GetCommModemStatus", func(h windows.Handle) error {
		return windows.GetCommModemStatus(h, &st)
	})
	if err != nil {
		return ms, err
	}
	ms.CTS = st&msCTSOn != 0
	ms.DSR = st&msDSROn != 0
	ms.RI = st&msRingOn != 0
	ms.DCD = st&msRLSDOn != 0
	return ms, nil
}

// CheckLine runs ClearCommError: it folds any line errors into the counters
// and returns the number of bytes waiting in the driver's input queue.
//
// This is the only way to see CE_FRAME / CE_RXPARITY / CE_OVERRUN /
// CE_RXOVER / CE_BREAK. Without it a line that is corrupting every byte
// looks exactly like a quiet one. It is also the cheapest honest liveness
// probe on a handle, which is why the supervisor calls it once a second: on
// a vanished adapter it fails immediately, instead of the vanish having to
// be inferred from a read-failure streak seconds later.
func (p *Port) CheckLine() (pending uint32, err error) {
	var ce uint32
	var cs windows.ComStat
	err = p.guard("ClearCommError", func(h windows.Handle) error {
		return windows.ClearCommError(h, &ce, &cs)
	})
	if err != nil {
		p.ceProbes.Add(1)
		return 0, err
	}
	if ce&ceFrameBit != 0 {
		p.ceFrame.Add(1)
	}
	if ce&ceRxParityBit != 0 {
		p.ceParity.Add(1)
	}
	if ce&ceOverrunBit != 0 {
		p.ceOverrun.Add(1)
	}
	if ce&ceRxOverBit != 0 {
		p.ceRxOver.Add(1)
	}
	if ce&ceBreakBit != 0 {
		p.ceBreak.Add(1)
	}
	return cs.CBInQue, nil
}

// Purge discards driver RX/TX queues. Not a clean slate — see Open.
func (p *Port) Purge() error {
	return p.guard("PurgeComm", func(h windows.Handle) error {
		return windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR)
	})
}

// Stats snapshot. Pure memory: safe to call from a UI thread at any time,
// including while the data path is failing.
func (p *Port) Stats() Stats {
	p.lastErrMu.Lock()
	le, la := p.lastErr, p.lastErrAt
	p.lastErrMu.Unlock()
	return Stats{
		Open:        !p.closed.Load() && !p.wedged.Load(),
		Watchdog:    p.wd.Load(),
		RxBytes:     p.rxBytes.Load(),
		TxBytes:     p.txBytes.Load(),
		Reads:       p.reads.Load(),
		Writes:      p.writes.Load(),
		Errors:      p.errs.Load(),
		ErrorStreak: p.errStreak.Load(),
		LastError:   le,
		LastErrorAt: la,
		Line: LineErrors{
			Frame:   p.ceFrame.Load(),
			Parity:  p.ceParity.Load(),
			Overrun: p.ceOverrun.Load(),
			RxOver:  p.ceRxOver.Load(),
			Break:   p.ceBreak.Load(),
			Probes:  p.ceProbes.Load(),
		},
		ProbeErrors:   p.probeErrs.Load(),
		Wedged:        p.wedged.Load(),
		TimedOutWaits: p.timedOutWaits.Load(),
	}
}

func (p *Port) Config() Config { return p.cfg }

// Wedged reports whether this port's handle was leaked rather than closed.
func (p *Port) Wedged() bool { return p.wedged.Load() }

// Close cancels pending I/O, joins the reader, then closes the handle.
// Idempotent, and bounded: it cannot block its caller indefinitely, which
// matters because the caller is often the operator clicking Close to escape
// a port that has just been unplugged.
//
// The handle is closed only once nothing can still be inside a syscall on
// it: the reader must have exited, no write may be in flight, and the handle
// guard must be held exclusively. If any of those cannot be established
// within its deadline the handle is LEAKED rather than closed (rule 3) — see
// leakHandle for why that is the safe direction.
func (p *Port) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.stopReader)

	// Cancel every pending request on this handle, for all threads. This is
	// what keeps the waits below short: a blocked read or a long write comes
	// back as ERROR_OPERATION_ABORTED almost at once.
	_ = windows.CancelIoEx(p.h, nil)

	if p.wedged.Load() {
		return ErrWedged // already leaked; never close it
	}

	select {
	case <-p.readerDone:
	case <-time.After(closeJoinMs * time.Millisecond):
		// The reader is still inside a syscall on h. Closing now is the
		// handle-recycling hazard, so do not.
		leakHandle(p)
		return fmt.Errorf("%s: reader did not exit within %d ms: %w", p.cfg.Name, closeJoinMs, ErrWedged)
	}

	// Drain the write gate so any in-flight Write is done with the handle.
	select {
	case p.writeGate <- struct{}{}:
		<-p.writeGate
	case <-time.After(writeGateWaitMs * time.Millisecond):
		leakHandle(p)
		return fmt.Errorf("%s: write still in flight after %d ms: %w", p.cfg.Name, writeGateWaitMs, ErrWedged)
	}

	// Exclusive: prove no RLock holder is left anywhere.
	if !p.lockHandleFor(writeGateWaitMs * time.Millisecond) {
		leakHandle(p)
		return fmt.Errorf("%s: handle still in use: %w", p.cfg.Name, ErrWedged)
	}
	err := windows.CloseHandle(p.h)
	p.hMu.Unlock()

	windows.CloseHandle(p.readEvt)
	windows.CloseHandle(p.writeEvt)
	return err
}

// lockHandleFor is hMu.Lock() with a deadline. sync.RWMutex offers TryLock
// but no timed wait, so this polls — the alternative is an unbounded wait
// inside Close, which is the hang.
func (p *Port) lockHandleFor(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if p.hMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *Port) ok() { p.errStreak.Store(0) }

// fail records a DATA-PATH failure: a read or a write. These feed the
// supervisor's streak detector.
func (p *Port) fail(err error) {
	p.errs.Add(1)
	p.errStreak.Add(1)
	p.noteErr(err)
}

// probe records an OUT-OF-BAND failure: a modem-status or line-error query.
// Deliberately kept out of Errors and ErrorStreak — a 1 Hz health probe
// against a vanished adapter would otherwise generate the exact streak the
// streak detector exists to find, and "the data path is dead" could no
// longer be distinguished from "my own polling is failing".
func (p *Port) probe(err error) {
	p.probeErrs.Add(1)
	p.noteErr(err)
}

func (p *Port) noteErr(err error) {
	p.lastErrMu.Lock()
	p.lastErr = err.Error()
	p.lastErrAt = time.Now()
	p.lastErrMu.Unlock()
	if p.onErr != nil {
		p.onErr(err)
	}
}
