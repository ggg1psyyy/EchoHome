// Package taps counts taps of a button on the device itself: double and
// triple taps are told apart by the gap between a release and the next
// press, timed where they happen. Timed on arrival at the controller (its
// buttonMultiTapMs), WiFi jitter blurred a double into two singles
// (EchoMuse #115, whose fix this is).
//
// A tap is decided only when Window passes with no next press, so a single
// tap is reported Window late; that is the price of telling it from a
// double, and why counting is an opt-in.
package taps

import (
	"sync"
	"time"
)

// Counter counts taps. Safe for concurrent use; Emit runs on a timer's
// goroutine, or the caller's for a hold.
type Counter struct {
	// Window is how long after a release a next press still counts.
	Window time.Duration
	// Emit is told each sequence: its taps (1, 2, 3 or more).
	Emit func(taps int)

	mu    sync.Mutex
	n     int
	timer *time.Timer
	gen   int
}

// Press is a press: the window it falls in stays open until its release.
func (c *Counter) Press() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil && c.timer.Stop() {
		c.timer = nil // the sequence goes on
	}
}

// Tap is a release that was a tap: counted, and the window opened again.
func (c *Counter) Tap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.gen++
	g := c.gen
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(c.Window, func() { c.fire(g) })
}

// Hold is a release that was a hold: the taps before it, if any, are
// emitted first, so neither is lost.
func (c *Counter) Hold() {
	c.mu.Lock()
	n := c.n
	c.n = 0
	c.gen++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	if n > 0 {
		c.Emit(n)
	}
}

func (c *Counter) fire(g int) {
	c.mu.Lock()
	if g != c.gen {
		c.mu.Unlock()
		return
	}
	n := c.n
	c.n = 0
	c.timer = nil
	c.mu.Unlock()
	if n > 0 {
		c.Emit(n)
	}
}
