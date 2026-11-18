package taps

import (
	"sync"
	"testing"
	"time"
)

type rec struct {
	mu  sync.Mutex
	got []int
}

func (r *rec) emit(n int) { r.mu.Lock(); r.got = append(r.got, n); r.mu.Unlock() }
func (r *rec) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.got...)
}

const w = 60 * time.Millisecond

func tap(c *Counter, gap time.Duration) {
	c.Press()
	c.Tap()
	time.Sleep(gap)
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSingleDoubleAndTripleAreToldApart(t *testing.T) {
	r := &rec{}
	c := &Counter{Window: w, Emit: r.emit}
	tap(c, 3*w) // single
	tap(c, w/4) // double
	tap(c, 3*w)
	tap(c, w/4) // triple
	tap(c, w/4)
	tap(c, 3*w)
	if got := r.seen(); !equal(got, []int{1, 2, 3}) {
		t.Fatalf("%v", got)
	}
}

// The window runs from a release to the next press: a slow second press
// that came in time still counts, however long it is held as a tap.
func TestAPressInTimeHoldsTheWindowOpen(t *testing.T) {
	r := &rec{}
	c := &Counter{Window: w, Emit: r.emit}
	c.Press()
	c.Tap()
	time.Sleep(w / 2)
	c.Press()
	time.Sleep(2 * w) // a long tap: past the window, but pressed within it
	c.Tap()
	time.Sleep(3 * w)
	if got := r.seen(); !equal(got, []int{2}) {
		t.Fatalf("%v", got)
	}
}

func TestAHoldEndsTheSequenceWithNothingLost(t *testing.T) {
	r := &rec{}
	c := &Counter{Window: w, Emit: r.emit}
	c.Press()
	c.Tap()
	c.Press()
	c.Hold() // tap then hold: the tap is reported, the hold is the caller's
	time.Sleep(3 * w)
	if got := r.seen(); !equal(got, []int{1}) {
		t.Fatalf("%v", got)
	}
	c.Press()
	c.Hold() // a hold alone: nothing counted
	time.Sleep(3 * w)
	if got := r.seen(); !equal(got, []int{1}) {
		t.Fatalf("%v", got)
	}
}
