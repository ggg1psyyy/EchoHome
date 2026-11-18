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
// within its last LED; a sooner one's LED is lit from its second half on.
// One LED steps round the whole ring once a second, unless paused, like a
// second hand: dark where the ring is lit, faintly lit in the soonest's
// colour where it is dark. (The VPE dims an LED of the soonest's arc alone,
// so its tick vanishes as the timer runs down; on an Echo, without the VPE's
// gamma, a dimmed LED was barely seen either; 00LQ, 2026-11-18.) The times
// count down from when the pattern starts.

// TimerArc is one running timer: its time left when the pattern starts, and
// its color, brightness already applied.
type TimerArc struct {
	LeftMs int
	Color  [3]uint8
}

// timerStepLit is the stepping LED's share of the soonest timer's colour
// where the ring is dark.
const timerStepLit = 0.5

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
	if !paused {
		step %= numLEDs
		if l := frame[step]; l.R|l.G|l.B != 0 {
			frame[step] = led.Led{ID: step}
		} else {
			c := arcs[0].Color
			frame[step] = led.Led{ID: step, R: scale8(c[0], timerStepLit), G: scale8(c[1], timerStepLit), B: scale8(c[2], timerStepLit)}
		}
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
