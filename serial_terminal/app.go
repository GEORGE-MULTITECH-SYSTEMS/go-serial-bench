package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serialkit/supervisor"
	"serialkit/winserial"
)

// App is the Wails adapter — nothing more. All port ownership and all
// policy (health AND-gate, degradation streak, bounded recycle) now live
// in the serialkit/supervisor package; App implements supervisor.Sink and
// turns what the supervisor says into WebView2 events, and turns what the
// UI form sends into a winserial.Config.
//
// The rule this file exists to keep: nothing in serialkit knows that a UI
// exists. Everything Wails-shaped is here.
type App struct {
	ctx context.Context
	sup *supervisor.Supervisor
}

func NewApp() *App {
	a := &App{}
	a.sup = supervisor.New(a)
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.sup.Start()
}

func (a *App) shutdown(ctx context.Context) { a.sup.Stop() }

// ---- events to the UI -------------------------------------------------

func (a *App) emit(name string, data any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, name, data)
	}
}

type LogLine struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"` // "rx" | "tx" | "info" | "err"
	Hex  string `json:"hex"`
	Text string `json:"text"`
	N    int    `json:"n"`
}

func now() string { return time.Now().Format("15:04:05.000") }

// ---- supervisor.Sink ---------------------------------------------------

func (a *App) OnLog(kind, text string) {
	a.emit("log", LogLine{TS: now(), Kind: kind, Text: text})
}

func (a *App) OnRx(b []byte) {
	a.emit("log", LogLine{TS: now(), Kind: "rx", Hex: hexSpaced(b), Text: printable(b), N: len(b)})
}

func (a *App) OnTx(b []byte, n int) {
	a.emit("log", LogLine{TS: now(), Kind: "tx", Hex: hexSpaced(b), Text: printable(b), N: n})
}

func (a *App) OnState(st supervisor.State)  { a.emit("state", st) }
func (a *App) OnHealth(st supervisor.State) { a.emit("health", st) }

// ---- bound methods -----------------------------------------------------

// ListPorts returns COM names, numerically sorted.
func (a *App) ListPorts() ([]string, error) { return supervisor.ListPorts() }

// PortConfig is what the UI form sends. Strings so the vanilla-JS side
// needs no type juggling. Parsing a form is a UI concern and stays here.
type PortConfig struct {
	Name           string `json:"name"`
	Baud           string `json:"baud"`
	DataBits       string `json:"dataBits"`
	Parity         string `json:"parity"`
	StopBits       string `json:"stopBits"`
	Handshake      string `json:"handshake"`
	DTR            bool   `json:"dtr"`
	RTS            bool   `json:"rts"`
	ReadGapMs      string `json:"readGapMs"`
	WriteTimeoutMs string `json:"writeTimeoutMs"`
}

func (a *App) Open(c PortConfig) error {
	baud, err := strconv.ParseUint(strings.TrimSpace(c.Baud), 10, 32)
	if err != nil || baud == 0 {
		return fmt.Errorf("bad baud %q", c.Baud)
	}
	db, _ := strconv.Atoi(c.DataBits)
	if db < 5 || db > 8 {
		db = 8
	}
	gap, _ := strconv.Atoi(c.ReadGapMs)
	wt, _ := strconv.Atoi(c.WriteTimeoutMs)
	return a.sup.Open(winserial.Config{
		Name: strings.ToUpper(strings.TrimSpace(c.Name)), Baud: uint32(baud), DataBits: byte(db),
		Parity: c.Parity, StopBits: c.StopBits, Handshake: c.Handshake,
		DTR: c.DTR, RTS: c.RTS, ReadGapMs: uint32(gap), WriteTimeoutMs: uint32(wt),
	})
}

func (a *App) Close() error { return a.sup.Close() }

// Send QUEUES either raw text or a hex string ("fe dc ba" / "FEDCBA").
// Framing the payload is a UI concern; the supervisor is handed bytes.
//
// It returns as soon as the bytes are ACCEPTED, never when they are gone.
// This is a bound method called from the WebView2 thread, and a serial write
// legitimately takes as long as the bytes need on the wire (8 KiB at 9600
// baud is ~9.4 s); waiting for it here is a frozen window. The outcome
// arrives as a TX or ERR log line, like every other asynchronous fact about
// the port.
func (a *App) Send(text string, isHex bool, appendCR, appendLF bool) error {
	var b []byte
	if isHex {
		clean := strings.Map(func(r rune) rune {
			if strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return r
			}
			return -1
		}, text)
		// Refuse input that contained nothing usable, rather than sending
		// zero bytes and reporting success. A zero-byte write logs as a
		// normal TX line, so the mistake looks like the device ignoring a
		// command it was never actually sent.
		if strings.TrimSpace(text) != "" && clean == "" {
			return fmt.Errorf("no hex digits in %q — untick HEX to send it as text", text)
		}
		if len(clean)%2 != 0 {
			return fmt.Errorf("odd number of hex digits (%d) in %q", len(clean), text)
		}
		var err error
		b, err = hex.DecodeString(clean)
		if err != nil {
			return err
		}
	} else {
		b = []byte(text)
	}
	if appendCR {
		b = append(b, '\r')
	}
	if appendLF {
		b = append(b, '\n')
	}
	// A write of nothing is never what was meant, and "TX (0)" looks enough
	// like success to hide the mistake.
	if len(b) == 0 {
		return fmt.Errorf("nothing to send")
	}
	return a.sup.WriteAsync(b)
}

func (a *App) SetDTR(on bool) error    { return a.sup.SetDTR(on) }
func (a *App) SetRTS(on bool) error    { return a.sup.SetRTS(on) }
func (a *App) SendBreak(ms int) error  { return a.sup.SendBreak(time.Duration(ms) * time.Millisecond) }
func (a *App) Purge() error            { return a.sup.Purge() }
func (a *App) SetAutoRecycle(on bool)  { a.sup.SetAutoRecycle(on) }
func (a *App) State() supervisor.State { return a.sup.State() }

// ---- presentation helpers ----------------------------------------------

func hexSpaced(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", x)
	}
	return sb.String()
}

func printable(b []byte) string {
	r := make([]rune, 0, len(b))
	for _, x := range b {
		if x >= 0x20 && x < 0x7f {
			r = append(r, rune(x))
		} else if x == '\r' {
			r = append(r, '␍')
		} else if x == '\n' {
			r = append(r, '␊')
		} else {
			r = append(r, '·')
		}
	}
	return string(r)
}
