//go:build windows

// hwreset pulses an ESP32-class board's reset line through the USB-serial
// bridge's modem outputs and captures whatever the board says on the way up.
//
// It exists to identify what is flashed on a board without erasing it: the
// ESP32 ROM prints a fixed banner at 115200 on every boot, naming the chip
// and the boot mode, and the application usually prints something too.
//
// # Why this is the destructive lever in this package
//
// On ESP32 devkits the bridge's DTR and RTS are not flow control. They are
// wired through a two-transistor circuit to EN (chip reset) and IO0 (boot
// mode select), which is why every other tool here opens with both
// DEASSERTED (F7): an adapter that asserts them "helpfully" on open reboots
// the target, or drops it into the download stub where it answers nothing.
//
// This tool asserts them deliberately, and only in the combination that
// means "reset and run the application":
//
//	DTR=false  -> IO0 HIGH -> normal boot (NOT the download stub)
//	RTS=true   -> EN  LOW  -> held in reset
//	RTS=false  -> EN  HIGH -> released, boots the flashed application
//
// Holding DTR true at release instead is what selects the serial bootloader.
// This tool never does that; it cannot put the board into a state where the
// flash is being written.
//
// Usage: hwreset COM3 [capture-seconds]
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

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: hwreset COM3 [capture-seconds]")
		os.Exit(2)
	}
	name := strings.ToUpper(os.Args[1])
	capture := 3 * time.Second
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			capture = time.Duration(n) * time.Second
		}
	}

	var mu sync.Mutex
	var rx []byte
	start := time.Now()

	// 115200 8N1 is the ESP32 ROM's fixed console rate. The application may
	// use another rate; the banner will still decode at this one.
	p, err := winserial.Open(winserial.Config{
		Name: name, Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1",
		Handshake: "off", DTR: false, RTS: false, ReadGapMs: 20,
	}, func(b []byte) {
		mu.Lock()
		rx = append(rx, b...)
		mu.Unlock()
	}, func(e error) {
		fmt.Printf("[%7.3fs] ERR %v\n", time.Since(start).Seconds(), e)
	})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer p.Close()

	if ms, err := p.Modem(); err == nil {
		fmt.Printf("modem before reset: CTS=%v DSR=%v RI=%v DCD=%v\n", ms.CTS, ms.DSR, ms.RI, ms.DCD)
	}

	// Let the port settle so bytes predating the reset are not mistaken for
	// boot output.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	pre := len(rx)
	rx = rx[:0]
	mu.Unlock()
	fmt.Printf("discarded %d pre-reset byte(s)\n", pre)

	fmt.Println("asserting RTS (EN low) with DTR deasserted (IO0 high = normal boot)")
	if err := p.SetDTR(false); err != nil {
		fmt.Println("SetDTR:", err)
	}
	if err := p.SetRTS(true); err != nil {
		fmt.Println("SetRTS:", err)
	}
	time.Sleep(150 * time.Millisecond)
	fmt.Println("releasing RTS (EN high) — board should boot its application")
	if err := p.SetRTS(false); err != nil {
		fmt.Println("SetRTS:", err)
	}

	time.Sleep(capture)

	mu.Lock()
	got := append([]byte(nil), rx...)
	mu.Unlock()
	st := p.Stats()

	fmt.Printf("\ncaptured %d byte(s) in %v after the reset\n", len(got), capture)
	fmt.Printf("line errors: frame=%d parity=%d overrun=%d rxover=%d break=%d\n",
		st.Line.Frame, st.Line.Parity, st.Line.Overrun, st.Line.RxOver, st.Line.Break)
	if len(got) == 0 {
		fmt.Println("\nNOTHING. Either the reset line is not wired to this bridge's RTS,")
		fmt.Println("or the flashed application prints nothing and the ROM banner is")
		fmt.Println("suppressed. Both are facts about the board, not about the port.")
		return
	}
	fmt.Println("\n---- boot output ----")
	fmt.Println(render(got))
	fmt.Println("---- end ----")
}

// render shows the capture as text, keeping newlines and marking every
// non-printable byte, so a banner that decoded at the wrong rate is obvious
// rather than silently pretty.
func render(b []byte) string {
	var sb strings.Builder
	for _, x := range b {
		switch {
		case x == '\n' || x == '\r' || x == '\t' || (x >= 0x20 && x < 0x7f):
			sb.WriteByte(x)
		default:
			fmt.Fprintf(&sb, "<%02X>", x)
		}
	}
	return sb.String()
}
