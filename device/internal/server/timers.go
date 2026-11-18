package server

import (
	"time"

	"github.com/wilbowes/EchoMuse/pkg/led"
)

// The "timers" pattern: running timers on the ring, as the Voice Incubator
// firmware's VPE draws them (devices/voice-pe.yaml "Timer Tick", at
// 64ce6c2a). The whole ring is the longest timer's length (the scale); each
// timer, soonest first, lies on top of the later ones as far as it runs, an
// LED past the one before at least. Only the last (the longest) fades out
// within its last LED; a sooner one's LED is lit from its second half on. A
// dimmer LED steps round once a second through the soonest, unless paused.
// The times count down from when the pattern starts.

// TimerArc is one running timer: its time left when the pattern starts, and
// its color, brightness already applied.
type TimerArc struct {
	LeftMs int
	Color  [3]uint8
}

// timerStepDim is the stepping LED's share of its color: a third. The VPE's
// 230/255 shows through its ring's 2.8 gamma; the Echo's ring has none, and
// at a fifth of full brightness 90% could not be told apart (00LQ,
// 2026-11-18).
const timerStepDim = 85

// timersFrame is the ring for arcs (soonest first) elapsed into the pattern,
// with the stepping LED at step.
func timersFrame(arcs []TimerArc, scaleMs int, elapsed time.Duration, step int, paused bool) []led.Led {
	frame := blackFrame()
	n := len(arcs)
	if n == 0 {
		return frame
	}
	scale := float64(max(scaleMs, 1000))
	ends := make([]float64, n)
	before := 0.0
	for k, a := range arcs {
		left := max(float64(a.LeftMs)-float64(elapsed.Milliseconds()), 0)
		ends[k] = min(max(float64(numLEDs)*left/scale, before+1), float64(numLEDs))
		before = ends[k]
	}
	for k := n - 1; k >= 0; k-- {
		c := arcs[k].Color
		for i := 0; i < numLEDs && float64(i) < ends[k]; i++ {
			f := min(ends[k]-float64(i), 1)
			switch {
			case k == n-1:
				frame[i] = led.Led{ID: i, R: scale8(c[0], f), G: scale8(c[1], f), B: scale8(c[2], f)}
			case f >= 0.5:
				frame[i] = led.Led{ID: i, R: c[0], G: c[1], B: c[2]}
			}
		}
	}
	step %= numLEDs
	if !paused && float64(step+1) < ends[0] {
		l := frame[step]
		frame[step] = led.Led{ID: step, R: uint8(uint16(l.R) * timerStepDim / 255),
			G: uint8(uint16(l.G) * timerStepDim / 255), B: uint8(uint16(l.B) * timerStepDim / 255)}
	}
	return frame
}

func scale8(v uint8, f float64) uint8 { return uint8(float64(v) * f) }

// runTimers draws the timers every 100 ms until replaced (no TTL: its
// owner, the satellite, clears it), so the ring comes back by itself after
// anything that held it meanwhile (mute, the volume arc, identify).
func (s *Server) runTimers(gen int, spec AnimSpec) {
	start := time.Now()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !s.animCurrent(gen) {
			return
		}
		var elapsed time.Duration
		if !spec.TimersPaused {
			elapsed = time.Since(start)
		}
		s.SetLEDs(timersFrame(spec.Timers, spec.TimerScaleMs, elapsed, int(elapsed/time.Second), spec.TimersPaused), boolPtr(false))
		<-ticker.C
	}
}
