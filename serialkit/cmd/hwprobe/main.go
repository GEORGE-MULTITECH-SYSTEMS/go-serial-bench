//go:build windows

// hwprobe is a NON-DESTRUCTIVE identification pass over a real COM port.
//
// It answers the questions you must not guess at before running a bench on
// someone else's hardware:
//
//   - is the port actually there, and can it be opened at all
//   - what do the modem inputs read (CTS/DSR/RI/DCD) — measured, not assumed
//   - is anything transmitting, and at which of the common baud rates does
//     it decode without framing errors
//   - does the line show CE_FRAME / CE_PARITY / CE_OVERRUN damage
//
// It opens with DTR and RTS DEASSERTED, which is the whole reason it is safe
// to run against an ESP32-class board: on those, the bridge's DTR and RTS
// are wired to EN and IO0 (reset and boot-select), so an adapter that
// asserts them "helpfully" on open reboots the target or drops it into the
// download stub. Nothing here writes a byte to the port either.
//
// Usage: hwprobe COM3 [seconds]
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"serialkit/winserial"
)

// Baud rates worth trying, commonest first. A stream that decodes with zero
// framing errors at one rate and solid framing errors at the others is the
// rate; framing errors everywhere means either nothing is talking or the
// line is not 8N1.
var bauds = []uint32{115200, 9600, 921600, 460800, 230400, 57600, 38400, 19200, 74880}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: hwprobe COM3 [seconds-per-baud]")
		os.Exit(2)
	}
	name := strings.ToUpper(os.Args[1])
	per := 1200 * time.Millisecond
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			per = time.Duration(n) * time.Second
		}
	}

	fmt.Printf("== hwprobe %s ==\n", name)
	fmt.Printf("ports present: %v\n", mustPorts())
	present, err := winserial.Present(name)
	fmt.Printf("Present(%s) = %v (err=%v)\n", name, present, err)
	if !present {
		fmt.Println("not present — nothing to probe")
		os.Exit(1)
	}

	for _, baud := range bauds {
		probeOne(name, baud, per)
	}
	fmt.Println("\nnote: DTR and RTS were left DEASSERTED throughout; no byte was written.")
}

func mustPorts() []string {
	p, err := winserial.ListPorts()
	if err != nil {
		return []string{"<" + err.Error() + ">"}
	}
	return p
}

func probeOne(name string, baud uint32, per time.Duration) {
	var mu sync.Mutex
	var rx []byte
	var errCount int

	cfg := winserial.Config{
		Name: name, Baud: baud, DataBits: 8, Parity: "none", StopBits: "1",
		Handshake: "off",
		DTR:       false, // F7: these are reset/boot-select on ESP32 boards
		RTS:       false,
		ReadGapMs: 20,
	}
	p, err := winserial.Open(cfg, func(b []byte) {
		mu.Lock()
		if len(rx) < 4096 {
			rx = append(rx, b...)
		}
		mu.Unlock()
	}, func(error) {
		mu.Lock()
		errCount++
		mu.Unlock()
	})
	if err != nil {
		fmt.Printf("\n%7d  OPEN FAILED: %v\n", baud, err)
		return
	}
	defer p.Close()

	ms, mErr := p.Modem()
	time.Sleep(per)
	pending, clErr := p.CheckLine()
	st := p.Stats()

	mu.Lock()
	got := append([]byte(nil), rx...)
	ec := errCount
	mu.Unlock()

	fmt.Printf("\n%7d baud 8N1\n", baud)
	if mErr == nil {
		fmt.Printf("  modem   CTS=%v DSR=%v RI=%v DCD=%v\n", ms.CTS, ms.DSR, ms.RI, ms.DCD)
	} else {
		fmt.Printf("  modem   query failed: %v\n", mErr)
	}
	fmt.Printf("  rx      %d byte(s) in %v, reads=%d, inque=%d (clearcommerror err=%v)\n",
		len(got), per, st.Reads, pending, clErr)
	fmt.Printf("  line    frame=%d parity=%d overrun=%d rxover=%d break=%d probes=%d\n",
		st.Line.Frame, st.Line.Parity, st.Line.Overrun, st.Line.RxOver, st.Line.Break, st.Line.Probes)
	fmt.Printf("  health  dataErrs=%d streak=%d probeErrs=%d wedged=%v timedOutWaits=%d callbackErrs=%d\n",
		st.Errors, st.ErrorStreak, st.ProbeErrors, st.Wedged, st.TimedOutWaits, ec)
	if len(got) > 0 {
		fmt.Printf("  hex     %s\n", hexPreview(got, 48))
		fmt.Printf("  text    %q\n", textPreview(got, 160))
		fmt.Printf("  verdict %s\n", verdict(got))
	}
}

// verdict reports how much of the captured data is plain printable text.
// A stream that is ~all printable at one baud and hash at every other is
// decoding correctly; this is evidence, not proof, so it says a proportion
// rather than a yes.
func verdict(b []byte) string {
	printable := 0
	for _, c := range b {
		if (c >= 0x20 && c < 0x7f) || c == '\r' || c == '\n' || c == '\t' {
			printable++
		}
	}
	pct := 100 * printable / len(b)
	switch {
	case pct >= 95:
		return fmt.Sprintf("%d%% printable — this baud decodes cleanly", pct)
	case pct >= 50:
		return fmt.Sprintf("%d%% printable — partial, suspect wrong baud or mixed binary", pct)
	default:
		return fmt.Sprintf("%d%% printable — looks like the wrong baud", pct)
	}
}

func hexPreview(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", x)
	}
	if len(b) == n {
		sb.WriteString(" ...")
	}
	return sb.String()
}

func textPreview(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	r := make([]rune, 0, len(b))
	for _, x := range b {
		switch {
		case x == '\n' || x == '\r' || x == '\t' || (x >= 0x20 && x < 0x7f):
			r = append(r, rune(x))
		default:
			r = append(r, '·')
		}
	}
	return string(r)
}
