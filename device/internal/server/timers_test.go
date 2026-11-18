package server

import (
	"testing"
	"time"

	"github.com/wilbowes/EchoMuse/pkg/led"
)

func rgb(l led.Led) [3]uint8 { return [3]uint8{l.R, l.G, l.B} }

// Worked by hand from voice-pe.yaml's "Timer Tick" formula.
func TestTheTimersFrameIsTheVoicePEs(t *testing.T) {
	y := [3]uint8{255, 255, 0}
	m := [3]uint8{255, 0, 255}

	// One timer, half its scale left: ends = 6. LEDs 0..5 full, the rest
	// dark; the stepping LED at 0 is dimmed to 230/255.
	f := timersFrame([]TimerArc{{LeftMs: 30000, Color: y}}, 60000, 0, 0, false)
	if rgb(f[0]) != [3]uint8{230, 230, 0} || rgb(f[5]) != y || rgb(f[6]) != [3]uint8{} {
		t.Fatalf("one timer at half: %v %v %v", f[0], f[5], f[6])
	}
	// A quarter LED into the 7th: 12*31250/60000 = 6.25 -> LED 6 at 25%.
	f = timersFrame([]TimerArc{{LeftMs: 31250, Color: y}}, 60000, 0, 3, false)
	if rgb(f[6]) != [3]uint8{63, 63, 0} || rgb(f[3]) != [3]uint8{230, 230, 0} {
		t.Fatalf("partial last LED: %v, step %v", f[6], f[3])
	}
	// Two: the soonest (magenta, 10 s of a 60 s scale: 2 LEDs) on top of the
	// longest (yellow, 60 s: 12). The soonest's LEDs are its own; the rest
	// the longest's.
	f = timersFrame([]TimerArc{{LeftMs: 10000, Color: m}, {LeftMs: 60000, Color: y}}, 60000, 0, 5, false)
	if rgb(f[1]) != m || rgb(f[2]) != y || rgb(f[11]) != y {
		t.Fatalf("two timers: %v %v %v", f[1], f[2], f[11])
	}
	// The soonest keeps one LED of its own however little is left, and the
	// next one LED past it: ends 1 and 2 for 0.1 s and 0.2 s of 60.
	f = timersFrame([]TimerArc{{LeftMs: 100, Color: m}, {LeftMs: 200, Color: y}}, 60000, 0, 11, false)
	if rgb(f[0]) != m || rgb(f[1]) != y || rgb(f[2]) != [3]uint8{} {
		t.Fatalf("one LED each at least: %v %v %v", f[0], f[1], f[2])
	}
	// A sooner timer's partial LED: lit from its second half on only.
	// 12*12500/60000 = 2.5: LED 2 is half the sooner one's, so it is its.
	f = timersFrame([]TimerArc{{LeftMs: 12500, Color: m}, {LeftMs: 60000, Color: y}}, 60000, 0, 11, false)
	if rgb(f[2]) != m {
		t.Fatalf("a sooner timer's half LED: %v", f[2])
	}
	// 12*12000/60000 = 2.4: less than half, so LED 2 is the longer one's.
	f = timersFrame([]TimerArc{{LeftMs: 12000, Color: m}, {LeftMs: 60000, Color: y}}, 60000, 0, 11, false)
	if rgb(f[2]) != y {
		t.Fatalf("a sooner timer's sliver: %v", f[2])
	}
	// Counting down: 30 s left, 15 s elapsed, ends 3.
	f = timersFrame([]TimerArc{{LeftMs: 30000, Color: y}}, 60000, 15*time.Second, 0, false)
	if rgb(f[2]) == [3]uint8{} || rgb(f[3]) != [3]uint8{} {
		t.Fatalf("counted down: %v %v", f[2], f[3])
	}
	// Paused: no stepping LED, no count-down.
	f = timersFrame([]TimerArc{{LeftMs: 30000, Color: y}}, 60000, 0, 0, true)
	if rgb(f[0]) != y {
		t.Fatalf("paused, the step still dims: %v", f[0])
	}
	// A scale under a second is a second: 0.5 s of it is half the ring.
	if f := timersFrame([]TimerArc{{LeftMs: 500, Color: y}}, 0, 0, 0, true); rgb(f[5]) != y || rgb(f[6]) != [3]uint8{} {
		t.Fatalf("scale 0: %v %v", f[5], f[6])
	}
	if f := timersFrame(nil, 60000, 0, 0, false); rgb(f[0]) != [3]uint8{} {
		t.Fatal("no timers, a lit ring")
	}
}
